package gossip

import (
	"errors"
	"time"

	"github.com/feellmoose/gridkv/internal/utils/logging"
)

type nodeUpdate struct {
	nodeID  string
	address string
	state   NodeState
	version int64
}

func (gm *GossipManager) updateNode(nodeID, address string, newState NodeState, version int64) {
	gm.mu.RLock()
	existing, found := gm.liveNodes[nodeID]
	if found && version < existing.Version {
		gm.mu.RUnlock()
		return
	}
	gm.mu.RUnlock()

	var add, remove bool
	now := time.Now()

	gm.mu.Lock()
	existing, found = gm.liveNodes[nodeID]

	if found && version < existing.Version {
		gm.mu.Unlock()
		return
	}

	if !found {
		newNode := &NodeInfo{
			NodeId:       nodeID,
			Address:      address,
			LastActiveTs: now,
			State:        NodeState_NODE_STATE_ALIVE,
			Version:      version,
		}
		gm.liveNodes[nodeID] = newNode
		if gm.liveNodesLF != nil {
			gm.liveNodesLF.Update(nodeID, newNode)
		}
		if nodeID != gm.localNodeID {
			add = true
		}
		gm.mu.Unlock()

		if add {
			gm.hashRing.Add(nodeID)
			if gm.hashRingCache != nil {
				gm.hashRingCache.Invalidate()
			}
			gm.mu.RLock()
			clusterSize := len(gm.liveNodes)
			gm.mu.RUnlock()
			gm.updateBatchClusterSize(clusterSize)
		}
		return
	}

	if version >= existing.Version {
		oldState := existing.State
		existing.State = newState
		existing.Address = address
		existing.LastActiveTs = now
		existing.Version = version

		if newState == NodeState_NODE_STATE_DEAD && oldState != NodeState_NODE_STATE_DEAD {
			remove = true
		}
		if newState == NodeState_NODE_STATE_ALIVE && oldState != NodeState_NODE_STATE_ALIVE && nodeID != gm.localNodeID {
			add = true
		}

		if gm.liveNodesLF != nil {
			gm.liveNodesLF.Update(nodeID, existing)
		}
	}
	gm.mu.Unlock()

	if remove {
		gm.hashRing.Remove(nodeID)
		if gm.hashRingCache != nil {
			gm.hashRingCache.Invalidate()
		}
		if gm.liveNodesLF != nil {
			gm.liveNodesLF.Delete(nodeID)
		}

		if gm.gradualMigration != nil {
			gm.gradualMigration.startGradualMigration(nodeID, true)
		} else {
			nodeIDCopy := nodeID
			_ = gm.replicationPool.Submit(func() {
				gm.migrateDataFromDeadNode(nodeIDCopy)
			})
		}

		gm.mu.RLock()
		clusterSize := len(gm.liveNodes)
		addr := existing.Address
		gm.mu.RUnlock()

		gm.updateBatchClusterSize(clusterSize)
		if addr != "" {
			gm.flushBatchForTarget(addr)
		}
		logging.Error(errors.New("MEMBER DEAD"), "removed from ring", "node", nodeID)
	}
	if add {
		gm.hashRing.Add(nodeID)
		if gm.hashRingCache != nil {
			gm.hashRingCache.Invalidate()
		}
		logging.Debug("re-added to ring", "node", nodeID)
	}
}

func (gm *GossipManager) batchUpdateNodes(updates []nodeUpdate) {
	if len(updates) == 0 {
		return
	}

	var adds, removes []string
	var removedAddresses []string
	now := time.Now()

	gm.mu.Lock()

	for _, update := range updates {
		existing, found := gm.liveNodes[update.nodeID]

		if found && update.version < existing.Version {
			continue
		}

		if !found {
			gm.liveNodes[update.nodeID] = &NodeInfo{
				NodeId:       update.nodeID,
				Address:      update.address,
				LastActiveTs: now,
				State:        NodeState_NODE_STATE_ALIVE,
				Version:      update.version,
			}
			if update.nodeID != gm.localNodeID {
				adds = append(adds, update.nodeID)
			}
		} else if update.version >= existing.Version {
			oldState := existing.State
			existing.State = update.state
			existing.Address = update.address
			existing.LastActiveTs = now
			existing.Version = update.version

			if update.state == NodeState_NODE_STATE_DEAD && oldState != NodeState_NODE_STATE_DEAD {
				removes = append(removes, update.nodeID)
				if existing.Address != "" {
					removedAddresses = append(removedAddresses, existing.Address)
				}
			}
			if update.state == NodeState_NODE_STATE_ALIVE && oldState != NodeState_NODE_STATE_ALIVE && update.nodeID != gm.localNodeID {
				adds = append(adds, update.nodeID)
			}
		}
	}

	clusterSize := len(gm.liveNodes)
	gm.mu.Unlock()

	for _, nodeID := range adds {
		gm.hashRing.Add(nodeID)
	}

	for i, nodeID := range removes {
		gm.hashRing.Remove(nodeID)

		if gm.gradualMigration != nil {
			gm.gradualMigration.startGradualMigration(nodeID, true)
		} else {
			nodeIDCopy := nodeID
			_ = gm.replicationPool.Submit(func() {
				gm.migrateDataFromDeadNode(nodeIDCopy)
			})
		}

		if i < len(removedAddresses) && removedAddresses[i] != "" {
			gm.flushBatchForTarget(removedAddresses[i])
			gm.batchMutex.Lock()
			delete(gm.batchBuffer, removedAddresses[i])
			gm.batchMutex.Unlock()
		}
	}

	if len(adds) > 0 || len(removes) > 0 {
		gm.updateBatchClusterSize(clusterSize)
	}
}

func (gm *GossipManager) markNodeAliveFromProbe(nodeID string) {
	gm.mu.Lock()
	defer gm.mu.Unlock()

	if n, ok := gm.liveNodes[nodeID]; ok && n.State == NodeState_NODE_STATE_SUSPECT {
		n.State = NodeState_NODE_STATE_ALIVE
		n.LastActiveTs = time.Now()
		n.Version = gm.incrementLocalVersion()
		logging.Debug("MEMBER RECOVER via probe", "node", nodeID)
	}
}

func (gm *GossipManager) runFailureDetection() {
	now := time.Now()
	var toRemove, toMarkDead []string

	gm.mu.Lock()
	clusterSize := len(gm.liveNodes)
	startupGracePeriod := 20 * time.Second

	for id, node := range gm.liveNodes {
		if id == gm.localNodeID {
			continue
		}

		elapsed := now.Sub(node.LastActiveTs)
		timeSinceFirstSeen := elapsed

		switch node.State {
		case NodeState_NODE_STATE_ALIVE:
			// Dynamic timeout based on cluster size and load
			// Larger clusters and high load need more tolerance to prevent false positives
			effectiveTimeout := gm.failureTimeout

			// Scale timeout with cluster size to handle network congestion
			if clusterSize < 5 {
				effectiveTimeout = gm.failureTimeout * 2
			} else if clusterSize < 10 {
				effectiveTimeout = gm.failureTimeout * 3 / 2
			} else if clusterSize < 20 {
				effectiveTimeout = gm.failureTimeout * 5 / 4 // 1.25x for medium clusters
			} else if clusterSize < 50 {
				effectiveTimeout = gm.failureTimeout * 3 / 2 // 1.5x for large clusters
			} else {
				effectiveTimeout = gm.failureTimeout * 2 // 2x for very large clusters (50+)
			}

			// Additional tolerance based on pending operations
			// If we have high pending reads, nodes may be busy processing requests
			// Don't mark as SUSPECT too aggressively in this case
			pendingReads := gm.pendingReadsCount.Load()
			if pendingReads > 1000 {
				// High load detected - extend timeout by 50%
				effectiveTimeout = effectiveTimeout * 3 / 2
			}

			if elapsed > effectiveTimeout {
				if timeSinceFirstSeen < startupGracePeriod {
					node.LastActiveTs = time.Now()
					if logging.Log.IsDebugEnabled() {
						logging.Debug("Node in startup grace period, extending timeout",
							"node", id, "elapsed", elapsed, "clusterSize", clusterSize)
					}
				} else {
					// Before marking as SUSPECT, check if we're under high load
					// Under extreme load, network delays can cause false positives
					// Only mark as SUSPECT if elapsed is significantly beyond timeout
					minSuspectThreshold := effectiveTimeout * 2 // Require 2x timeout for high load tolerance
					if elapsed < minSuspectThreshold {
						// Within tolerance - extend LastActiveTs to give benefit of doubt
						// This prevents false positives under high load
						if pendingReads > 500 || clusterSize > 15 {
							node.LastActiveTs = time.Now()
							if logging.Log.IsDebugEnabled() {
								logging.Debug("High load detected, extending node timeout",
									"node", id, "elapsed", elapsed, "pendingReads", pendingReads, "clusterSize", clusterSize)
							}
							continue // Skip marking as SUSPECT
						}
					}

					// Only mark as SUSPECT if we're sure (elapsed significantly beyond threshold)
					// This ensures we only mark nodes as SUSPECT under extreme circumstances
					node.State = NodeState_NODE_STATE_SUSPECT
					node.Version = gm.incrementLocalVersion()
					nodeIDCopy := id
					_ = gm.replicationPool.Submit(func() {
						gm.initiateProbe(nodeIDCopy)
					})
					logging.Warn("MEMBER SUSPECT", "node", id, "elapsed", elapsed, "clusterSize", clusterSize, "effectiveTimeout", effectiveTimeout)
				}
			}

		case NodeState_NODE_STATE_SUSPECT:
			suspectDeadline := gm.failureTimeout + gm.suspectTimeout
			if clusterSize > 10 {
				suspectDeadline = suspectDeadline * 2
			}

			if elapsed > suspectDeadline {
				node.State = NodeState_NODE_STATE_DEAD
				node.Version = gm.incrementLocalVersion()
				toMarkDead = append(toMarkDead, id)
				logging.Error(errors.New("MEMBER DEAD"), "member dead", "node", id, "clusterSize", clusterSize)
			}

		case NodeState_NODE_STATE_DEAD:
			if elapsed > gm.failureTimeout+gm.suspectTimeout {
				toRemove = append(toRemove, id)
			}
		}
	}
	gm.mu.Unlock()

	for _, id := range toMarkDead {
		nodeIDCopy := id
		_ = gm.replicationPool.Submit(func() {
			gm.migrateDataFromDeadNode(nodeIDCopy)
		})

		gm.hashRing.Remove(id)
		gm.mu.RLock()
		if node, ok := gm.liveNodes[id]; ok && node.Address != "" {
			addr := node.Address
			gm.mu.RUnlock()
			gm.flushBatchForTarget(addr)
		} else {
			gm.mu.RUnlock()
		}
	}

	if len(toRemove) > 0 {
		gm.mu.Lock()
		for _, id := range toRemove {
			if node, ok := gm.liveNodes[id]; ok && node.Address != "" {
				addr := node.Address
				gm.mu.Unlock()
				gm.flushBatchForTarget(addr)
				gm.mu.Lock()
			}
			delete(gm.liveNodes, id)
		}
		gm.mu.Unlock()
	}
}

func (gm *GossipManager) initiateProbe(suspectID string) {
	pinger := gm.getRandomPeerID(suspectID)
	if pinger == "" {
		logging.Warn("PROBE: no pinger", "suspect", suspectID)
		return
	}

	peer, ok := gm.getNode(pinger)
	if !ok {
		return
	}

	msg := &GossipMessage{
		Type:   GossipMessageType_MESSAGE_TYPE_PROBE_REQUEST,
		Sender: gm.localNodeID,
		Payload: &GossipMessage_ProbeRequestPayload{
			ProbeRequestPayload: &ProbePayload{
				TargetNodeId: suspectID,
				RequesterId:  gm.localNodeID,
			},
		},
	}
	gm.signMessageCanonical(msg)
	gm.network.SendWithTimeout(peer.Address, msg, 500*time.Millisecond)
}
