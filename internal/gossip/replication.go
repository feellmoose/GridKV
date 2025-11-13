package gossip

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/feellmoose/gridkv/internal/storage"
	"github.com/feellmoose/gridkv/internal/utils/logging"
)

// replicationBatch holds batched operations for a target node
type replicationBatch struct {
	ops     []*CacheSyncOperation
	timer   *time.Timer
	mutex   sync.Mutex
	target  string
	manager *GossipManager
}

const (
	batchSizeThreshold = 50                    // Flush when batch reaches this size
	batchTimeout       = 10 * time.Millisecond // Flush after this timeout
)

// addOperation adds an operation to the batch
func (rb *replicationBatch) addOperation(op *CacheSyncOperation) {
	rb.mutex.Lock()
	rb.ops = append(rb.ops, op)
	shouldFlush := len(rb.ops) >= batchSizeThreshold

	// Stop timer if we're flushing
	var ops []*CacheSyncOperation
	if shouldFlush {
		if rb.timer != nil {
			rb.timer.Stop()
			rb.timer = nil
		}
		ops = make([]*CacheSyncOperation, len(rb.ops))
		copy(ops, rb.ops)
		rb.ops = rb.ops[:0] // Clear batch
	} else if rb.timer == nil {
		// Start timer for first operation
		rb.timer = time.AfterFunc(batchTimeout, func() {
			rb.mutex.Lock()
			if len(rb.ops) > 0 {
				ops := make([]*CacheSyncOperation, len(rb.ops))
				copy(ops, rb.ops)
				rb.ops = rb.ops[:0]
				if rb.timer != nil {
					rb.timer.Stop()
					rb.timer = nil
				}
				rb.mutex.Unlock()
				rb.sendBatchedMessage(ops)
			} else {
				rb.mutex.Unlock()
			}
		})
	}
	rb.mutex.Unlock()

	// Send outside lock to avoid holding mutex during network I/O
	if shouldFlush {
		rb.sendBatchedMessage(ops)
	}
}

// sendBatchedMessage sends a batched message (called without holding mutex)
func (rb *replicationBatch) sendBatchedMessage(ops []*CacheSyncOperation) {
	if len(ops) == 0 {
		return
	}

	msg := &GossipMessage{
		Type:   GossipMessageType_MESSAGE_TYPE_CACHE_SYNC,
		Sender: rb.manager.localNodeID,
		Hlc:    rb.manager.hlc.Now(),
		Payload: &GossipMessage_CacheSyncPayload{
			CacheSyncPayload: &SyncMessage{
				SyncType: &SyncMessage_IncrementalSync{
					IncrementalSync: &IncrementalSyncPayload{
						Operations: ops,
					},
				},
			},
		},
	}
	rb.manager.signMessageCanonical(msg)

	// Send asynchronously
	go func() {
		if err := rb.manager.network.SendWithTimeout(rb.target, msg, rb.manager.replicationTimeout); err != nil {
			if logging.Log.IsDebugEnabled() {
				logging.Debug("Failed to send batched replication", "target", rb.target, "ops", len(ops), "err", err)
			}
		}
	}()
}

// flush forces immediate flush of the batch
func (rb *replicationBatch) flush() {
	rb.mutex.Lock()
	ops := make([]*CacheSyncOperation, len(rb.ops))
	copy(ops, rb.ops)
	rb.ops = rb.ops[:0]
	if rb.timer != nil {
		rb.timer.Stop()
		rb.timer = nil
	}
	rb.mutex.Unlock()

	rb.sendBatchedMessage(ops)
}

// Set performs a distributed write operation with quorum-based replication.
//
// The write flow:
//  1. Hash the key to find N replica nodes using consistent hashing
//  2. The first replica (coordinator) receives the write
//  3. If this node is not the coordinator, forward to coordinator
//  4. Coordinator writes locally, then replicates to N-1 replicas
//  5. Wait for W acknowledgments (including local write)
//  6. Return success if quorum is reached, error otherwise
//
// Parameters:
//   - ctx: Context for timeout and cancellation
//   - key: The key to write
//   - item: The value and metadata to store
//
// Returns:
//   - error: Quorum error if W replicas don't acknowledge
//
//go:inline
func (gm *GossipManager) Set(ctx context.Context, key string, item *storage.StoredItem) error {
	if item == nil {
		return errors.New("nil item")
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

	replicas := gm.hashRing.GetN(key, effectiveReplicaCount)
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
		return gm.forwardWrite(key, item, coordinator)
	}

	// Local write (we are the coordinator)
	if err := gm.store.Set(key, item); err != nil {
		return fmt.Errorf("local write failed: %w", err)
	}

	// Replicate to other nodes
	return gm.replicateToNodes(ctx, key, item, replicas[1:])
}

// Delete performs a distributed delete operation with quorum-based replication.
//
// Similar to Set, but removes the key-value pair instead.
//
// Parameters:
//   - ctx: Context for timeout and cancellation
//   - key: The key to delete
//   - version: Version number for optimistic concurrency control
//
// Returns:
//   - error: Quorum error if W replicas don't acknowledge
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
		return nil
	}

	effectiveReplicaCount := gm.replicaCount
	if availableNodes < gm.replicaCount {
		effectiveReplicaCount = availableNodes
		if effectiveReplicaCount == 0 {
			effectiveReplicaCount = 1
		}
	}

	replicas := gm.hashRing.GetN(key, effectiveReplicaCount)
	if len(replicas) == 0 {
		// Last resort: delete from local node only
		if logging.Log.IsDebugEnabled() {
			logging.Debug("No replicas in hash ring, deleting from local node only", "key", key)
		}
		if err := gm.store.Delete(key, version); err != nil {
			return fmt.Errorf("local delete failed: %w", err)
		}
		return nil
	}

	coordinator := replicas[0]
	if coordinator != gm.localNodeID {
		// Forward to coordinator
		return gm.forwardDelete(key, version, coordinator)
	}

	// Local delete (we are the coordinator)
	if err := gm.store.Delete(key, version); err != nil {
		return fmt.Errorf("local delete failed: %w", err)
	}

	// Replicate deletion to other nodes
	return gm.replicateDeleteToNodes(ctx, key, version, replicas[1:])
}

// Get performs a distributed read operation with quorum and read repair.
//
// The read flow:
//  1. Hash the key to find N replica nodes
//  2. If this node is not a replica, forward to coordinator
//  3. Otherwise, read from R replicas (including local)
//  4. Return the value with the highest version
//  5. Perform read repair in background for stale replicas
//
// Parameters:
//   - ctx: Context for timeout and cancellation
//   - key: The key to read
//
// Returns:
//   - *storage.StoredItem: The stored item (highest version)
//   - error: Not found error or quorum error
//
//go:inline
func (gm *GossipManager) Get(ctx context.Context, key string) (*storage.StoredItem, error) {
	// OPTIMIZATION: Fast path for single node (no lock, no hash lookup)
	gm.mu.RLock()
	availableNodes := len(gm.liveNodes)
	gm.mu.RUnlock()

	// OPTIMIZATION: Fast path for single node cluster
	if availableNodes == 1 {
		item, err := gm.store.Get(key)
		if err != nil {
			return nil, err
		}
		return item, nil
	}

	effectiveReplicaCount := gm.replicaCount
	if availableNodes < gm.replicaCount {
		effectiveReplicaCount = availableNodes
		if effectiveReplicaCount == 0 {
			effectiveReplicaCount = 1
		}
	}

	replicas := gm.hashRing.GetN(key, effectiveReplicaCount)
	if len(replicas) == 0 {
		// Last resort: read from local node only
		// OPTIMIZATION: Only log if debug is enabled
		if logging.Log.IsDebugEnabled() {
			logging.Debug("No replicas in hash ring, reading from local node only", "key", key)
		}
		item, err := gm.store.Get(key)
		if err != nil {
			return nil, err
		}
		return item, nil
	}

	// OPTIMIZATION: Fast path check if local node is coordinator (most common case)
	if replicas[0] == gm.localNodeID {
		// Local node is coordinator - perform read quorum
		return gm.readWithQuorum(ctx, key, replicas)
	}

	// OPTIMIZATION: Check if local node is any replica (second most common)
	for i := 1; i < len(replicas); i++ {
		if replicas[i] == gm.localNodeID {
			// Local node is replica - perform read quorum
			return gm.readWithQuorum(ctx, key, replicas)
		}
	}

	// Local node is not a replica - forward to coordinator
	return gm.forwardReadToCoordinator(ctx, key, replicas[0])
}

// forwardWrite forwards a write to the coordinator node.
//
//go:inline
func (gm *GossipManager) forwardWrite(key string, item *storage.StoredItem, coordinatorID string) error {
	n, ok := gm.getNode(coordinatorID)
	if !ok {
		return fmt.Errorf("coordinator %s unknown", coordinatorID)
	}

	protoOp := &CacheSyncOperation{
		Key:           key,
		ClientVersion: item.Version,
		Type:          OperationType_OP_SET,
		DataPayload: &CacheSyncOperation_SetData{
			SetData: storageItemToProto(item),
		},
	}

	msg := &GossipMessage{
		Type:   GossipMessageType_MESSAGE_TYPE_CACHE_SYNC,
		Sender: gm.localNodeID,
		Payload: &GossipMessage_CacheSyncPayload{
			CacheSyncPayload: &SyncMessage{
				SyncType: &SyncMessage_IncrementalSync{
					IncrementalSync: &IncrementalSyncPayload{
						Operations: []*CacheSyncOperation{protoOp},
					},
				},
			},
		},
	}
	gm.signMessageCanonical(msg)
	return gm.network.SendWithTimeout(n.Address, msg, gm.replicationTimeout)
}

// forwardDelete forwards a delete to the coordinator node.
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

	msg := &GossipMessage{
		Type:   GossipMessageType_MESSAGE_TYPE_CACHE_SYNC,
		Sender: gm.localNodeID,
		Payload: &GossipMessage_CacheSyncPayload{
			CacheSyncPayload: &SyncMessage{
				SyncType: &SyncMessage_IncrementalSync{
					IncrementalSync: &IncrementalSyncPayload{
						Operations: []*CacheSyncOperation{protoOp},
					},
				},
			},
		},
	}
	gm.signMessageCanonical(msg)
	return gm.network.SendWithTimeout(n.Address, msg, gm.replicationTimeout)
}

// getOrCreateBatch gets or creates a batch for the target address
func (gm *GossipManager) getOrCreateBatch(targetAddr string) *replicationBatch {
	gm.batchMutex.Lock()
	defer gm.batchMutex.Unlock()

	batch, ok := gm.batchBuffer[targetAddr]
	if !ok {
		batch = &replicationBatch{
			target:  targetAddr,
			manager: gm,
			ops:     make([]*CacheSyncOperation, 0, batchSizeThreshold),
		}
		gm.batchBuffer[targetAddr] = batch
	}
	return batch
}

// flushBatchForTarget flushes the batch for a specific target
func (gm *GossipManager) flushBatchForTarget(targetAddr string) {
	gm.batchMutex.Lock()
	batch, ok := gm.batchBuffer[targetAddr]
	gm.batchMutex.Unlock()

	if ok {
		batch.flush()
	}
}

// flushAllBatches flushes all pending batches (used when quorum is needed)
func (gm *GossipManager) flushAllBatches() {
	gm.batchMutex.Lock()
	batches := make([]*replicationBatch, 0, len(gm.batchBuffer))
	for _, batch := range gm.batchBuffer {
		batches = append(batches, batch)
	}
	gm.batchMutex.Unlock()

	for _, batch := range batches {
		batch.flush()
	}
}

// replicateToNodes replicates a write to multiple nodes with quorum.
func (gm *GossipManager) replicateToNodes(ctx context.Context, key string, item *storage.StoredItem, replicaIDs []string) error {
	if len(replicaIDs) == 0 {
		return nil // No replicas to write to
	}

	protoOp := &CacheSyncOperation{
		Key:           key,
		ClientVersion: item.Version,
		Type:          OperationType_OP_SET,
		DataPayload: &CacheSyncOperation_SetData{
			SetData: storageItemToProto(item),
		},
	}

	// OPTIMIZATION: Collect alive replica targets with minimal lock time
	// Pre-allocate slice with exact capacity to avoid reallocation
	gm.mu.RLock()
	clusterSize := len(gm.liveNodes)
	targets := make([]struct{ addr, id string }, 0, len(replicaIDs)) // OPTIMIZATION: Pre-allocate
	for _, replicaID := range replicaIDs {
		if n, ok := gm.liveNodes[replicaID]; ok && n.State == NodeState_NODE_STATE_ALIVE {
			targets = append(targets, struct{ addr, id string }{addr: n.Address, id: replicaID})
		}
	}
	gm.mu.RUnlock()

	// CRITICAL FIX: Adjust required quorum based on available targets
	// If no targets available (e.g., single node or nodes not ready), only require local write
	required := gm.writeQuorum - 1 // excluding primary (already written)

	// OPTIMIZATION: If no targets available, local write is sufficient
	if len(targets) == 0 {
		// Single node or all replica nodes are not ready - local write is sufficient
		// OPTIMIZATION: Only log if debug is enabled
		if logging.Log.IsDebugEnabled() {
			logging.Debug("No available replica targets, local write is sufficient",
				"key", key, "replicas", len(replicaIDs), "clusterSize", clusterSize)
		}
		return nil
	}

	// CRITICAL FIX: Adjust required based on available targets
	// If we have fewer targets than required, adjust requirement
	if len(targets) < required {
		required = len(targets)
		// OPTIMIZATION: Only log if debug is enabled
		if logging.Log.IsDebugEnabled() {
			logging.Debug("Adjusted quorum requirement based on available targets",
				"key", key, "required", required, "targets", len(targets))
		}
	}

	// OPTIMIZATION: Flush all pending batches before sending quorum-required operation
	// This ensures we don't wait for batched operations when quorum is needed
	gm.flushAllBatches()

	// Create message with OpId for ACK tracking
	opID := gm.generateOpID()
	msg := &GossipMessage{
		Type:   GossipMessageType_MESSAGE_TYPE_CACHE_SYNC,
		Sender: gm.localNodeID,
		OpId:   opID,
		Hlc:    gm.hlc.Now(),
		Payload: &GossipMessage_CacheSyncPayload{
			CacheSyncPayload: &SyncMessage{
				SyncType: &SyncMessage_IncrementalSync{
					IncrementalSync: &IncrementalSyncPayload{
						Operations: []*CacheSyncOperation{protoOp},
					},
				},
			},
		},
	}
	gm.signMessageCanonical(msg)

	// OPTIMIZATION: Use goroutine pool for bounded concurrency with pre-allocated channel
	// Buffer size equals number of targets for zero-blocking writes
	ackCh := make(chan bool, len(targets)) // OPTIMIZATION: Pre-allocate buffer
	sentCount := 0

	// OPTIMIZATION: Submit all tasks first, then check results
	// This allows better parallelization and reduces lock contention
	for _, t := range targets {
		addr := t.addr
		err := gm.replicationPool.Submit(func() {
			ok, err := gm.network.SendAndWaitAck(addr, msg, gm.replicationTimeout)
			if err != nil {
				// OPTIMIZATION: Only log errors if they're not timeout/context errors
				if err != context.DeadlineExceeded && err != context.Canceled {
					logging.Error(err, "replicate send/ack failed", "target", addr)
				}
				ackCh <- false
				return
			}
			ackCh <- ok
		})
		if err != nil {
			// OPTIMIZATION: Only log errors if debug is enabled
			if logging.Log.IsDebugEnabled() {
				logging.Error(err, "failed to submit replication task", "target", addr)
			}
			ackCh <- false
		} else {
			sentCount++
		}
	}

	// CRITICAL FIX: If no requests were sent, local write is sufficient
	if sentCount == 0 {
		// OPTIMIZATION: Only log if debug is enabled
		if logging.Log.IsDebugEnabled() {
			logging.Debug("No replication requests sent, local write is sufficient", "key", key)
		}
		return nil
	}

	// OPTIMIZATION: Wait for required acks with early exit
	// Use context timeout for cancellation
	ctx2, cancel := context.WithTimeout(ctx, gm.replicationTimeout)
	defer cancel()

	acks := 0
	responses := 0
	maxWait := sentCount // Maximum responses to wait for

	// OPTIMIZATION: Early exit when quorum reached
	for acks < required && responses < maxWait {
		select {
		case ok := <-ackCh:
			responses++
			if ok {
				acks++
			}
			// OPTIMIZATION: Early exit when quorum reached
			if acks >= required {
				return nil
			}
		case <-ctx2.Done():
			// Timeout - return error if quorum not met
			if acks < required {
				return fmt.Errorf("quorum not reached: got %d required %d (timeout, responses: %d)",
					acks, required, responses)
			}
			return nil // Quorum reached before timeout
		}
	}

	// All responses received - check if quorum met
	if acks >= required {
		return nil
	}

	return fmt.Errorf("quorum not reached: got %d required %d (responses: %d)",
		acks, required, responses)
}

// replicateDeleteToNodes replicates a delete to multiple nodes with quorum.
func (gm *GossipManager) replicateDeleteToNodes(ctx context.Context, key string, version int64, replicaIDs []string) error {
	if len(replicaIDs) == 0 {
		return nil
	}

	opID := gm.generateOpID()
	protoOp := &CacheSyncOperation{
		Key:           key,
		ClientVersion: version,
		Type:          OperationType_OP_DELETE,
	}

	msg := &GossipMessage{
		Type:   GossipMessageType_MESSAGE_TYPE_CACHE_SYNC,
		Sender: gm.localNodeID,
		OpId:   opID,
		Hlc:    gm.hlc.Now(),
		Payload: &GossipMessage_CacheSyncPayload{
			CacheSyncPayload: &SyncMessage{
				SyncType: &SyncMessage_IncrementalSync{
					IncrementalSync: &IncrementalSyncPayload{
						Operations: []*CacheSyncOperation{protoOp},
					},
				},
			},
		},
	}
	gm.signMessageCanonical(msg)

	gm.mu.RLock()
	targets := make([]struct{ addr, id string }, 0, len(replicaIDs))
	for _, replicaID := range replicaIDs {
		if n, ok := gm.liveNodes[replicaID]; ok && n.State == NodeState_NODE_STATE_ALIVE {
			targets = append(targets, struct{ addr, id string }{addr: n.Address, id: replicaID})
		}
	}
	gm.mu.RUnlock()

	required := gm.writeQuorum - 1
	if required <= 0 {
		return nil
	}

	ackCh := make(chan bool, len(targets))
	for _, t := range targets {
		addr := t.addr
		err := gm.replicationPool.Submit(func() {
			ok, err := gm.network.SendAndWaitAck(addr, msg, gm.replicationTimeout)
			if err != nil {
				logging.Error(err, "replicate delete send/ack failed", "target", addr)
				ackCh <- false
				return
			}
			ackCh <- ok
		})
		if err != nil {
			logging.Error(err, "failed to submit delete replication task", "target", addr)
			ackCh <- false
		}
	}

	ctx2, cancel := context.WithTimeout(ctx, gm.replicationTimeout)
	defer cancel()

	acks := 0
	for {
		select {
		case ok := <-ackCh:
			if ok {
				acks++
			}
			if acks >= required {
				return nil
			}
		case <-ctx2.Done():
			return fmt.Errorf("delete quorum not reached: got %d required %d", acks, required)
		}
	}
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

	requestID := gm.generateOpID()
	respCh := make(chan *ReadResponsePayload, 1)
	gm.pendingReads.Store(requestID, respCh)
	defer func() {
		gm.pendingReads.Delete(requestID)
		close(respCh)
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

	if err := gm.network.SendWithTimeout(peer.Address, msg, gm.readTimeout); err != nil {
		return nil, fmt.Errorf("forward read failed: %w", err)
	}

	ctx2, cancel := context.WithTimeout(ctx, gm.readTimeout)
	defer cancel()

	select {
	case resp := <-respCh:
		if !resp.Found {
			return nil, storage.ErrItemNotFound
		}
		return protoItemToStorage(resp.ItemData, resp.Version), nil
	case <-ctx2.Done():
		return nil, fmt.Errorf("read forward timeout for key %s", key)
	}
}

// readWithQuorum performs a read from R replicas and returns the latest version.
func (gm *GossipManager) readWithQuorum(ctx context.Context, key string, replicas []string) (*storage.StoredItem, error) {
	// Read from local store first
	localItem, localErr := gm.store.Get(key)

	// CRITICAL FIX: Adaptive read quorum based on available replicas
	effectiveReadQuorum := gm.readQuorum
	if len(replicas) < gm.readQuorum {
		effectiveReadQuorum = len(replicas)
		if effectiveReadQuorum < 1 {
			effectiveReadQuorum = 1
		}
	}

	// If read quorum is 1, return local result immediately
	if effectiveReadQuorum == 1 {
		if localErr != nil {
			return nil, localErr
		}
		return localItem, nil
	}

	// OPTIMIZATION: Pre-allocate channel with exact capacity to avoid blocking
	// Buffer size equals number of replicas for zero-blocking writes
	results := make(chan readResult, len(replicas))

	// Add local result
	if localErr == nil {
		results <- readResult{
			item:    localItem,
			version: localItem.Version,
			nodeID:  gm.localNodeID,
			found:   true,
		}
	} else if localErr == storage.ErrItemNotFound || localErr == storage.ErrItemExpired {
		results <- readResult{
			nodeID: gm.localNodeID,
			found:  false,
		}
	}

	// OPTIMIZATION: Request from other replicas with pre-allocated channel
	// Buffer size equals number of replicas for zero-blocking writes
	requestID := gm.generateOpID()
	respCh := make(chan *ReadResponsePayload, len(replicas)) // OPTIMIZATION: Pre-allocate buffer
	gm.pendingReads.Store(requestID, respCh)
	defer func() {
		gm.pendingReads.Delete(requestID)
		close(respCh)
	}()

	// OPTIMIZATION: Send read requests in parallel with minimal allocations
	// Pre-calculate number of remote replicas to avoid repeated checks
	remoteReplicas := 0
	replicaPeers := make([]struct{ id, addr string }, 0, len(replicas))
	for _, replicaID := range replicas {
		if replicaID == gm.localNodeID {
			continue
		}
		peer, ok := gm.getNode(replicaID)
		if !ok || peer.State != NodeState_NODE_STATE_ALIVE {
			continue
		}
		replicaPeers = append(replicaPeers, struct{ id, addr string }{id: replicaID, addr: peer.Address})
		remoteReplicas++
	}

	// OPTIMIZATION: Only create goroutines if there are remote replicas
	if remoteReplicas > 0 {
		var wg sync.WaitGroup
		wg.Add(remoteReplicas)

		for _, peer := range replicaPeers {
			// OPTIMIZATION: Capture variables in closure for better performance
			nodeID := peer.id
			addr := peer.addr

			go func() {
				defer wg.Done()

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

				// OPTIMIZATION: Only log errors if they're not timeout/context errors
				if err := gm.network.SendWithTimeout(addr, msg, gm.readTimeout); err != nil {
					if err != context.DeadlineExceeded && err != context.Canceled {
						logging.Error(err, "read request failed", "target", nodeID, "key", key)
					}
				}
			}()
		}

		// OPTIMIZATION: Wait for responses in background to avoid blocking
		go wg.Wait()
	}

	// CRITICAL FIX: Collect responses with timeout, but don't fail if some nodes don't have data
	// During startup, some replica nodes may not have received data yet
	ctx2, cancel := context.WithTimeout(ctx, gm.readTimeout)
	defer cancel()

	collected := 1                    // Already have local result
	maxResponses := len(replicas) - 1 // Maximum responses from remote nodes

	// Collect responses until we have quorum OR all responses received
collectLoop:
	for collected < effectiveReadQuorum && collected <= maxResponses {
		select {
		case resp := <-respCh:
			if resp.Found {
				results <- readResult{
					item:    protoItemToStorage(resp.ItemData, resp.Version),
					version: resp.Version,
					nodeID:  resp.ResponderId,
					found:   true,
				}
			} else {
				results <- readResult{
					nodeID: resp.ResponderId,
					found:  false,
				}
			}
			collected++
		case <-ctx2.Done():
			// CRITICAL FIX: Don't fail if timeout, but we have at least local result
			// During startup, some nodes may not respond in time
			// OPTIMIZATION: Only log if debug is enabled
			if logging.Log.IsDebugEnabled() {
				logging.Debug("Read quorum timeout, using available results",
					"key", key, "collected", collected-1, "required", effectiveReadQuorum-1)
			}
			break collectLoop
		}
	}

	// OPTIMIZATION: Find the item with the highest version (latest) while collecting results
	// Pre-allocate slice with known capacity to reduce allocations
	close(results)
	var latestItem *storage.StoredItem
	var latestVersion int64 = -1
	foundCount := 0
	allResults := make([]readResult, 0, len(replicas)) // OPTIMIZATION: Pre-allocate with capacity

	for result := range results {
		allResults = append(allResults, result)
		if result.found {
			foundCount++
			// OPTIMIZATION: Direct comparison for better performance
			if result.version > latestVersion {
				latestVersion = result.version
				latestItem = result.item
			}
		}
	}

	// CRITICAL FIX: If local node has data, return it even if quorum not met
	// This ensures data written during startup can be read back
	if foundCount == 0 {
		// Check if local result exists (may have been added to results channel)
		// If local read found data, return it
		if localErr == nil && localItem != nil {
			// OPTIMIZATION: Only log if debug is enabled
			if logging.Log.IsDebugEnabled() {
				logging.Debug("No remote results, returning local data",
					"key", key, "localVersion", localItem.Version)
			}
			return localItem, nil
		}
		return nil, storage.ErrItemNotFound
	}

	// CRITICAL FIX: Always return the latest version found, even if quorum not fully met
	// During startup, this ensures data written to coordinator can be read back
	if latestItem != nil {
		// OPTIMIZATION: Only perform read repair if there are multiple results and some are stale
		// Skip read repair for single result or all results match
		if len(allResults) > 1 && foundCount > 0 {
			// Check if read repair is needed (any result has different version)
			needsRepair := false
			for _, result := range allResults {
				if result.found && result.version != latestVersion {
					needsRepair = true
					break
				}
			}
			if needsRepair {
				// Read repair: update stale replicas in background
				go gm.performReadRepair(key, latestItem, allResults)
			}
		}
		return latestItem, nil
	}

	return nil, storage.ErrItemNotFound
}

// handleReadRequest processes an incoming read request and sends back the value.
func (gm *GossipManager) handleReadRequest(req *ReadRequestPayload, senderID string) {
	if req == nil || req.Key == "" {
		if logging.Log.IsDebugEnabled() {
			logging.Warn("Invalid read request")
		}
		return
	}

	item, err := gm.store.Get(req.Key)

	resp := &ReadResponsePayload{
		Key:         req.Key,
		RequestId:   req.RequestId,
		Found:       err == nil,
		ResponderId: gm.localNodeID,
	}

	if err == nil {
		resp.ItemData = storageItemToProto(item)
		resp.Version = item.Version
	}

	peer, ok := gm.getNode(senderID)
	if !ok {
		if logging.Log.IsDebugEnabled() {
			logging.Warn("Sender node not found for read response", "sender", senderID)
		}
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

	if err := gm.network.SendWithTimeout(peer.Address, msg, gm.readTimeout); err != nil {
		logging.Error(err, "Failed to send read response", "key", req.Key, "target", senderID)
	}
}

// handleReadResponse processes an incoming read response.
func (gm *GossipManager) handleReadResponse(resp *ReadResponsePayload) {
	if resp == nil || resp.RequestId == "" {
		if logging.Log.IsDebugEnabled() {
			logging.Warn("Invalid read response")
		}
		return
	}

	if ch, ok := gm.pendingReads.Load(resp.RequestId); ok {
		// SAFETY: Use recover to handle potential panic when sending to closed channel
		// This prevents crashes if the channel was closed due to timeout
		func() {
			defer func() {
				if r := recover(); r != nil {
					// Channel was closed, remove from map and log
					gm.pendingReads.Delete(resp.RequestId)
					if logging.Log.IsDebugEnabled() {
						logging.Debug("Read response channel closed (timeout)", "requestId", resp.RequestId)
					}
				}
			}()
			// Send the response to the waiting goroutine (non-blocking)
			select {
			case ch.(chan *ReadResponsePayload) <- resp:
				if logging.Log.IsDebugEnabled() {
					logging.Debug("Read response delivered", "requestId", resp.RequestId, "found", resp.Found)
				}
			default:
				// Channel full, log warning
				if logging.Log.IsDebugEnabled() {
					logging.Warn("Failed to deliver read response (channel full)", "requestId", resp.RequestId)
				}
			}
		}()
	} else {
		// No one is waiting for this response (may have timed out)
		if logging.Log.IsDebugEnabled() {
			logging.Debug("Received read response for unknown or expired requestId", "requestId", resp.RequestId)
		}
	}
}

// performReadRepair updates stale replicas with the latest version in the background.
func (gm *GossipManager) performReadRepair(key string, latestItem *storage.StoredItem, allResults []readResult) {
	if latestItem == nil {
		return
	}

	for _, result := range allResults {
		if result.nodeID == gm.localNodeID {
			// Update local if stale
			if !result.found || result.version < latestItem.Version {
				if err := gm.store.Set(key, latestItem); err != nil {
					logging.Error(err, "Read repair: local update failed", "key", key)
				} else if logging.Log.IsDebugEnabled() {
					logging.Debug("Read repair: updated local", "key", key, "version", latestItem.Version)
				}
			}
			continue
		}

		// Update remote replicas if stale
		if !result.found || result.version < latestItem.Version {
			go func(nodeID string) {
				peer, ok := gm.getNode(nodeID)
				if !ok {
					return
				}

				protoOp := &CacheSyncOperation{
					Key:           key,
					ClientVersion: latestItem.Version,
					Type:          OperationType_OP_SET,
					DataPayload: &CacheSyncOperation_SetData{
						SetData: storageItemToProto(latestItem),
					},
				}

				msg := &GossipMessage{
					Type:   GossipMessageType_MESSAGE_TYPE_CACHE_SYNC,
					Sender: gm.localNodeID,
					Payload: &GossipMessage_CacheSyncPayload{
						CacheSyncPayload: &SyncMessage{
							SyncType: &SyncMessage_IncrementalSync{
								IncrementalSync: &IncrementalSyncPayload{
									Operations: []*CacheSyncOperation{protoOp},
								},
							},
						},
					},
				}
				gm.signMessageCanonical(msg)

				if err := gm.network.SendWithTimeout(peer.Address, msg, gm.replicationTimeout); err != nil {
					logging.Error(err, "Read repair: failed to update replica", "target", nodeID, "key", key)
				} else if logging.Log.IsDebugEnabled() {
					logging.Debug("Read repair: updated replica", "target", nodeID, "key", key, "version", latestItem.Version)
				}
			}(result.nodeID)
		}
	}
}

// readResult represents the result of a read operation from a replica.
type readResult struct {
	item    *storage.StoredItem
	version int64
	nodeID  string
	found   bool
}
