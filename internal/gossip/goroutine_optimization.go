package gossip

import (
	"context"
	"runtime"
	"sync"
	"time"

	"github.com/feellmoose/gridkv/internal/utils/logging"
	"github.com/feellmoose/gridkv/internal/utils/workerpool"
)

// submitWithResize submits a task to pool with adaptive resize retry (no fallback goroutines)
// Priority: pool -> resize -> retry (no fallback to prevent goroutine leaks)
// This is a unified helper to avoid code duplication
func (gm *GossipManager) submitWithResize(task func(), pool *workerpool.Pool, resizer *adaptivePoolResizer, poolName string) error {
	if pool == nil {
		return ErrPoolExhausted
	}

	// Try pool first
	if err := pool.Submit(task); err == nil {
		return nil
	}

	// Pool full - trigger immediate resize and retry multiple times
	if resizer != nil {
		for retry := 0; retry < 3; retry++ {
			resizer.emergencyResize()
			// Small delay to allow resize to take effect
			time.Sleep(10 * time.Millisecond)
			if err := pool.Submit(task); err == nil {
				return nil
			}
		}
	}

	// All retries failed
	return ErrPoolExhausted
}

// submitWithFallback is deprecated - use submitWithResize instead
// Kept for backward compatibility but no longer creates fallback goroutines
func (gm *GossipManager) submitWithFallback(task func(), poolName string) error {
	// Try replication pool first
	if gm.replicationPool != nil {
		if err := gm.submitWithResize(task, gm.replicationPool, gm.replicationPoolResizer, poolName); err == nil {
			return nil
		}
	}

	// Attempt inbound pools (use submitInboundTask which has its own resize logic)
	if gm.inboundPriorityPool != nil || gm.inboundPool != nil {
		return gm.submitInboundTask(task, context.Background(), poolName)
	}

	return ErrPoolExhausted
}

// ErrPoolExhausted is returned when all pools are full and fallback limit is reached
var ErrPoolExhausted = &PoolExhaustedError{}

type PoolExhaustedError struct{}

func (e *PoolExhaustedError) Error() string {
	return "all worker pools exhausted and fallback limit reached"
}

// goroutineMonitor monitors goroutine count and provides diagnostics
type goroutineMonitor struct {
	mu            sync.Mutex
	baseline      int
	peak          int
	lastCheck     time.Time
	checkInterval time.Duration
	leakThreshold int // Threshold for potential leak detection
	enabled       bool
}

var globalGoroutineMonitor = &goroutineMonitor{
	checkInterval: 10 * time.Second,
	leakThreshold: 10000, // Alert if goroutines exceed 10k
	enabled:       true,
}

// checkGoroutines checks current goroutine count and detects potential leaks
func (gm *goroutineMonitor) checkGoroutines() {
	if !gm.enabled {
		return
	}

	gm.mu.Lock()
	defer gm.mu.Unlock()

	current := runtime.NumGoroutine()
	now := time.Now()

	if gm.baseline == 0 {
		gm.baseline = current
		gm.lastCheck = now
		return
	}

	if current > gm.peak {
		gm.peak = current
	}

	// Check for potential leak (sustained growth)
	if now.Sub(gm.lastCheck) > gm.checkInterval {
		growth := current - gm.baseline
		if growth > gm.leakThreshold {
			logging.Warn("Potential goroutine leak detected",
				"current", current,
				"baseline", gm.baseline,
				"growth", growth,
				"peak", gm.peak)
		}

		gm.lastCheck = now
	}
}

// Start starts periodic goroutine monitoring
func (gm *goroutineMonitor) Start() {
	if !gm.enabled {
		return
	}

	go func() {
		ticker := time.NewTicker(gm.checkInterval)
		defer ticker.Stop()

		for range ticker.C {
			gm.checkGoroutines()
		}
	}()
}

// GetStats returns current goroutine statistics
func (gm *goroutineMonitor) GetStats() (current, baseline, peak int) {
	gm.mu.Lock()
	defer gm.mu.Unlock()

	return runtime.NumGoroutine(), gm.baseline, gm.peak
}

// SetBaseline sets the baseline goroutine count
func (gm *goroutineMonitor) SetBaseline(count int) {
	gm.mu.Lock()
	defer gm.mu.Unlock()
	gm.baseline = count
}
