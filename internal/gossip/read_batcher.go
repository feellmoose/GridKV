package gossip

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/feellmoose/gridkv/internal/metrics"
	"github.com/feellmoose/gridkv/internal/utils/logging"
)

// PendingReadRequest represents a single read request in a batch
type PendingReadRequest struct {
	Key         string
	RequestID   string
	ResponseCh  chan *ReadResponsePayload
	CreatedAt   time.Time
	Coordinator string
}

// ReadBatch collects read requests for a target coordinator
type ReadBatch struct {
	mu           sync.Mutex
	target       string
	coordinator  string
	requests     []*PendingReadRequest
	requestMap   map[string]int // key -> index in requests for deduplication
	timer        *time.Timer
	timerDone    chan struct{}
	flushSize    int
	flushWindow  time.Duration
	stopped      atomic.Bool
	sender       func(target string, batchID string, requests []*PendingReadRequest) error
	idGen        func() string
	metrics      *metrics.GridKVMetrics
	maxBatchSize atomic.Int64 // Track max batch size for metrics
}

// NewReadBatch creates a new read batch for a target
func NewReadBatch(target string, coordinator string, sender func(string, string, []*PendingReadRequest) error, idGen func() string, flushSize int, flushWindow time.Duration, m *metrics.GridKVMetrics) *ReadBatch {
	return &ReadBatch{
		target:      target,
		coordinator: coordinator,
		requests:    make([]*PendingReadRequest, 0, flushSize),
		requestMap:  make(map[string]int),
		flushSize:   flushSize,
		flushWindow: flushWindow,
		sender:      sender,
		idGen:       idGen,
		metrics:     m,
	}
}

// Add adds a read request to the batch
func (rb *ReadBatch) Add(req *PendingReadRequest) bool {
	if req == nil {
		return false
	}

	rb.mu.Lock()
	defer rb.mu.Unlock()

	if rb.stopped.Load() {
		return false
	}

	// Deduplicate by key - keep the latest request
	if idx, exists := rb.requestMap[req.Key]; exists {
		rb.requests[idx] = req
	} else {
		rb.requestMap[req.Key] = len(rb.requests)
		rb.requests = append(rb.requests, req)
	}

	shouldFlush := len(rb.requests) >= rb.flushSize

	if shouldFlush {
		rb.flushLocked()
		return true
	} else if rb.timer == nil {
		rb.startTimerLocked()
	}

	return false
}

// startTimerLocked starts the flush timer (must be called with lock held)
func (rb *ReadBatch) startTimerLocked() {
	timer := time.NewTimer(rb.flushWindow)
	timerDone := make(chan struct{})
	rb.timer = timer
	rb.timerDone = timerDone

	go func() {
		select {
		case <-timer.C:
			rb.mu.Lock()
			if rb.timer == timer && !rb.stopped.Load() {
				rb.timer = nil
				rb.timerDone = nil
				if len(rb.requests) > 0 {
					rb.flushLocked()
				}
			}
			rb.mu.Unlock()
		case <-timerDone:
			return
		}
	}()
}

// flushLocked flushes the batch (must be called with lock held)
func (rb *ReadBatch) flushLocked() {
	if len(rb.requests) == 0 {
		return
	}

	if rb.timer != nil {
		rb.timer.Stop()
		if rb.timerDone != nil {
			close(rb.timerDone)
			rb.timerDone = nil
		}
		rb.timer = nil
	}

	requests := make([]*PendingReadRequest, len(rb.requests))
	copy(requests, rb.requests)
	rb.requests = rb.requests[:0]
	for k := range rb.requestMap {
		delete(rb.requestMap, k)
	}

	rb.mu.Unlock()
	rb.flush(requests)
	rb.mu.Lock()
}

// flush sends the batch
func (rb *ReadBatch) flush(requests []*PendingReadRequest) {
	if len(requests) == 0 {
		return
	}

	batchSize := int64(len(requests))

	// Update metrics (optimized - only key indicators)
	if rb.metrics != nil {
		rb.metrics.IncrementReadBatchBatchesSent()
		rb.metrics.AddReadBatchRequestsTotal(batchSize)
	}

	batchID := rb.idGen()
	if err := rb.sender(rb.target, batchID, requests); err != nil {
		if rb.metrics != nil {
			rb.metrics.IncrementReadBatchErrors()
		}
		if logging.Log.IsDebugEnabled() {
			logging.Debug("Batch read send failed", "target", rb.target, "count", len(requests), "err", err)
		}
		// On error, send individual errors to all pending requests
		for _, req := range requests {
			select {
			case req.ResponseCh <- nil:
			default:
			}
		}
	}
}

// Flush forces immediate flush
func (rb *ReadBatch) Flush() {
	rb.mu.Lock()
	rb.flushLocked()
	rb.mu.Unlock()
}

// Stop stops the batch and cleans up
func (rb *ReadBatch) Stop() {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	rb.stopped.Store(true)

	if rb.timer != nil {
		rb.timer.Stop()
		rb.timer = nil
	}

	if rb.timerDone != nil {
		close(rb.timerDone)
		rb.timerDone = nil
	}
}

// ReadBatchManager manages read batches per target coordinator
type ReadBatchManager struct {
	mu          sync.RWMutex
	batches     map[string]*ReadBatch
	sender      func(target string, batchID string, requests []*PendingReadRequest) error
	flushSize   int
	flushWindow time.Duration
	idGen       func() string
	metrics     *metrics.GridKVMetrics
	tuner       *AdaptiveReadBatchTuner
}

// NewReadBatchManager creates a new read batch manager
func NewReadBatchManager(sender func(string, string, []*PendingReadRequest) error, idGen func() string, flushSize int, flushWindow time.Duration, m *metrics.GridKVMetrics, enableAdaptive bool) *ReadBatchManager {
	if flushSize <= 0 {
		flushSize = 200 // Increased default batch size for 1M+ QPS
	}
	if flushWindow <= 0 {
		flushWindow = 10 * time.Millisecond // Increased to 10ms for better batching
	}
	if idGen == nil {
		// Fallback ID generator using timestamp
		idGen = func() string {
			return time.Now().Format("20060102150405.000000000")
		}
	}

	rbm := &ReadBatchManager{
		batches:     make(map[string]*ReadBatch),
		sender:      sender,
		flushSize:   flushSize,
		flushWindow: flushWindow,
		idGen:       idGen,
		metrics:     m,
	}

	// Initialize adaptive tuner if enabled
	if enableAdaptive && m != nil {
		tuner := NewAdaptiveReadBatchTuner(&AdaptiveReadBatchTunerOptions{
			InitialBatchSize:   flushSize,
			InitialWindow:      flushWindow,
			MinBatchSize:       50,                    // Increased minimum for better throughput
			MaxBatchSize:       1000,                  // Increased maximum for 1M+ QPS scenarios
			MinWindow:          2 * time.Millisecond,  // Reduced for lower latency
			MaxWindow:          50 * time.Millisecond, // Reduced maximum window
			AdjustInterval:     10 * time.Second,
			TargetAvgBatchSize: 150, // Increased target for better throughput
			TargetErrorRate:    0.05,
			Metrics:            m,
		})
		rbm.tuner = tuner
		tuner.Start()
	}

	return rbm
}

// Add adds a read request to the appropriate batch
func (rbm *ReadBatchManager) Add(target string, coordinator string, req *PendingReadRequest) bool {
	// Get current batch size and window (may be adjusted by tuner)
	batchSize := rbm.flushSize
	window := rbm.flushWindow
	if rbm.tuner != nil {
		batchSize = rbm.tuner.GetBatchSize()
		window = rbm.tuner.GetWindow()
	}

	rbm.mu.Lock()
	batch, exists := rbm.batches[target]
	if !exists {
		batch = NewReadBatch(target, coordinator, rbm.sender, rbm.idGen, batchSize, window, rbm.metrics)
		rbm.batches[target] = batch
	} else {
		// Update batch parameters if tuner changed them
		// We don't update existing batches dynamically to avoid race conditions
		// New batches will use the updated parameters
	}
	rbm.mu.Unlock()

	// Update pending requests count
	if rbm.metrics != nil {
		pending := rbm.getPendingCount()
		rbm.metrics.SetReadBatchPendingRequests(pending)
	}

	return batch.Add(req)
}

// getPendingCount returns the total number of pending requests across all batches
func (rbm *ReadBatchManager) getPendingCount() int64 {
	rbm.mu.RLock()
	batches := make([]*ReadBatch, 0, len(rbm.batches))
	for _, batch := range rbm.batches {
		batches = append(batches, batch)
	}
	rbm.mu.RUnlock()

	var total int64
	for _, batch := range batches {
		batch.mu.Lock()
		total += int64(len(batch.requests))
		batch.mu.Unlock()
	}
	return total
}

// Flush flushes a specific target's batch
func (rbm *ReadBatchManager) Flush(target string) {
	rbm.mu.RLock()
	batch, exists := rbm.batches[target]
	rbm.mu.RUnlock()

	if exists {
		batch.Flush()
	}
}

// FlushAll flushes all batches
func (rbm *ReadBatchManager) FlushAll() {
	rbm.mu.RLock()
	batches := make([]*ReadBatch, 0, len(rbm.batches))
	for _, batch := range rbm.batches {
		batches = append(batches, batch)
	}
	rbm.mu.RUnlock()

	for _, batch := range batches {
		batch.Flush()
	}
}

// Remove removes a target's batch
func (rbm *ReadBatchManager) Remove(target string) {
	rbm.mu.Lock()
	if batch, exists := rbm.batches[target]; exists {
		batch.Stop()
		delete(rbm.batches, target)
	}
	rbm.mu.Unlock()
}

// Stop stops all batches
func (rbm *ReadBatchManager) Stop() {
	// Stop adaptive tuner
	if rbm.tuner != nil {
		rbm.tuner.Stop()
	}

	rbm.mu.Lock()
	defer rbm.mu.Unlock()

	for _, batch := range rbm.batches {
		batch.Stop()
	}
	rbm.batches = make(map[string]*ReadBatch)

	// Reset pending count
	if rbm.metrics != nil {
		rbm.metrics.SetReadBatchPendingRequests(0)
	}
}
