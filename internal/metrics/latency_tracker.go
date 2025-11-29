package metrics

import (
	"sort"
	"sync"
	"sync/atomic"
)

// LatencyTracker tracks latency samples and computes percentiles.
// Uses sliding window for accurate percentile calculation.
// Uses sampling to reduce lock contention (Stage 0.3).
type LatencyTracker struct {
	mu      sync.RWMutex
	samples []int64
	maxSize int
	count   atomic.Int64
	sum     atomic.Int64
	sampler *Sampler
}

// NewLatencyTracker creates a new latency tracker with specified window size.
// Uses sampling to reduce lock contention (Stage 0.3).
func NewLatencyTracker(windowSize int) *LatencyTracker {
	if windowSize < 100 {
		windowSize = 100
	}
	if windowSize > 10000 {
		windowSize = 10000
	}
	// Use sampling: sample 1 in 5 calls (20% sampling) for high-frequency latency tracking
	sampler := NewSampler(5)
	return &LatencyTracker{
		samples: make([]int64, 0, windowSize),
		maxSize: windowSize,
		sampler: sampler,
	}
}

// Record records a latency sample in nanoseconds.
// Uses sampling to reduce lock contention (Stage 0.3).
func (lt *LatencyTracker) Record(nanos int64) {
	// Always update atomic counters (lock-free)
	lt.count.Add(1)
	lt.sum.Add(nanos)

	// Sample lock-protected operations to reduce contention
	if lt.sampler != nil && !lt.sampler.ShouldSample() {
		return
	}

	lt.mu.Lock()
	defer lt.mu.Unlock()

	if len(lt.samples) < lt.maxSize {
		lt.samples = append(lt.samples, nanos)
	} else {
		// Replace oldest sample (FIFO)
		lt.samples = append(lt.samples[1:], nanos)
	}
}

// P50 returns the 50th percentile latency in nanoseconds.
func (lt *LatencyTracker) P50() int64 {
	return lt.percentile(0.50)
}

// P95 returns the 95th percentile latency in nanoseconds.
func (lt *LatencyTracker) P95() int64 {
	return lt.percentile(0.95)
}

// P99 returns the 99th percentile latency in nanoseconds.
func (lt *LatencyTracker) P99() int64 {
	return lt.percentile(0.99)
}

// percentile calculates the specified percentile from samples.
func (lt *LatencyTracker) percentile(p float64) int64 {
	lt.mu.RLock()
	defer lt.mu.RUnlock()

	if len(lt.samples) == 0 {
		return 0
	}

	// Create sorted copy for percentile calculation
	sorted := make([]int64, len(lt.samples))
	copy(sorted, lt.samples)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i] < sorted[j]
	})

	idx := int(float64(len(sorted)) * p)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// Reset clears all samples.
func (lt *LatencyTracker) Reset() {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	lt.samples = lt.samples[:0]
	lt.count.Store(0)
	lt.sum.Store(0)
}
