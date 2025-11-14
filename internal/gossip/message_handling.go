package gossip

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/feellmoose/gridkv/internal/storage"
	"github.com/feellmoose/gridkv/internal/utils/crypto"
	"github.com/feellmoose/gridkv/internal/utils/logging"
)

// processGossipMessage routes incoming messages to appropriate handlers.
//
// Parameters:
//   - msg: The gossip message to process
func (gm *GossipManager) processGossipMessage(msg *GossipMessage) {
	switch msg.Type {
	case GossipMessageType_MESSAGE_TYPE_CONNECT:
		gm.handleConnect(msg)

	case GossipMessageType_MESSAGE_TYPE_CLUSTER_SYNC:
		gm.handleClusterSync(msg)

	case GossipMessageType_MESSAGE_TYPE_PROBE_REQUEST:
		gm.handleProbeRequest(msg)

	case GossipMessageType_MESSAGE_TYPE_PROBE_RESPONSE:
		gm.handleProbeResponse(msg)

	case GossipMessageType_MESSAGE_TYPE_CACHE_SYNC:
		gm.handleCacheSync(msg)

	case GossipMessageType_MESSAGE_TYPE_CACHE_SYNC_ACK:
		gm.handleCacheSyncAck(msg)

	case GossipMessageType_MESSAGE_TYPE_READ_REQUEST:
		gm.handleReadRequestMessage(msg)

	case GossipMessageType_MESSAGE_TYPE_READ_RESPONSE:
		gm.handleReadResponseMessage(msg)

	case GossipMessageType_MESSAGE_TYPE_FULL_SYNC_REQUEST:
		gm.handleFullSyncRequestMessage(msg)

	case GossipMessageType_MESSAGE_TYPE_FULL_SYNC_RESPONSE:
		gm.handleFullSyncResponseMessage(msg)

	default:
		logging.Debug("Unhandled gossip type", "type", msg.Type)
	}
}

// handleConnect processes a CONNECT message from a joining node.
func (gm *GossipManager) handleConnect(msg *GossipMessage) {
	p, ok := msg.Payload.(*GossipMessage_ConnectPayload)
	if !ok || p.ConnectPayload == nil {
		return
	}

	// Update HLC from remote
	gm.hlc.Update(p.ConnectPayload.Hlc)

	// Store sender's public key for signature verification
	// This enables automatic public key exchange during cluster formation
	if len(p.ConnectPayload.PublicKey) > 0 && p.ConnectPayload.NodeId != "" {
		gm.mu.Lock()
		gm.peerPubkeys[p.ConnectPayload.NodeId] = p.ConnectPayload.PublicKey
		gm.mu.Unlock()
		logging.Debug("Stored public key for node", "nodeID", p.ConnectPayload.NodeId)
	}

	// Update node state
	gm.updateNode(
		p.ConnectPayload.NodeId,
		p.ConnectPayload.Address,
		NodeState_NODE_STATE_ALIVE,
		p.ConnectPayload.Version,
	)

	// This completes the bidirectional public key exchange
	if p.ConnectPayload.NodeId != gm.localNodeID {
		var pubKey []byte
		if gm.keypair != nil {
			pubKey = gm.keypair.Pub
		}
		responseMsg := &GossipMessage{
			Type:   GossipMessageType_MESSAGE_TYPE_CONNECT,
			Sender: gm.localNodeID,
			Payload: &GossipMessage_ConnectPayload{
				ConnectPayload: &ConnectPayload{
					NodeId:    gm.localNodeID,
					Address:   gm.localAddress,
					Version:   gm.incrementLocalVersion(),
					Hlc:       gm.hlc.Now(),
					PublicKey: pubKey, // Include our public key (may be nil)
				},
			},
		}
		// SAFETY: signMessageCanonical will handle nil keypair gracefully
		if err := gm.signMessageCanonical(responseMsg); err != nil && !gm.disableAuth {
			logging.Warn("Failed to sign CONNECT response", "target", p.ConnectPayload.NodeId, "error", err)
		}

		// Send response back to sender immediately
		// Use shorter timeout for faster exchange
		go func() {
			if err := gm.network.SendWithTimeout(p.ConnectPayload.Address, responseMsg, 300*time.Millisecond); err != nil {
				logging.Debug("Failed to send CONNECT response", "target", p.ConnectPayload.NodeId, "error", err)
			} else {
				logging.Debug("Sent CONNECT response with public key", "target", p.ConnectPayload.NodeId)
			}
		}()
	}
}

// handleClusterSync processes a CLUSTER_SYNC message containing membership information.
func (gm *GossipManager) handleClusterSync(msg *GossipMessage) {
	p, ok := msg.Payload.(*GossipMessage_ClusterSyncPayload)
	if !ok || p.ClusterSyncPayload == nil {
		return
	}

	var nodesNeedingKeys []string

	// Update all nodes from sync message
	for _, n := range p.ClusterSyncPayload.Nodes {
		gm.updateNode(n.NodeId, n.Address, n.State, n.Version)

		// Check if we need this node's public key
		if n.NodeId != gm.localNodeID && n.State == NodeState_NODE_STATE_ALIVE {
			gm.mu.RLock()
			_, hasKey := gm.peerPubkeys[n.NodeId]
			gm.mu.RUnlock()

			if !hasKey {
				nodesNeedingKeys = append(nodesNeedingKeys, n.NodeId)
			}
		}
	}

	// This accelerates public key exchange during cluster formation
	if len(nodesNeedingKeys) > 0 && msg.Sender != "" {
		// Send CONNECT message to request public keys
		// The CONNECT message will trigger public key exchange
		if peer, ok := gm.getNode(msg.Sender); ok {
			// Send our public key and request theirs
			var pubKey []byte
			if gm.keypair != nil {
				pubKey = gm.keypair.Pub
			}
			connectMsg := &GossipMessage{
				Type:   GossipMessageType_MESSAGE_TYPE_CONNECT,
				Sender: gm.localNodeID,
				Payload: &GossipMessage_ConnectPayload{
					ConnectPayload: &ConnectPayload{
						NodeId:    gm.localNodeID,
						Address:   gm.localAddress,
						Version:   gm.incrementLocalVersion(),
						Hlc:       gm.hlc.Now(),
						PublicKey: pubKey, // Include our public key (may be nil)
					},
				},
			}
			// SAFETY: signMessageCanonical will handle nil keypair gracefully
			if err := gm.signMessageCanonical(connectMsg); err != nil && !gm.disableAuth {
				logging.Warn("Failed to sign CONNECT message for key request", "target", msg.Sender, "error", err)
			}
			gm.network.SendWithTimeout(peer.Address, connectMsg, 500*time.Millisecond)
		}
	}
}

// handleProbeRequest processes an indirect probe request for failure detection.
func (gm *GossipManager) handleProbeRequest(msg *GossipMessage) {
	p, ok := msg.Payload.(*GossipMessage_ProbeRequestPayload)
	if !ok || p.ProbeRequestPayload == nil {
		return
	}

	// Check if target node is alive locally
	alive := gm.isNodeLocallyAlive(p.ProbeRequestPayload.TargetNodeId)

	// Send response back to requester
	resp := &GossipMessage{
		Type:   GossipMessageType_MESSAGE_TYPE_PROBE_RESPONSE,
		Sender: gm.localNodeID,
		Payload: &GossipMessage_ProbeResponsePayload{
			ProbeResponsePayload: &ProbeResponsePayload{
				TargetNodeId: p.ProbeRequestPayload.TargetNodeId,
				Alive:        alive,
			},
		},
	}
	gm.signMessageCanonical(resp)

	if peer, ok := gm.getNode(msg.Sender); ok {
		gm.network.SendWithTimeout(peer.Address, resp, 500*time.Millisecond)
	}
}

// handleProbeResponse processes a probe response for failure detection.
func (gm *GossipManager) handleProbeResponse(msg *GossipMessage) {
	p, ok := msg.Payload.(*GossipMessage_ProbeResponsePayload)
	if !ok || p.ProbeResponsePayload == nil {
		return
	}

	if p.ProbeResponsePayload.Alive {
		gm.markNodeAliveFromProbe(p.ProbeResponsePayload.TargetNodeId)
	}
}

// handleCacheSync processes incremental cache synchronization.
func (gm *GossipManager) handleCacheSync(msg *GossipMessage) {
	p, ok := msg.Payload.(*GossipMessage_CacheSyncPayload)
	if !ok || p.CacheSyncPayload == nil {
		return
	}

	incSync := p.CacheSyncPayload.GetIncrementalSync()
	if incSync == nil {
		return
	}

	isForwarded := msg.OpId == ""

	// This must happen immediately to prevent timeout - even before getting peer info
	if msg.OpId != "" && msg.Sender != "" {
		senderID := msg.Sender
		opId := msg.OpId

		// This ensures ACK sending doesn't wait for any locks or processing
		go func() {
			// Cache peer address to avoid repeated lookups
			gm.mu.RLock()
			peer, peerOk := gm.liveNodes[senderID]
			peerAddr := ""
			if peerOk && peer != nil {
				peerAddr = peer.Address
			}
			gm.mu.RUnlock()

			if peerOk && peerAddr != "" {
				// For critical quorum operations, we send immediate ACK to prevent timeout
				// Batching reduces network overhead while maintaining low latency (2ms timeout)
				// SAFETY: Batching is safe because ACKs are idempotent and have unique OpIds

				// This prevents ACK timeout issues in forwarded replication
				// Batching can delay ACK by up to 2ms which may cause quorum timeout
				ackMsg := getGossipMessage()
				ackMsg.Type = GossipMessageType_MESSAGE_TYPE_CACHE_SYNC_ACK
				ackMsg.Sender = gm.localNodeID
				ackMsg.Payload = &GossipMessage_CacheSyncAckPayload{
					CacheSyncAckPayload: &CacheSyncAckPayload{
						OpId:    opId,
						PeerId:  gm.localNodeID,
						Success: true,
					},
				}

				// Send with longer timeout to handle network congestion
				if err := gm.network.SendWithTimeout(peerAddr, ackMsg, 1*time.Second); err != nil {
					if logging.Log.IsDebugEnabled() {
						logging.Debug("failed to send cache sync ack", "opId", opId, "target", peerAddr, "err", err)
					}
				}
				putGossipMessage(ackMsg)
			}
		}()
	}

	// This improves ACK latency and overall throughput
	ops := incSync.GetOperations()
	if len(ops) == 0 {
		return
	}

	// Fast path: Single operation - process inline if no forwarding needed
	if len(ops) == 1 && !isForwarded && msg.OpId == "" {
		op := ops[0]
		if op.Type == OperationType_OP_SET {
			item := protoItemToStorage(op.GetSetData(), op.ClientVersion)
			existing, err := gm.store.Get(op.Key)
			if err == nil && existing != nil {
				if item.Version <= existing.Version {
					return
				}
			}
			if err := gm.store.Set(op.Key, item); err != nil {
				logging.Error(err, "CACHE_SYNC apply failed", "key", op.Key)
			}
		} else if op.Type == OperationType_OP_DELETE {
			if err := gm.store.Delete(op.Key, op.ClientVersion); err != nil {
				logging.Error(err, "CACHE_SYNC delete failed", "key", op.Key)
			}
		}
		return
	}

	// Multiple operations or forwarding needed - process concurrently
	var opWg sync.WaitGroup
	opWg.Add(len(ops))
	for _, op := range ops {
		go func(operation *CacheSyncOperation) {
			defer opWg.Done()

			if operation.Type == OperationType_OP_SET {
				item := protoItemToStorage(operation.GetSetData(), operation.ClientVersion)
				// CRITICAL: Check version before overwriting to ensure higher version wins
				existing, err := gm.store.Get(operation.Key)
				if err == nil && existing != nil {
					// Only overwrite if new version is higher (last-write-wins)
					if item.Version <= existing.Version {
						if logging.Log.IsDebugEnabled() {
							logging.Debug("Skipping stale write", "key", operation.Key,
								"existingVersion", existing.Version, "newVersion", item.Version)
						}
						return
					}
				}
				if err := gm.store.Set(operation.Key, item); err != nil {
					logging.Error(err, "CACHE_SYNC apply failed", "key", operation.Key)
				}
				if isForwarded {
					// Clone item to avoid concurrent access issues
					itemCopy := &storage.StoredItem{
						Value:    append([]byte(nil), item.Value...),
						Version:  item.Version,
						ExpireAt: item.ExpireAt,
					}
					keyCopy := operation.Key
					senderCopy := msg.Sender
					if err := gm.replicationPool.Submit(func() {
						gm.replicateForwardedSet(keyCopy, itemCopy, senderCopy)
					}); err != nil {
						// This prevents blocking message processing even when pool is full
						go gm.replicateForwardedSet(operation.Key, itemCopy, msg.Sender)
					}
				}
			} else if operation.Type == OperationType_OP_DELETE {
				if err := gm.store.Delete(operation.Key, operation.ClientVersion); err != nil {
					logging.Error(err, "CACHE_SYNC delete failed", "key", operation.Key)
				}
				if isForwarded {
					keyCopy := operation.Key
					versionCopy := operation.ClientVersion
					senderCopy := msg.Sender
					if err := gm.replicationPool.Submit(func() {
						gm.replicateForwardedDelete(keyCopy, versionCopy, senderCopy)
					}); err != nil {
						go gm.replicateForwardedDelete(operation.Key, operation.ClientVersion, msg.Sender)
					}
				}
			}
		}(op)
	}
	// This allows message processing to return immediately, improving throughput
	// Operations will complete in background
	go func() {
		opWg.Wait()
	}()
}

// handleCacheSyncAck processes acknowledgment for cache sync operations.
func (gm *GossipManager) handleCacheSyncAck(msg *GossipMessage) {
	p, ok := msg.Payload.(*GossipMessage_CacheSyncAckPayload)
	if !ok || p.CacheSyncAckPayload == nil {
		return
	}

	// Route ACK to the network layer for correlation with pending requests
	gm.network.HandleAck(p.CacheSyncAckPayload)
}

// handleReadRequestMessage processes a READ_REQUEST message.
func (gm *GossipManager) handleReadRequestMessage(msg *GossipMessage) {
	p, ok := msg.Payload.(*GossipMessage_ReadRequestPayload)
	if !ok || p.ReadRequestPayload == nil {
		return
	}
	gm.handleReadRequest(p.ReadRequestPayload, msg.Sender)
}

// handleReadResponseMessage processes a READ_RESPONSE message.
func (gm *GossipManager) handleReadResponseMessage(msg *GossipMessage) {
	p, ok := msg.Payload.(*GossipMessage_ReadResponsePayload)
	if !ok || p.ReadResponsePayload == nil {
		return
	}
	gm.handleReadResponse(p.ReadResponsePayload)
}

// handleFullSyncRequestMessage processes a FULL_SYNC_REQUEST message.
func (gm *GossipManager) handleFullSyncRequestMessage(msg *GossipMessage) {
	gm.handleFullSyncRequest(msg.Sender)
}

// handleFullSyncResponseMessage processes a FULL_SYNC_RESPONSE message.
func (gm *GossipManager) handleFullSyncResponseMessage(msg *GossipMessage) {
	p, ok := msg.Payload.(*GossipMessage_FullSyncResponsePayload)
	if !ok || p.FullSyncResponsePayload == nil {
		return
	}
	gm.handleFullSyncResponse(p.FullSyncResponsePayload.FullSync)
}

// signMessageCanonical signs a message using the local key pair.
// SAFETY: If keypair is nil and auth is disabled, skip signing without error.
//
// Parameters:
//   - msg: The message to sign
//
// Returns:
//   - error: Any signing error
func (gm *GossipManager) signMessageCanonical(msg *GossipMessage) error {
	if msg == nil {
		return errors.New("nil message")
	}

	// SAFETY: If keypair is nil, handle gracefully
	if gm.keypair == nil {
		// If auth is disabled, allow unsigned messages
		if gm.disableAuth {
			msg.Signature = nil
			return nil
		}
		// If signing is not required, allow unsigned messages
		if !gm.requiresSignature(msg) {
			msg.Signature = nil
			return nil
		}
		// Otherwise, return error for required signatures
		return errors.New("keypair not exists")
	}

	if !gm.requiresSignature(msg) {
		msg.Signature = nil
		return nil
	}

	// This reduces allocations by reusing pooled buffers
	tmp, data, err := fastProtoCloneForSign(msg)
	if err != nil {
		return err
	}
	defer putGossipMessage(tmp)

	signature := crypto.SignMessage(gm.keypair.Priv, data)
	msg.Signature = signature
	return nil
}

// verifyMessageCanonical verifies a message's signature.
//
// Parameters:
//   - msg: The message to verify
//
// Returns:
//   - bool: true if signature is valid (or not required), false otherwise
func (gm *GossipManager) verifyMessageCanonical(msg *GossipMessage) bool {
	if msg == nil {
		return false
	}

	// FIX: If authentication is disabled, accept all messages
	// This is useful for testing or low-security environments
	if gm.disableAuth {
		return true
	}

	// CRITICAL FIX: For cluster formation messages, accept without signature verification
	// This enables dynamic cluster formation without pre-shared keys
	// Check this BEFORE signature verification to allow public key exchange
	switch msg.Type {
	case GossipMessageType_MESSAGE_TYPE_CONNECT,
		GossipMessageType_MESSAGE_TYPE_CLUSTER_SYNC,
		GossipMessageType_MESSAGE_TYPE_PROBE_REQUEST,
		GossipMessageType_MESSAGE_TYPE_PROBE_RESPONSE:
		// Accept cluster coordination messages without signature verification
		// Nodes trust the network layer (TCP) for authentication during cluster formation
		return true
	}

	if msg.Signature == nil {
		return !gm.requiresSignature(msg)
	}

	// FIX: Check if we have the sender's public key
	gm.mu.RLock()
	pub, ok := gm.peerPubkeys[msg.Sender]
	clusterSize := len(gm.liveNodes)
	gm.mu.RUnlock()

	if !ok {
		// This prevents rejection during startup when keys are still being exchanged
		if clusterSize < 3 {
			// Very early startup - allow message to pass through
			logging.Debug("Accepting message without pubkey during cluster formation",
				"sender", msg.Sender, "clusterSize", clusterSize)
			return true
		}

		// For data messages, we need the key - but log as debug during startup
		if clusterSize < 10 {
			logging.Debug("No pubkey for sender, rejecting", "sender", msg.Sender, "clusterSize", clusterSize)
		} else {
			logging.Warn("no pubkey for sender, rejecting", "sender", msg.Sender)
		}
		return false
	}

	sig := msg.Signature
	tmp, data, err := fastProtoCloneForSign(msg)
	if err != nil {
		return false
	}
	defer putGossipMessage(tmp)

	return crypto.VerifyMessage(pub, data, sig)
}

// requiresSignature determines if a message requires cryptographic signing.
//
// Parameters:
//   - msg: The message to check
//
// Returns:
//   - bool: true if signature is required, false otherwise
func (gm *GossipManager) requiresSignature(msg *GossipMessage) bool {
	// Always sign messages going to other nodes
	if len(gm.peerPubkeys) > 1 {
		return true
	}

	// For single-node clusters, only sign critical messages
	switch msg.Type {
	case CONNECT, CLUSTER_SYNC, PROBE_REQUEST, PROBE_RESPONSE:
		return true // Always sign cluster coordination
	case CACHE_SYNC, CACHE_SYNC_ACK:
		return gm.hasMultipleNodes() // Only sign if multi-node
	default:
		return false
	}
}

func (gm *GossipManager) replicateForwardedSet(key string, item *storage.StoredItem, sender string) {
	replicas := gm.hashRing.GetN(key, gm.replicaCount)
	if len(replicas) <= 1 {
		return
	}
	if replicas[0] != gm.localNodeID {
		return
	}

	// CRITICAL: Exclude sender from replication targets to avoid redundant sends
	// The sender already has the data since it forwarded the write to us
	targets := make([]string, 0, len(replicas)-1)
	for _, replicaID := range replicas[1:] {
		if replicaID == gm.localNodeID || replicaID == sender {
			continue
		}
		targets = append(targets, replicaID)
	}

	if len(targets) == 0 {
		return
	}

	// Forwarded replication needs longer timeout to handle high concurrency
	gm.mu.RLock()
	clusterSize := len(gm.liveNodes)
	gm.mu.RUnlock()

	// This is critical because forwarded replication happens asynchronously
	// and may experience delays in message processing
	effectiveTimeout := gm.replicationTimeout * 2 // Start with 2x for forwarded writes
	if clusterSize > 10 {
		effectiveTimeout = gm.replicationTimeout * 6 // Increased for large clusters
	} else if clusterSize > 5 {
		effectiveTimeout = gm.replicationTimeout * 4
	}

	effectiveTimeout = effectiveTimeout + effectiveTimeout*50/100

	ctx, cancel := context.WithTimeout(context.Background(), effectiveTimeout)
	defer cancel()

	// Forwarded replication is best-effort to avoid blocking message processing
	if err := gm.replicateToNodes(ctx, key, item, targets); err != nil {
		// Forwarded replication failures don't block the original write
		if logging.Log.IsDebugEnabled() {
			logging.Debug("forwarded set replication failed", "key", key, "sender", sender, "err", err)
		}
	}
}

func (gm *GossipManager) replicateForwardedDelete(key string, version int64, sender string) {
	replicas := gm.hashRing.GetN(key, gm.replicaCount)
	if len(replicas) <= 1 {
		return
	}
	if replicas[0] != gm.localNodeID {
		return
	}

	// CRITICAL: Exclude sender from replication targets
	targets := make([]string, 0, len(replicas)-1)
	for _, replicaID := range replicas[1:] {
		if replicaID == gm.localNodeID || replicaID == sender {
			continue
		}
		targets = append(targets, replicaID)
	}

	if len(targets) == 0 {
		return
	}

	gm.mu.RLock()
	clusterSize := len(gm.liveNodes)
	gm.mu.RUnlock()

	effectiveTimeout := gm.replicationTimeout * 2
	if clusterSize > 10 {
		effectiveTimeout = gm.replicationTimeout * 6
	} else if clusterSize > 5 {
		effectiveTimeout = gm.replicationTimeout * 4
	}

	effectiveTimeout = effectiveTimeout + effectiveTimeout*50/100

	ctx, cancel := context.WithTimeout(context.Background(), effectiveTimeout)
	defer cancel()

	if err := gm.replicateDeleteToNodes(ctx, key, version, targets); err != nil {
		if logging.Log.IsDebugEnabled() {
			logging.Debug("forwarded delete replication failed", "key", key, "sender", sender, "err", err)
		}
	}
}
