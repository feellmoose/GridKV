package gossip

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type UnifiedPipeline struct {
	target     string
	ops        []*CacheSyncOperation
	opsMu      sync.Mutex
	flushSize  int
	flushTick  time.Duration
	pendingOps atomic.Int64
	manager    *GossipManager
	stopCh     chan struct{}
	useBinary  bool
	wg         sync.WaitGroup // WaitGroup to ensure goroutine exits
	stopped    atomic.Bool    // Atomic flag to prevent double stop
}

func NewUnifiedPipeline(target string, manager *GossipManager, useBinary bool) *UnifiedPipeline {
	flushSize, flushTick := manager.getBatchConfig(BatchRoleWrite)
	up := &UnifiedPipeline{
		target:    target,
		ops:       make([]*CacheSyncOperation, 0, flushSize),
		flushSize: flushSize,
		flushTick: flushTick,
		manager:   manager,
		stopCh:    make(chan struct{}),
		useBinary: useBinary,
	}
	up.wg.Add(1)
	go up.run()
	return up
}

func (up *UnifiedPipeline) Add(op *CacheSyncOperation) {
	if op == nil {
		return
	}

	up.opsMu.Lock()
	up.ops = append(up.ops, op)
	count := len(up.ops)
	shouldFlush := count >= up.flushSize
	up.opsMu.Unlock()

	up.pendingOps.Add(1)

	if shouldFlush {
		up.flush()
	}
}

func (up *UnifiedPipeline) flush() {
	up.opsMu.Lock()
	if len(up.ops) == 0 {
		up.opsMu.Unlock()
		return
	}

	// Optimized: reuse slice directly to avoid copy when possible
	// Only copy if we need to keep the original slice
	ops := up.ops
	up.ops = make([]*CacheSyncOperation, 0, up.flushSize) // Pre-allocate for next batch

	// Shrink if capacity is too large (memory optimization)
	if cap(ops) > up.flushSize*8 {
		// Create new slice with reasonable capacity to free memory
		newOps := make([]*CacheSyncOperation, len(ops))
		copy(newOps, ops)
		ops = newOps
	}
	up.opsMu.Unlock()

	up.pendingOps.Add(-int64(len(ops)))
	up.sendBatch(ops)
}

// Pending returns the number of buffered operations waiting to flush.
func (up *UnifiedPipeline) Pending() int64 {
	if up == nil {
		return 0
	}
	return up.pendingOps.Load()
}

func (up *UnifiedPipeline) run() {
	defer up.wg.Done()
	interval := up.getAdaptiveFlushInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-up.stopCh:
			up.flush()
			return
		case <-ticker.C:
			up.opsMu.Lock()
			count := len(up.ops)
			up.opsMu.Unlock()
			if count > 0 {
				up.flush()
			}
			// Update interval based on current load
			newInterval := up.getAdaptiveFlushInterval()
			if newInterval != interval {
				interval = newInterval
				ticker.Reset(interval)
			}
		}
	}
}

func (up *UnifiedPipeline) getAdaptiveFlushInterval() time.Duration {
	pending := up.pendingOps.Load()
	// Optimized intervals for better throughput
	if pending >= int64(up.flushSize) {
		return 10 * time.Microsecond // More aggressive for high load
	}
	if pending > int64(up.flushSize*3/4) {
		return 25 * time.Microsecond
	}
	if pending > int64(up.flushSize/2) {
		return 50 * time.Microsecond
	}
	if pending > 10 {
		return 100 * time.Microsecond
	}
	if pending > 0 {
		return 250 * time.Microsecond // Reduced from 500us for faster response
	}
	return up.flushTick
}

func (up *UnifiedPipeline) sendBatch(ops []*CacheSyncOperation) {
	if len(ops) == 0 {
		return
	}

	if up.useBinary {
		// Check message size and split if needed to avoid TCP limit
		payload, _ := encodeCacheSyncPayload(ops)
		estimatedSize := len(payload) + 64 // Add overhead for binary message header

		if estimatedSize > maxSerializedMessageSize {
			// Split operations into smaller batches
			splitSize := len(ops) / 2
			if splitSize == 0 {
				splitSize = 1
			}
			for i := 0; i < len(ops); i += splitSize {
				end := i + splitSize
				if end > len(ops) {
					end = len(ops)
				}
				batch := ops[i:end]
				up.sendBatch(batch) // Recursive call with smaller batch
			}
			return
		}

		msg := GetBinaryMessage()
		msg.Type = BinaryMsgTypeCacheSync
		senderBytes := []byte(up.manager.localNodeID)
		copy(msg.Sender[:], senderBytes)
		if len(senderBytes) < 16 {
			for i := len(senderBytes); i < 16; i++ {
				msg.Sender[i] = 0
			}
		}
		msg.Payload = payload
		data := msg.Marshal()
		PutBinaryMessage(msg)

		ctx, cancel := context.WithTimeout(context.Background(), up.manager.replicationTimeout)
		err := up.manager.network.SendRaw(ctx, up.target, data)
		cancel()
		if err != nil {
			// Check if error is due to message size or connection reset
			errStr := err.Error()
			isSizeError := len(data) > maxSerializedMessageSize ||
				strings.Contains(errStr, "message too large") ||
				strings.Contains(errStr, "connection reset")

			if isSizeError && len(ops) > 1 {
				// Split and retry if it's a size-related error
				splitSize := len(ops) / 2
				if splitSize == 0 {
					splitSize = 1
				}
				for i := 0; i < len(ops); i += splitSize {
					end := i + splitSize
					if end > len(ops) {
						end = len(ops)
					}
					up.sendBatch(ops[i:end]) // Recursive call with smaller batch
				}
				return
			}

			if up.manager.metrics != nil {
				up.manager.metrics.IncrementReplicationFailures()
			}
			up.requeueOps(ops)
			return
		}
		if up.manager.metrics != nil {
			up.manager.metrics.IncrementReplicationSuccess()
		}
	} else {
		// Build message and check actual serialized size before sending
		msg := getGossipMessage()
		msg.Type = GossipMessageType_MESSAGE_TYPE_CACHE_SYNC
		msg.Sender = up.manager.localNodeID
		msg.Hlc = up.manager.hlc.Now()
		msg.Payload = &GossipMessage_CacheSyncPayload{
			CacheSyncPayload: &SyncMessage{
				SyncType: &SyncMessage_IncrementalSync{
					IncrementalSync: &IncrementalSyncPayload{Operations: ops},
				},
			},
		}
		up.manager.signMessageCanonical(msg)

		// Check actual serialized size (gossip messages convert to binary internally)
		binary := convertGossipMessageToBinary(msg)
		if binary != nil {
			data := binary.Marshal()
			PutBinaryMessage(binary)

			const maxMessageSize = 10 * 1024 * 1024 // 10MB max
			if len(data) > maxMessageSize {
				putGossipMessage(msg)
				// Split operations into smaller batches
				splitSize := len(ops) / 2
				if splitSize == 0 {
					splitSize = 1
				}
				for i := 0; i < len(ops); i += splitSize {
					end := i + splitSize
					if end > len(ops) {
						end = len(ops)
					}
					up.sendBatch(ops[i:end]) // Recursive call with smaller batch
				}
				return
			}
		}

		timeout := up.manager.replicationTimeout
		if len(ops) > 50000 {
			timeout = timeout * 3
		}
		err := up.manager.network.SendWithTimeout(up.target, msg, timeout)
		if up.manager.metrics != nil {
			if err == nil {
				up.manager.metrics.IncrementReplicationSuccess()
			} else {
				up.manager.metrics.IncrementReplicationFailures()
			}
		}
		if err != nil {
			// Check if error is due to message size
			errStr := err.Error()
			isSizeError := strings.Contains(errStr, "message too large")

			if isSizeError && len(ops) > 1 {
				// Split and retry if it's a size-related error
				splitSize := len(ops) / 2
				if splitSize == 0 {
					splitSize = 1
				}
				for i := 0; i < len(ops); i += splitSize {
					end := i + splitSize
					if end > len(ops) {
						end = len(ops)
					}
					up.sendBatch(ops[i:end]) // Recursive call with smaller batch
				}
				putGossipMessage(msg)
				return
			}

			up.requeueOps(ops)
			putGossipMessage(msg)
			return
		}
		putGossipMessage(msg)
	}
}

func (up *UnifiedPipeline) sendDirect(op *CacheSyncOperation) {
	if op == nil {
		return
	}

	if up.useBinary {
		msg := GetBinaryMessage()
		msg.Type = BinaryMsgTypeCacheSync
		senderBytes := []byte(up.manager.localNodeID)
		copy(msg.Sender[:], senderBytes)
		if len(senderBytes) < 16 {
			for i := len(senderBytes); i < 16; i++ {
				msg.Sender[i] = 0
			}
		}
		payload, _ := encodeCacheSyncPayload([]*CacheSyncOperation{op})
		msg.Payload = payload
		data := msg.Marshal()
		PutBinaryMessage(msg)

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_ = up.manager.network.SendRaw(ctx, up.target, data)
		cancel()
	} else {
		msg := getGossipMessage()
		msg.Type = GossipMessageType_MESSAGE_TYPE_CACHE_SYNC
		msg.Sender = up.manager.localNodeID
		msg.Hlc = up.manager.hlc.Now()
		msg.Payload = &GossipMessage_CacheSyncPayload{
			CacheSyncPayload: &SyncMessage{
				SyncType: &SyncMessage_IncrementalSync{
					IncrementalSync: &IncrementalSyncPayload{
						Operations: []*CacheSyncOperation{op},
					},
				},
			},
		}
		up.manager.signMessageCanonical(msg)
		_ = up.manager.network.SendWithTimeout(up.target, msg, 100*time.Millisecond)
		putGossipMessage(msg)
	}
}

func (up *UnifiedPipeline) requeueOps(ops []*CacheSyncOperation) {
	if up == nil || len(ops) == 0 || up.manager == nil || up.stopped.Load() {
		return
	}
	if !up.manager.isNodeLocallyAlive(up.target) {
		return
	}
	up.opsMu.Lock()
	up.ops = append(ops, up.ops...)
	up.opsMu.Unlock()
	up.pendingOps.Add(int64(len(ops)))
	go func() {
		time.Sleep(5 * time.Millisecond)
		up.flush()
	}()
}

func (up *UnifiedPipeline) Stop() {
	// Prevent double stop
	if !up.stopped.CompareAndSwap(false, true) {
		return
	}
	close(up.stopCh)
	// Wait for goroutine to exit with timeout
	done := make(chan struct{})
	go func() {
		up.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		// Goroutine exited successfully
	case <-time.After(2 * time.Second):
		// Timeout - continue anyway to prevent blocking
	}
}
