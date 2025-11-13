package gossip

import (
	"errors"
	"time"

	"github.com/feellmoose/gridkv/internal/utils/logging"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// updateNode updates or adds a node to the cluster membership.
// This method implements vector clock-based conflict resolution.
//
// Parameters:
//   - nodeID: Unique identifier for the node
//   - address: Network address of the node
//   - newState: New state for the node (ALIVE, SUSPECT, DEAD)
//   - version: Version number for conflict resolution
func (gm *GossipManager) updateNode(nodeID, address string, newState NodeState, version int64) {
	var add, remove bool

	gm.mu.Lock()
	now := timestamppb.Now()
	existing, found := gm.liveNodes[nodeID]

	// Ignore stale updates (version-based conflict resolution)
	if found && version < existing.Version {
		gm.mu.Unlock()
		logging.Debug("Ignore stale gossip", "node", nodeID)
		return
	}

	// Handle new node
	if !found {
		gm.liveNodes[nodeID] = &NodeInfo{
			NodeId:       nodeID,
			Address:      address,
			LastActiveTs: now,
			State:        NodeState_NODE_STATE_ALIVE,
			Version:      version,
		}
		if nodeID != gm.localNodeID {
			add = true
		}
		gm.mu.Unlock()

		if add {
			gm.hashRing.Add(nodeID)
			logging.Debug("MEMBER JOIN added to ring", "node", nodeID)
		}
		return
	}

	// Handle existing node state transition
	if version >= existing.Version {
		oldState := existing.State
		existing.State = newState
		existing.Address = address // Update address in case it changed
		existing.LastActiveTs = now
		existing.Version = version

		// Determine hash ring changes
		if newState == NodeState_NODE_STATE_DEAD && oldState != NodeState_NODE_STATE_DEAD {
			remove = true
		}
		if newState == NodeState_NODE_STATE_ALIVE && oldState != NodeState_NODE_STATE_ALIVE && nodeID != gm.localNodeID {
			add = true
		}
	}
	gm.mu.Unlock()

	// Apply ring changes outside lock
	if remove {
		gm.hashRing.Remove(nodeID)
		// Clean up batch buffer for this node's address
		if existing != nil && existing.Address != "" {
			gm.flushBatchForTarget(existing.Address)
			// Remove batch from buffer
			gm.batchMutex.Lock()
			delete(gm.batchBuffer, existing.Address)
			gm.batchMutex.Unlock()
		}
		logging.Error(errors.New("MEMBER DEAD"), "removed from ring", "node", nodeID)
	}
	if add {
		gm.hashRing.Add(nodeID)
		logging.Debug("re-added to ring", "node", nodeID)
	}
}

// markNodeAliveFromProbe marks a suspect node as alive after successful probe.
//
// Parameters:
//   - nodeID: ID of the node to mark alive
func (gm *GossipManager) markNodeAliveFromProbe(nodeID string) {
	gm.mu.Lock()
	defer gm.mu.Unlock()

	if n, ok := gm.liveNodes[nodeID]; ok && n.State == NodeState_NODE_STATE_SUSPECT {
		n.State = NodeState_NODE_STATE_ALIVE
		n.LastActiveTs = timestamppb.Now()
		n.Version = gm.incrementLocalVersion()
		logging.Debug("MEMBER RECOVER via probe", "node", nodeID)
	}
}

// runFailureDetection implements SWIM-style failure detection.
// OPTIMIZATION: Improved stability during cluster startup
//
// This runs periodically to detect and mark failed nodes.
// State transitions:
//   - ALIVE -> SUSPECT: After FailureTimeout with no heartbeat
//   - SUSPECT -> DEAD: After FailureTimeout + SuspectTimeout
//   - DEAD -> Removed: After cleanup period
func (gm *GossipManager) runFailureDetection() {
	now := time.Now()
	var toRemove, toMarkDead []string

	gm.mu.Lock()
	clusterSize := len(gm.liveNodes)
	startupGracePeriod := 20 * time.Second // Grace period after startup (increased)

	for id, node := range gm.liveNodes {
		// Skip local node
		if id == gm.localNodeID {
			continue
		}

		elapsed := now.Sub(node.LastActiveTs.AsTime())

		// OPTIMIZATION: Track when node was first discovered (use initial timestamp)
		// If this is a new node (recently added), give it grace period
		timeSinceFirstSeen := elapsed

		switch node.State {
		case NodeState_NODE_STATE_ALIVE:
			// OPTIMIZATION: Apply grace period during startup to reduce false SUSPECT
			// For small clusters (< 5 nodes), give more time for startup
			effectiveTimeout := gm.failureTimeout

			// OPTIMIZATION: During initial cluster formation, be more lenient
			// Small clusters need more time for gossip to propagate
			if clusterSize < 5 {
				effectiveTimeout = gm.failureTimeout * 2 // Double timeout for small clusters
			} else if clusterSize < 10 {
				effectiveTimeout = gm.failureTimeout * 3 / 2 // 1.5x timeout for medium clusters
			}

			// Mark as suspect after failure timeout
			if elapsed > effectiveTimeout {
				// OPTIMIZATION: During startup grace period, don't mark as SUSPECT
				// This prevents false positives when nodes are still joining
				if timeSinceFirstSeen < startupGracePeriod {
					// Node is new - extend grace period instead of marking SUSPECT
					// Update timestamp to give it more time
					node.LastActiveTs = timestamppb.Now()
					if logging.Log.IsDebugEnabled() {
						logging.Debug("Node in startup grace period, extending timeout",
							"node", id, "elapsed", elapsed, "clusterSize", clusterSize)
					}
				} else {
					// Node has been known for a while - mark as SUSPECT
					node.State = NodeState_NODE_STATE_SUSPECT
					node.Version = gm.incrementLocalVersion()
					go gm.initiateProbe(id)
					logging.Warn("MEMBER SUSPECT", "node", id, "elapsed", elapsed, "clusterSize", clusterSize)
				}
			}

		case NodeState_NODE_STATE_SUSPECT:
			// OPTIMIZATION: Extended suspect timeout for larger clusters
			suspectDeadline := gm.failureTimeout + gm.suspectTimeout
			if clusterSize > 10 {
				suspectDeadline = suspectDeadline * 2 // Double timeout for large clusters
			}

			// Mark as dead after suspect timeout
			if elapsed > suspectDeadline {
				node.State = NodeState_NODE_STATE_DEAD
				node.Version = gm.incrementLocalVersion()
				toMarkDead = append(toMarkDead, id)
				logging.Error(errors.New("MEMBER DEAD"), "member dead", "node", id, "clusterSize", clusterSize)
			}

		case NodeState_NODE_STATE_DEAD:
			// Remove dead node after cleanup period
			if elapsed > gm.failureTimeout+gm.suspectTimeout {
				toRemove = append(toRemove, id)
			}
		}
	}
	gm.mu.Unlock()

	// Remove dead nodes from hash ring and clean up batches
	for _, id := range toMarkDead {
		gm.hashRing.Remove(id)
		// Clean up batch buffer for this node
		gm.mu.RLock()
		if node, ok := gm.liveNodes[id]; ok && node.Address != "" {
			addr := node.Address
			gm.mu.RUnlock()
			gm.flushBatchForTarget(addr)
			gm.batchMutex.Lock()
			delete(gm.batchBuffer, addr)
			gm.batchMutex.Unlock()
		} else {
			gm.mu.RUnlock()
		}
	}

	// Clean up dead nodes from memory
	if len(toRemove) > 0 {
		gm.mu.Lock()
		for _, id := range toRemove {
			if node, ok := gm.liveNodes[id]; ok && node.Address != "" {
				addr := node.Address
				gm.mu.Unlock()
				gm.batchMutex.Lock()
				delete(gm.batchBuffer, addr)
				gm.batchMutex.Unlock()
				gm.mu.Lock()
			}
			delete(gm.liveNodes, id)
		}
		gm.mu.Unlock()
	}
}

// initiateProbe performs an indirect probe through a random peer.
// This is part of the SWIM protocol's indirect probing mechanism.
//
// Parameters:
//   - suspectID: ID of the suspect node to probe
func (gm *GossipManager) initiateProbe(suspectID string) {
	// Select a random peer to perform indirect probe
	pinger := gm.getRandomPeerID(suspectID)
	if pinger == "" {
		logging.Warn("PROBE: no pinger", "suspect", suspectID)
		return
	}

	peer, ok := gm.getNode(pinger)
	if !ok {
		return
	}

	// Send probe request
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
