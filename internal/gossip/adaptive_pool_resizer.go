package gossip

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/feellmoose/gridkv/internal/utils/logging"
	"github.com/feellmoose/gridkv/internal/utils/workerpool"
)

// adaptivePoolResizer automatically adjusts pool size based on runtime metrics
type adaptivePoolResizer struct {
	mu sync.RWMutex

	pool        *workerpool.Pool
	poolName    string
	currentSize int
	minSize     int
	maxSize     int

	// Thresholds for scaling decisions
	highQueueThreshold    float64 // Queue utilization > this triggers scale-up (default: 0.8)
	lowQueueThreshold     float64 // Queue utilization < this triggers scale-down (default: 0.2)
	highDropRateThreshold float64 // Drop rate > this triggers scale-up (default: 0.05 = 5%)
	scaleUpFactor         float64 // Scale up by this factor (default: 1.5x)
	scaleDownFactor       float64 // Scale down by this factor (default: 0.75x)

	// State tracking
	lastResizeTime atomic.Int64  // Unix nano timestamp
	resizeCooldown time.Duration // Minimum time between resizes (default: 10s)

	// Metrics history for trend analysis
	queueUtilHistory []float64 // Last N queue utilization samples
	dropRateHistory  []float64 // Last N drop rate samples
	maxHistorySize   int

	enabled bool
}

// newAdaptivePoolResizer creates a new adaptive resizer for a pool
func newAdaptivePoolResizer(pool *workerpool.Pool, poolName string, initialSize, minSize, maxSize int) *adaptivePoolResizer {
	if minSize <= 0 {
		minSize = initialSize / 4
		if minSize < 8 {
			minSize = 8
		}
	}
	if maxSize <= 0 {
		maxSize = initialSize * 4
	}

	return &adaptivePoolResizer{
		pool:                  pool,
		poolName:              poolName,
		currentSize:           initialSize,
		minSize:               minSize,
		maxSize:               maxSize,
		highQueueThreshold:    0.6,             // 60% queue utilization triggers scale-up (more responsive)
		lowQueueThreshold:     0.1,             // 10% queue utilization triggers scale-down (smoother, less aggressive)
		highDropRateThreshold: 0.02,            // 2% drop rate triggers scale-up (more sensitive)
		scaleUpFactor:         1.5,             // Scale up by 50% (faster response to high load)
		scaleDownFactor:       0.9,             // Scale down to 90% (smoother, less aggressive - only 10% reduction)
		resizeCooldown:        5 * time.Second, // Faster response (reduced from 10s)
		maxHistorySize:        10,
		enabled:               true,
	}
}

// emergencyResize performs immediate resize when pool is full (bypasses cooldown)
func (apr *adaptivePoolResizer) emergencyResize() {
	if !apr.enabled || apr.pool == nil {
		return
	}

	stats := apr.pool.Stats()
	if stats.Capacity == 0 {
		return
	}

	// Check if pool is actually under pressure
	// If Submit() failed, pool is likely full, so always try to resize
	// But check metrics to avoid unnecessary resizes
	queueUtil := float64(stats.QueueLen) / float64(stats.Capacity)
	var dropRate float64
	if stats.Submitted > 0 {
		dropRate = float64(stats.Dropped) / float64(stats.Submitted)
	}

	// Since emergencyResize is called when Submit() failed, pool is likely full
	// Check if already at max - can't resize more
	apr.mu.Lock()
	if apr.currentSize >= apr.maxSize {
		apr.mu.Unlock()
		return // Already at max, can't resize more
	}
	apr.mu.Unlock()

	apr.mu.Lock()
	defer apr.mu.Unlock()

	// Emergency scale-up: increase by 50% immediately, or double if close to max
	scaleUp := int(float64(apr.currentSize) * 0.5)
	if scaleUp < 4 {
		scaleUp = 4 // Minimum emergency scale-up
	}

	// If we're close to max (within 20%), be more aggressive
	utilization := float64(apr.currentSize) / float64(apr.maxSize)
	if utilization > 0.8 {
		// Close to max, try to double instead of 50%
		scaleUp = apr.currentSize
		if scaleUp > apr.maxSize-apr.currentSize {
			scaleUp = apr.maxSize - apr.currentSize
		}
	}

	newSize := apr.currentSize + scaleUp
	if newSize > apr.maxSize {
		newSize = apr.maxSize
	}

	if newSize == apr.currentSize {
		return
	}

	// Perform resize immediately (bypass cooldown)
	if err := apr.pool.Resize(newSize); err != nil {
		return
	}

	oldSize := apr.currentSize
	apr.currentSize = newSize
	apr.lastResizeTime.Store(time.Now().UnixNano())

	if logging.Log.IsDebugEnabled() {
		logging.Debug("Emergency pool resize",
			"pool", apr.poolName,
			"oldSize", oldSize,
			"newSize", newSize,
			"queueUtil", queueUtil,
			"dropRate", dropRate)
	}
}

// checkAndResize checks current pool metrics and adjusts size if needed
func (apr *adaptivePoolResizer) checkAndResize() {
	if !apr.enabled || apr.pool == nil {
		return
	}

	// Check cooldown (but allow faster scale-up for high load)
	now := time.Now()
	lastResize := time.Unix(0, apr.lastResizeTime.Load())
	elapsed := now.Sub(lastResize)

	stats := apr.pool.Stats()
	if stats.Capacity == 0 {
		return
	}

	// Calculate metrics first to check if we need fast cooldown
	queueUtil := float64(stats.QueueLen) / float64(stats.Capacity)
	var dropRate float64
	if stats.Submitted > 0 {
		dropRate = float64(stats.Dropped) / float64(stats.Submitted)
	}

	// Fast scale-up cooldown for high load (2s), normal for scale-down (5s)
	fastCooldown := 2 * time.Second
	if queueUtil > apr.highQueueThreshold || dropRate > apr.highDropRateThreshold {
		if elapsed < fastCooldown {
			return
		}
	} else {
		if elapsed < apr.resizeCooldown {
			return
		}
	}

	// Update history
	apr.updateHistory(queueUtil, dropRate)

	// Make scaling decision
	decision := apr.makeScalingDecision(stats, queueUtil, dropRate)
	if decision == 0 {
		return
	}

	apr.mu.Lock()
	defer apr.mu.Unlock()

	newSize := apr.currentSize + decision
	if newSize < apr.minSize {
		newSize = apr.minSize
	}
	if newSize > apr.maxSize {
		newSize = apr.maxSize
	}

	if newSize == apr.currentSize {
		return
	}

	// Perform resize
	if err := apr.pool.Resize(newSize); err != nil {
		logging.Warn("Adaptive resize failed",
			"pool", apr.poolName,
			"oldSize", apr.currentSize,
			"newSize", newSize,
			"error", err)
		return
	}

	oldSize := apr.currentSize
	apr.currentSize = newSize
	apr.lastResizeTime.Store(now.UnixNano())

	logging.Info("Adaptive pool resize",
		"pool", apr.poolName,
		"oldSize", oldSize,
		"newSize", newSize,
		"queueUtil", queueUtil,
		"dropRate", dropRate,
		"running", stats.Running)
}

// makeScalingDecision determines if and how much to scale
// Returns: positive for scale-up, negative for scale-down, 0 for no change
func (apr *adaptivePoolResizer) makeScalingDecision(stats workerpool.Stats, queueUtil, dropRate float64) int {
	apr.mu.RLock()
	defer apr.mu.RUnlock()

	// Scale-up conditions: fast and aggressive response to high load
	if queueUtil > apr.highQueueThreshold || dropRate > apr.highDropRateThreshold {
		// Immediate scale-up for high drop rate
		if dropRate > apr.highDropRateThreshold {
			// More aggressive scale-up for drop rate
			scaleUp := int(float64(apr.currentSize) * (apr.scaleUpFactor - 1.0))
			if scaleUp < 4 {
				scaleUp = 4 // Minimum scale-up of 4 workers for drop rate
			}
			// Cap at 2x for emergency (prevent explosion)
			maxScaleUp := apr.currentSize
			if scaleUp > maxScaleUp {
				scaleUp = maxScaleUp
			}
			return scaleUp
		}
		// Fast scale-up for high queue utilization (no need to wait for trend)
		if queueUtil > apr.highQueueThreshold {
			scaleUp := int(float64(apr.currentSize) * (apr.scaleUpFactor - 1.0))
			if scaleUp < 2 {
				scaleUp = 2 // Minimum scale-up of 2 workers
			}
			return scaleUp
		}
		// Check trend: consistent high utilization (backup check)
		if apr.hasConsistentHighUtilization() {
			scaleUp := int(float64(apr.currentSize) * (apr.scaleUpFactor - 1.0))
			if scaleUp < 1 {
				scaleUp = 1
			}
			return scaleUp
		}
	}

	// Scale-down conditions: smooth and conservative (only when truly idle)
	// Require sustained low utilization to prevent oscillation
	if queueUtil < apr.lowQueueThreshold && dropRate < 0.01 {
		// More conservative: require consistent low utilization (smoother)
		if apr.hasConsistentLowUtilization() {
			// Smooth scale-down: only reduce by 10% (scaleDownFactor = 0.9)
			scaleDown := int(float64(apr.currentSize) * (1.0 - apr.scaleDownFactor))
			if scaleDown < 1 {
				scaleDown = 1
			}
			// Don't scale down if we're already close to minSize
			// This prevents unnecessary churn
			if apr.currentSize-scaleDown < apr.minSize*2 {
				// Too close to minimum, skip this cycle
				return 0
			}
			return -scaleDown
		}
	}

	return 0
}

// updateHistory updates the metrics history
func (apr *adaptivePoolResizer) updateHistory(queueUtil, dropRate float64) {
	apr.mu.Lock()
	defer apr.mu.Unlock()

	apr.queueUtilHistory = append(apr.queueUtilHistory, queueUtil)
	if len(apr.queueUtilHistory) > apr.maxHistorySize {
		apr.queueUtilHistory = apr.queueUtilHistory[1:]
	}

	apr.dropRateHistory = append(apr.dropRateHistory, dropRate)
	if len(apr.dropRateHistory) > apr.maxHistorySize {
		apr.dropRateHistory = apr.dropRateHistory[1:]
	}
}

// hasConsistentHighUtilization checks if queue has been consistently high
func (apr *adaptivePoolResizer) hasConsistentHighUtilization() bool {
	if len(apr.queueUtilHistory) < 1 {
		return false
	}

	// Fast response: check last sample only (immediate response to high load)
	// This is a backup check - primary scale-up happens immediately above
	recent := apr.queueUtilHistory[len(apr.queueUtilHistory)-1:]
	count := 0
	for _, util := range recent {
		if util > apr.highQueueThreshold {
			count++
		}
	}
	// Scale up if sample is high (immediate response)
	return count >= 1
}

// hasConsistentLowUtilization checks if queue has been consistently low
func (apr *adaptivePoolResizer) hasConsistentLowUtilization() bool {
	if len(apr.queueUtilHistory) < 5 {
		return false
	}

	// Smooth scale-down: require 5 samples (25 seconds) with at least 4 low
	// This prevents oscillation and ensures truly idle before scaling down
	recent := apr.queueUtilHistory[len(apr.queueUtilHistory)-5:]
	count := 0
	for _, util := range recent {
		if util < apr.lowQueueThreshold {
			count++
		}
	}
	// Scale down only if 4 of last 5 samples are low (smooth, conservative)
	return count >= 4
}

// SetEnabled enables or disables adaptive resizing
func (apr *adaptivePoolResizer) SetEnabled(enabled bool) {
	apr.mu.Lock()
	defer apr.mu.Unlock()
	apr.enabled = enabled
}

// GetCurrentSize returns the current pool size
func (apr *adaptivePoolResizer) GetCurrentSize() int {
	apr.mu.RLock()
	defer apr.mu.RUnlock()
	return apr.currentSize
}

// UpdateLimits updates min/max size limits
func (apr *adaptivePoolResizer) UpdateLimits(minSize, maxSize int) {
	apr.mu.Lock()
	defer apr.mu.Unlock()

	if minSize > 0 {
		apr.minSize = minSize
	}
	if maxSize > 0 {
		apr.maxSize = maxSize
	}

	// Ensure current size is within limits
	if apr.currentSize < apr.minSize {
		apr.currentSize = apr.minSize
		_ = apr.pool.Resize(apr.currentSize)
	}
	if apr.currentSize > apr.maxSize {
		apr.currentSize = apr.maxSize
		_ = apr.pool.Resize(apr.currentSize)
	}
}
