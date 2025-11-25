package gossip

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feellmoose/gridkv/internal/storage"
	"github.com/feellmoose/gridkv/internal/utils/logging"
)

type EventType int

const (
	EventTypeFailureDetection EventType = iota
	EventTypeGossipBroadcast
	EventTypeCleanup
	EventTypeReadRepair
	EventTypeStateSync
	EventTypeConnectToSeeds
	EventTypeAdaptiveBatchConfig
)

type Event struct {
	Type      EventType
	Timestamp time.Time
	Data      interface{}
	Priority  int
}

type EventHandler func(event *Event) error

type UnifiedEventLoop struct {
	mu           sync.RWMutex
	events       chan *Event
	handlers     map[EventType]EventHandler
	stopCh       chan struct{}
	stopOnce     sync.Once
	wg           sync.WaitGroup
	eventCount   atomic.Int64
	processCount atomic.Int64
}

func NewUnifiedEventLoop(bufferSize int) *UnifiedEventLoop {
	return &UnifiedEventLoop{
		events:   make(chan *Event, bufferSize),
		handlers: make(map[EventType]EventHandler),
		stopCh:   make(chan struct{}),
	}
}

func (e *UnifiedEventLoop) RegisterHandler(eventType EventType, handler EventHandler) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.handlers[eventType] = handler
}

func (e *UnifiedEventLoop) Start() {
	e.wg.Add(1)
	go e.processLoop()
}

func (e *UnifiedEventLoop) Stop() {
	e.stopOnce.Do(func() {
		close(e.stopCh)
		e.wg.Wait()
	})
}

func (e *UnifiedEventLoop) Submit(event *Event) bool {
	if event == nil {
		return false
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	select {
	case e.events <- event:
		e.eventCount.Add(1)
		return true
	default:
		if logging.Log.IsDebugEnabled() {
			logging.Debug("Event loop channel full, dropping event", "type", event.Type)
		}
		return false
	}
}

func (e *UnifiedEventLoop) processLoop() {
	defer e.wg.Done()

	for {
		select {
		case <-e.stopCh:
			return

		case event := <-e.events:
			e.processEvent(event)
		}
	}
}

func (e *UnifiedEventLoop) processEvent(event *Event) {
	e.mu.RLock()
	handler, exists := e.handlers[event.Type]
	e.mu.RUnlock()

	if !exists {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			logPanicWithStack(fmt.Sprintf("Panic in event handler (type=%v)", event.Type), r)
		}
	}()

	if err := handler(event); err != nil {
		if logging.Log.IsDebugEnabled() {
			logging.Debug("Event handler error", "type", event.Type, "error", err)
		}
	}

	e.processCount.Add(1)
}

type EventScheduler struct {
	mu        sync.Mutex
	schedules map[EventType]*time.Timer
	eventLoop *UnifiedEventLoop
	intervals map[EventType]time.Duration
}

func NewEventScheduler(eventLoop *UnifiedEventLoop) *EventScheduler {
	return &EventScheduler{
		schedules: make(map[EventType]*time.Timer),
		eventLoop: eventLoop,
		intervals: make(map[EventType]time.Duration),
	}
}

func (s *EventScheduler) Schedule(eventType EventType, interval time.Duration, eventData interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if timer, exists := s.schedules[eventType]; exists {
		timer.Stop()
	}

	s.intervals[eventType] = interval

	timer := time.AfterFunc(interval, func() {
		event := &Event{
			Type:      eventType,
			Timestamp: time.Now(),
			Data:      eventData,
		}
		s.eventLoop.Submit(event)

		s.mu.Lock()
		if interval := s.intervals[eventType]; interval > 0 {
			s.schedules[eventType] = time.AfterFunc(interval, func() {
				s.Schedule(eventType, interval, eventData)
			})
		}
		s.mu.Unlock()
	})

	s.schedules[eventType] = timer
}

func (s *EventScheduler) Cancel(eventType EventType) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if timer, exists := s.schedules[eventType]; exists {
		timer.Stop()
		delete(s.schedules, eventType)
		delete(s.intervals, eventType)
	}
}

func (s *EventScheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, timer := range s.schedules {
		if timer != nil {
			timer.Stop()
		}
	}
	s.schedules = make(map[EventType]*time.Timer)
	s.intervals = make(map[EventType]time.Duration)
}

type BatchedGossipBroadcast struct {
	mu          sync.Mutex
	targets     map[string]bool
	batchTimer  *time.Timer
	timerDone   chan struct{} // Channel to signal timer goroutine to stop
	manager     *GossipManager
	batchWindow time.Duration
	maxTargets  int
}

func NewBatchedGossipBroadcast(manager *GossipManager, batchWindow time.Duration, maxTargets int) *BatchedGossipBroadcast {
	return &BatchedGossipBroadcast{
		targets:     make(map[string]bool),
		manager:     manager,
		batchWindow: batchWindow,
		maxTargets:  maxTargets,
	}
}

func (b *BatchedGossipBroadcast) AddTarget(target string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.targets[target] = true

	if len(b.targets) >= b.maxTargets {
		if b.batchTimer != nil {
			b.batchTimer.Stop()
			if b.timerDone != nil {
				close(b.timerDone)
				b.timerDone = nil
			}
			b.batchTimer = nil
		}
		b.flushLocked()
		return
	}

	if b.batchTimer == nil {
		timer := time.NewTimer(b.batchWindow)
		timerDone := make(chan struct{})
		b.batchTimer = timer
		b.timerDone = timerDone
		go func() {
			select {
			case <-timer.C:
				b.mu.Lock()
				if b.batchTimer == timer {
					b.batchTimer = nil
					b.timerDone = nil
				}
				b.flushLocked()
				b.mu.Unlock()
			case <-timerDone:
				// Timer was stopped, exit goroutine
				return
			}
		}()
	}
}

func (b *BatchedGossipBroadcast) flushLocked() {
	if len(b.targets) == 0 {
		return
	}

	targets := make([]string, 0, len(b.targets))
	for target := range b.targets {
		targets = append(targets, target)
	}
	b.targets = make(map[string]bool)

	if b.batchTimer != nil {
		b.batchTimer.Stop()
		b.batchTimer = nil
	}

	b.manager.mu.RLock()
	membersPtr := nodeInfoSlicePool.Get().(*[]*NodeInfo)
	members := (*membersPtr)[:0]

	for _, n := range b.manager.liveNodes {
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
	b.manager.mu.RUnlock()

	syncMsg := &GossipMessage{
		Type:   CLUSTER_SYNC,
		Sender: b.manager.localNodeID,
		Payload: &GossipMessage_ClusterSyncPayload{
			ClusterSyncPayload: &ClusterSyncPayload{Nodes: members},
		},
	}
	b.manager.signMessageCanonical(syncMsg)

	for _, target := range targets {
		targetCopy := target
		b.manager.replicationPool.Submit(func() {
			if peer, ok := b.manager.getNode(targetCopy); ok && peer != nil && b.manager.network != nil {
				b.manager.network.SendWithTimeout(peer.Address, syncMsg, 300*time.Millisecond)
			}
		})
	}

	nodeInfoSlicePool.Put(membersPtr)
}

func (b *BatchedGossipBroadcast) Flush() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.flushLocked()
}

type BatchedReadRepair struct {
	mu          sync.Mutex
	repairs     map[string]*ReadRepairOp
	batchTimer  *time.Timer
	timerDone   chan struct{} // Channel to signal timer goroutine to stop
	manager     *GossipManager
	batchWindow time.Duration
	maxRepairs  int
}

type ReadRepairOp struct {
	Key       string
	Item      *storage.StoredItem
	TargetIDs []string
	Timestamp time.Time
}

func NewBatchedReadRepair(manager *GossipManager, batchWindow time.Duration, maxRepairs int) *BatchedReadRepair {
	return &BatchedReadRepair{
		repairs:     make(map[string]*ReadRepairOp),
		manager:     manager,
		batchWindow: batchWindow,
		maxRepairs:  maxRepairs,
	}
}

func (b *BatchedReadRepair) AddRepair(key string, item *storage.StoredItem, targetIDs []string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.repairs[key] = &ReadRepairOp{
		Key:       key,
		Item:      item,
		TargetIDs: targetIDs,
		Timestamp: time.Now(),
	}

	if len(b.repairs) >= b.maxRepairs {
		if b.batchTimer != nil {
			b.batchTimer.Stop()
			if b.timerDone != nil {
				close(b.timerDone)
				b.timerDone = nil
			}
			b.batchTimer = nil
		}
		b.flushLocked()
		return
	}

	if b.batchTimer == nil {
		timer := time.NewTimer(b.batchWindow)
		timerDone := make(chan struct{})
		b.batchTimer = timer
		b.timerDone = timerDone
		go func() {
			select {
			case <-timer.C:
				b.mu.Lock()
				if b.batchTimer == timer {
					b.batchTimer = nil
					b.timerDone = nil
				}
				b.flushLocked()
				b.mu.Unlock()
			case <-timerDone:
				// Timer was stopped, exit goroutine
				return
			}
		}()
	}
}

func (b *BatchedReadRepair) flushLocked() {
	if len(b.repairs) == 0 {
		return
	}

	repairs := make([]*ReadRepairOp, 0, len(b.repairs))
	for _, repair := range b.repairs {
		repairs = append(repairs, repair)
	}
	b.repairs = make(map[string]*ReadRepairOp)

	if b.batchTimer != nil {
		b.batchTimer.Stop()
		if b.timerDone != nil {
			close(b.timerDone)
			b.timerDone = nil
		}
		b.batchTimer = nil
	}

	targetToRepairs := make(map[string][]*ReadRepairOp)
	for _, repair := range repairs {
		for _, targetID := range repair.TargetIDs {
			targetToRepairs[targetID] = append(targetToRepairs[targetID], repair)
		}
	}

	for targetID, targetRepairs := range targetToRepairs {
		targetIDCopy := targetID
		repairsCopy := make([]*ReadRepairOp, len(targetRepairs))
		copy(repairsCopy, targetRepairs)

		b.manager.replicationPool.Submit(func() {
			peer, ok := b.manager.getNode(targetIDCopy)
			if !ok || peer == nil || b.manager.network == nil {
				return
			}

			opsMap := make(map[string]*CacheSyncOperation)
			for _, repair := range repairsCopy {
				setData := storageItemToProto(repair.Item)
				op := &CacheSyncOperation{
					Key:           repair.Key,
					ClientVersion: repair.Item.Version,
					Type:          OperationType_OP_SET,
					SetData:       setData,
					DataPayload: &CacheSyncOperation_SetData{
						SetData: setData,
					},
				}
				opsMap[repair.Key] = op
			}

			if len(opsMap) == 0 {
				return
			}

			ops := make([]*CacheSyncOperation, 0, len(opsMap))
			for _, op := range opsMap {
				ops = append(ops, op)
			}

			msg := getGossipMessage()
			msg.Type = GossipMessageType_MESSAGE_TYPE_CACHE_SYNC
			msg.Sender = b.manager.localNodeID
			msg.Hlc = b.manager.hlc.Now()
			msg.Payload = &GossipMessage_CacheSyncPayload{
				CacheSyncPayload: &SyncMessage{
					SyncType: &SyncMessage_IncrementalSync{
						IncrementalSync: &IncrementalSyncPayload{
							Operations: ops,
						},
					},
				},
			}
			b.manager.signMessageCanonical(msg)
			_ = b.manager.network.SendWithTimeout(peer.Address, msg, 500*time.Millisecond)
			putGossipMessage(msg)
		})
	}
}

func (b *BatchedReadRepair) Flush() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.flushLocked()
}

func (gm *GossipManager) registerEventHandlers() {
	if gm.eventLoop == nil {
		return
	}

	gm.eventLoop.RegisterHandler(EventTypeFailureDetection, func(event *Event) error {
		defer func() {
			if r := recover(); r != nil {
				logPanicWithStack("Panic in failure detection", r)
			}
		}()
		gm.runFailureDetection()
		return nil
	})

	gm.eventLoop.RegisterHandler(EventTypeGossipBroadcast, func(event *Event) error {
		defer func() {
			if r := recover(); r != nil {
				logPanicWithStack("Panic in gossip broadcast", r)
			}
		}()
		gm.gossipPeriodically()
		return nil
	})

	gm.eventLoop.RegisterHandler(EventTypeCleanup, func(event *Event) error {
		gm.cleanupStaleData()
		return nil
	})

	if gm.eventScheduler != nil {
		gm.eventScheduler.Schedule(EventTypeFailureDetection, gm.failureTimeout/4, nil)
		gm.eventScheduler.Schedule(EventTypeGossipBroadcast, gm.gossipInterval, nil)
		gm.eventScheduler.Schedule(EventTypeCleanup, 200*time.Millisecond, nil)
	}
}

func (gm *GossipManager) cleanupStaleData() {
	// Aggressively clean up stale pending reads to prevent accumulation
	// Clean up pending reads that are older than timeout threshold (likely timed out)
	// Use 2x readTimeout for cleanup (allows time for batch processing)
	timeoutThreshold := gm.readTimeout * 2
	if timeoutThreshold < 4*time.Second {
		timeoutThreshold = 4 * time.Second // Minimum threshold for batch read cleanup
	}
	if timeoutThreshold > 10*time.Second {
		timeoutThreshold = 10 * time.Second // Maximum threshold (reduced from 20s to 10s) for faster cleanup
	}

	now := time.Now()
	cleaned := 0
	gm.pendingReads.Range(func(key, value interface{}) bool {
		entry, ok := value.(*pendingReadEntry)
		if !ok || entry == nil {
			// Invalid entry, remove it
			if strKey, ok := key.(string); ok {
				gm.removePendingRead(strKey)
				// Try to recycle channel if possible
				if entry != nil && entry.ch != nil {
					putReadResponseChannel(entry.ch)
				}
			}
			cleaned++
			return true
		}

		// Check if entry is too old (timed out)
		age := now.Sub(entry.createdAt)
		if age > timeoutThreshold {
			// Entry is too old, remove it and recycle channel
			if strKey, ok := key.(string); ok {
				gm.removePendingRead(strKey)
				// Try to drain channel before recycling to prevent goroutine leaks
				select {
				case <-entry.ch:
					// Drained stale response
				default:
					// Channel empty - safe to recycle
				}
				putReadResponseChannel(entry.ch)
			}
			cleaned++
			return true
		}

		// Skip draining for entries that are still fresh
		// Only try to drain if entry is close to timeout (within 500ms)
		if age > timeoutThreshold-500*time.Millisecond {
			// Try non-blocking read to drain stale responses
			select {
			case <-entry.ch:
				// Successfully drained, remove from map and recycle channel
				if strKey, ok := key.(string); ok {
					gm.removePendingRead(strKey)
					putReadResponseChannel(entry.ch)
				}
				cleaned++
			default:
				// Channel empty - entry is still waiting for response
			}
		}
		return true
	})
	if cleaned > 0 && logging.Log.IsDebugEnabled() {
		logging.Debug("Cleaned up stale pending reads", "count", cleaned, "threshold", timeoutThreshold)
	}

	gm.connectingNodes.Range(func(key, value interface{}) bool {
		if state, ok := value.(*connectingState); ok {
			state.mu.Lock()
			elapsed := time.Since(state.lastAttempt)
			state.mu.Unlock()

			if elapsed > 5*time.Second {
				gm.connectingNodes.Delete(key)
			}
		}
		return true
	})
}
