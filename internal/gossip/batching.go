package gossip

import (
	"sync"
	"time"

	"github.com/feellmoose/gridkv/internal/utils/logging"
)

// ackBatch buffers ACK messages for batching to reduce network overhead
type ackBatch struct {
	mutex     sync.Mutex
	target    string
	acks      []*CacheSyncAckPayload
	timer     *time.Timer
	manager   *GossipManager
	flushSize int
}

// newAckBatch creates a new ACK batch
func newAckBatch(target string, manager *GossipManager) *ackBatch {
	ab := &ackBatch{
		target:    target,
		manager:   manager,
		acks:      make([]*CacheSyncAckPayload, 0, 32),
		flushSize: 32,
	}
	return ab
}

// addAck adds an ACK to the batch
func (ab *ackBatch) addAck(opId string, peerId string, success bool) {
	ab.mutex.Lock()
	defer ab.mutex.Unlock()

	ack := &CacheSyncAckPayload{
		OpId:    opId,
		PeerId:  peerId,
		Success: success,
	}
	ab.acks = append(ab.acks, ack)

	shouldFlush := len(ab.acks) >= ab.flushSize

	if shouldFlush {
		if ab.timer != nil {
			ab.timer.Stop()
			ab.timer = nil
		}
		acks := make([]*CacheSyncAckPayload, len(ab.acks))
		copy(acks, ab.acks)
		ab.acks = ab.acks[:0]
		ab.mutex.Unlock()
		ab.flushAcks(acks)
		ab.mutex.Lock()
	} else if ab.timer == nil {
		ab.timer = time.AfterFunc(2*time.Millisecond, func() {
			ab.mutex.Lock()
			if len(ab.acks) > 0 {
				acks := make([]*CacheSyncAckPayload, len(ab.acks))
				copy(acks, ab.acks)
				ab.acks = ab.acks[:0]
				if ab.timer != nil {
					ab.timer.Stop()
					ab.timer = nil
				}
				ab.mutex.Unlock()
				ab.flushAcks(acks)
			} else {
				ab.mutex.Unlock()
			}
		})
	}
}

// flushAcks sends batched ACKs
func (ab *ackBatch) flushAcks(acks []*CacheSyncAckPayload) {
	if len(acks) == 0 {
		return
	}

	for _, ack := range acks {
		msg := getGossipMessage()
		msg.Type = GossipMessageType_MESSAGE_TYPE_CACHE_SYNC_ACK
		msg.Sender = ab.manager.localNodeID
		msg.Payload = &GossipMessage_CacheSyncAckPayload{
			CacheSyncAckPayload: ack,
		}

		if err := ab.manager.network.SendWithTimeout(ab.target, msg, 500*time.Millisecond); err != nil {
			if logging.Log.IsDebugEnabled() {
				logging.Debug("failed to send batched ack", "opId", ack.OpId, "target", ab.target, "err", err)
			}
		}
		putGossipMessage(msg)
	}
}

// flush forces immediate flush of pending ACKs
func (ab *ackBatch) flush() {
	ab.mutex.Lock()
	acks := make([]*CacheSyncAckPayload, len(ab.acks))
	copy(acks, ab.acks)
	ab.acks = ab.acks[:0]
	if ab.timer != nil {
		ab.timer.Stop()
		ab.timer = nil
	}
	ab.mutex.Unlock()

	ab.flushAcks(acks)
}

// batchCleanupManager manages periodic cleanup of batch buffers and object pools
type batchCleanupManager struct {
	gm              *GossipManager
	cleanupInterval time.Duration
	stopCh          chan struct{}
	wg              sync.WaitGroup
}

// newBatchCleanupManager creates a new batch cleanup manager
func newBatchCleanupManager(gm *GossipManager) *batchCleanupManager {
	return &batchCleanupManager{
		gm:              gm,
		cleanupInterval: 30 * time.Second,
		stopCh:          make(chan struct{}),
	}
}

// start begins periodic cleanup
func (bcm *batchCleanupManager) start() {
	bcm.wg.Add(1)
	go bcm.run()
}

// stop stops the cleanup manager
func (bcm *batchCleanupManager) stop() {
	close(bcm.stopCh)
	bcm.wg.Wait()
}

// run performs periodic cleanup
func (bcm *batchCleanupManager) run() {
	defer bcm.wg.Done()

	ticker := time.NewTicker(bcm.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-bcm.stopCh:
			return
		case <-ticker.C:
			bcm.cleanup()
		}
	}
}

// cleanup performs cleanup operations
func (bcm *batchCleanupManager) cleanup() {
	bcm.gm.batchMutex.Lock()
	now := time.Now()
	for addr, batch := range bcm.gm.batchBuffer {
		batch.mutex.Lock()
		if batch.timer != nil {
			if len(batch.ops) == 0 && time.Since(now) > 5*time.Minute {
				delete(bcm.gm.batchBuffer, addr)
			}
		}
		batch.mutex.Unlock()
	}
	bcm.gm.batchMutex.Unlock()
}
