package gossip

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/feellmoose/gridkv/internal/metrics"
	"github.com/feellmoose/gridkv/internal/utils/logging"
)

// AdaptiveReadBatchTuner automatically adjusts batch size and window based on performance metrics
type AdaptiveReadBatchTuner struct {
	mu sync.RWMutex

	// Current settings
	currentBatchSize int
	currentWindow    time.Duration

	// Configuration
	minBatchSize int
	maxBatchSize int
	minWindow    time.Duration
	maxWindow    time.Duration

	// Metrics for decision making
	metrics *metrics.GridKVMetrics

	// Performance tracking
	lastAdjustTime atomic.Int64
	adjustInterval time.Duration

	// Target metrics
	targetAvgBatchSize int
	targetErrorRate    float64 // 0.0-1.0

	// State
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// AdaptiveReadBatchTunerOptions configures the adaptive tuner
type AdaptiveReadBatchTunerOptions struct {
	InitialBatchSize   int
	InitialWindow      time.Duration
	MinBatchSize       int
	MaxBatchSize       int
	MinWindow          time.Duration
	MaxWindow          time.Duration
	AdjustInterval     time.Duration
	TargetAvgBatchSize int
	TargetErrorRate    float64
	Metrics            *metrics.GridKVMetrics
}

// NewAdaptiveReadBatchTuner creates a new adaptive batch tuner
func NewAdaptiveReadBatchTuner(opts *AdaptiveReadBatchTunerOptions) *AdaptiveReadBatchTuner {
	if opts == nil {
		opts = &AdaptiveReadBatchTunerOptions{}
	}

	// Set defaults
	if opts.InitialBatchSize <= 0 {
		opts.InitialBatchSize = 50
	}
	if opts.InitialWindow <= 0 {
		opts.InitialWindow = 20 * time.Millisecond
	}
	if opts.MinBatchSize <= 0 {
		opts.MinBatchSize = 10
	}
	if opts.MaxBatchSize <= 0 {
		opts.MaxBatchSize = 500
	}
	if opts.MinWindow <= 0 {
		opts.MinWindow = 5 * time.Millisecond
	}
	if opts.MaxWindow <= 0 {
		opts.MaxWindow = 100 * time.Millisecond
	}
	if opts.AdjustInterval <= 0 {
		opts.AdjustInterval = 10 * time.Second
	}
	if opts.TargetAvgBatchSize <= 0 {
		opts.TargetAvgBatchSize = 50
	}
	if opts.TargetErrorRate <= 0 {
		opts.TargetErrorRate = 0.05 // 5% error rate target
	}

	return &AdaptiveReadBatchTuner{
		currentBatchSize:   opts.InitialBatchSize,
		currentWindow:      opts.InitialWindow,
		minBatchSize:       opts.MinBatchSize,
		maxBatchSize:       opts.MaxBatchSize,
		minWindow:          opts.MinWindow,
		maxWindow:          opts.MaxWindow,
		metrics:            opts.Metrics,
		adjustInterval:     opts.AdjustInterval,
		targetAvgBatchSize: opts.TargetAvgBatchSize,
		targetErrorRate:    opts.TargetErrorRate,
		stopCh:             make(chan struct{}),
	}
}

// Start begins adaptive adjustment
func (t *AdaptiveReadBatchTuner) Start() {
	t.wg.Add(1)
	go t.adjustmentLoop()
}

// Stop halts adaptive adjustment
func (t *AdaptiveReadBatchTuner) Stop() {
	close(t.stopCh)
	t.wg.Wait()
}

// GetBatchSize returns the current batch size
func (t *AdaptiveReadBatchTuner) GetBatchSize() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.currentBatchSize
}

// GetWindow returns the current batch window
func (t *AdaptiveReadBatchTuner) GetWindow() time.Duration {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.currentWindow
}

// adjustmentLoop periodically adjusts batch parameters
func (t *AdaptiveReadBatchTuner) adjustmentLoop() {
	defer t.wg.Done()

	ticker := time.NewTicker(t.adjustInterval)
	defer ticker.Stop()

	for {
		select {
		case <-t.stopCh:
			return
		case <-ticker.C:
			t.adjust()
		}
	}
}

// adjust performs adaptive adjustments based on metrics
func (t *AdaptiveReadBatchTuner) adjust() {
	if t.metrics == nil {
		return
	}

	// Get current metrics from exporter
	exporter := t.metrics.GetExporter()
	if exporter == nil {
		return
	}

	// Collect metrics (we need to access internal state)
	// For now, we'll use a simpler approach: adjust based on pending requests
	pending := t.getPendingRequests()
	avgBatchSize := t.getAverageBatchSize()
	errorRate := t.getErrorRate()

	// Adjust batch size based on average batch size
	newBatchSize := t.currentBatchSize
	if avgBatchSize > 0 {
		// If average batch size is consistently high, increase batch size
		if avgBatchSize >= float64(t.currentBatchSize)*0.9 {
			// Increase batch size to improve throughput (conservative: 10%)
			newBatchSize = int(float64(t.currentBatchSize) * 1.1)
		} else if avgBatchSize < float64(t.currentBatchSize)*0.6 {
			// Average is much lower - decrease batch size to reduce latency
			newBatchSize = int(float64(t.currentBatchSize) * 0.9)
		}
	} else {
		// No data yet - use target as guide
		if t.currentBatchSize < t.targetAvgBatchSize {
			newBatchSize = int(float64(t.currentBatchSize) * 1.05) // Gradual increase
		}
	}

	// Adjust window based on pending requests
	newWindow := t.currentWindow
	if pending > int64(t.currentBatchSize*2) {
		// High pending - reduce window to flush more frequently
		newWindow = time.Duration(float64(t.currentWindow) * 0.8)
	} else if pending < int64(t.currentBatchSize/2) {
		// Low pending - increase window to accumulate larger batches
		newWindow = time.Duration(float64(t.currentWindow) * 1.2)
	}

	// Adjust based on error rate
	if errorRate > t.targetErrorRate*2 {
		// High error rate - reduce batch size and increase window
		newBatchSize = int(float64(newBatchSize) * 0.9)
		newWindow = time.Duration(float64(newWindow) * 1.1)
	} else if errorRate < t.targetErrorRate/2 {
		// Low error rate - can be more aggressive
		newBatchSize = int(float64(newBatchSize) * 1.1)
	}

	// Apply bounds
	if newBatchSize < t.minBatchSize {
		newBatchSize = t.minBatchSize
	}
	if newBatchSize > t.maxBatchSize {
		newBatchSize = t.maxBatchSize
	}
	if newWindow < t.minWindow {
		newWindow = t.minWindow
	}
	if newWindow > t.maxWindow {
		newWindow = t.maxWindow
	}

	// Update if changed
	t.mu.Lock()
	changed := false
	if newBatchSize != t.currentBatchSize {
		t.currentBatchSize = newBatchSize
		changed = true
	}
	if newWindow != t.currentWindow {
		t.currentWindow = newWindow
		changed = true
	}
	t.mu.Unlock()

	if changed {
		t.lastAdjustTime.Store(time.Now().UnixNano())
		if logging.Log.IsDebugEnabled() {
			logging.Debug("Adaptive batch tuning",
				"batchSize", newBatchSize,
				"window", newWindow,
				"pending", pending,
				"avgBatchSize", avgBatchSize,
				"errorRate", errorRate)
		}
	}
}

// getPendingRequests gets current pending requests count from metrics
func (t *AdaptiveReadBatchTuner) getPendingRequests() int64 {
	if t.metrics == nil {
		return 0
	}
	exporter := t.metrics.GetExporter()
	if exporter == nil {
		return 0
	}
	// Get gauge value for pending requests
	return exporter.GetGauge("read_batch_pending_requests")
}

// getAverageBatchSize gets average batch size from metrics
func (t *AdaptiveReadBatchTuner) getAverageBatchSize() float64 {
	if t.metrics == nil {
		return 0
	}
	exporter := t.metrics.GetExporter()
	if exporter == nil {
		return 0
	}
	// Get average batch size from gauge
	avgSize := exporter.GetGauge("read_batch_size_avg")
	if avgSize > 0 {
		return float64(avgSize)
	}
	// Fallback: calculate from max batch size (rough estimate)
	maxSize := exporter.GetGauge("read_batch_size_max")
	if maxSize > 0 {
		return float64(maxSize) * 0.7 // Assume average is ~70% of max
	}
	return 0
}

// getErrorRate gets current error rate from metrics
func (t *AdaptiveReadBatchTuner) getErrorRate() float64 {
	if t.metrics == nil {
		return 0
	}
	exporter := t.metrics.GetExporter()
	if exporter == nil {
		return 0
	}
	// Get error count and total requests
	errors := exporter.GetCounter("read_batch_errors")
	total := exporter.GetCounter("read_batch_requests_total")
	if total > 0 {
		return float64(errors) / float64(total)
	}
	return 0
}

// SetBatchSize manually sets batch size (for testing or manual override)
func (t *AdaptiveReadBatchTuner) SetBatchSize(size int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if size >= t.minBatchSize && size <= t.maxBatchSize {
		t.currentBatchSize = size
	}
}

// SetWindow manually sets window (for testing or manual override)
func (t *AdaptiveReadBatchTuner) SetWindow(window time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if window >= t.minWindow && window <= t.maxWindow {
		t.currentWindow = window
	}
}
