package gossip

import (
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/feellmoose/gridkv/internal/utils/logging"
)

var (
	// NodeInfo slice pool for cluster sync
	nodeInfoSlicePool = sync.Pool{
		New: func() interface{} {
			slice := make([]*NodeInfo, 0, 32)
			return &slice
		},
	}
)

// gossipPeriodically broadcasts cluster membership to a random peer.
//
// This is the core of the gossip protocol for membership dissemination.
func (gm *GossipManager) gossipPeriodically() {
	gm.mu.RLock()
	peerCount := len(gm.liveNodes) - 1 // Exclude self
	gm.mu.RUnlock()

	if peerCount == 0 {
		return // No peers to gossip with
	}

	gm.mu.RLock()
	membersPtr := nodeInfoSlicePool.Get().(*[]*NodeInfo)
	members := (*membersPtr)[:0] // Reset to zero length

	for _, n := range gm.liveNodes {
		if n == nil {
			continue
		}
		members = append(members, &NodeInfo{
			NodeId:       n.NodeId,
			Address:      n.Address,
			LastActiveTs: n.LastActiveTs,
			State:        n.State,
			Version:      n.Version,
		})
	}
	gm.mu.RUnlock()

	// This accelerates cluster convergence during startup
	gossipTargets := gm.getGossipTargets(peerCount)

	if len(gossipTargets) == 0 {
		nodeInfoSlicePool.Put(membersPtr)
		return
	}

	// Create cluster sync message once (shared for all targets)
	syncMsg := &GossipMessage{
		Type:   CLUSTER_SYNC,
		Sender: gm.localNodeID,
		Payload: &GossipMessage_ClusterSyncPayload{
			ClusterSyncPayload: &ClusterSyncPayload{Nodes: members},
		},
	}
	gm.signMessageCanonical(syncMsg)

	// Send to all targets (parallel for faster convergence)
	var wg sync.WaitGroup
	for _, target := range gossipTargets {
		target := target // Capture loop variable
		wg.Add(1)
		go func(t string) {
			defer wg.Done()
			if peer, ok := gm.getNode(t); ok && peer != nil && gm.network != nil {
				// Use shorter timeout for faster gossip
				gm.network.SendWithTimeout(peer.Address, syncMsg, 300*time.Millisecond)
			}
		}(target)
	}
	wg.Wait()

	// Return pooled slice
	nodeInfoSlicePool.Put(membersPtr)

	if len(gossipTargets) > 0 {
		gm.gossipCachePeriodically(gossipTargets[0])
	}
}

// getGossipTargets returns targets for gossip based on cluster size.
func (gm *GossipManager) getGossipTargets(peerCount int) []string {
	gm.mu.RLock()
	defer gm.mu.RUnlock()

	var targets []string

	if peerCount <= 3 {
		// Small cluster: gossip to all peers for fastest convergence
		for id := range gm.liveNodes {
			if id != gm.localNodeID {
				targets = append(targets, id)
			}
		}
	} else if peerCount <= 10 {
		// Medium cluster: gossip to 2-3 random peers
		count := 3
		if peerCount < 5 {
			count = 2
		}
		ids := make([]string, 0, len(gm.liveNodes))
		for id := range gm.liveNodes {
			if id != gm.localNodeID {
				ids = append(ids, id)
			}
		}
		// Select random peers
		for i := 0; i < count && i < len(ids); i++ {
			idx := rand.Intn(len(ids) - i)
			targets = append(targets, ids[idx])
			ids[idx], ids[len(ids)-1-i] = ids[len(ids)-1-i], ids[idx]
		}
	} else if peerCount <= 30 {
		// Large cluster: gossip to 1-2 random peers (reduced frequency)
		count := 1
		if peerCount > 20 {
			count = 1 // Very large clusters: only 1 peer
		}
		ids := make([]string, 0, len(gm.liveNodes))
		for id := range gm.liveNodes {
			if id != gm.localNodeID {
				ids = append(ids, id)
			}
		}
		// Select random peers
		for i := 0; i < count && i < len(ids); i++ {
			idx := rand.Intn(len(ids) - i)
			targets = append(targets, ids[idx])
			ids[idx], ids[len(ids)-1-i] = ids[len(ids)-1-i], ids[idx]
		}
	} else {
		// Very large cluster (>30 nodes): gossip to 1 random peer only
		target := gm.getRandomPeerID("")
		if target != "" {
			targets = append(targets, target)
		}
	}

	return targets
}

// gossipCachePeriodically broadcasts incremental cache updates to a target node.
//
// Parameters:
//   - targetNodeID: The node to send cache updates to
func (gm *GossipManager) gossipCachePeriodically(targetNodeID string) {
	if gm.store == nil || gm.network == nil {
		return
	}

	items, err := gm.store.GetSyncBuffer()
	if err != nil {
		if logging.Log.IsDebugEnabled() {
			logging.Debug("get sync buffer failed", "err", err)
		}
		return
	}

	if len(items) == 0 {
		return
	}

	maxOpsPerBatch := gm.getAdaptiveBatchSize()

	for i := 0; i < len(items); i += maxOpsPerBatch {
		end := i + maxOpsPerBatch
		if end > len(items) {
			end = len(items)
		}
		batch := items[i:end]

		// Create message for this batch
		msg := &GossipMessage{
			Type:   CACHE_SYNC,
			Sender: gm.localNodeID,
			Payload: &GossipMessage_CacheSyncPayload{
				CacheSyncPayload: &SyncMessage{
					SyncType: &SyncMessage_IncrementalSync{
						IncrementalSync: &IncrementalSyncPayload{
							Operations: batch,
						},
					},
				},
			},
		}
		gm.signMessageCanonical(msg)

		if peer, ok := gm.getNode(targetNodeID); ok {
			gm.network.SendWithTimeout(peer.Address, msg, 500*time.Millisecond)
			gm.msgRateCounter.Add(1)
		}
	}
}

// getAdaptiveBatchSize calculates optimal batch size based on message rate.
//
// Returns:
//   - int: Recommended batch size
func (gm *GossipManager) getAdaptiveBatchSize() int {
	now := time.Now().Unix()
	lastCheck := gm.lastRateCheck.Load()

	// Update rate every second
	if now > lastCheck {
		msgCount := gm.msgRateCounter.Swap(0) // Reset counter
		gm.lastRateCheck.Store(now)

		// Adaptive algorithm
		var newBatchSize int32
		if msgCount > 10000 {
			newBatchSize = 200 // High rate: larger batches
		} else if msgCount > 1000 {
			newBatchSize = 100 // Medium rate: default batches
		} else {
			newBatchSize = 50 // Low rate: smaller batches (less delay)
		}

		gm.lastBatchSize.Store(newBatchSize)
		logging.Debug("Adaptive batch size updated", "rate", msgCount, "batchSize", newBatchSize)
	}

	return int(gm.lastBatchSize.Load())
}

// RequestFullSync initiates a full state synchronization from a peer node.
// This is typically used during node recovery or initial cluster join.
//
// Parameters:
//   - targetNodeID: The node to request full sync from (empty string = random peer)
//
// Returns:
//   - error: Any error encountered
func (gm *GossipManager) RequestFullSync(targetNodeID string) error {
	if targetNodeID == "" {
		targetNodeID = gm.getRandomPeerID("")
		if targetNodeID == "" {
			return errors.New("no peer for full sync")
		}
	}

	peer, ok := gm.getNode(targetNodeID)
	if !ok {
		return fmt.Errorf("peer %s not found", targetNodeID)
	}

	msg := &GossipMessage{
		Type:   GossipMessageType_MESSAGE_TYPE_FULL_SYNC_REQUEST,
		Sender: gm.localNodeID,
		Payload: &GossipMessage_FullSyncRequestPayload{
			FullSyncRequestPayload: &FullSyncRequestPayload{
				RequesterId: gm.localNodeID,
			},
		},
	}
	gm.signMessageCanonical(msg)
	logging.Info("SYNC request", "target", targetNodeID)
	return gm.network.SendWithTimeout(peer.Address, msg, gm.replicationTimeout)
}

// handleFullSyncRequest processes a full sync request and sends back complete state.
//
// Parameters:
//   - requesterID: The node requesting the full sync
func (gm *GossipManager) handleFullSyncRequest(requesterID string) {
	if gm.store == nil {
		logging.Warn("SYNC store nil")
		return
	}

	items, err := gm.store.GetFullSyncSnapshot()
	if err != nil {
		logging.Error(err, "get full snapshot failed")
		return
	}

	payload := &FullSyncResponsePayload{
		FullSync: &FullSyncPayload{
			Items:             items,
			SnapshotTimestamp: uint64(gm.localVersion),
		},
	}

	peer, ok := gm.getNode(requesterID)
	if !ok {
		logging.Warn("requester not found", "req", requesterID)
		return
	}

	resp := &GossipMessage{
		Type:   GossipMessageType_MESSAGE_TYPE_FULL_SYNC_RESPONSE,
		Sender: gm.localNodeID,
		Payload: &GossipMessage_FullSyncResponsePayload{
			FullSyncResponsePayload: payload,
		},
	}
	gm.signMessageCanonical(resp)
	gm.network.SendWithTimeout(peer.Address, resp, gm.replicationTimeout)
}

// handleFullSyncResponse applies a full snapshot from a peer node.
//
// Parameters:
//   - payload: The full sync payload containing all state
func (gm *GossipManager) handleFullSyncResponse(payload *FullSyncPayload) {
	if gm.store == nil {
		logging.Warn("SYNC apply store nil")
		return
	}

	if err := gm.store.ApplyFullSyncSnapshot(payload.GetItems(), time.Unix(int64(payload.GetSnapshotTimestamp()), 0)); err != nil {
		logging.Error(err, "apply full sync failed")
	}
	logging.Info("SYNC applied", "items", len(payload.Items))
}
