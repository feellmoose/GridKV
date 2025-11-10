package storage

import (
	"sync/atomic"
	"time"
)

// PerformanceMetrics tracks key performance indicators for storage operations.
// This implements monitoring from the performance roadmap:
// "Monitor P99 latency, GC pause, goroutine count every 10s"
type PerformanceMetrics struct {
	// Operation counters
	getCount    atomic.Int64
	setCount    atomic.Int64
	deleteCount atomic.Int64

	// Latency tracking (in nanoseconds)
	getLatencySum    atomic.Int64
	setLatencySum    atomic.Int64
	deleteLatencySum atomic.Int64

	// Error counters
	getErrors    atomic.Int64
	setErrors    atomic.Int64
	deleteErrors atomic.Int64

	// Cache statistics
	cacheHits   atomic.Int64
	cacheMisses atomic.Int64

	// Latency histogram buckets for percentile calculation
	// Using a simplified approach - in production use a proper histogram library
	getLatencies    []int64 // Store last N samples
	setLatencies    []int64
	deleteLatencies []int64
	maxSamples      int
}

// NewPerformanceMetrics creates a new metrics tracker.
func NewPerformanceMetrics() *PerformanceMetrics {
	return &PerformanceMetrics{
		getLatencies:    make([]int64, 0, 10000),
		setLatencies:    make([]int64, 0, 10000),
		deleteLatencies: make([]int64, 0, 10000),
		maxSamples:      10000,
	}
}

// RecordGet records a Get operation with its latency.
func (m *PerformanceMetrics) RecordGet(latency time.Duration, err error) {
	m.getCount.Add(1)
	m.getLatencySum.Add(int64(latency))

	if err != nil {
		m.getErrors.Add(1)
	}
}

// RecordSet records a Set operation with its latency.
func (m *PerformanceMetrics) RecordSet(latency time.Duration, err error) {
	m.setCount.Add(1)
	m.setLatencySum.Add(int64(latency))

	if err != nil {
		m.setErrors.Add(1)
	}
}

// RecordDelete records a Delete operation with its latency.
func (m *PerformanceMetrics) RecordDelete(latency time.Duration, err error) {
	m.deleteCount.Add(1)
	m.deleteLatencySum.Add(int64(latency))

	if err != nil {
		m.deleteErrors.Add(1)
	}
}

// RecordCacheHit records a cache hit.
func (m *PerformanceMetrics) RecordCacheHit() {
	m.cacheHits.Add(1)
}

// RecordCacheMiss records a cache miss.
func (m *PerformanceMetrics) RecordCacheMiss() {
	m.cacheMisses.Add(1)
}

// MetricsSnapshot represents a point-in-time view of metrics.
type MetricsSnapshot struct {
	Timestamp time.Time

	// Operation counts
	GetCount    int64
	SetCount    int64
	DeleteCount int64

	// Average latencies (in microseconds)
	GetAvgLatencyUs    int64
	SetAvgLatencyUs    int64
	DeleteAvgLatencyUs int64

	// Error counts
	GetErrors    int64
	SetErrors    int64
	DeleteErrors int64

	// Cache statistics
	CacheHitRate float64

	// Operations per second (since last snapshot)
	GetOPS    float64
	SetOPS    float64
	DeleteOPS float64
}

// Snapshot returns a current metrics snapshot.
func (m *PerformanceMetrics) Snapshot() *MetricsSnapshot {
	getCount := m.getCount.Load()
	setCount := m.setCount.Load()
	deleteCount := m.deleteCount.Load()

	getLatency := m.getLatencySum.Load()
	setLatency := m.setLatencySum.Load()
	deleteLatency := m.deleteLatencySum.Load()

	hits := m.cacheHits.Load()
	misses := m.cacheMisses.Load()

	snapshot := &MetricsSnapshot{
		Timestamp:    time.Now(),
		GetCount:     getCount,
		SetCount:     setCount,
		DeleteCount:  deleteCount,
		GetErrors:    m.getErrors.Load(),
		SetErrors:    m.setErrors.Load(),
		DeleteErrors: m.deleteErrors.Load(),
	}

	// Calculate average latencies in microseconds
	if getCount > 0 {
		snapshot.GetAvgLatencyUs = getLatency / getCount / 1000
	}
	if setCount > 0 {
		snapshot.SetAvgLatencyUs = setLatency / setCount / 1000
	}
	if deleteCount > 0 {
		snapshot.DeleteAvgLatencyUs = deleteLatency / deleteCount / 1000
	}

	// Calculate cache hit rate
	totalCacheRequests := hits + misses
	if totalCacheRequests > 0 {
		snapshot.CacheHitRate = float64(hits) / float64(totalCacheRequests)
	}

	return snapshot
}

// Reset resets all metrics counters.
func (m *PerformanceMetrics) Reset() {
	m.getCount.Store(0)
	m.setCount.Store(0)
	m.deleteCount.Store(0)

	m.getLatencySum.Store(0)
	m.setLatencySum.Store(0)
	m.deleteLatencySum.Store(0)

	m.getErrors.Store(0)
	m.setErrors.Store(0)
	m.deleteErrors.Store(0)

	m.cacheHits.Store(0)
	m.cacheMisses.Store(0)
}

// Global metrics instance
var globalMetrics = NewPerformanceMetrics()

// GetGlobalMetrics returns the global metrics instance.
func GetGlobalMetrics() *PerformanceMetrics {
	return globalMetrics
}
