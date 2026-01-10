package cluster

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feellmoose/gridkv/internal/mem_storage"
	"github.com/feellmoose/gridkv/internal/utils/cache"
	"github.com/feellmoose/gridkv/internal/utils/executor"
	"github.com/feellmoose/gridkv/internal/utils/hlc"
)

// Object pools for reducing allocations in hot paths
var (
	// Map pool for targetOpsMap (map[string][]*SyncOperation)
	targetOpsMapPool = sync.Pool{
		New: func() interface{} {
			return make(map[string][]*mem_storage.SyncOperation, 16)
		},
	}

	// Map pool for keyTargetsCache (map[string][]string)
	keyTargetsCachePool = sync.Pool{
		New: func() interface{} {
			return make(map[string][]string, 32)
		},
	}

	// Map pool for nodeIDToAddr (map[string]string)
	nodeIDToAddrPool = sync.Pool{
		New: func() interface{} {
			return make(map[string]string, 32)
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
	if cfg.BatchThreshold <= 0 {
		cfg.BatchThreshold = 15
	}
	if cfg.BatchWindow <= 0 {
		cfg.BatchWindow = 50 * time.Millisecond
	}
	if cfg.ReplicaCount <= 0 {
		cfg.ReplicaCount = 3
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

// storeOrQueue stores item locally if replica, otherwise queues for gossip
func (w *writer) storeOrQueue(key string, item *mem_storage.StoredItem, opType mem_storage.OpType) error {
	targets := w.ring.GetN(key, w.replicaCount)
	if len(targets) == 0 {
		return fmt.Errorf("no replica nodes available for key %s", key)
	}

	if w.isLocalReplica(targets) {
		if err := w.store.Set(key, item); err != nil {
			return err
		}
		if w.cache != nil {
			w.cache.Delete(key)
		}
	} else {
		w.pendingMu.Lock()
		w.pendingOps = append(w.pendingOps, &mem_storage.SyncOperation{
			Key:    key,
			OpType: opType,
			Item:   item.DeepCopy(),
		})
		w.pendingMu.Unlock()
	}
	return nil
}

func (w *writer) Set(ctx context.Context, key string, item *mem_storage.StoredItem) error {
	hlcStr := w.hlc.Now()
	version := hlcToInt64(hlcStr)

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
	keyTargetsCache := make(map[string][]string, len(items))
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
			_ = w.store.Set(key, newItem)
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
	if int(count) >= w.batchThreshold {
		// Use CAS to avoid duplicate flush calls
		if w.flushPending.CompareAndSwap(false, true) {
			_ = w.executor.Do(func() {
				w.flush()
				w.flushPending.Store(false)
			})
		}
	} else {
		w.resetTimer()
	}
}

func (w *writer) resetTimer() {
	w.flushMu.Lock()
	defer w.flushMu.Unlock()

	if !w.flushTimer.Stop() {
		select {
		case <-w.flushTimer.C:
		default:
		}
	}
	w.flushTimer.Reset(w.batchWindow)
}

func (w *writer) flushLoop() {
	defer w.wg.Done()
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
			// Use CAS to avoid duplicate flush calls
			if w.flushPending.CompareAndSwap(false, true) {
				_ = w.executor.Do(func() {
					w.flush()
					w.flushPending.Store(false)
				})
			}
		}
	}
}

// Stop stops the writer and waits for flush loop to finish
func (w *writer) Stop() {
	w.stopOnce.Do(func() {
		close(w.stopCh)
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
	w.flushMu.Lock()
	defer w.flushMu.Unlock()
	w.flushInternal()
}

// flushInternal does the actual flush work without acquiring lock (caller must hold lock)
func (w *writer) flushInternal() {
	// Get ops from store (for replica keys)
	ops, err := w.store.GetSyncBuffer()
	if err != nil {
		ops = nil
	}

	// Get pending ops (for non-replica keys)
	w.pendingMu.Lock()
	pendingOps := w.pendingOps
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
		// Reset counter and timer even if no ops
		w.batchCount.Store(0)
		w.flushTimer.Reset(w.batchWindow)
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
	// Push to targets asynchronously for better throughput
	if gg, ok := w.gossip.(*gossip); ok {
		// Use executor for async push to avoid blocking flush
		for targetNodeID, targetOps := range targetOpsMap {
			if len(targetOps) == 0 {
				continue
			}
			targetAddr := nodeIDToAddr[targetNodeID]
			if targetAddr == "" {
				continue
			}
			// Capture variables for goroutine
			ops := targetOps
			addr := targetAddr
			_ = w.executor.Do(func() {
				data, err := SerializeSyncOps(ops)
				if err != nil {
					return
				}
				gg.pushToTarget(addr, data, 5)
			})
		}
	} else {
		for targetNodeID, targetOps := range targetOpsMap {
			if len(targetOps) > 0 {
				targetAddr := nodeIDToAddr[targetNodeID]
				if targetAddr == "" {
					continue
				}
				ops := targetOps
				addr := targetAddr
				_ = w.executor.Do(func() {
					_ = w.gossip.Push(ops, []string{addr})
				})
			}
		}
	}

	// Reset timer after flush completes
	w.flushTimer.Reset(w.batchWindow)
}

// hlcToInt64 converts HLC string to int64 for version comparison
// HLC format: "nodeID:timestamp:counter"
// We encode it as: timestamp (high 48 bits) + counter (low 16 bits)
// This preserves causality: if HLC(A) < HLC(B), then int64(A) < int64(B)
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

	// Encode: timestamp (high 48 bits) + counter (low 16 bits)
	// Max counter: 65535, which is sufficient for HLC
	if ctr > 65535 {
		ctr = 65535
	}

	// Shift timestamp left by 16 bits, add counter
	// This preserves ordering: higher timestamp or same timestamp with higher counter = higher version
	return (ts << 16) | int64(ctr&0xFFFF)
}
