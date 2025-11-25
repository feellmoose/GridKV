package gossip

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feellmoose/gridkv/internal/storage"
)

type ClusterGossipManager struct {
	*GossipManager
	directRouting bool
	routingTable  sync.Map
	routeVersion  atomic.Int64
}

func NewClusterGossipManager(gm *GossipManager) *ClusterGossipManager {
	cgm := &ClusterGossipManager{
		GossipManager: gm,
		directRouting: true,
	}
	cgm.routeVersion.Store(1)
	return cgm
}

func (cgm *ClusterGossipManager) Set(ctx context.Context, key string, item *storage.StoredItem) error {
	if item == nil {
		return errors.New("nil item")
	}
	if cgm.store == nil {
		return fmt.Errorf("store not initialized")
	}

	startTime := time.Now()
	if cgm.metrics != nil {
		cgm.metrics.IncrementRequestsTotal()
		cgm.metrics.IncrementSet()
	}

	targetNode := cgm.getTargetNode(key)
	if targetNode == "" {
		if cgm.metrics != nil {
			cgm.metrics.IncrementRequestsErrors()
		}
		return fmt.Errorf("no target node for key")
	}

	var err error
	if targetNode == cgm.localNodeID {
		if err = cgm.store.Set(key, item); err != nil {
			if cgm.metrics != nil {
				cgm.metrics.IncrementRequestsErrors()
			}
			return fmt.Errorf("local write failed: %w", err)
		}
		cgm.replicateAsync(key, item, nil)
	} else {
		err = cgm.forwardDirect(targetNode, key, item)
		if err != nil {
			if cgm.metrics != nil {
				cgm.metrics.IncrementRequestsErrors()
			}
			return err
		}
	}

	if cgm.metrics != nil {
		cgm.metrics.IncrementRequestsSuccess()
		latency := time.Since(startTime).Nanoseconds()
		// Update latency percentiles (improved threshold-based approximation)
		if latency > 0 {
			// Improved thresholds: track max values in each range for better approximation
			// P99: > 2ms (high latency tail)
			// P95: > 1ms (moderate latency)
			// P50: <= 1ms (typical latency)
			if latency > 2000000 { // 2ms - P99 range
				cgm.metrics.SetLatencyP99(latency)
			} else if latency > 1000000 { // 1ms - P95 range
				cgm.metrics.SetLatencyP95(latency)
			} else { // <= 1ms - P50 range
				cgm.metrics.SetLatencyP50(latency)
			}
		}
	}

	return nil
}

func (cgm *ClusterGossipManager) Get(ctx context.Context, key string) (*storage.StoredItem, error) {
	if cgm.store == nil {
		return nil, fmt.Errorf("storage not initialized")
	}

	startTime := time.Now()
	if cgm.metrics != nil {
		cgm.metrics.IncrementRequestsTotal()
		cgm.metrics.IncrementGet()
	}

	if cgm.hotCacheTTL > 0 {
		if v, ok := cgm.hotCache.Load(key); ok {
			if e, ok2 := v.(hotCacheEntry); ok2 {
				now := time.Now()
				if now.Before(e.expireAt) && e.item != nil && len(e.item.Value) > 0 {
					if cgm.metrics != nil {
						cgm.metrics.IncrementRequestsSuccess()
						latency := time.Since(startTime).Nanoseconds()
						if latency > 0 {
							if latency > 2000000 {
								cgm.metrics.SetLatencyP99(latency)
							} else if latency > 1000000 {
								cgm.metrics.SetLatencyP95(latency)
							} else {
								cgm.metrics.SetLatencyP50(latency)
							}
						}
					}
					return e.item, nil
				}
				cgm.hotCache.Delete(key)
			}
		}
	}

	targetNode := cgm.getTargetNode(key)
	if targetNode == "" {
		if cgm.metrics != nil {
			cgm.metrics.IncrementRequestsErrors()
		}
		return nil, storage.ErrItemNotFound
	}

	var item *storage.StoredItem
	var err error

	if targetNode == cgm.localNodeID {
		var localItem *storage.StoredItem
		var localErr error
		if noCopyStorage, ok := cgm.store.(interface {
			GetNoCopy(key string) (*storage.StoredItem, error)
		}); ok {
			localItem, localErr = noCopyStorage.GetNoCopy(key)
		} else {
			localItem, localErr = cgm.store.Get(key)
		}

		if localErr == nil && localItem != nil && len(localItem.Value) > 0 {
			itemCopy := copyStorageItem(localItem)
			if cgm.hotCacheTTL > 0 {
				cgm.hotCache.Store(key, hotCacheEntry{item: itemCopy, expireAt: time.Now().Add(cgm.hotCacheTTL)})
			}
			item = itemCopy
			err = nil
		} else {
			err = storage.ErrItemNotFound
		}
	} else {
		item, err = cgm.readDirect(ctx, key, targetNode)
	}

	if cgm.metrics != nil {
		if err == nil && item != nil {
			cgm.metrics.IncrementRequestsSuccess()
		} else {
			cgm.metrics.IncrementRequestsErrors()
		}
		latency := time.Since(startTime).Nanoseconds()
		if latency > 0 {
			if latency > 1000000 {
				cgm.metrics.SetLatencyP99(latency)
			} else if latency > 500000 {
				cgm.metrics.SetLatencyP95(latency)
			} else {
				cgm.metrics.SetLatencyP50(latency)
			}
		}
	}

	return item, err
}

func (cgm *ClusterGossipManager) Delete(ctx context.Context, key string, version int64) error {
	startTime := time.Now()
	if cgm.metrics != nil {
		cgm.metrics.IncrementRequestsTotal()
		cgm.metrics.IncrementDelete()
	}

	targetNode := cgm.getTargetNode(key)
	if targetNode == "" {
		if cgm.metrics != nil {
			cgm.metrics.IncrementRequestsErrors()
		}
		return fmt.Errorf("no target node for key")
	}

	var err error
	if targetNode == cgm.localNodeID {
		if err = cgm.store.Delete(key, version); err != nil {
			if cgm.metrics != nil {
				cgm.metrics.IncrementRequestsErrors()
			}
			return fmt.Errorf("local delete failed: %w", err)
		}
		cgm.replicateDeleteAsync(key, version, nil)
	} else {
		err = cgm.forwardDeleteDirect(targetNode, key, version)
		if err != nil {
			if cgm.metrics != nil {
				cgm.metrics.IncrementRequestsErrors()
			}
			return err
		}
	}

	if cgm.metrics != nil {
		cgm.metrics.IncrementRequestsSuccess()
		latency := time.Since(startTime).Nanoseconds()
		if latency > 0 {
			if latency > 2000000 {
				cgm.metrics.SetLatencyP99(latency)
			} else if latency > 1000000 {
				cgm.metrics.SetLatencyP95(latency)
			} else {
				cgm.metrics.SetLatencyP50(latency)
			}
		}
	}

	return nil
}

func (cgm *ClusterGossipManager) getTargetNode(key string) string {
	if cgm.hashRing == nil {
		return cgm.localNodeID
	}

	cgm.mu.RLock()
	availableNodes := len(cgm.liveNodes)
	cgm.mu.RUnlock()

	if availableNodes == 1 {
		return cgm.localNodeID
	}

	replicas := cgm.GossipManager.getReplicas(key, 1)
	if len(replicas) == 0 {
		return cgm.localNodeID
	}

	return replicas[0]
}

func (cgm *ClusterGossipManager) forwardDirect(targetNodeID string, key string, item *storage.StoredItem) error {
	peer, ok := cgm.getNode(targetNodeID)
	if !ok {
		return fmt.Errorf("target node %s not found", targetNodeID)
	}

	setData := storageItemToProto(item)
	protoOp := &CacheSyncOperation{
		Key:           key,
		ClientVersion: item.Version,
		Type:          OperationType_OP_SET,
		SetData:       setData,
		DataPayload: &CacheSyncOperation_SetData{
			SetData: setData,
		},
	}

	if protoOp.GetSetData() == nil {
		return fmt.Errorf("proto conversion failed for key %s", key)
	}

	cgm.enqueueToPipeline(peer.Address, protoOp)
	return nil
}

func (cgm *ClusterGossipManager) forwardDeleteDirect(targetNodeID string, key string, version int64) error {
	peer, ok := cgm.getNode(targetNodeID)
	if !ok {
		return fmt.Errorf("target node %s not found", targetNodeID)
	}

	protoOp := &CacheSyncOperation{
		Key:           key,
		ClientVersion: version,
		Type:          OperationType_OP_DELETE,
	}

	cgm.enqueueToPipeline(peer.Address, protoOp)
	return nil
}

func (cgm *ClusterGossipManager) readDirect(ctx context.Context, key string, targetNodeID string) (*storage.StoredItem, error) {
	peer, ok := cgm.getNode(targetNodeID)
	if !ok {
		return nil, fmt.Errorf("target node %s not found", targetNodeID)
	}

	if peer.State != NodeState_NODE_STATE_ALIVE {
		return nil, fmt.Errorf("target node %s is not alive", targetNodeID)
	}

	requestID := cgm.generateOpID()
	respCh := getReadResponseChannel()
	entry := &pendingReadEntry{
		ch:        respCh,
		createdAt: time.Now(),
	}
	cgm.addPendingRead(requestID, entry)
	defer func() {
		cgm.removePendingRead(requestID)
		putReadResponseChannel(respCh)
	}()

	msg := &GossipMessage{
		Type:   GossipMessageType_MESSAGE_TYPE_READ_REQUEST,
		Sender: cgm.localNodeID,
		Payload: &GossipMessage_ReadRequestPayload{
			ReadRequestPayload: &ReadRequestPayload{
				Key:         key,
				RequesterId: cgm.localNodeID,
				RequestId:   requestID,
			},
		},
	}
	cgm.signMessageCanonical(msg)

	timeout := cgm.readTimeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining > 0 && remaining < timeout {
			timeout = remaining * 80 / 100
			if timeout < 200*time.Millisecond {
				timeout = 200 * time.Millisecond
			}
		}
	}

	if err := cgm.network.SendWithTimeout(peer.Address, msg, timeout); err != nil {
		return nil, fmt.Errorf("read request failed: %w", err)
	}

	ctx2, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	select {
	case resp := <-respCh:
		if resp == nil || !resp.Found || resp.ItemData == nil {
			return nil, storage.ErrItemNotFound
		}
		item := protoItemToStorage(resp.ItemData, resp.Version)
		if item == nil || len(item.Value) == 0 {
			return nil, storage.ErrItemNotFound
		}
		if cgm.hotCacheTTL > 0 {
			cgm.hotCache.Store(key, hotCacheEntry{item: item, expireAt: time.Now().Add(cgm.hotCacheTTL)})
		}
		return item, nil
	case <-ctx2.Done():
		if cgm.metrics != nil {
			cgm.metrics.IncrementRequestsTimeout()
		}
		return nil, fmt.Errorf("read timeout: %w", ctx2.Err())
	}
}

func (cgm *ClusterGossipManager) replicateAsync(key string, item *storage.StoredItem, replicaIDs []string) {
	if item == nil {
		return
	}

	if replicaIDs == nil {
		if cgm.hashRing == nil {
			return
		}
		cgm.mu.RLock()
		availableNodes := len(cgm.liveNodes)
		cgm.mu.RUnlock()

		if availableNodes <= 1 {
			return
		}

		effectiveReplicaCount := cgm.replicaCount
		if availableNodes < cgm.replicaCount {
			effectiveReplicaCount = availableNodes
		}

		replicas := cgm.getReplicas(key, effectiveReplicaCount)
		if len(replicas) <= 1 {
			return
		}
		replicaIDs = replicas[1:]
	}

	targets := make([]struct{ addr, id string }, 0, len(replicaIDs))
	cgm.mu.RLock()
	for _, replicaID := range replicaIDs {
		if n, ok := cgm.liveNodes[replicaID]; ok && n != nil && n.State != NodeState_NODE_STATE_DEAD {
			targets = append(targets, struct{ addr, id string }{addr: n.Address, id: replicaID})
		}
	}
	cgm.mu.RUnlock()

	if len(targets) == 0 {
		return
	}

	if cgm.metrics != nil {
		cgm.metrics.IncrementReplicationTotal()
	}

	baseOp := &CacheSyncOperation{
		Key:           key,
		ClientVersion: item.Version,
		Type:          OperationType_OP_SET,
	}
	baseOp.SetData = storageItemToProto(item)
	baseOp.DataPayload = &CacheSyncOperation_SetData{
		SetData: baseOp.SetData,
	}

	if baseOp.GetSetData() == nil {
		if cgm.metrics != nil {
			cgm.metrics.IncrementReplicationFailures()
		}
		return
	}

	for _, t := range targets {
		opClone := CloneCacheSyncOperation(baseOp)
		cgm.enqueueToPipeline(t.addr, opClone)
	}
}

func (cgm *ClusterGossipManager) replicateDeleteAsync(key string, version int64, replicaIDs []string) {
	if replicaIDs == nil {
		if cgm.hashRing == nil {
			return
		}
		cgm.mu.RLock()
		availableNodes := len(cgm.liveNodes)
		cgm.mu.RUnlock()

		if availableNodes <= 1 {
			return
		}

		effectiveReplicaCount := cgm.replicaCount
		if availableNodes < cgm.replicaCount {
			effectiveReplicaCount = availableNodes
		}

		replicas := cgm.getReplicas(key, effectiveReplicaCount)
		if len(replicas) <= 1 {
			return
		}
		replicaIDs = replicas[1:]
	}

	targets := make([]struct{ addr, id string }, 0, len(replicaIDs))
	cgm.mu.RLock()
	for _, replicaID := range replicaIDs {
		if n, ok := cgm.liveNodes[replicaID]; ok && n != nil && n.State != NodeState_NODE_STATE_DEAD {
			targets = append(targets, struct{ addr, id string }{addr: n.Address, id: replicaID})
		}
	}
	cgm.mu.RUnlock()

	if len(targets) == 0 {
		return
	}

	if cgm.metrics != nil {
		cgm.metrics.IncrementReplicationTotal()
	}

	baseOp := &CacheSyncOperation{
		Key:           key,
		ClientVersion: version,
		Type:          OperationType_OP_DELETE,
	}

	for _, t := range targets {
		opClone := CloneCacheSyncOperation(baseOp)
		cgm.enqueueToPipeline(t.addr, opClone)
	}
}

func (cgm *ClusterGossipManager) SetBatch(ctx context.Context, items map[string]*storage.StoredItem) error {
	if len(items) == 0 {
		return nil
	}

	byNode := make(map[string][]struct {
		key  string
		item *storage.StoredItem
	})

	for key, item := range items {
		if item == nil {
			continue
		}
		targetNode := cgm.getTargetNode(key)
		if targetNode == "" {
			continue
		}
		byNode[targetNode] = append(byNode[targetNode], struct {
			key  string
			item *storage.StoredItem
		}{key: key, item: item})
	}

	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex

	for nodeID, nodeItems := range byNode {
		wg.Add(1)
		go func(nid string, its []struct {
			key  string
			item *storage.StoredItem
		}) {
			defer wg.Done()
			if nid == cgm.localNodeID {
				if batchStore, ok := cgm.store.(interface {
					BatchSet(items map[string]*storage.StoredItem) error
				}); ok {
					batchItems := make(map[string]*storage.StoredItem, len(its))
					for _, it := range its {
						batchItems[it.key] = it.item
					}
					if err := batchStore.BatchSet(batchItems); err != nil {
						errMu.Lock()
						if firstErr == nil {
							firstErr = err
						}
						errMu.Unlock()
					} else {
						for _, it := range its {
							cgm.replicateAsync(it.key, it.item, nil)
						}
					}
				} else {
					for _, it := range its {
						if err := cgm.store.Set(it.key, it.item); err != nil {
							errMu.Lock()
							if firstErr == nil {
								firstErr = err
							}
							errMu.Unlock()
							continue
						}
						cgm.replicateAsync(it.key, it.item, nil)
					}
				}
			} else {
				peer, ok := cgm.getNode(nid)
				if !ok {
					return
				}
				for _, it := range its {
					setData := storageItemToProto(it.item)
					protoOp := &CacheSyncOperation{
						Key:           it.key,
						ClientVersion: it.item.Version,
						Type:          OperationType_OP_SET,
						SetData:       setData,
						DataPayload: &CacheSyncOperation_SetData{
							SetData: setData,
						},
					}
					if protoOp.GetSetData() != nil {
						cgm.enqueueToPipeline(peer.Address, protoOp)
					}
				}
			}
		}(nodeID, nodeItems)
	}

	wg.Wait()
	return firstErr
}

func (cgm *ClusterGossipManager) GetBatch(ctx context.Context, keys []string) (map[string]*storage.StoredItem, error) {
	if len(keys) == 0 {
		return make(map[string]*storage.StoredItem), nil
	}

	byNode := make(map[string][]string)
	for _, key := range keys {
		targetNode := cgm.getTargetNode(key)
		if targetNode == "" {
			continue
		}
		byNode[targetNode] = append(byNode[targetNode], key)
	}

	results := make(map[string]*storage.StoredItem)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for nodeID, nodeKeys := range byNode {
		wg.Add(1)
		go func(nid string, ks []string) {
			defer wg.Done()
			if nid == cgm.localNodeID {
				if batchStorage, ok := cgm.store.(interface {
					BatchGet(keys []string) (map[string]*storage.StoredItem, error)
				}); ok {
					items, err := batchStorage.BatchGet(ks)
					if err == nil {
						mu.Lock()
						for k, v := range items {
							if v != nil && len(v.Value) > 0 {
								results[k] = copyStorageItem(v)
							}
						}
						mu.Unlock()
					}
				} else if batchStorageNoCopy, ok := cgm.store.(interface {
					BatchGetNoCopy(keys []string) (map[string]*storage.StoredItem, error)
				}); ok {
					items, err := batchStorageNoCopy.BatchGetNoCopy(ks)
					if err == nil {
						mu.Lock()
						for k, v := range items {
							if v != nil && len(v.Value) > 0 {
								results[k] = copyStorageItem(v)
							}
						}
						mu.Unlock()
					}
				} else {
					for _, k := range ks {
						var item *storage.StoredItem
						var err error
						if noCopyStorage, ok := cgm.store.(interface {
							GetNoCopy(key string) (*storage.StoredItem, error)
						}); ok {
							item, err = noCopyStorage.GetNoCopy(k)
						} else {
							item, err = cgm.store.Get(k)
						}
						if err == nil && item != nil && len(item.Value) > 0 {
							itemCopy := copyStorageItem(item)
							mu.Lock()
							results[k] = itemCopy
							mu.Unlock()
						}
					}
				}
			} else {
				if cgm.readBatchManager != nil && len(ks) > 1 {
					for _, k := range ks {
						item, err := cgm.enqueueReadRequest(ctx, k, nid)
						if err == nil && item != nil {
							mu.Lock()
							results[k] = item
							mu.Unlock()
						}
					}
				} else {
					for _, k := range ks {
						item, err := cgm.readDirect(ctx, k, nid)
						if err == nil && item != nil {
							mu.Lock()
							results[k] = item
							mu.Unlock()
						}
					}
				}
			}
		}(nodeID, nodeKeys)
	}

	wg.Wait()
	return results, nil
}

func (cgm *ClusterGossipManager) UpdateRoutingTable() {
	cgm.mu.RLock()
	nodes := make(map[string]*NodeInfo, len(cgm.liveNodes))
	for id, node := range cgm.liveNodes {
		if node.State == NodeState_NODE_STATE_ALIVE {
			nodes[id] = node
		}
	}
	cgm.mu.RUnlock()

	cgm.routingTable.Store("nodes", nodes)
	cgm.routeVersion.Add(1)
}

func (cgm *ClusterGossipManager) GetRoutingTable() map[string]*NodeInfo {
	if v, ok := cgm.routingTable.Load("nodes"); ok {
		if nodes, ok2 := v.(map[string]*NodeInfo); ok2 {
			return nodes
		}
	}
	return make(map[string]*NodeInfo)
}
