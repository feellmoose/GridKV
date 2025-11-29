package gossip

import (
	"context"
	"errors"
	"time"

	"github.com/feellmoose/gridkv/internal/storage"
	"github.com/feellmoose/gridkv/internal/utils/crypto"
	"github.com/feellmoose/gridkv/internal/utils/logging"
	"github.com/feellmoose/gridkv/internal/utils/workerpool"
)

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

	case GossipMessageType_MESSAGE_TYPE_READ_REQUEST:
		gm.handleReadRequestMessage(msg)

	case GossipMessageType_MESSAGE_TYPE_READ_RESPONSE:
		gm.handleReadResponseMessage(msg)

	case GossipMessageType_MESSAGE_TYPE_BATCH_READ_REQUEST:
		gm.handleBatchReadRequestMessage(msg)

	case GossipMessageType_MESSAGE_TYPE_BATCH_READ_RESPONSE:
		gm.handleBatchReadResponseMessage(msg)

	case GossipMessageType_MESSAGE_TYPE_FULL_SYNC_REQUEST:
		gm.handleFullSyncRequestMessage(msg)

	case GossipMessageType_MESSAGE_TYPE_FULL_SYNC_RESPONSE:
		gm.handleFullSyncResponseMessage(msg)

	default:
		logging.Debug("Unhandled gossip type", "type", msg.Type)
	}
}

func (gm *GossipManager) handleConnect(msg *GossipMessage) {
	p, ok := msg.Payload.(*GossipMessage_ConnectPayload)
	if !ok || p.ConnectPayload == nil {
		return
	}

	gm.hlc.Update(p.ConnectPayload.Hlc)

	if len(p.ConnectPayload.PublicKey) > 0 && p.ConnectPayload.NodeId != "" {
		gm.mu.Lock()
		gm.peerPubkeys[p.ConnectPayload.NodeId] = p.ConnectPayload.PublicKey
		gm.mu.Unlock()
		logging.Debug("Stored public key for node", "nodeID", p.ConnectPayload.NodeId)
	}

	gm.updateNode(
		p.ConnectPayload.NodeId,
		p.ConnectPayload.Address,
		NodeState_NODE_STATE_ALIVE,
		p.ConnectPayload.Version,
	)

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
					PublicKey: pubKey,
				},
			},
		}
		if err := gm.signMessageCanonical(responseMsg); err != nil && !gm.disableAuth {
			logging.Warn("Failed to sign CONNECT response", "target", p.ConnectPayload.NodeId, "error", err)
		}

		addr := p.ConnectPayload.Address
		nodeId := p.ConnectPayload.NodeId

		now := time.Now()
		stateInterface, exists := gm.connectingNodes.LoadOrStore(nodeId, &connectingState{
			lastAttempt: now,
			attempts:    1,
		})

		state := stateInterface.(*connectingState)
		state.mu.Lock()
		shouldConnect := false
		if exists {
			timeSinceLastAttempt := now.Sub(state.lastAttempt)
			if timeSinceLastAttempt > 500*time.Millisecond {
				state.lastAttempt = now
				state.attempts++
				shouldConnect = true
			} else {
				shouldConnect = false
			}
		} else {
			shouldConnect = true
		}
		state.mu.Unlock()

		if !shouldConnect {
			if logging.Log.IsDebugEnabled() {
				logging.Debug("Dropped CONNECT response: rate limited", "target", nodeId)
			}
			return
		}

		select {
		case gm.connectRateLimiter <- struct{}{}:
		default:
			// Rate limiter full - try to send anyway (connection is critical)
			// Don't drop, just proceed without rate limiter token
			if logging.Log.IsDebugEnabled() {
				logging.Debug("CONNECT response: rate limiter full, sending anyway", "target", nodeId)
			}
		}

		sendConnectResponse := func() {
			defer func() {
				select {
				case <-gm.connectRateLimiter:
				default:
				}
				gm.connectingNodes.Delete(nodeId)
			}()

			// Send response with longer timeout for initial connection
			if err := gm.network.SendWithTimeout(addr, responseMsg, 2*time.Second); err != nil {
				if logging.Log.IsDebugEnabled() {
					logging.Debug("Failed to send CONNECT response", "target", nodeId, "error", err)
				}
			} else {
				if logging.Log.IsDebugEnabled() {
					logging.Debug("Sent CONNECT response", "target", nodeId, "address", addr)
				}
			}
		}

		if err := gm.replicationPool.Submit(sendConnectResponse); err != nil {
			if err := gm.submitInboundTaskWithPriority(sendConnectResponse, context.Background(), workerpool.PriorityCritical, "connect-response", nodeId); err != nil {
				// Both pools full - retry with resize or skip
				if gm.replicationPoolResizer != nil {
					gm.replicationPoolResizer.emergencyResize()
					// Stage 2 Sleep优化: pool resize是同步的，无需等待
					// emergencyResize会立即调整pool大小，可以直接重试
					if err := gm.replicationPool.Submit(sendConnectResponse); err != nil {
						<-gm.connectRateLimiter
						gm.connectingNodes.Delete(nodeId)
						if logging.Log.IsDebugEnabled() {
							logging.Debug("Dropped CONNECT response: pool exhausted after resize", "target", nodeId)
						}
					}
				} else {
					<-gm.connectRateLimiter
					gm.connectingNodes.Delete(nodeId)
					if logging.Log.IsDebugEnabled() {
						logging.Debug("Dropped CONNECT response: pool exhausted", "target", nodeId)
					}
				}
			}
		}
	}
}

func (gm *GossipManager) handleClusterSync(msg *GossipMessage) {
	p, ok := msg.Payload.(*GossipMessage_ClusterSyncPayload)
	if !ok || p.ClusterSyncPayload == nil {
		return
	}

	var nodesNeedingKeys []string
	var nodeUpdates []nodeUpdate

	// Batch node updates to reduce lock contention
	for _, n := range p.ClusterSyncPayload.Nodes {
		nodeUpdates = append(nodeUpdates, nodeUpdate{
			nodeID:  n.NodeId,
			address: n.Address,
			state:   n.State,
			version: n.Version,
		})

		if n.NodeId != gm.localNodeID && n.State == NodeState_NODE_STATE_ALIVE {
			gm.mu.RLock()
			_, hasKey := gm.peerPubkeys[n.NodeId]
			gm.mu.RUnlock()

			if !hasKey {
				nodesNeedingKeys = append(nodesNeedingKeys, n.NodeId)
			}
		}
	}

	// Batch update all nodes in a single lock acquisition
	if len(nodeUpdates) > 0 {
		gm.batchUpdateNodes(nodeUpdates)
	}

	if len(nodesNeedingKeys) > 0 && msg.Sender != "" {
		if peer, ok := gm.getNode(msg.Sender); ok {
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
						PublicKey: pubKey,
					},
				},
			}
			if err := gm.signMessageCanonical(connectMsg); err != nil && !gm.disableAuth {
				logging.Warn("Failed to sign CONNECT message for key request", "target", msg.Sender, "error", err)
			}
			gm.network.SendWithTimeout(peer.Address, connectMsg, 500*time.Millisecond)
		}
	}
}

func (gm *GossipManager) handleProbeRequest(msg *GossipMessage) {
	p, ok := msg.Payload.(*GossipMessage_ProbeRequestPayload)
	if !ok || p.ProbeRequestPayload == nil {
		return
	}

	alive := gm.isNodeLocallyAlive(p.ProbeRequestPayload.TargetNodeId)

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

func (gm *GossipManager) handleProbeResponse(msg *GossipMessage) {
	p, ok := msg.Payload.(*GossipMessage_ProbeResponsePayload)
	if !ok || p.ProbeResponsePayload == nil {
		return
	}

	if p.ProbeResponsePayload.Alive {
		gm.markNodeAliveFromProbe(p.ProbeResponsePayload.TargetNodeId)
	}
}

// handleCacheSync processes CACHE_SYNC messages with batch processing.
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
	ops := incSync.GetOperations()
	if len(ops) == 0 {
		return
	}

	// Fast path: single operation
	if len(ops) == 1 {
		op := ops[0]
		if op.Type == OperationType_OP_SET {
			item := protoItemToStorage(op.GetSetData(), op.ClientVersion)
			if item == nil || len(item.Value) == 0 {
				return
			}
			existing, err := gm.store.Get(op.Key)
			if err == nil && existing != nil && item.Version <= existing.Version {
				return
			}
			if err := gm.store.Set(op.Key, item); err != nil {
				logging.Error(err, "CACHE_SYNC apply failed", "key", op.Key)
			} else if isForwarded {
				// Async replication for forwarded writes
				itemCopy := copyStorageItem(item)
				keyCopy := op.Key
				senderCopy := msg.Sender
				_ = gm.replicationPool.Submit(func() {
					gm.replicateForwardedSet(keyCopy, itemCopy, senderCopy)
				})
			}
		} else if op.Type == OperationType_OP_DELETE {
			if err := gm.store.Delete(op.Key, op.ClientVersion); err != nil {
				logging.Error(err, "CACHE_SYNC delete failed", "key", op.Key)
			} else {
				if gm.hotCacheTTL > 0 {
					gm.hotCache.Delete(op.Key)
				}
				if isForwarded {
					keyCopy := op.Key
					versionCopy := op.ClientVersion
					senderCopy := msg.Sender
					_ = gm.replicationPool.Submit(func() {
						gm.replicateForwardedDelete(keyCopy, versionCopy, senderCopy)
					})
				}
			}
		}
		return
	}

	// Batch path: use storage batch API to reduce IO operations
	// Group operations by type for efficient batch processing
	setOps := make([]*CacheSyncOperation, 0, len(ops))
	deleteOps := make([]*CacheSyncOperation, 0, len(ops))

	// Pre-process operations: validate and group
	for _, op := range ops {
		if op.Type == OperationType_OP_SET {
			item := protoItemToStorage(op.GetSetData(), op.ClientVersion)
			if item == nil || len(item.Value) == 0 {
				continue
			}
			setOps = append(setOps, op)
		} else if op.Type == OperationType_OP_DELETE {
			deleteOps = append(deleteOps, op)
		}
	}

	// Batch apply SET operations using storage batch API
	if len(setOps) > 0 {
		// Use ApplyIncrementalSync for batch processing - reduces IO operations
		if err := gm.store.ApplyIncrementalSync(setOps); err != nil {
			// Fallback to individual operations if batch fails
			for _, op := range setOps {
				item := protoItemToStorage(op.GetSetData(), op.ClientVersion)
				if item == nil || len(item.Value) == 0 {
					continue
				}
				existing, err := gm.store.Get(op.Key)
				if err == nil && existing != nil && item.Version <= existing.Version {
					continue
				}
				if err := gm.store.Set(op.Key, item); err != nil {
					logging.Error(err, "CACHE_SYNC apply failed", "key", op.Key)
				} else if isForwarded {
					// Batch replication for forwarded writes
					itemCopy := copyStorageItem(item)
					keyCopy := op.Key
					senderCopy := msg.Sender
					_ = gm.replicationPool.Submit(func() {
						gm.replicateForwardedSet(keyCopy, itemCopy, senderCopy)
					})
				}
			}
		} else if isForwarded {
			// Batch replication for forwarded writes after successful batch apply
			for _, op := range setOps {
				item := protoItemToStorage(op.GetSetData(), op.ClientVersion)
				if item == nil || len(item.Value) == 0 {
					continue
				}
				itemCopy := copyStorageItem(item)
				keyCopy := op.Key
				senderCopy := msg.Sender
				_ = gm.replicationPool.Submit(func() {
					gm.replicateForwardedSet(keyCopy, itemCopy, senderCopy)
				})
			}
		}
	}

	if len(deleteOps) > 0 {
		if err := gm.store.ApplyIncrementalSync(deleteOps); err != nil {
			for _, op := range deleteOps {
				if err := gm.store.Delete(op.Key, op.ClientVersion); err != nil {
					logging.Error(err, "CACHE_SYNC delete failed", "key", op.Key)
				} else {
					if gm.hotCacheTTL > 0 {
						gm.hotCache.Delete(op.Key)
					}
					if isForwarded {
						keyCopy := op.Key
						versionCopy := op.ClientVersion
						senderCopy := msg.Sender
						_ = gm.replicationPool.Submit(func() {
							gm.replicateForwardedDelete(keyCopy, versionCopy, senderCopy)
						})
					}
				}
			}
		} else {
			if gm.hotCacheTTL > 0 {
				for _, op := range deleteOps {
					gm.hotCache.Delete(op.Key)
				}
			}
			if isForwarded {
				for _, op := range deleteOps {
					keyCopy := op.Key
					versionCopy := op.ClientVersion
					senderCopy := msg.Sender
					_ = gm.replicationPool.Submit(func() {
						gm.replicateForwardedDelete(keyCopy, versionCopy, senderCopy)
					})
				}
			}
		}
	}
}

func (gm *GossipManager) handleReadRequestMessage(msg *GossipMessage) {
	p, ok := msg.Payload.(*GossipMessage_ReadRequestPayload)
	if !ok || p.ReadRequestPayload == nil {
		return
	}
	gm.handleReadRequest(p.ReadRequestPayload, msg.Sender)
}

func (gm *GossipManager) handleReadResponseMessage(msg *GossipMessage) {
	p, ok := msg.Payload.(*GossipMessage_ReadResponsePayload)
	if !ok || p.ReadResponsePayload == nil {
		return
	}
	gm.handleReadResponse(p.ReadResponsePayload)
}

func (gm *GossipManager) handleBatchReadRequestMessage(msg *GossipMessage) {
	p, ok := msg.Payload.(*GossipMessage_BatchReadRequestPayload)
	if !ok || p.BatchReadRequestPayload == nil {
		return
	}
	gm.handleBatchReadRequest(p.BatchReadRequestPayload, msg.Sender)
}

func (gm *GossipManager) handleBatchReadResponseMessage(msg *GossipMessage) {
	p, ok := msg.Payload.(*GossipMessage_BatchReadResponsePayload)
	if !ok || p.BatchReadResponsePayload == nil {
		return
	}
	gm.handleBatchReadResponse(p.BatchReadResponsePayload)
}

func (gm *GossipManager) handleFullSyncRequestMessage(msg *GossipMessage) {
	gm.handleFullSyncRequest(msg.Sender)
}

func (gm *GossipManager) handleFullSyncResponseMessage(msg *GossipMessage) {
	p, ok := msg.Payload.(*GossipMessage_FullSyncResponsePayload)
	if !ok || p.FullSyncResponsePayload == nil {
		return
	}
	gm.handleFullSyncResponse(p.FullSyncResponsePayload.FullSync)
}

func (gm *GossipManager) signMessageCanonical(msg *GossipMessage) error {
	if msg == nil {
		return errors.New("nil message")
	}

	if gm.keypair == nil {
		if gm.disableAuth {
			msg.Signature = nil
			return nil
		}
		if !gm.requiresSignature(msg) {
			msg.Signature = nil
			return nil
		}
		return errors.New("keypair not exists")
	}

	if !gm.requiresSignature(msg) {
		msg.Signature = nil
		return nil
	}

	tmp, data, err := fastProtoCloneForSign(msg)
	if err != nil {
		return err
	}
	defer putGossipMessage(tmp)

	signature := crypto.SignMessage(gm.keypair.Priv, data)
	msg.Signature = signature
	return nil
}

func (gm *GossipManager) verifyMessageCanonical(msg *GossipMessage) bool {
	if msg == nil {
		return false
	}

	if gm.disableAuth {
		return true
	}

	// For messages that don't require signature, allow them
	if msg.Signature == nil {
		// Enforce signatures for all messages that require auth.
		// For backward-compatibility during controlled bootstrap, callers
		// should explicitly set DisableAuth or use a dedicated bootstrap
		// configuration rather than relying on implicit grace windows.
		if gm.requiresSignature(msg) {
			if gm.metrics != nil {
				gm.metrics.IncrementSecurityUnauthenticatedMessages()
			}
			return false
		}
		return true
	}

	gm.mu.RLock()
	pub, ok := gm.peerPubkeys[msg.Sender]
	gm.mu.RUnlock()

	if !ok {
		// Unknown sender or missing public key: treat as unauthenticated.
		if gm.metrics != nil {
			gm.metrics.IncrementSecurityUnauthenticatedMessages()
		}
		return false
	}

	sig := msg.Signature
	tmp, data, err := fastProtoCloneForSign(msg)
	if err != nil {
		return false
	}
	defer putGossipMessage(tmp)

	verified := crypto.VerifyMessage(pub, data, sig)
	if !verified && logging.Log.IsDebugEnabled() {
		logging.Debug("Signature verification failed",
			"sender", msg.Sender, "type", msg.Type)
	}
	if !verified && gm.metrics != nil {
		gm.metrics.IncrementSecuritySignatureFailures()
	}
	return verified
}

func (gm *GossipManager) requiresSignature(msg *GossipMessage) bool {
	switch msg.Type {
	case CONNECT, CLUSTER_SYNC, PROBE_REQUEST, PROBE_RESPONSE,
		READ_REQUEST, READ_RESPONSE,
		BATCH_READ_REQUEST, BATCH_READ_RESPONSE,
		CACHE_SYNC, FULL_SYNC_REQUEST, FULL_SYNC_RESPONSE:
		return true
	default:
		return false
	}
}

func (gm *GossipManager) replicateForwardedSet(key string, item *storage.StoredItem, sender string) {
	replicas := gm.getReplicas(key, gm.replicaCount)
	if len(replicas) <= 1 {
		return
	}
	if replicas[0] != gm.localNodeID {
		return
	}

	targets := GetStringSlice()
	defer func() {
		PutStringSlice(targets)
	}()
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

	if err := gm.replicateToNodes(ctx, key, item, targets); err != nil {
		if logging.Log.IsDebugEnabled() {
			logging.Debug("forwarded set replication failed", "key", key, "sender", sender, "err", err)
		}
	}
}

func (gm *GossipManager) replicateForwardedDelete(key string, version int64, sender string) {
	replicas := gm.getReplicas(key, gm.replicaCount)
	if len(replicas) <= 1 {
		return
	}
	if replicas[0] != gm.localNodeID {
		return
	}

	targets := GetStringSlice()
	defer func() {
		PutStringSlice(targets)
	}()
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
