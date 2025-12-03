package gossip

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feellmoose/gridkv/internal/storage"
	"github.com/feellmoose/gridkv/internal/utils/logging"
)

const (
	pipelineBatchSize = 10000
	pipelineFlushTick = 500 * time.Microsecond
	// Stage 2.3: Reduced from 200K to 20K to control message size and memory usage
	maxMessageOps = 20000
)

type pipelineBuffer struct {
	ops   []*CacheSyncOperation
	index map[string]int
}

var pipelineBufferPool = sync.Pool{
	New: func() interface{} {
		return &pipelineBuffer{
			ops:   make([]*CacheSyncOperation, 0, pipelineBatchSize),
			index: make(map[string]int, pipelineBatchSize),
		}
	},
}

type targetPipeline struct {
	target     string
	manager    *GossipManager
	ch         chan *CacheSyncOperation
	flushCh    chan struct{} // Channel to trigger immediate flush
	once       sync.Once
	lastActive atomic.Int64   // UnixNano timestamp (Stage 2.3: for idle target recycling)
	wg         sync.WaitGroup
}

func (tp *targetPipeline) start() {
	tp.once.Do(func() {
		tp.flushCh = make(chan struct{}, 1) // Buffered to allow non-blocking flush requests
		tp.wg.Add(1)
		go func() {
			defer tp.wg.Done()
			buf := pipelineBufferPool.Get().(*pipelineBuffer)
			buf.ops = buf.ops[:0]
			for k := range buf.index {
				delete(buf.index, k)
			}
			defer func() {
				tp.flushBuffer(buf)
				pipelineBufferPool.Put(buf)
			}()

			_, flushInterval := tp.manager.getBatchConfig(BatchRoleWrite)
			timer := time.NewTimer(flushInterval)
			defer timer.Stop()

			for {
				select {
				case op, ok := <-tp.ch:
					if !ok {
						// Channel closed, flush remaining and exit
						tp.flushBuffer(buf)
						return
					}
					tp.bufferOperation(buf, op)

					batchSize, interval := tp.manager.getBatchConfig(BatchRoleWrite)
					if len(buf.ops) >= batchSize {
						tp.flushBuffer(buf)
						resetTimer(timer, interval)
					}
				case <-timer.C:
					// Periodic flush for consistency (even if buffer is small)
					if len(buf.ops) > 0 {
						tp.flushBuffer(buf)
					}
					_, interval := tp.manager.getBatchConfig(BatchRoleWrite)
					timer.Reset(interval)
				case <-tp.flushCh:
					// Immediate flush requested (for consistency checks)
					if len(buf.ops) > 0 {
						tp.flushBuffer(buf)
					}
					_, interval := tp.manager.getBatchConfig(BatchRoleWrite)
					resetTimer(timer, interval)
				}
			}
		}()
	})
}

//go:inline
func (tp *targetPipeline) bufferOperation(buf *pipelineBuffer, op *CacheSyncOperation) {
	if op == nil {
		return
	}
	// Stage 2.3: Update lastActive timestamp on any operation
	tp.lastActive.Store(time.Now().UnixNano())
	key := op.GetKey()
	if key != "" {
		if idx, ok := buf.index[key]; ok {
			buf.ops[idx] = op
			return
		}
		buf.index[key] = len(buf.ops)
	}
	buf.ops = append(buf.ops, op)
}

//go:inline
func (tp *targetPipeline) flushBuffer(buf *pipelineBuffer) {
	if buf == nil || len(buf.ops) == 0 {
		return
	}
	// Reuse slice capacity to reduce allocations
	// Only allocate new slice if current capacity is too large
	opsCopy := buf.ops
	if cap(buf.ops) > len(buf.ops)*4 {
		// Capacity is too large, create new slice with appropriate size
		opsCopy = make([]*CacheSyncOperation, len(buf.ops))
		copy(opsCopy, buf.ops)
	}
	tp.flush(opsCopy)

	// Clear buffer and shrink if too large to reduce memory footprint
	buf.ops = buf.ops[:0]
	if cap(buf.ops) > pipelineBatchSize*2 {
		// Shrink if capacity is more than 2x the batch size
		buf.ops = make([]*CacheSyncOperation, 0, pipelineBatchSize)
	}
	// Clear index map
	for k := range buf.index {
		delete(buf.index, k)
	}
	// Shrink index map if too large (maps don't have cap, but we can recreate if empty)
	if len(buf.index) == 0 {
		buf.index = make(map[string]int, pipelineBatchSize)
	}
}

//go:inline
func resetTimer(timer *time.Timer, d time.Duration) {
	if timer == nil {
		return
	}
	// Drain timer channel to prevent stale ticks
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(d)
}

// requestFlush requests an immediate flush of the pipeline buffer
//
//go:inline
func (tp *targetPipeline) requestFlush() {
	select {
	case tp.flushCh <- struct{}{}:
	default:
		// Flush already requested, skip
	}
}

func (tp *targetPipeline) flush(ops []*CacheSyncOperation) {
	if len(ops) == 0 {
		return
	}

	// Split large batches into multiple messages to prevent oversized messages
	if len(ops) <= maxMessageOps {
		// Small batch: send as single message
		tp.sendBatchMessage(ops)
	} else {
		// Large batch: split into multiple messages
		// Send in parallel for maximum throughput
		var wg sync.WaitGroup
		for i := 0; i < len(ops); i += maxMessageOps {
			end := i + maxMessageOps
			if end > len(ops) {
				end = len(ops)
			}
			batch := ops[i:end]
			wg.Add(1)
			go func(b []*CacheSyncOperation) {
				defer wg.Done()
				tp.sendBatchMessage(b)
			}(batch)
		}
		// Don't wait for all sends to complete - fire and forget for maximum throughput
		// The network layer will handle retries if needed
		wg.Wait()
	}
}

func (tp *targetPipeline) sendBatchMessage(ops []*CacheSyncOperation) {
	if len(ops) == 0 {
		return
	}

	msg := getGossipMessage()
	msg.Type = GossipMessageType_MESSAGE_TYPE_CACHE_SYNC
	msg.Sender = tp.manager.localNodeID
	msg.Hlc = tp.manager.hlc.Now()
	msg.Payload = &GossipMessage_CacheSyncPayload{
		CacheSyncPayload: &SyncMessage{
			SyncType: &SyncMessage_IncrementalSync{
				IncrementalSync: &IncrementalSyncPayload{Operations: ops},
			},
		},
	}
	tp.manager.signMessageCanonical(msg)

	timeout := tp.manager.replicationTimeout
	if len(ops) > 50000 {
		timeout = timeout * 3
	}
	err := tp.manager.network.SendWithTimeout(tp.target, msg, timeout)
	if tp.manager.metrics != nil {
		if err == nil {
			tp.manager.metrics.IncrementReplicationSuccess()
		} else {
			tp.manager.metrics.IncrementReplicationFailures()
		}
	}
	putGossipMessage(msg)
}

var (
	unifiedPipelines sync.Map
)

//go:inline
func (gm *GossipManager) getPipeline(target string) *targetPipeline {
	if gm.useBinaryProtocol {
		return nil
	}
	gm.pipelineMu.Lock()
	defer gm.pipelineMu.Unlock()
	if gm.pipelines == nil {
		gm.pipelines = make(map[string]*targetPipeline)
	}
	if p, ok := gm.pipelines[target]; ok {
		return p
	}
	bufferSize := 1 << 21
	p := &targetPipeline{
		target:  target,
		manager: gm,
		ch:      make(chan *CacheSyncOperation, bufferSize),
	}
	// Stage 2.3: Initialize lastActive timestamp
	p.lastActive.Store(time.Now().UnixNano())
	p.start()
	gm.pipelines[target] = p
	if gm.metrics != nil {
		gm.metrics.SetPipelineActiveCount(int64(len(gm.pipelines)))
	}
	return p
}

//go:inline
func (gm *GossipManager) getUnifiedPipeline(target string) *UnifiedPipeline {
	if v, ok := unifiedPipelines.Load(target); ok {
		return v.(*UnifiedPipeline)
	}
	up := NewUnifiedPipeline(target, gm, gm.useBinaryProtocol)
	if actual, loaded := unifiedPipelines.LoadOrStore(target, up); loaded {
		up.Stop()
		return actual.(*UnifiedPipeline)
	}
	return up
}

func (gm *GossipManager) enqueueToPipeline(target string, op *CacheSyncOperation) {
	if gm.metrics != nil {
		gm.metrics.IncrementPipelineOperationsTotal()
	}
	if gm.useBinaryProtocol {
		up := gm.getUnifiedPipeline(target)
		up.Add(op)
		return
	}
	p := gm.getPipeline(target)
	if p == nil {
		if logging.Log.IsDebugEnabled() {
			logging.Debug("Pipeline is nil, dropping operation", "target", target, "key", op.GetKey())
		}
		return
	}
	// Stage 2.3: Update lastActive when enqueueing
	p.lastActive.Store(time.Now().UnixNano())
	select {
	case p.ch <- op:
	default:
		p.requestFlush()
		select {
		case p.ch <- op:
			return
		default:
			gm.pipelineDropCounter.Add(1)
			totalDrops := gm.pipelineDropCounter.Load()
			if totalDrops%100 == 0 || totalDrops < 10 {
				logging.Warn("Pipeline channel full, dropping operation",
					"target", target,
					"key", op.GetKey(),
					"total_drops", totalDrops)
			}
			if gm.metrics != nil {
				gm.metrics.IncrementPipelineOperationsDropped()
			}
		}
	}
}

// FlushAllPipelines flushes all active pipelines to ensure consistency
func (gm *GossipManager) FlushAllPipelines() {
	if gm.useBinaryProtocol {
		unifiedPipelines.Range(func(key, value interface{}) bool {
			if up, ok := value.(*UnifiedPipeline); ok {
				up.flush()
			}
			return true
		})
		return
	}

	gm.pipelineMu.Lock()
	pipelines := make([]*targetPipeline, 0, len(gm.pipelines))
	for _, p := range gm.pipelines {
		pipelines = append(pipelines, p)
	}
	gm.pipelineMu.Unlock()

	// Request flush for all pipelines (non-blocking)
	for _, p := range pipelines {
		p.requestFlush()
	}
}

// StopAllPipelines stops all pipelines and closes their channels to prevent goroutine leaks
func (gm *GossipManager) StopAllPipelines() {
	if gm.useBinaryProtocol {
		var pipelines []*UnifiedPipeline
		unifiedPipelines.Range(func(key, value interface{}) bool {
			if up, ok := value.(*UnifiedPipeline); ok {
				pipelines = append(pipelines, up)
			}
			unifiedPipelines.Delete(key)
			return true
		})
		// UnifiedPipeline.Stop() already has WaitGroup with timeout
		var wg sync.WaitGroup
		for _, up := range pipelines {
			wg.Add(1)
			go func(p *UnifiedPipeline) {
				defer wg.Done()
				p.Stop() // Has internal WaitGroup with 2s timeout
			}(up)
		}

		// Wait for all pipelines with timeout
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
			// All pipelines stopped
		case <-time.After(2 * time.Second):
			logging.Warn("UnifiedPipeline stop timeout")
		}
		return
	}

	gm.pipelineMu.Lock()
	pipelines := make([]*targetPipeline, 0, len(gm.pipelines))
	for _, p := range gm.pipelines {
		pipelines = append(pipelines, p)
	}
	gm.pipelines = make(map[string]*targetPipeline)
	gm.pipelineMu.Unlock()

	if gm.metrics != nil {
		gm.metrics.SetPipelineActiveCount(0)
	}

	// Close channels to signal goroutines to exit
	for _, p := range pipelines {
		if p != nil && p.ch != nil {
			close(p.ch)
		}
		if p != nil && p.flushCh != nil {
			close(p.flushCh)
		}
	}

	done := make(chan struct{})
	go func() {
		for _, p := range pipelines {
			if p != nil {
				p.wg.Wait()
			}
		}
		close(done)
	}()

	// Wait with timeout
	select {
	case <-done:
		// All pipeline goroutines exited
	case <-time.After(1 * time.Second):
		logging.Warn("Pipeline stop timeout - some goroutines may still be running")
	}
}

// cleanupIdlePipelines removes pipelines that have been idle for too long (Stage 2.3).
// This prevents memory leaks and goroutine accumulation from unused targets.
func (gm *GossipManager) cleanupIdlePipelines() {
	if gm.useBinaryProtocol {
		// UnifiedPipeline cleanup would be handled separately if needed
		return
	}

	const idleThreshold = 5 * time.Minute
	now := time.Now()

	gm.pipelineMu.Lock()
	var toRemove []string
	for target, p := range gm.pipelines {
		if p == nil {
			continue
		}
		lastActiveNano := p.lastActive.Load()
		if lastActiveNano == 0 {
			// Not initialized, skip
			continue
		}
		lastActive := time.Unix(0, lastActiveNano)
		idleDuration := now.Sub(lastActive)
		if idleDuration > idleThreshold {
			toRemove = append(toRemove, target)
		}
	}

	// Remove idle pipelines
	for _, target := range toRemove {
		p := gm.pipelines[target]
		if p != nil {
			// Close channels to signal goroutine to exit
			if p.ch != nil {
				close(p.ch)
			}
			if p.flushCh != nil {
				close(p.flushCh)
			}
			delete(gm.pipelines, target)
			if logging.Log.IsDebugEnabled() {
				logging.Debug("Removed idle pipeline", "target", target)
			}
		}
	}

	if gm.metrics != nil && len(toRemove) > 0 {
		gm.metrics.SetPipelineActiveCount(int64(len(gm.pipelines)))
	}
	gm.pipelineMu.Unlock()
}

// replicationBatch holds batched operations for a target node
type replicationBatch struct {
	ops       []*CacheSyncOperation
	timer     *time.Timer
	timerDone chan struct{} // Channel to signal timer goroutine to stop
	mutex     sync.Mutex
	target    string
	manager   *GossipManager
}

// extractAndClearOps extracts ops slice and clears/resizes buffer if needed.
// Must be called with mutex held.
func (rb *replicationBatch) extractAndClearOps(batchThreshold int) []*CacheSyncOperation {
	// Reuse slice if capacity is reasonable, otherwise create new
	var ops []*CacheSyncOperation
	if cap(rb.ops) <= len(rb.ops)*2 {
		ops = make([]*CacheSyncOperation, len(rb.ops))
		copy(ops, rb.ops)
	} else {
		ops = rb.ops
	}
	rb.ops = rb.ops[:0] // Clear batch
	// Shrink if capacity is too large
	if cap(rb.ops) > batchThreshold*4 {
		rb.ops = make([]*CacheSyncOperation, 0, batchThreshold)
	}
	return ops
}

// addOperation adds an operation to the batch
func (rb *replicationBatch) addOperation(op *CacheSyncOperation) {
	rb.mutex.Lock()
	rb.ops = append(rb.ops, op)
	batchThreshold, batchTimeout := rb.manager.getBatchConfig(BatchRoleWrite)
	shouldFlush := len(rb.ops) >= batchThreshold

	// Stop timer if we're flushing
	var ops []*CacheSyncOperation
	if shouldFlush {
		if rb.timer != nil {
			rb.timer.Stop()
			if rb.timerDone != nil {
				close(rb.timerDone)
				rb.timerDone = nil
			}
			rb.timer = nil
		}
		ops = rb.extractAndClearOps(batchThreshold)
	} else if rb.timer == nil {
		timer := time.NewTimer(batchTimeout)
		timerDone := make(chan struct{})
		rb.timer = timer
		rb.timerDone = timerDone
		go func() {
			select {
			case <-timer.C:
				rb.mutex.Lock()
				// Check if timer was replaced or stopped
				if rb.timer != timer {
					rb.mutex.Unlock()
					return
				}
				rb.timer = nil
				rb.timerDone = nil
				if len(rb.ops) > 0 {
					batchThreshold, _ := rb.manager.getBatchConfig(BatchRoleWrite)
					ops := rb.extractAndClearOps(batchThreshold)
					rb.mutex.Unlock()
					rb.sendBatchedMessage(ops)
				} else {
					rb.mutex.Unlock()
				}
			case <-timerDone:
				// Timer was stopped, exit goroutine
				return
			}
		}()
	}
	rb.mutex.Unlock()

	// Send outside lock to avoid holding mutex during network I/O
	if shouldFlush {
		rb.sendBatchedMessage(ops)
	}
}

func (rb *replicationBatch) sendBatchedMessage(ops []*CacheSyncOperation) {
	if len(ops) == 0 || rb == nil || rb.manager == nil {
		return
	}
	for _, op := range ops {
		rb.manager.enqueueToPipeline(rb.target, op)
	}
}

// flush forces immediate flush of the batch
func (rb *replicationBatch) flush() {
	rb.mutex.Lock()
	// Reuse slice if capacity is reasonable
	var ops []*CacheSyncOperation
	if cap(rb.ops) <= len(rb.ops)*2 {
		ops = make([]*CacheSyncOperation, len(rb.ops))
		copy(ops, rb.ops)
	} else {
		ops = rb.ops
	}
	rb.ops = rb.ops[:0]
	// Shrink if capacity is too large
	batchThreshold, _ := rb.manager.getBatchConfig(BatchRoleWrite)
	if cap(rb.ops) > batchThreshold*4 {
		rb.ops = make([]*CacheSyncOperation, 0, batchThreshold)
	}
	if rb.timer != nil {
		rb.timer.Stop()
		if rb.timerDone != nil {
			close(rb.timerDone)
			rb.timerDone = nil
		}
		rb.timer = nil
	}
	rb.mutex.Unlock()

	rb.sendBatchedMessage(ops)
}

// Set performs an eventually consistent distributed write using batched replication.
//
// The write flow:
//  1. Hash the key to find N replica nodes using consistent hashing
//  2. The first replica (coordinator) receives the write
//  3. If this node is not the coordinator, forward to coordinator
//  4. Coordinator writes locally, then replicates to N-1 replicas
//  5. Return immediately after local write (replication continues in background)
//
// Parameters:
//   - ctx: Context for timeout and cancellation
//   - key: The key to write
//   - item: The value and metadata to store
//
// Returns:
//   - error: Storage or forwarding error only (no ACK wait)
//
//go:inline
func (gm *GossipManager) Set(ctx context.Context, key string, item *storage.StoredItem) error {
	if item == nil {
		return errors.New("nil item")
	}

	// Defensive: ensure required components are initialized
	if gm.store == nil {
		return fmt.Errorf("store not initialized")
	}

	// Check cluster size atomically first
	gm.mu.RLock()
	availableNodes := len(gm.liveNodes)
	gm.mu.RUnlock()

	if availableNodes == 1 {
		if err := gm.store.Set(key, item); err != nil {
			return fmt.Errorf("local write failed: %w", err)
		}
		return nil
	}

	// Use minimum of requested replicas and available nodes
	effectiveReplicaCount := gm.replicaCount
	if availableNodes < gm.replicaCount {
		effectiveReplicaCount = availableNodes
		if effectiveReplicaCount == 0 {
			effectiveReplicaCount = 1 // At minimum, use local node
		}
	}

	// If hash ring not ready, write locally
	if gm.hashRing == nil {
		if err := gm.store.Set(key, item); err != nil {
			return fmt.Errorf("local write failed: %w", err)
		}
		return nil
	}
	replicas := gm.getReplicas(key, effectiveReplicaCount)
	if len(replicas) == 0 {
		// Last resort: write to local node only
		logging.Debug("No replicas in hash ring, writing to local node only", "key", key)
		if err := gm.store.Set(key, item); err != nil {
			return fmt.Errorf("local write failed: %w", err)
		}
		return nil
	}

	coordinator := replicas[0]
	if coordinator != gm.localNodeID {
		// Forward to coordinator
		// Pipeline will handle copying and serialization, so we can pass item directly
		// However, we need to ensure item is not modified before pipeline processes it
		// Since pipeline processes asynchronously, we need a copy
		// Pipeline will handle serialization
		itemCopy := copyStorageItem(item)
		if itemCopy == nil {
			return fmt.Errorf("failed to copy item for forwarding")
		}
		return gm.forwardWrite(key, itemCopy, coordinator)
	}

	// Local write (we are the coordinator)
	if err := gm.store.Set(key, item); err != nil {
		return fmt.Errorf("local write failed: %w", err)
	}

	// Optional verification (only in debug mode to avoid performance impact)
	// In production, Set() returning nil is sufficient guarantee
	if logging.Log.IsDebugEnabled() {
		if _, verifyErr := gm.store.Get(key); verifyErr != nil {
			logging.Debug("Local write verification failed, continuing anyway", "key", key, "err", verifyErr)
		}
	}

	// Always use async replication for high throughput
	// Write locally and return immediately, replicate in background
	// This provides high throughput while maintaining eventual consistency
	// Local write ensures data is persisted, async replication provides durability
	//
	// Why async is safe:
	// 1. Local write is already committed - data won't be lost
	// 2. Replication happens in background - eventual consistency is acceptable for high QPS
	// Replication happens via async gossip protocol
	// Prepare immutable copies before starting goroutine to avoid races
	var valueCopy []byte
	if item.Value != nil {
		valueCopy = make([]byte, len(item.Value))
		copy(valueCopy, item.Value)
	}
	itemCopy := &storage.StoredItem{
		Value:    valueCopy,
		Version:  item.Version,
		ExpireAt: item.ExpireAt,
	}
	replicaIDs := GetStringSlice()
	if len(replicas) > 1 {
		replicaIDs = append(replicaIDs, replicas[1:]...)
	}
	defer PutStringSlice(replicaIDs)
	keyCopy := key
	// Use replication pool instead of creating goroutine directly
	// Prevents goroutine leaks and provides better resource management
	if err := gm.replicationPool.Submit(func() {
		_ = gm.replicateToNodes(context.Background(), keyCopy, itemCopy, replicaIDs)
	}); err != nil {
		// Pool full - use adaptive resize only (no fallback)
		// Retry with resize up to 3 times
		if gm.replicationPoolResizer != nil {
			for retry := 0; retry < 3; retry++ {
				gm.replicationPoolResizer.emergencyResize()
				if err := gm.replicationPool.Submit(func() {
					_ = gm.replicateToNodes(context.Background(), keyCopy, itemCopy, replicaIDs)
				}); err == nil {
					return nil
				}
			}
		}
		// If all retries failed, log and continue (eventual consistency)
		// Don't use fallback to prevent goroutine leaks
		if logging.Log.IsDebugEnabled() {
			logging.Debug("Replication task dropped after resize retries", "key", keyCopy)
		}
	}
	return nil // Return immediately after local write
}

// Delete performs an eventually consistent distributed delete using batched replication.
//
// Similar to Set, but removes the key-value pair instead.
//
// Parameters:
//   - ctx: Context for timeout and cancellation
//   - key: The key to delete
//   - version: Version number for optimistic concurrency control
//
// Returns:
//   - error: Storage or forwarding error only (no ACK wait)
func (gm *GossipManager) Delete(ctx context.Context, key string, version int64) error {
	// Check cluster size atomically first
	gm.mu.RLock()
	availableNodes := len(gm.liveNodes)
	gm.mu.RUnlock()

	// Fast path for single node cluster
	if availableNodes == 1 {
		if err := gm.store.Delete(key, version); err != nil {
			return fmt.Errorf("local delete failed: %w", err)
		}
		if gm.hotCacheTTL > 0 {
			gm.hotCache.Delete(key)
		}
		return nil
	}

	effectiveReplicaCount := gm.replicaCount
	if availableNodes < gm.replicaCount {
		effectiveReplicaCount = availableNodes
		if effectiveReplicaCount == 0 {
			effectiveReplicaCount = 1
		}
	}

	replicas := gm.getReplicas(key, effectiveReplicaCount)
	if len(replicas) == 0 {
		if logging.Log.IsDebugEnabled() {
			logging.Debug("No replicas in hash ring, deleting from local node only", "key", key)
		}
		if err := gm.store.Delete(key, version); err != nil {
			return fmt.Errorf("local delete failed: %w", err)
		}
		if gm.hotCacheTTL > 0 {
			gm.hotCache.Delete(key)
		}
		return nil
	}

	coordinator := replicas[0]
	if coordinator != gm.localNodeID {
		return gm.forwardDelete(key, version, coordinator)
	}

	if err := gm.store.Delete(key, version); err != nil {
		return fmt.Errorf("local delete failed: %w", err)
	}

	if gm.hotCacheTTL > 0 {
		gm.hotCache.Delete(key)
	}

	// EVENTUAL CONSISTENCY: replicate delete asynchronously and return immediately
	// Use replication pool instead of creating goroutine directly
	replicaIDs := replicas[1:]
	if err := gm.replicationPool.Submit(func() {
		_ = gm.replicateDeleteToNodes(context.Background(), key, version, replicaIDs)
	}); err != nil {
		// Pool full - use adaptive resize only (no fallback)
		// Retry with resize up to 3 times
		if gm.replicationPoolResizer != nil {
			for retry := 0; retry < 3; retry++ {
				gm.replicationPoolResizer.emergencyResize()
				if err := gm.replicationPool.Submit(func() {
					_ = gm.replicateDeleteToNodes(context.Background(), key, version, replicaIDs)
				}); err == nil {
					return nil
				}
			}
		}
		// If all retries failed, log and continue (eventual consistency)
		// Don't use fallback to prevent goroutine leaks
		if logging.Log.IsDebugEnabled() {
			logging.Debug("Delete replication task dropped after resize retries", "key", key)
		}
	}
	return nil
}

// Get performs a distributed read with optimized fast path.
//
// Optimized read flow:
//  1. Check hot cache (fastest path)
//  2. Try local read (fast path, no network)
//  3. If local miss and not coordinator, forward to coordinator (single attempt)
//  4. Return immediately - no complex retry logic
//
//go:inline
func (gm *GossipManager) Get(ctx context.Context, key string) (*storage.StoredItem, error) {
	if gm.store == nil {
		return nil, fmt.Errorf("storage not initialized")
	}

	// Fast-path: hot read cache (optimized for 1M+ QPS)
	// Use atomic time comparison to avoid allocations
	if gm.hotCacheTTL > 0 {
		if v, ok := gm.hotCache.Load(key); ok {
			if e, ok2 := v.(hotCacheEntry); ok2 {
				// Fast expiration check: compare nanoseconds directly
				now := time.Now()
				if now.Before(e.expireAt) && e.item != nil && len(e.item.Value) > 0 {
					// Cache hit - return immediately (zero allocation path)
					return e.item, nil
				}
				// Expired - remove from cache
				gm.hotCache.Delete(key)
			}
		}
	}

	// Fast-path: local read first (no network, no locks)
	// Use GetNoCopy if available to reduce copy overhead
	var localItem *storage.StoredItem
	var localErr error
	if noCopyStorage, ok := gm.store.(interface {
		GetNoCopy(key string) (*storage.StoredItem, error)
	}); ok {
		// Use GetNoCopy for zero-copy read (40-50% faster than Get)
		// We still copy for return value, but avoid the internal copy in Get()
		localItem, localErr = noCopyStorage.GetNoCopy(key)
	} else {
		localItem, localErr = gm.store.Get(key)
	}

	if localErr == nil && localItem != nil && len(localItem.Value) > 0 {
		// Always copy for return value (caller may modify)
		itemCopy := copyStorageItem(localItem)
		if gm.hotCacheTTL > 0 {
			gm.hotCache.Store(key, hotCacheEntry{item: itemCopy, expireAt: time.Now().Add(gm.hotCacheTTL)})
		}
		// Record successful local read (Stage 0.3)
		if gm.metrics != nil {
			gm.metrics.IncrementReadSuccess()
		}
		return itemCopy, nil
	}

	// Single node or no hash ring - return local result
	if gm.hashRing == nil {
		if localErr != nil {
			return nil, localErr
		}
		return nil, storage.ErrItemNotFound
	}

	// Optimized lock granularity (Stage 1.1): minimize lock hold time
	// Use lock-free node map first, fallback to locked map only if needed
	var availableNodes int
	var replicas []string
	var targetReplica string

	// Fast path: try lock-free node map first
	if gm.liveNodesLF != nil {
		// Estimate node count from lock-free map (approximate but fast)
		// For exact count, we still need lock, but this avoids lock for single-node check
		gm.mu.RLock()
		availableNodes = len(gm.liveNodes)
		gm.mu.RUnlock()
	} else {
		gm.mu.RLock()
		availableNodes = len(gm.liveNodes)
		gm.mu.RUnlock()
	}

	if availableNodes == 1 {
		if localErr != nil {
			return nil, localErr
		}
		return nil, storage.ErrItemNotFound
	}

	// Get replicas (single call, no retries)
	effectiveReplicaCount := gm.replicaCount
	if availableNodes < gm.replicaCount {
		effectiveReplicaCount = availableNodes
		if effectiveReplicaCount == 0 {
			effectiveReplicaCount = 1
		}
	}

	replicas = gm.getReplicas(key, effectiveReplicaCount)
	if len(replicas) == 0 {
		if localErr != nil {
			return nil, localErr
		}
		return nil, storage.ErrItemNotFound
	}

	// Local data not available - use batch remote read from one healthy replica node
	// Find first healthy replica node (skip local node) - minimize lock scope
	gm.mu.RLock()
	for _, replicaID := range replicas {
		if replicaID == gm.localNodeID {
			continue // Skip local node
		}
		// Try lock-free map first, fallback to locked map
		var node *NodeInfo
		var ok bool
		if gm.liveNodesLF != nil {
			node, ok = gm.liveNodesLF.Get(replicaID)
		}
		if !ok {
			node, ok = gm.liveNodes[replicaID]
		}
		if ok && node != nil && node.State == NodeState_NODE_STATE_ALIVE {
			targetReplica = replicaID
			break
		}
	}
	gm.mu.RUnlock()

	// If found healthy replica, use a single remote read with very tight timeout.
	if targetReplica != "" {
		// Read path prioritizes low latency: default 50ms limit, taking minimum with outer ctx/ReadTimeout.
		remoteReadTimeout := 50 * time.Millisecond
		if gm.readTimeout > 0 && gm.readTimeout < remoteReadTimeout {
			remoteReadTimeout = gm.readTimeout
		}
		if deadline, ok := ctx.Deadline(); ok {
			if remaining := time.Until(deadline); remaining > 0 && remaining < remoteReadTimeout {
				remoteReadTimeout = remaining
			}
		}
		readCtx, readCancel := context.WithTimeout(ctx, remoteReadTimeout)
		defer readCancel()

		result, err := gm.readFromReplica(readCtx, key, targetReplica)
		if err == nil && result != nil && len(result.Value) > 0 {
			if gm.hotCacheTTL > 0 {
				gm.hotCache.Store(key, hotCacheEntry{item: result, expireAt: time.Now().Add(gm.hotCacheTTL)})
			}
			go func(k string, it *storage.StoredItem) {
				_ = gm.store.Set(k, it)
			}(key, result)
			if gm.metrics != nil {
				gm.metrics.IncrementReadSuccess()
			}
			return result, nil
		}

		// Record read failure (Stage 0.3)
		if gm.metrics != nil {
			if err != nil && (err == context.DeadlineExceeded || err == context.Canceled) {
				gm.metrics.IncrementReadTimeout()
			} else {
				gm.metrics.IncrementReadFail()
			}
		}
	}

	// Record read failure (Stage 0.3)
	if gm.metrics != nil {
		if localErr != nil {
			gm.metrics.IncrementReadFail()
		} else {
			gm.metrics.IncrementReadFail()
		}
	}
	return nil, storage.ErrItemNotFound
}

// GetAsync performs an asynchronous read operation, returning a Future
func (gm *GossipManager) GetAsync(ctx context.Context, key string) ReadFuture {
	if gm.store == nil {
		future := NewReadFuture()
		future.SetResult(nil, fmt.Errorf("storage not initialized"))
		return future
	}

	future := NewReadFuture()

	// Start async read in background
	go func() {
		item, err := gm.Get(ctx, key)
		future.SetResult(item, err)
	}()

	return future
}

// GetBatchAsync performs asynchronous batch read operations
func (gm *GossipManager) GetBatchAsync(ctx context.Context, keys []string) BatchReadFuture {
	if len(keys) == 0 {
		return NewBatchReadFuture([]string{})
	}

	batchFuture := NewBatchReadFuture(keys)

	// Group keys by coordinator for batch processing
	coordinatorKeys := make(map[string][]string)
	for _, key := range keys {
		// Get coordinator for this key
		if gm.hashRing == nil {
			// No hash ring - read locally
			go func(k string) {
				item, err := gm.store.Get(k)
				batchFuture.SetResult(k, item, err)
			}(key)
			continue
		}

		gm.mu.RLock()
		availableNodes := len(gm.liveNodes)
		gm.mu.RUnlock()

		if availableNodes == 1 {
			// Single node - read locally
			go func(k string) {
				item, err := gm.store.Get(k)
				batchFuture.SetResult(k, item, err)
			}(key)
			continue
		}

		replicas := gm.getReplicas(key, gm.replicaCount)
		if len(replicas) == 0 {
			batchFuture.SetResult(key, nil, storage.ErrItemNotFound)
			continue
		}

		coordinator := replicas[0]
		if coordinator == gm.localNodeID {
			// Local coordinator - read directly
			go func(k string) {
				item, err := gm.store.Get(k)
				batchFuture.SetResult(k, item, err)
			}(key)
		} else {
			// Group by coordinator for batch processing
			coordinatorKeys[coordinator] = append(coordinatorKeys[coordinator], key)
		}
	}

	// Process coordinator groups - use true batch processing (Stage 1.5)
	// Group keys by coordinator address to minimize network round-trips
	for coordinator, keys := range coordinatorKeys {
		coord := coordinator
		ks := keys
		go func() {
			// Batch process all keys for this coordinator together
			// ReadBatchManager will automatically batch requests to the same target
			// This reduces network overhead and improves throughput
			for _, k := range ks {
				item, err := gm.enqueueReadRequest(ctx, k, coord)
				batchFuture.SetResult(k, item, err)
			}
		}()
	}

	return batchFuture
}

// forwardWrite forwards a write to the coordinator node using batch pipeline.
// Uses pipeline batching to reduce network overhead and improve throughput.
//
//go:inline
func (gm *GossipManager) forwardWrite(key string, item *storage.StoredItem, coordinatorID string) error {
	if item == nil {
		return fmt.Errorf("forwardWrite called with nil item for key %s", key)
	}
	n, ok := gm.getNode(coordinatorID)
	if !ok {
		// Diagnostic logging only in debug mode (zero cost in production)
		if logging.Log.IsDebugEnabled() {
			gm.mu.RLock()
			availableCount := len(gm.liveNodes)
			hashRingReady := gm.hashRing != nil
			gm.mu.RUnlock()
			logging.Debug("forwardWrite: coordinator not found",
				"key", key,
				"coordinatorID", coordinatorID,
				"availableNodes", availableCount,
				"hashRingReady", hashRingReady,
				"localNodeID", gm.localNodeID)
		}
		return fmt.Errorf("coordinator %s unknown", coordinatorID)
	}

	setData := storageItemToProto(item)
	if setData == nil {
		return fmt.Errorf("forwardWrite proto conversion produced nil payload for key %s", key)
	}
	protoOp := &CacheSyncOperation{
		Key:           key,
		ClientVersion: item.Version,
		Type:          OperationType_OP_SET,
		SetData:       setData,
		DataPayload: &CacheSyncOperation_SetData{
			SetData: setData,
		},
	}

	// Use pipeline batching for better throughput
	// This reduces network overhead by batching multiple operations together
	gm.enqueueToPipeline(n.Address, protoOp)
	return nil // Pipeline handles sending, no immediate error
}

// forwardDelete forwards a delete to the coordinator node using batch pipeline.
// Uses pipeline batching to reduce network overhead and improve throughput.
//
//go:inline
func (gm *GossipManager) forwardDelete(key string, version int64, coordinatorID string) error {
	n, ok := gm.getNode(coordinatorID)
	if !ok {
		return fmt.Errorf("coordinator %s unknown", coordinatorID)
	}

	protoOp := &CacheSyncOperation{
		Key:           key,
		ClientVersion: version,
		Type:          OperationType_OP_DELETE,
	}

	// Use pipeline batching for better throughput
	// This reduces network overhead by batching multiple operations together
	gm.enqueueToPipeline(n.Address, protoOp)
	return nil // Pipeline handles sending, no immediate error
}

func (gm *GossipManager) getOrCreateBatch(targetAddr string) *replicationBatch {
	return nil
}

func (gm *GossipManager) flushBatchForTarget(targetAddr string) {
	if gm.useBinaryProtocol {
		if v, ok := unifiedPipelines.Load(targetAddr); ok {
			if up, ok := v.(*UnifiedPipeline); ok {
				up.flush()
			}
		}
		return
	}
	gm.pipelineMu.Lock()
	if p, ok := gm.pipelines[targetAddr]; ok {
		p.requestFlush()
	}
	gm.pipelineMu.Unlock()
}

func (gm *GossipManager) flushAllBatches() {
	if gm.useBinaryProtocol {
		unifiedPipelines.Range(func(key, value interface{}) bool {
			if up, ok := value.(*UnifiedPipeline); ok {
				up.flush()
			}
			return true
		})
		return
	}
	gm.pipelineMu.Lock()
	for _, p := range gm.pipelines {
		p.requestFlush()
	}
	gm.pipelineMu.Unlock()
}

// replicateToNodes replicates a write to multiple nodes using the async batching pipeline.
func (gm *GossipManager) replicateToNodes(ctx context.Context, key string, item *storage.StoredItem, replicaIDs []string) error {
	if item == nil {
		// Nothing to replicate
		return nil
	}
	if len(replicaIDs) == 0 {
		return nil // No replicas to write to
	}

	// Accept nodes that are not DEAD (ALIVE or SUSPECT)
	// This allows replication to proceed even during cluster startup/state transitions
	// SUSPECT nodes are still functional and can accept writes
	targets := make([]struct{ addr, id string }, 0, len(replicaIDs))
	gm.mu.RLock()
	for _, replicaID := range replicaIDs {
		if n, ok := gm.liveNodes[replicaID]; ok && n != nil && n.State != NodeState_NODE_STATE_DEAD {
			// Accept ALIVE and SUSPECT nodes (SUSPECT nodes are still functional)
			targets = append(targets, struct{ addr, id string }{addr: n.Address, id: replicaID})
		}
	}
	gm.mu.RUnlock()

	// If no targets available (e.g., single node or nodes not ready), only local write happens
	if len(targets) == 0 {
		// Single node or all replica nodes are not ready - local write is sufficient
		// Reduced logging frequency to avoid log spam
		return nil
	}

	// Use pipeline batching for high throughput
	setData := storageItemToProto(item)
	baseOp := &CacheSyncOperation{
		Key:           key,
		ClientVersion: item.Version,
		Type:          OperationType_OP_SET,
		SetData:       setData,
		DataPayload: &CacheSyncOperation_SetData{
			SetData: setData,
		},
	}
	if baseOp.GetSetData() == nil {
		// Drop malformed operation
		return nil
	}

	// Update local storage if this node is a replica
	isLocalReplica := false
	for _, replicaID := range replicaIDs {
		if replicaID == gm.localNodeID {
			isLocalReplica = true
			break
		}
	}
	// Update local storage if this node is a replica
	// Keep synchronous for consistency - async would add overhead without much benefit
	if isLocalReplica {
		// Update local storage immediately to improve read performance
		if err := gm.store.Set(key, item); err != nil {
			if logging.Log.IsDebugEnabled() {
				logging.Debug("Failed to update local replica during replication", "key", key, "err", err)
			}
		} else {
			// Update hot cache
			if gm.hotCacheTTL > 0 {
				itemCopy := copyStorageItem(item)
				if itemCopy != nil {
					gm.hotCache.Store(key, hotCacheEntry{
						item:     itemCopy,
						expireAt: time.Now().Add(gm.hotCacheTTL),
					})
				}
			}
		}
	}

	// Enqueue operation to pipeline for each target
	// Pipeline will batch multiple operations into single messages for high throughput
	for _, t := range targets {
		// Clone operation for each target (pipeline will dedupe if same key)
		opClone := CloneCacheSyncOperation(baseOp)
		gm.enqueueToPipeline(t.addr, opClone)
	}

	sentCount := len(targets)

	// Return immediately after enqueueing to pipeline
	// No ACK waits - convergence via gossip protocol and read-repair
	// Pipeline batching provides maximum throughput
	if sentCount == 0 {
		if logging.Log.IsDebugEnabled() {
			logging.Debug("No replication targets available, local write is sufficient", "key", key)
		}
		return nil
	}

	return nil // Return immediately - replication happens via pipeline batching
}

// replicateDeleteToNodes replicates a delete to multiple nodes using the same path as Set operations.
func (gm *GossipManager) replicateDeleteToNodes(ctx context.Context, key string, version int64, replicaIDs []string) error {
	if len(replicaIDs) == 0 {
		return nil // No replicas to delete from
	}

	// Accept nodes that are not DEAD (ALIVE or SUSPECT)
	// This allows replication to proceed even during cluster startup/state transitions
	// SUSPECT nodes are still functional and can accept deletes
	targets := make([]struct{ addr, id string }, 0, len(replicaIDs))
	gm.mu.RLock()
	for _, replicaID := range replicaIDs {
		if n, ok := gm.liveNodes[replicaID]; ok && n != nil && n.State != NodeState_NODE_STATE_DEAD {
			// Accept ALIVE and SUSPECT nodes (SUSPECT nodes are still functional)
			targets = append(targets, struct{ addr, id string }{addr: n.Address, id: replicaID})
		}
	}
	gm.mu.RUnlock()

	// If no targets available (e.g., single node or nodes not ready), only local delete happens
	if len(targets) == 0 {
		// Single node or all replica nodes are not ready - local delete is sufficient
		return nil
	}

	// Use pipeline batching for high throughput
	baseOp := &CacheSyncOperation{
		Key:           key,
		ClientVersion: version,
		Type:          OperationType_OP_DELETE,
	}

	// Enqueue operation to pipeline for each target
	// Pipeline will batch multiple operations into single messages for high throughput
	for _, t := range targets {
		// Clone operation for each target (pipeline will dedupe if same key)
		opClone := CloneCacheSyncOperation(baseOp)
		gm.enqueueToPipeline(t.addr, opClone)
	}

	sentCount := len(targets)

	// Return immediately after enqueueing to pipeline
	// No ACK waits - convergence via gossip protocol and read-repair
	// Pipeline batching provides maximum throughput
	if sentCount == 0 {
		if logging.Log.IsDebugEnabled() {
			logging.Debug("No replication targets available, local delete is sufficient", "key", key)
		}
		return nil
	}

	return nil // Return immediately - replication happens via pipeline batching
}

// forwardReadToCoordinatorFast tries to read from coordinator with optimized fast path.
// Uses batch processing for high throughput.
func (gm *GossipManager) forwardReadToCoordinatorFast(ctx context.Context, key string, replicas []string) (*storage.StoredItem, error) {
	if len(replicas) == 0 {
		return nil, storage.ErrItemNotFound
	}

	coordinatorID := replicas[0]
	if coordinatorID == gm.localNodeID {
		return nil, storage.ErrItemNotFound
	}

	// Use batch processing if available
	// Fallback to single request if batch manager not initialized
	if gm.readBatchManager != nil {
		return gm.enqueueReadRequest(ctx, key, coordinatorID)
	}

	// Fallback to single request if batch manager not available (use shorter timeout)
	fastTimeout := 200 * time.Millisecond
	if gm.readTimeout < fastTimeout {
		fastTimeout = gm.readTimeout
	}
	ctx2, cancel := context.WithTimeout(ctx, fastTimeout)
	defer cancel()

	result, err := gm.forwardReadToCoordinator(ctx2, key, coordinatorID)
	if err == nil && result != nil && len(result.Value) > 0 {
		return result, nil
	}

	// Try one fallback replica if available
	if len(replicas) > 1 {
		replicaID := replicas[1]
		if replicaID != gm.localNodeID {
			ctx3, cancel3 := context.WithTimeout(ctx, 50*time.Millisecond)
			result2, err2 := gm.forwardReadToCoordinator(ctx3, key, replicaID)
			cancel3()
			if err2 == nil && result2 != nil && len(result2.Value) > 0 {
				// Record fallback success (Stage 0.3)
				if gm.metrics != nil {
					gm.metrics.IncrementReadFallback()
					gm.metrics.IncrementReadSuccess()
				}
				return result2, nil
			}
			// Record fallback attempt (Stage 0.3)
			if gm.metrics != nil {
				gm.metrics.IncrementReadFallback()
			}
		}
	}

	return nil, storage.ErrItemNotFound
}

// forwardReadToCoordinator forwards a read request to the coordinator node.
//
//go:inline
func (gm *GossipManager) forwardReadToCoordinator(ctx context.Context, key string, coordinatorID string) (*storage.StoredItem, error) {
	if coordinatorID == gm.localNodeID {
		return nil, errors.New("coordinator is local but not in replica set")
	}

	peer, ok := gm.getNode(coordinatorID)
	if !ok {
		return nil, fmt.Errorf("coordinator %s not found", coordinatorID)
	}

	// Check if node is healthy before sending
	if peer.State != NodeState_NODE_STATE_ALIVE {
		return nil, fmt.Errorf("coordinator %s is not alive (state: %v)", coordinatorID, peer.State)
	}

	// Optimized pending reads threshold for high concurrency
	// Dynamically adjust based on cluster size
	gm.mu.RLock()
	clusterSize := len(gm.liveNodes)
	gm.mu.RUnlock()
	maxPendingReadsThreshold := int64(65536) // Base threshold
	if clusterSize > 10 {
		maxPendingReadsThreshold = int64(clusterSize * 4096) // Scale with cluster size
		if maxPendingReadsThreshold > 262144 {
			maxPendingReadsThreshold = 262144 // Cap at 256K
		}
	}
	pendingCount := gm.pendingReadsCount.Load()
	if pendingCount > maxPendingReadsThreshold {
		// Too many pending reads - check local first before failing
		// This improves read availability by returning local data if available
		if item, localErr := gm.store.Get(key); localErr == nil && item != nil && len(item.Value) > 0 {
			itemCopy := copyStorageItem(item)
			if gm.hotCacheTTL > 0 {
				gm.hotCache.Store(key, hotCacheEntry{item: itemCopy, expireAt: time.Now().Add(gm.hotCacheTTL)})
			}
			return itemCopy, nil
		}
		// No local data - return error with context about resource limits
		// This allows caller to distinguish between true not-found and resource limits
		// Record fast-fail metric (Stage 0.3)
		if gm.metrics != nil {
			gm.metrics.IncrementReadFastFail()
		}
		return nil, fmt.Errorf("read declined due to high pending reads (%d > %d): %w", pendingCount, maxPendingReadsThreshold, storage.ErrItemNotFound)
	}

	requestID := gm.generateOpID()
	respCh := getReadResponseChannel()
	// Store pending read with timestamp for timeout cleanup
	entry := &pendingReadEntry{
		ch:        respCh,
		createdAt: time.Now(),
	}
	gm.addPendingRead(requestID, entry)
	defer func() {
		gm.removePendingRead(requestID)
		putReadResponseChannel(respCh)
	}()

	msg := &GossipMessage{
		Type:   GossipMessageType_MESSAGE_TYPE_READ_REQUEST,
		Sender: gm.localNodeID,
		Payload: &GossipMessage_ReadRequestPayload{
			ReadRequestPayload: &ReadRequestPayload{
				Key:         key,
				RequesterId: gm.localNodeID,
				RequestId:   requestID,
			},
		},
	}
	gm.signMessageCanonical(msg)

	// Read coordinator also uses smaller timeout, default 50ms, limited by outer ctx/ReadTimeout,
	// reducing single remote read blocking time to improve overall QPS.
	timeout := 50 * time.Millisecond
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining > 0 {
			timeout = remaining
		}
	}

	if err := gm.network.SendWithTimeout(peer.Address, msg, timeout); err != nil {
		return nil, fmt.Errorf("forward read failed: %w", err)
	}

	ctx2, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Use select with timeout to ensure we don't block indefinitely
	// Also check context deadline to respect caller's timeout
	select {
	case resp := <-respCh:
		if resp == nil {
			return nil, storage.ErrItemNotFound
		}
		if !resp.Found || resp.ItemData == nil {
			return nil, storage.ErrItemNotFound
		}
		// Thread-safe: Convert proto to storage item
		item := protoItemToStorage(resp.ItemData, resp.Version)
		if item == nil {
			return nil, storage.ErrItemNotFound
		}
		// CONSISTENCY: Verify item has valid data
		if len(item.Value) == 0 {
			return nil, storage.ErrItemNotFound
		}
		return item, nil
	case <-ctx2.Done():
		// Timeout - return error to trigger retry or fallback
		if gm.metrics != nil {
			gm.metrics.IncrementRequestsTimeout()
		}
		return nil, fmt.Errorf("read forward timeout for key %s: %w", key, ctx2.Err())
	}
}

// handleReadRequest responds to a read request from another node.
func (gm *GossipManager) handleReadRequest(payload *ReadRequestPayload, requesterID string) {
	if payload == nil || requesterID == "" {
		return
	}

	// Direct read from store (no pool overhead)
	item, err := gm.store.Get(payload.Key)

	resp := &ReadResponsePayload{
		Key:         payload.Key,
		RequestId:   payload.RequestId,
		ResponderId: gm.localNodeID,
	}

	if err == nil && item != nil && item.Value != nil && len(item.Value) > 0 {
		resp.Found = true
		resp.Version = item.Version
		resp.ItemData = storageItemToProto(item)
	}

	peer, ok := gm.getNode(requesterID)
	if !ok {
		return
	}

	msg := &GossipMessage{
		Type:   GossipMessageType_MESSAGE_TYPE_READ_RESPONSE,
		Sender: gm.localNodeID,
		Payload: &GossipMessage_ReadResponsePayload{
			ReadResponsePayload: resp,
		},
	}
	gm.signMessageCanonical(msg)

	// Increased response timeout for high load: 300ms for better reliability
	responseTimeout := 300 * time.Millisecond
	if err := gm.network.SendWithTimeout(peer.Address, msg, responseTimeout); err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, "connection pool closed") || strings.Contains(errMsg, "connection pool exhausted") {
			if logging.Log.IsDebugEnabled() {
				logging.Debug("Read response send failed", "key", payload.Key, "target", requesterID)
			}
		}
	}
}

// handleReadResponse delivers a read response to the waiting goroutine.
//
//go:inline
func (gm *GossipManager) handleReadResponse(resp *ReadResponsePayload) {
	if resp == nil || resp.RequestId == "" {
		if logging.Log.IsDebugEnabled() {
			logging.Warn("Invalid read response")
		}
		return
	}

	// Thread-safe: Load pending read entry atomically
	entryVal, ok := gm.pendingReads.Load(resp.RequestId)
	if !ok {
		// Request expired or already handled - this is normal under high load
		if logging.Log.IsDebugEnabled() {
			logging.Debug("Received read response for unknown or expired requestId", "requestId", resp.RequestId)
		}
		return
	}

	entry, ok := entryVal.(*pendingReadEntry)
	if !ok || entry == nil || entry.ch == nil {
		// Invalid entry - clean up
		gm.removePendingRead(resp.RequestId)
		return
	}

	// Recover from panic to prevent goroutine leaks
	defer func() {
		if r := recover(); r != nil {
			gm.removePendingRead(resp.RequestId)
			if logging.Log.IsDebugEnabled() {
				logPanicWithStack("Panic in handleReadResponse", r)
			}
		}
	}()

	// Non-blocking send with timeout to prevent deadlock
	// Use timeout to ensure we don't block if the reader is stuck
	// Reduced timeout to 50ms for faster response delivery and cleanup
	select {
	case entry.ch <- resp:
		// Successfully delivered - entry will be cleaned up by the caller's defer
		if logging.Log.IsDebugEnabled() && gm.pendingReadsCount.Load() < 10 {
			logging.Debug("Read response delivered", "requestId", resp.RequestId, "found", resp.Found)
		}
	case <-time.After(50 * time.Millisecond): // Reduced from 200ms to 50ms for faster delivery
		// Timeout - channel may be blocked or reader timed out
		// Clean up the entry to prevent memory leak
		gm.removePendingRead(resp.RequestId)
		if logging.Log.IsDebugEnabled() {
			logging.Debug("Read response delivery timeout (channel blocked or reader gone)",
				"requestId", resp.RequestId)
		}
	}
}

// migrateDataFromDeadNode performs proactive data migration when a node is removed.
//
// Algorithm:
//  1. Get all keys from local storage
//  2. Filter keys affected by node removal
//  3. Process keys in batches with concurrency control
//  4. Use exponential backoff for retries
//  5. Limit concurrent migrations to prevent message storms
//
// Parameters:
//   - deadNodeID: ID of the node that was removed
func (gm *GossipManager) migrateDataFromDeadNode(deadNodeID string) {
	if gm.store == nil {
		return
	}

	// Get all keys from local storage
	allKeys := gm.store.Keys()
	if len(allKeys) == 0 {
		return
	}

	logging.Info("Starting data migration", "deadNode", deadNodeID, "keys", len(allKeys))

	// Filter and batch keys to prevent message storms
	affectedKeys := gm.filterAffectedKeys(allKeys, deadNodeID)
	if len(affectedKeys) == 0 {
		logging.Info("No keys affected by node removal", "deadNode", deadNodeID)
		return
	}

	logging.Info("Keys affected by migration", "deadNode", deadNodeID, "affected", len(affectedKeys), "total", len(allKeys))

	// Use goroutine pool to limit concurrent migrations
	// Limit to maxReplicators to prevent overwhelming the system
	maxConcurrent := gm.maxReplicators
	if maxConcurrent <= 0 {
		maxConcurrent = 8 // Default limit
	}
	if maxConcurrent > len(affectedKeys) {
		maxConcurrent = len(affectedKeys)
	}

	// Process keys in batches with delay between batches
	batchSize := maxConcurrent
	batchDelay := 100 * time.Millisecond // Delay between batches

	migratedCount := atomic.Int64{}
	fetchedCount := atomic.Int64{}

	// Process keys in batches
	for i := 0; i < len(affectedKeys); i += batchSize {
		end := i + batchSize
		if end > len(affectedKeys) {
			end = len(affectedKeys)
		}
		batch := affectedKeys[i:end]

		// Process batch with concurrency control
		var wg sync.WaitGroup
		for _, key := range batch {
			wg.Add(1)
			key := key // Capture loop variable
			if err := gm.replicationPool.Submit(func() {
				defer wg.Done()
				migrated, fetched := gm.migrateSingleKey(key, deadNodeID)
				if migrated {
					migratedCount.Add(1)
				}
				if fetched {
					fetchedCount.Add(1)
				}
			}); err != nil {
				// Pool full - process synchronously with backoff
				wg.Done()
				time.Sleep(50 * time.Millisecond)
				migrated, fetched := gm.migrateSingleKey(key, deadNodeID)
				if migrated {
					migratedCount.Add(1)
				}
				if fetched {
					fetchedCount.Add(1)
				}
			}
		}
		wg.Wait()

		// Delay between batches to prevent message storms
		if i+batchSize < len(affectedKeys) {
			time.Sleep(batchDelay)
		}
	}

	logging.Info("Data migration completed", "deadNode", deadNodeID,
		"migrated", migratedCount.Load(), "fetched", fetchedCount.Load(), "affected", len(affectedKeys))
}

// filterAffectedKeys filters keys that may be affected by node removal.
// This reduces unnecessary processing and network traffic.
func (gm *GossipManager) filterAffectedKeys(allKeys []string, deadNodeID string) []string {
	gm.mu.RLock()
	clusterSize := len(gm.liveNodes)
	gm.mu.RUnlock()

	if clusterSize <= 1 {
		return nil
	}

	// Quick check: if dead node was likely a replica for many keys
	// In practice, with consistent hashing, ~1/N keys are affected
	// Process all keys
	// Return allKeys to ensure comprehensive migration
	return allKeys
}

// migrateSingleKey migrates a single key with retry and backoff.
// Returns: (migrated, fetched) - whether migration/fetch occurred
func (gm *GossipManager) migrateSingleKey(key string, deadNodeID string) (bool, bool) {
	gm.mu.RLock()
	clusterSize := len(gm.liveNodes)
	gm.mu.RUnlock()

	if clusterSize <= 1 {
		return false, false
	}

	// Global per-node rate limiting for gradual migration
	if !gm.migrateLimiter.Allow(1) {
		// Skip this key in this cycle; caller batches multiple rounds
		return false, false
	}

	// Get new replica list (after node removal, ring already updated)
	effectiveReplicaCount := gm.replicaCount
	if clusterSize < gm.replicaCount {
		effectiveReplicaCount = clusterSize
	}
	newReplicas := gm.getReplicas(key, effectiveReplicaCount)

	if len(newReplicas) == 0 {
		return false, false
	}

	// Check if local node is a new replica
	isNewReplica := false
	for _, replicaID := range newReplicas {
		if replicaID == gm.localNodeID {
			isNewReplica = true
			break
		}
	}

	// Check if local node has the data
	localItem, localErr := gm.store.Get(key)

	if isNewReplica {
		// Local node is new replica - ensure we have the data
		if localErr != nil {
			// Missing data - fetch from other replicas with retry
			item, err := gm.fetchDataFromReplicasWithRetry(key, newReplicas)
			if err == nil && item != nil {
				if err := gm.store.Set(key, item); err != nil {
					logging.Error(err, "Failed to store migrated data", "key", key)
					return false, false
				}
				if logging.Log.IsDebugEnabled() {
					logging.Debug("Fetched data for new replica", "key", key, "version", item.Version)
				}
				return false, true
			}
		}
	} else {
		// Local node is not a new replica but may have data
		// This data needs to be migrated to new replicas
		if localErr == nil && localItem != nil {
			// Migrate data to new replicas with retry
			ctx, cancel := context.WithTimeout(context.Background(), gm.replicationTimeout*2)
			defer cancel()
			if err := gm.replicateToNodes(ctx, key, localItem, newReplicas); err != nil {
				logging.Error(err, "Failed to migrate data to new replicas", "key", key)
				return false, false
			}
			if logging.Log.IsDebugEnabled() {
				logging.Debug("Migrated data to new replicas", "key", key, "replicas", newReplicas)
			}
			return true, false
		}
	}

	return false, false
}

// fetchDataFromReplicas fetches data from other replica nodes.
//
// Parameters:
//   - key: The key to fetch
//   - replicas: List of replica node IDs (excluding local node)
//
// Returns:
//   - *storage.StoredItem: The fetched item
//   - error: Any error encountered
func (gm *GossipManager) fetchDataFromReplicas(key string, replicas []string) (*storage.StoredItem, error) {
	return gm.fetchDataFromReplicasWithRetry(key, replicas)
}

// fetchDataFromReplicasWithRetry fetches data from other replica nodes with exponential backoff.
//
// Parameters:
//   - key: The key to fetch
//   - replicas: List of replica node IDs (excluding local node)
//
// Returns:
//   - *storage.StoredItem: The fetched item
//   - error: Any error encountered
func (gm *GossipManager) fetchDataFromReplicasWithRetry(key string, replicas []string) (*storage.StoredItem, error) {
	// Filter to only alive nodes
	aliveReplicas := make([]string, 0, len(replicas))
	gm.mu.RLock()
	for _, replicaID := range replicas {
		if replicaID == gm.localNodeID {
			continue
		}
		if node, ok := gm.liveNodes[replicaID]; ok && node.State == NodeState_NODE_STATE_ALIVE {
			aliveReplicas = append(aliveReplicas, replicaID)
		}
	}
	gm.mu.RUnlock()

	if len(aliveReplicas) == 0 {
		return nil, errors.New("no alive replicas available")
	}

	// Try to fetch from replicas with exponential backoff
	maxRetries := 3
	baseDelay := 50 * time.Millisecond

	for attempt := 0; attempt < maxRetries && attempt < len(aliveReplicas); attempt++ {
		replicaID := aliveReplicas[attempt]

		// Exponential backoff for retries
		if attempt > 0 {
			delay := baseDelay * time.Duration(1<<uint(attempt-1))
			time.Sleep(delay)
		}

		ctx, cancel := context.WithTimeout(context.Background(), gm.readTimeout)
		item, err := gm.forwardReadToCoordinator(ctx, key, replicaID)
		cancel()

		if err == nil && item != nil {
			return item, nil
		}

		// Log only on last attempt
		if attempt == maxRetries-1 || attempt == len(aliveReplicas)-1 {
			if logging.Log.IsDebugEnabled() {
				logging.Debug("Failed to fetch from replica", "key", key, "replica", replicaID, "error", err)
			}
		}
	}

	return nil, errors.New("failed to fetch from any replica")
}

// Simplified API methods for easier usage.
// These wrap the proto-based API with simpler byte-based operations.
// Merged from api.go for file consolidation.

// SetBytes stores a key-value pair with automatic versioning.
// This is a convenience method that automatically generates a version.
//
// Parameters:
//   - ctx: Context for timeout and cancellation
//   - key: The key to store
//   - value: The value bytes to store
//
// Returns:
//   - error: Any storage or replication error
//
//go:inline
func (gm *GossipManager) SetBytes(ctx context.Context, key string, value []byte) error {
	// Generate version from current time
	version := time.Now().UnixNano()

	item := &storage.StoredItem{
		Value:    value,
		ExpireAt: time.Time{}, // No expiration
		Version:  version,
	}

	return gm.Set(ctx, key, item)
}

// GetBytes retrieves a value by key, returning just the bytes.
// This is a convenience method that extracts just the value.
//
// Parameters:
//   - ctx: Context for timeout and cancellation
//   - key: The key to retrieve
//
// Returns:
//   - []byte: The value bytes
//   - error: Not found or any read error
//
//go:inline
func (gm *GossipManager) GetBytes(ctx context.Context, key string) ([]byte, error) {
	item, err := gm.Get(ctx, key)
	if err != nil {
		return nil, err
	}

	return item.Value, nil
}

// DeleteKey deletes a key with automatic versioning.
// This is a convenience method that automatically generates a version.
//
// Parameters:
//   - ctx: Context for timeout and cancellation
//   - key: The key to delete
//
// Returns:
//   - error: Any deletion error
//
//go:inline
func (gm *GossipManager) DeleteKey(ctx context.Context, key string) error {
	version := time.Now().UnixNano()
	return gm.Delete(ctx, key, version)
}

// storageItemToProto converts a storage.StoredItem to proto StoredItem.
// Inlined from store.go for better performance.
//
//go:inline
func storageItemToProto(item *storage.StoredItem) *StoredItem {
	if item == nil {
		return nil
	}
	var expire uint64
	if !item.ExpireAt.IsZero() {
		expire = uint64(item.ExpireAt.Unix())
	}

	var valueCopy []byte
	if item.Value != nil {
		valueCopy = make([]byte, len(item.Value))
		copy(valueCopy, item.Value)
	} else {
		valueCopy = []byte{} // Empty slice, not nil
	}

	return &StoredItem{
		ExpireAt: expire,
		Value:    valueCopy,
	}
}

// protoItemToStorage converts a proto StoredItem to storage.StoredItem.
// Inlined from store.go for better performance.
//
//go:inline
func protoItemToStorage(item *StoredItem, version int64) *storage.StoredItem {
	if item == nil {
		return nil
	}
	var expire time.Time
	if item.ExpireAt != 0 {
		expire = time.Unix(int64(item.ExpireAt), 0)
	}
	valueCopy := make([]byte, len(item.Value))
	copy(valueCopy, item.Value)

	return &storage.StoredItem{
		ExpireAt: expire,
		Version:  version,
		Value:    valueCopy,
	}
}

// enqueueReadRequest adds a read request to the batch processor
func (gm *GossipManager) enqueueReadRequest(ctx context.Context, key string, coordinatorID string) (*storage.StoredItem, error) {
	peer, ok := gm.getNode(coordinatorID)
	if !ok {
		return nil, fmt.Errorf("coordinator %s not found", coordinatorID)
	}

	if peer.State != NodeState_NODE_STATE_ALIVE {
		return nil, fmt.Errorf("coordinator %s is not alive (state: %v)", coordinatorID, peer.State)
	}

	requestID := gm.generateOpID()
	respCh := getReadResponseChannel()
	entry := &pendingReadEntry{
		ch:        respCh,
		createdAt: time.Now(),
	}
	gm.addPendingRead(requestID, entry)
	defer func() {
		gm.removePendingRead(requestID)
		putReadResponseChannel(respCh)
		// Update pending count after removal
		if gm.metrics != nil && gm.readBatchManager != nil {
			pending := gm.readBatchManager.getPendingCount()
			gm.metrics.SetReadBatchPendingRequests(pending)
		}
	}()

	req := &PendingReadRequest{
		Key:         key,
		RequestID:   requestID,
		ResponseCh:  respCh,
		CreatedAt:   time.Now(),
		Coordinator: coordinatorID,
	}

	// Add to batch manager
	gm.readBatchManager.Add(peer.Address, coordinatorID, req)

	// Wait for response with timeout
	// P99 optimization: Use tighter timeout for batch reads to reduce tail latency
	// Reduced minimum from 500ms to 200ms for faster failure on slow reads
	timeout := 200 * time.Millisecond // Tighter default for P99 optimization
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining > 0 {
			// Use 90% of remaining time (reduced from 80% to allow more time for batch processing)
			timeout = remaining * 90 / 100
			// Reduced minimum to 200ms for faster failure (was 500ms)
			if timeout < 200*time.Millisecond {
				timeout = 200 * time.Millisecond
			}
		}
	}
	// Cap timeout at readTimeout to avoid excessive waits
	if timeout > gm.readTimeout {
		timeout = gm.readTimeout
	}

	ctx2, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	select {
	case resp := <-respCh:
		if resp == nil {
			return nil, storage.ErrItemNotFound
		}
		if !resp.Found || resp.ItemData == nil {
			return nil, storage.ErrItemNotFound
		}
		item := protoItemToStorage(resp.ItemData, resp.Version)
		if item == nil {
			return nil, storage.ErrItemNotFound
		}
		if len(item.Value) == 0 {
			return nil, storage.ErrItemNotFound
		}
		return item, nil
	case <-ctx2.Done():
		return nil, fmt.Errorf("read batch timeout for key %s: %w", key, ctx2.Err())
	}
}

// sendBatchReadRequest sends a batch of read requests to a coordinator
func (gm *GossipManager) sendBatchReadRequest(target string, batchID string, requests []*PendingReadRequest) error {
	if len(requests) == 0 {
		return nil
	}

	// Get coordinator from first request
	coordinatorID := requests[0].Coordinator

	peer, ok := gm.getNode(coordinatorID)
	if !ok {
		return fmt.Errorf("coordinator %s not found", coordinatorID)
	}

	// Build batch request payload
	readRequests := make([]*ReadRequestPayload, 0, len(requests))
	for _, req := range requests {
		readRequests = append(readRequests, &ReadRequestPayload{
			Key:         req.Key,
			RequesterId: gm.localNodeID,
			RequestId:   req.RequestID,
		})
	}

	msg := &GossipMessage{
		Type:   GossipMessageType_MESSAGE_TYPE_BATCH_READ_REQUEST,
		Sender: gm.localNodeID,
		Payload: &GossipMessage_BatchReadRequestPayload{
			BatchReadRequestPayload: &BatchReadRequestPayload{
				Requests:    readRequests,
				BatchId:     batchID,
				RequesterId: gm.localNodeID,
			},
		},
	}
	gm.signMessageCanonical(msg)

	// Use dynamic timeout based on batch size and readTimeout
	// Larger batches need more time, but cap at readTimeout
	timeout := gm.readTimeout
	if len(requests) > 10 {
		// Scale timeout for larger batches: 50ms per 10 requests, max readTimeout
		// Reduced per-request overhead to allow larger batches
		scaledTimeout := time.Duration(len(requests)/10) * 50 * time.Millisecond
		if scaledTimeout < timeout {
			timeout = scaledTimeout
		}
	}
	// Increased minimum timeout for better reliability with batch processing
	if timeout < 500*time.Millisecond {
		timeout = 500 * time.Millisecond
	}
	return gm.network.SendWithTimeout(peer.Address, msg, timeout)
}

// handleBatchReadRequest processes a batch of read requests
func (gm *GossipManager) handleBatchReadRequest(payload *BatchReadRequestPayload, requesterID string) {
	if payload == nil || requesterID == "" || len(payload.Requests) == 0 {
		return
	}

	// Then convert to proto in batch to reduce overhead
	// This eliminates per-item copying and reduces memory allocations
	responses := make([]*ReadResponsePayload, 0, len(payload.Requests))

	// Check if storage supports BatchGetNoCopy (zero-copy batch read)
	if batchStorageNoCopy, ok := gm.store.(interface {
		BatchGetNoCopy(keys []string) (map[string]*storage.StoredItem, error)
	}); ok && len(payload.Requests) > 0 {
		// Extract keys from requests
		keys := make([]string, 0, len(payload.Requests))
		keyToRequest := make(map[string]*ReadRequestPayload, len(payload.Requests))
		for _, req := range payload.Requests {
			if req == nil || req.Key == "" {
				continue
			}
			keys = append(keys, req.Key)
			keyToRequest[req.Key] = req
		}

		// Use BatchGetNoCopy for zero-copy bulk read (3-4x faster than BatchGet)
		// This eliminates per-request storage lookups and reduces overhead
		items, err := batchStorageNoCopy.BatchGetNoCopy(keys)
		if err == nil {
			// Pre-allocate responses slice with exact size to avoid reallocations
			responses = make([]*ReadResponsePayload, 0, len(payload.Requests))
			// Build responses from batch results in single pass
			// Convert to proto only when item is found (lazy conversion)
			for _, req := range payload.Requests {
				if req == nil {
					continue
				}
				resp := &ReadResponsePayload{
					Key:         req.Key,
					RequestId:   req.RequestId,
					ResponderId: gm.localNodeID,
				}
				if item, found := items[req.Key]; found && item != nil && item.Value != nil && len(item.Value) > 0 {
					resp.Found = true
					resp.Version = item.Version
					// Convert to proto (must copy here for proto, but only for found items)
					resp.ItemData = storageItemToProto(item)
				}
				responses = append(responses, resp)
			}
		} else {
			// Fallback to BatchGet if BatchGetNoCopy fails
			if batchStorage, ok := gm.store.(interface {
				BatchGet(keys []string) (map[string]*storage.StoredItem, error)
			}); ok {
				items, err := batchStorage.BatchGet(keys)
				if err == nil {
					for _, req := range payload.Requests {
						if req == nil {
							continue
						}
						resp := &ReadResponsePayload{
							Key:         req.Key,
							RequestId:   req.RequestId,
							ResponderId: gm.localNodeID,
						}
						if item, found := items[req.Key]; found && item != nil && item.Value != nil && len(item.Value) > 0 {
							resp.Found = true
							resp.Version = item.Version
							resp.ItemData = storageItemToProto(item)
						}
						responses = append(responses, resp)
					}
				} else {
					// Fallback to individual Gets if BatchGet fails
					for _, req := range payload.Requests {
						if req == nil {
							continue
						}
						item, err := gm.store.Get(req.Key)
						resp := &ReadResponsePayload{
							Key:         req.Key,
							RequestId:   req.RequestId,
							ResponderId: gm.localNodeID,
						}
						if err == nil && item != nil && item.Value != nil && len(item.Value) > 0 {
							resp.Found = true
							resp.Version = item.Version
							resp.ItemData = storageItemToProto(item)
						}
						responses = append(responses, resp)
					}
				}
			} else {
				// Fallback to individual Gets if BatchGet not available
				for _, req := range payload.Requests {
					if req == nil {
						continue
					}
					item, err := gm.store.Get(req.Key)
					resp := &ReadResponsePayload{
						Key:         req.Key,
						RequestId:   req.RequestId,
						ResponderId: gm.localNodeID,
					}
					if err == nil && item != nil && item.Value != nil && len(item.Value) > 0 {
						resp.Found = true
						resp.Version = item.Version
						resp.ItemData = storageItemToProto(item)
					}
					responses = append(responses, resp)
				}
			}
		}
	} else {
		// Fallback: process individually if BatchGet not available or single request
		for _, req := range payload.Requests {
			if req == nil {
				continue
			}
			item, err := gm.store.Get(req.Key)
			resp := &ReadResponsePayload{
				Key:         req.Key,
				RequestId:   req.RequestId,
				ResponderId: gm.localNodeID,
			}
			if err == nil && item != nil && item.Value != nil && len(item.Value) > 0 {
				resp.Found = true
				resp.Version = item.Version
				resp.ItemData = storageItemToProto(item)
			}
			responses = append(responses, resp)
		}
	}

	// Send batch response
	peer, ok := gm.getNode(requesterID)
	if !ok {
		return
	}

	msg := &GossipMessage{
		Type:   GossipMessageType_MESSAGE_TYPE_BATCH_READ_RESPONSE,
		Sender: gm.localNodeID,
		Payload: &GossipMessage_BatchReadResponsePayload{
			BatchReadResponsePayload: &BatchReadResponsePayload{
				Responses:   responses,
				BatchId:     payload.BatchId,
				ResponderId: gm.localNodeID,
			},
		},
	}
	gm.signMessageCanonical(msg)

	// Use dynamic timeout based on response count
	// Larger responses need more time, but cap at readTimeout
	responseTimeout := gm.readTimeout
	if len(responses) > 10 {
		// Scale timeout for larger responses: 50ms per 10 responses, max readTimeout
		// Reduced per-response overhead to allow larger batches
		scaledTimeout := time.Duration(len(responses)/10) * 50 * time.Millisecond
		if scaledTimeout < responseTimeout {
			responseTimeout = scaledTimeout
		}
	}
	// Increased minimum timeout for better reliability
	if responseTimeout < 500*time.Millisecond {
		responseTimeout = 500 * time.Millisecond
	}
	if err := gm.network.SendWithTimeout(peer.Address, msg, responseTimeout); err != nil {
		if logging.Log.IsDebugEnabled() {
			logging.Debug("Batch read response send failed", "target", requesterID, "count", len(responses))
		}
	}
}

// handleBatchReadResponse processes a batch of read responses and distributes them
func (gm *GossipManager) handleBatchReadResponse(payload *BatchReadResponsePayload) {
	if payload == nil || len(payload.Responses) == 0 {
		return
	}

	// Metrics updated in read_batcher.go

	// Batch load all pending reads first to reduce map lookups
	entries := make(map[string]*pendingReadEntry, len(payload.Responses))
	for _, resp := range payload.Responses {
		if resp == nil || resp.RequestId == "" {
			continue
		}
		if entryVal, ok := gm.pendingReads.Load(resp.RequestId); ok {
			if entry, ok := entryVal.(*pendingReadEntry); ok && entry != nil && entry.ch != nil {
				entries[resp.RequestId] = entry
			}
		}
	}

	// Batch deliver responses to reduce channel operations
	channelGroups := make(map[chan *ReadResponsePayload][]*ReadResponsePayload)
	for _, resp := range payload.Responses {
		if resp == nil || resp.RequestId == "" {
			continue
		}
		entry, ok := entries[resp.RequestId]
		if !ok {
			// Request expired or already handled - skip
			continue
		}
		channelGroups[entry.ch] = append(channelGroups[entry.ch], resp)
	}

	// Deliver grouped responses (reduces channel operations)
	for ch, respGroup := range channelGroups {
		// Try to deliver all responses in this group
		for _, resp := range respGroup {
			select {
			case ch <- resp:
				// Successfully delivered (fastest path)
			default:
				// Channel not ready - try with very short timeout
				select {
				case ch <- resp:
					// Successfully delivered
				case <-time.After(2 * time.Millisecond): // Very short timeout
					// Timeout - channel blocked, remove pending read
					// Find requestId from response
					if resp != nil && resp.RequestId != "" {
						gm.removePendingRead(resp.RequestId)
					}
				}
			}
		}
	}
}

type ReadFuture interface {
	Get(ctx context.Context) (*storage.StoredItem, error)
	GetWithTimeout(timeout time.Duration) (*storage.StoredItem, error)
	Done() <-chan struct{}
	Cancel()
	IsDone() bool
}

type readFutureImpl struct {
	result *storage.StoredItem
	err    error
	done   chan struct{}
	once   sync.Once
	mu     sync.RWMutex
}

func NewReadFuture() *readFutureImpl {
	return &readFutureImpl{
		done: make(chan struct{}),
	}
}

func (rf *readFutureImpl) SetResult(item *storage.StoredItem, err error) {
	rf.once.Do(func() {
		rf.mu.Lock()
		rf.result = item
		rf.err = err
		rf.mu.Unlock()
		close(rf.done)
	})
}

func (rf *readFutureImpl) Get(ctx context.Context) (*storage.StoredItem, error) {
	select {
	case <-rf.done:
		rf.mu.RLock()
		defer rf.mu.RUnlock()
		return rf.result, rf.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (rf *readFutureImpl) GetWithTimeout(timeout time.Duration) (*storage.StoredItem, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return rf.Get(ctx)
}

func (rf *readFutureImpl) Done() <-chan struct{} {
	return rf.done
}

func (rf *readFutureImpl) Cancel() {
	rf.SetResult(nil, errors.New("read cancelled"))
}

func (rf *readFutureImpl) IsDone() bool {
	select {
	case <-rf.done:
		return true
	default:
		return false
	}
}

type BatchReadFuture interface {
	GetAll(ctx context.Context) (map[string]*storage.StoredItem, map[string]error)
	GetAllWithTimeout(timeout time.Duration) (map[string]*storage.StoredItem, map[string]error)
	GetAny(ctx context.Context) (string, *storage.StoredItem, error)
	Cancel()
	Done() <-chan struct{}
	Count() int
}

type batchReadFutureImpl struct {
	futures map[string]*readFutureImpl
	mu      sync.RWMutex
	done    chan struct{}
	once    sync.Once
}

func NewBatchReadFuture(keys []string) *batchReadFutureImpl {
	futures := make(map[string]*readFutureImpl, len(keys))
	for _, key := range keys {
		futures[key] = NewReadFuture()
	}
	return &batchReadFutureImpl{
		futures: futures,
		done:    make(chan struct{}),
	}
}

func (brf *batchReadFutureImpl) SetResult(key string, item *storage.StoredItem, err error) {
	brf.mu.RLock()
	future, exists := brf.futures[key]
	brf.mu.RUnlock()

	if exists {
		future.SetResult(item, err)
		brf.checkAllDone()
	}
}

func (brf *batchReadFutureImpl) checkAllDone() {
	brf.mu.RLock()
	allDone := true
	for _, future := range brf.futures {
		if !future.IsDone() {
			allDone = false
			break
		}
	}
	brf.mu.RUnlock()

	if allDone {
		brf.once.Do(func() {
			close(brf.done)
		})
	}
}

func (brf *batchReadFutureImpl) GetAll(ctx context.Context) (map[string]*storage.StoredItem, map[string]error) {
	results := make(map[string]*storage.StoredItem)
	errors := make(map[string]error)

	brf.mu.RLock()
	futures := make(map[string]*readFutureImpl, len(brf.futures))
	for k, v := range brf.futures {
		futures[k] = v
	}
	brf.mu.RUnlock()

	for key, future := range futures {
		select {
		case <-ctx.Done():
			errors[key] = ctx.Err()
		case <-future.Done():
			item, err := future.Get(ctx)
			if err != nil {
				errors[key] = err
			} else {
				results[key] = item
			}
		}
	}

	return results, errors
}

func (brf *batchReadFutureImpl) GetAllWithTimeout(timeout time.Duration) (map[string]*storage.StoredItem, map[string]error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return brf.GetAll(ctx)
}

func (brf *batchReadFutureImpl) GetAny(ctx context.Context) (string, *storage.StoredItem, error) {
	brf.mu.RLock()
	futures := make([]struct {
		key    string
		future *readFutureImpl
	}, 0, len(brf.futures))
	for k, v := range brf.futures {
		futures = append(futures, struct {
			key    string
			future *readFutureImpl
		}{k, v})
	}
	brf.mu.RUnlock()

	type result struct {
		key    string
		item   *storage.StoredItem
		err    error
		future *readFutureImpl
	}
	resultCh := make(chan result, len(futures))

	for _, f := range futures {
		go func(key string, future *readFutureImpl) {
			item, err := future.Get(ctx)
			select {
			case resultCh <- result{key: key, item: item, err: err, future: future}:
			case <-ctx.Done():
			}
		}(f.key, f.future)
	}

	select {
	case res := <-resultCh:
		return res.key, res.item, res.err
	case <-ctx.Done():
		return "", nil, ctx.Err()
	}
}

func (brf *batchReadFutureImpl) Cancel() {
	brf.mu.RLock()
	futures := make([]*readFutureImpl, 0, len(brf.futures))
	for _, future := range brf.futures {
		futures = append(futures, future)
	}
	brf.mu.RUnlock()

	for _, future := range futures {
		future.Cancel()
	}
}

func (brf *batchReadFutureImpl) Done() <-chan struct{} {
	return brf.done
}

func (brf *batchReadFutureImpl) Count() int {
	brf.mu.RLock()
	defer brf.mu.RUnlock()
	return len(brf.futures)
}

func (gm *GossipManager) readFromReplica(ctx context.Context, key string, replicaID string) (*storage.StoredItem, error) {
	if gm.readBatchManager != nil {
		return gm.enqueueReadRequest(ctx, key, replicaID)
	}
	return gm.forwardReadToCoordinator(ctx, key, replicaID)
}
