package cluster

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feellmoose/gridkv/internal/mem_storage"
	"github.com/feellmoose/gridkv/internal/utils/cache"
	"github.com/feellmoose/gridkv/internal/utils/executor"
	"github.com/feellmoose/gridkv/internal/utils/hlc"
	"github.com/feellmoose/gridkv/internal/utils/logging"
)

// Pool size constants for object pools
const (
	targetOpsMapInitialSize    = 8
	keyTargetsCacheInitialSize = 16
	nodeIDToAddrInitialSize    = 16
	stringSliceInitialSize     = 16
	syncOpSliceInitialSize     = 32
	initialVersionMapSize      = 1024 // Pre-allocated size for version tracking map
)

// Object pools for reducing allocations in hot paths
var (
	// Map pool for targetOpsMap (map[string][]*SyncOperation)
	targetOpsMapPool = sync.Pool{
		New: func() interface{} {
			return make(map[string][]*mem_storage.SyncOperation, targetOpsMapInitialSize)
		},
	}

	// Map pool for keyTargetsCache (map[string][]string)
	keyTargetsCachePool = sync.Pool{
		New: func() interface{} {
			return make(map[string][]string, keyTargetsCacheInitialSize)
		},
	}

	// Map pool for nodeIDToAddr (map[string]string)
	nodeIDToAddrPool = sync.Pool{
		New: func() interface{} {
			return make(map[string]string, nodeIDToAddrInitialSize)
		},
	}

	// Error channel pool for error channels
	errorChanPool = sync.Pool{
		New: func() interface{} {
			return make(chan error, 1)
		},
	}
)

// Writer handles write operations with batch processing
type Writer interface {
	Set(ctx context.Context, key string, item *mem_storage.StoredItem) error
	BatchSet(ctx context.Context, items map[string]*mem_storage.StoredItem) error
	Delete(ctx context.Context, key string, version int64) error
}

type writer struct {
	nodeID   string
	hlc      *hlc.HLC
	store    *mem_storage.MemStorage
	ring     HashRing
	gossip   Gossip
	executor *executor.Exec
	cache    *cache.Cache
	member   MemberMgr

	batchThreshold int
	batchWindow    time.Duration
	batchCount     atomic.Int64
	flushTimer     *time.Timer
	flushMu        sync.Mutex
	replicaCount   int
	flushPending   atomic.Bool

	lastVersions   map[string]int64
	lastVersionsMu sync.RWMutex

	// Pending ops for non-replica keys (to be pushed to replicas)
	pendingOps []*mem_storage.SyncOperation
	pendingMu  sync.Mutex

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

type writerConfig struct {
	NodeID         string
	HLC            *hlc.HLC
	Store          *mem_storage.MemStorage
	Ring           HashRing
	Gossip         Gossip
	Cache          *cache.Cache
	Executor       *executor.Exec
	Member         MemberMgr
	BatchThreshold int
	BatchWindow    time.Duration
	ReplicaCount   int
}

var _ Writer = (*writer)(nil)

func newWriter(cfg writerConfig) (*writer, error) {
	// Validate required dependencies
	if cfg.Store == nil {
		return nil, fmt.Errorf("store cannot be nil")
	}
	if cfg.HLC == nil {
		return nil, fmt.Errorf("HLC cannot be nil")
	}
	if cfg.Executor == nil {
		return nil, fmt.Errorf("executor cannot be nil")
	}
	if cfg.Ring == nil {
		return nil, fmt.Errorf("ring cannot be nil")
	}

	// Default configuration constants
	const (
		defaultBatchThreshold = 200
		defaultBatchWindow    = 10 * time.Millisecond
		defaultReplicaCount   = 3
	)

	// Set defaults for optional parameters - optimized for high throughput
	if cfg.BatchThreshold <= 0 {
		cfg.BatchThreshold = defaultBatchThreshold
	}
	if cfg.BatchWindow <= 0 {
		cfg.BatchWindow = defaultBatchWindow
	}
	if cfg.ReplicaCount <= 0 {
		cfg.ReplicaCount = defaultReplicaCount
	}

	w := &writer{
		nodeID:         cfg.NodeID,
		hlc:            cfg.HLC,
		store:          cfg.Store,
		ring:           cfg.Ring,
		gossip:         cfg.Gossip,
		cache:          cfg.Cache,
		executor:       cfg.Executor,
		member:         cfg.Member,
		batchThreshold: cfg.BatchThreshold,
		batchWindow:    cfg.BatchWindow,
		flushTimer:     time.NewTimer(cfg.BatchWindow),
		replicaCount:   cfg.ReplicaCount,
		stopCh:         make(chan struct{}),
		lastVersions:   make(map[string]int64, initialVersionMapSize), // Pre-allocate for performance
	}

	w.wg.Add(1)
	go w.flushLoop()

	return w, nil
}

// isLocalReplica checks if local node is a replica for the given targets
func (w *writer) isLocalReplica(targets []string) bool {
	for _, target := range targets {
		if target == w.nodeID {
			return true
		}
	}
	return false
}

// storeOrQueue stores item locally always, and queues for gossip to other replicas
func (w *writer) storeOrQueue(key string, item *mem_storage.StoredItem, opType mem_storage.OpType) error {
	targets := w.ring.GetN(key, w.replicaCount)
	if len(targets) == 0 {
		return fmt.Errorf("no replica nodes available for key %s", key)
	}

	// Always store locally first for immediate consistency
	if err := w.store.Set(key, item); err != nil {
		return fmt.Errorf("failed to store key %s locally: %v", key, err)
	}

	// Queue for replication to other replicas (exclude local node)
	w.pendingMu.Lock()
	for _, target := range targets {
		if target != w.nodeID {
			w.pendingOps = append(w.pendingOps, &mem_storage.SyncOperation{
				Key:    key,
				OpType: opType,
				Item:   item.DeepCopy(),
			})
		}
	}
	w.pendingMu.Unlock()

	// Trigger batch after queuing to ensure ops are included
	w.triggerBatch()
	if w.cache != nil {
		w.cache.Delete(key)
	}
	return nil
}

func (w *writer) Set(ctx context.Context, key string, item *mem_storage.StoredItem) error {
	hlcStr := w.hlc.Now()
	version := hlcToInt64(hlcStr)

	w.lastVersionsMu.Lock()
	lastVersion, exists := w.lastVersions[key]
	if exists && version <= lastVersion {
		logging.Warn("version non-monotonic detected", "node", w.nodeID, "key", key,
			"current_version", version, "last_version", lastVersion)
		// Force monotonicity: use lastVersion + 1 if current is not greater
		version = lastVersion + 1
	}
	w.lastVersions[key] = version

	// Limit map size to prevent memory leak (keep last 10K keys)
	const (
		maxVersions     = 10000
		cleanupFraction = 5 // Remove 20% (1/5) when limit exceeded
	)
	if len(w.lastVersions) > maxVersions {
		// Simple cleanup: remove oldest 20% of entries
		// In practice, this is rarely needed as keys are reused
		toRemove := len(w.lastVersions) - maxVersions + maxVersions/cleanupFraction
		count := 0
		for k := range w.lastVersions {
			if count >= toRemove {
				break
			}
			delete(w.lastVersions, k)
			count++
		}
	}
	w.lastVersionsMu.Unlock()

	newItem := &mem_storage.StoredItem{
		Version:  version,
		ExpireAt: item.ExpireAt,
		Value:    item.Value,
		Key:      key,
	}

	if err := w.storeOrQueue(key, newItem, mem_storage.OpSet); err != nil {
		return err
	}

	w.triggerBatch()
	return nil
}

func (w *writer) BatchSet(ctx context.Context, items map[string]*mem_storage.StoredItem) error {
	if len(items) == 0 {
		return nil
	}

	hlcStr := w.hlc.Now()
	version := hlcToInt64(hlcStr)

	// Cache GetN results for same keys to reduce ring lookups
	keyTargetsCache := keyTargetsCachePool.Get().(map[string][]string)
	defer func() {
		// Clear and return to pool
		for k := range keyTargetsCache {
			delete(keyTargetsCache, k)
		}
		keyTargetsCachePool.Put(keyTargetsCache)
	}()
	getTargets := func(key string) []string {
		if cached, ok := keyTargetsCache[key]; ok {
			return cached
		}
		targets := w.ring.GetN(key, w.replicaCount)
		keyTargetsCache[key] = targets
		return targets
	}

	for key, item := range items {
		newItem := &mem_storage.StoredItem{
			Version:  version,
			ExpireAt: item.ExpireAt,
			Value:    item.Value,
			Key:      key,
		}

		// Use cached targets
		targets := getTargets(key)
		if len(targets) == 0 {
			continue
		}

		if w.isLocalReplica(targets) {
			// Store locally (error ignored as this is async replication path)
			if err := w.store.Set(key, newItem); err != nil {
				logging.Warn("failed to store locally during flush", "node", w.nodeID, "key", key, "error", err)
			}
			if w.cache != nil {
				w.cache.Delete(key)
			}
		} else {
			w.pendingMu.Lock()
			w.pendingOps = append(w.pendingOps, &mem_storage.SyncOperation{
				Key:    key,
				OpType: mem_storage.OpSet,
				Item:   newItem.DeepCopy(),
			})
			w.pendingMu.Unlock()
		}
	}

	w.triggerBatch()
	return nil
}

func (w *writer) Delete(ctx context.Context, key string, version int64) error {
	hlcStr := w.hlc.Now()
	newVersion := hlcToInt64(hlcStr)
	if newVersion <= version {
		newVersion = version + 1
	}

	// Ensure version is positive for tombstone detection
	if newVersion <= 0 {
		newVersion = 1
	}

	tombstone := &mem_storage.StoredItem{
		Version:  newVersion,
		ExpireAt: time.Time{},
		Value:    nil,
		Key:      key,
	}

	if err := w.storeOrQueue(key, tombstone, mem_storage.OpDelete); err != nil {
		return err
	}

	w.triggerBatch()
	return nil
}

func (w *writer) triggerBatch() {
	count := w.batchCount.Add(1)

	// Check if we should flush immediately (batch threshold reached)
	if int(count) >= w.batchThreshold {
		// Use CAS to avoid duplicate flush calls
		if w.flushPending.CompareAndSwap(false, true) {
			w.flushAsync()
		}
		return
	}

	// Check pending ops size to prevent accumulation
	w.pendingMu.Lock()
	pendingSize := len(w.pendingOps)
	w.pendingMu.Unlock()

	if pendingSize > w.batchThreshold*10 {
		if w.flushPending.CompareAndSwap(false, true) {
			w.flushAsync()
			return
		}
	}
	w.ensureTimerRunning()
}

// stopTimer safely stops the timer and drains its channel
// Must be called with flushMu lock held
func (w *writer) stopTimer() {
	if !w.flushTimer.Stop() {
		select {
		case <-w.flushTimer.C:
		default:
		}
	}
}

// ensureTimerRunning ensures the flush timer is running for time-based triggering
func (w *writer) ensureTimerRunning() {
	w.flushMu.Lock()
	defer w.flushMu.Unlock()

	// Stop timer if running and drain channel
	w.stopTimer()

	// Reset timer
	w.flushTimer.Reset(w.batchWindow)
}

// flushAsync performs flush asynchronously with proper error handling
func (w *writer) flushAsync() {
	if err := w.executor.Do(func() {
		defer w.flushPending.Store(false)
		w.flush()
	}); err != nil {
		// Executor error - retry with exponential backoff
		logging.Debug("flush executor error, will retry", "node", w.nodeID, "error", err)
		// Retry in a goroutine with backoff to avoid blocking
		// Use executor to prevent goroutine leak
		_ = w.executor.Do(func() {
			time.Sleep(50 * time.Millisecond)
			if w.flushPending.CompareAndSwap(true, false) {
				w.triggerBatch() // Retry flush trigger
			}
		})
	}
}

func (w *writer) flushLoop() {
	defer w.wg.Done()

	lastGC := time.Now()
	gcInterval := 10 * time.Minute // Periodic GC hint for long-running processes

	for {
		select {
		case <-w.stopCh:
			// Final flush before exit
			w.flushMu.Lock()
			// Call flushInternal to avoid double locking
			w.flushInternal()
			w.flushMu.Unlock()
			return
		case <-w.flushTimer.C:
			// Periodic GC hint for long-running processes
			if time.Since(lastGC) > gcInterval {
				runtime.GC()
				lastGC = time.Now()
			}

			// Use CAS to avoid duplicate flush calls
			if w.flushPending.CompareAndSwap(false, true) {
				// flushAsync will reset timer after flush completes
				w.flushAsync()
			} else {
				// If flush is already pending, reset timer to avoid missing next cycle
				w.flushMu.Lock()
				w.flushTimer.Reset(w.batchWindow)
				w.flushMu.Unlock()
			}
		}
	}
}

// Stop stops the writer and waits for flush loop to finish
func (w *writer) Stop() {
	w.stopOnce.Do(func() {
		close(w.stopCh)
		// Stop timer to prevent leaks
		w.flushMu.Lock()
		w.stopTimer()
		w.flushMu.Unlock()
	})
	w.wg.Wait()
}

// lifecycle.Component implementation
func (w *writer) Name() string                    { return "writer" }
func (w *writer) Start(ctx context.Context) error { return nil }
func (w *writer) Close(ctx context.Context) error {
	w.Stop()
	return nil
}

func (w *writer) flush() {
	// Try to acquire lock without blocking if possible
	// This reduces contention in high-concurrency scenarios
	select {
	case <-w.stopCh:
		// Stopping, use locked version
		w.flushMu.Lock()
		w.flushInternal()
		// Reset timer for next flush cycle
		w.flushTimer.Reset(w.batchWindow)
		w.flushMu.Unlock()
		return
	default:
		// Normal flush with lock
		w.flushMu.Lock()
		w.flushInternal()
		// Reset timer for next flush cycle
		w.flushTimer.Reset(w.batchWindow)
		w.flushMu.Unlock()
	}
}

// flushInternal does the actual flush work without acquiring lock (caller must hold lock)
func (w *writer) flushInternal() {
	// Get ops from store (for replica keys)
	ops, err := w.store.GetSyncBuffer()
	if err != nil {
		ops = nil
	}

	// Get pending ops (for non-replica keys)
	// Limit pending ops size to prevent memory accumulation
	// Use dynamic limit based on batch threshold for better scaling
	// Increased multiplier for high-concurrency scenarios
	const pendingOpsMultiplier = 200 // Max pending ops = batchThreshold * multiplier (increased from 50)
	w.pendingMu.Lock()
	pendingOps := w.pendingOps
	maxPendingOps := w.batchThreshold * pendingOpsMultiplier
	if len(pendingOps) > maxPendingOps {
		// Keep only the most recent ops to prevent unbounded growth
		dropped := len(w.pendingOps) - maxPendingOps
		pendingOps = pendingOps[dropped:]
		// Only log occasionally to reduce overhead
		if dropped > w.batchThreshold {
			logging.Warn("pending ops limit reached, dropping oldest", "node", w.nodeID, "dropped", dropped, "remaining", maxPendingOps, "threshold", w.batchThreshold)
		}
	}
	w.pendingOps = nil // Clear pending ops
	w.pendingMu.Unlock()

	// Combine ops
	if len(pendingOps) > 0 {
		if ops == nil {
			ops = pendingOps
		} else {
			ops = append(ops, pendingOps...)
		}
	}

	if len(ops) == 0 {
		// Reset counter even if no ops (timer reset handled by caller)
		w.batchCount.Store(0)
		return
	}

	// Reset counter atomically after getting buffer to avoid race condition
	// This ensures we don't miss ops that arrive during flush
	w.batchCount.Store(0)

	targetOpsMap := targetOpsMapPool.Get().(map[string][]*mem_storage.SyncOperation)
	defer func() {
		// Clean up maps before returning to pool
		for k := range targetOpsMap {
			delete(targetOpsMap, k)
		}
		targetOpsMapPool.Put(targetOpsMap)
	}()

	keyTargetsCache := keyTargetsCachePool.Get().(map[string][]string)
	defer func() {
		for k := range keyTargetsCache {
			delete(keyTargetsCache, k)
		}
		keyTargetsCachePool.Put(keyTargetsCache)
	}()

	for _, op := range ops {
		if op.Key == "" {
			continue
		}

		// Get replica nodes (use cache if available)
		targets, cached := keyTargetsCache[op.Key]
		if !cached {
			targets = w.ring.GetN(op.Key, w.replicaCount)
			if len(targets) > 0 {
				keyTargetsCache[op.Key] = targets
			}
		}
		if len(targets) == 0 {
			continue
		}

		// Directly append to target ops (exclude local node, already stored)
		for _, target := range targets {
			if target != w.nodeID {
				targetOpsMap[target] = append(targetOpsMap[target], op)
			}
		}
	}

	// Push to targets (convert nodeID to address)
	nodeIDToAddr := nodeIDToAddrPool.Get().(map[string]string)
	defer func() {
		for k := range nodeIDToAddr {
			delete(nodeIDToAddr, k)
		}
		nodeIDToAddrPool.Put(nodeIDToAddr)
	}()
	if w.member != nil {
		members := w.member.Members()
		for _, m := range members {
			nodeIDToAddr[m.NodeID] = m.Address
		}
	}
	// Push to targets using gossip protocol (follows README specification)
	if gg, ok := w.gossip.(*gossip); ok {
		for targetNodeID, targetOps := range targetOpsMap {
			if len(targetOps) == 0 {
				continue
			}
			targetAddr := nodeIDToAddr[targetNodeID]
			if targetAddr == "" {
				logging.Debug("Writer skipping target with empty address", "node", w.nodeID, "target", targetNodeID)
				continue
			}

			// Convert targetNodeID to address for gossip
			targetAddrs := []string{targetAddr}

			// Use gossip.Push() as specified in README
			// Push to all targets to ensure complete replication
			err := gg.Push(targetOps, targetAddrs)
			if err != nil {
				logging.Debug("gossip push failed", "node", w.nodeID, "target", targetNodeID, "ops_count", len(targetOps), "error", err)
			}
		}
	}

	// Timer reset handled by caller (ensureTimerRunning or flushLoop)
}

// hlcToInt64 converts HLC string to int64 for version comparison
// HLC format: "nodeID:timestamp:counter"
// We encode it as: timestamp (high 48 bits) + counter (low 16 bits)
// This preserves monotonicity while preventing overflow
func hlcToInt64(hlcStr string) int64 {
	if hlcStr == "" {
		return time.Now().UnixNano()
	}

	// Manual parsing: "nodeID:timestamp:counter"
	// Find first colon (skip nodeID)
	idx1 := -1
	for i := 0; i < len(hlcStr); i++ {
		if hlcStr[i] == ':' {
			idx1 = i
			break
		}
	}
	if idx1 < 0 || idx1 >= len(hlcStr)-1 {
		return time.Now().UnixNano()
	}

	// Find second colon
	idx2 := -1
	for i := idx1 + 1; i < len(hlcStr); i++ {
		if hlcStr[i] == ':' {
			idx2 = i
			break
		}
	}
	if idx2 < 0 || idx2 >= len(hlcStr)-1 {
		return time.Now().UnixNano()
	}

	// Parse timestamp (between first and second colon)
	var ts int64
	tsStr := hlcStr[idx1+1 : idx2]
	for i := 0; i < len(tsStr); i++ {
		c := tsStr[i]
		if c < '0' || c > '9' {
			return time.Now().UnixNano()
		}
		ts = ts*10 + int64(c-'0')
	}

	// Parse counter (after second colon)
	var ctr uint64
	ctrStr := hlcStr[idx2+1:]
	for i := 0; i < len(ctrStr); i++ {
		c := ctrStr[i]
		if c < '0' || c > '9' {
			break
		}
		ctr = ctr*10 + uint64(c-'0')
	}

	// Use 48 bits for timestamp (high bits) + 16 bits for counter (low bits)
	// This provides ~8.9 years of nanosecond precision and 65535 counter values
	// Truncate timestamp to 48 bits to prevent overflow
	tsTruncated := ts & 0xFFFFFFFFFFFF // 48 bits

	// Max counter: (2^16)-1 = 65535, which is sufficient for high-frequency operations
	if ctr > 65535 {
		ctr = 65535
	}

	// Encode: timestamp (high 48 bits) + counter (low 16 bits)
	// This preserves ordering and monotonicity within reasonable time windows
	return (tsTruncated << 16) | int64(ctr&0xFFFF)
}
