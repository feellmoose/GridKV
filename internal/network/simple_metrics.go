package network

import (
	"sync/atomic"
	"time"
)

// SimpleMetrics provides lightweight metrics collection
// Optimized for minimal overhead and high performance
type SimpleMetrics struct {
	// Request counters (atomic for lock-free updates)
	totalRequests  atomic.Int64
	totalErrors    atomic.Int64
	totalLatencyNs atomic.Int64

	// Network counters
	totalProbes  atomic.Int64
	failedProbes atomic.Int64

	// Multi-DC counters (optional, only if MultiDC is enabled)
	localOps  atomic.Int64
	remoteOps atomic.Int64

	startTime time.Time
}

// NewSimpleMetrics creates a new simple metrics collector
func NewSimpleMetrics() *SimpleMetrics {
	return &SimpleMetrics{
		startTime: time.Now(),
	}
}

// RecordRequest records a request completion
//
//go:inline
func (sm *SimpleMetrics) RecordRequest(latency time.Duration, err error) {
	sm.totalRequests.Add(1)
	if err != nil {
		sm.totalErrors.Add(1)
	}
	if latency > 0 {
		sm.totalLatencyNs.Add(int64(latency))
	}
}

// RecordProbe records a probe attempt
//
//go:inline
func (sm *SimpleMetrics) RecordProbe(success bool) {
	sm.totalProbes.Add(1)
	if !success {
		sm.failedProbes.Add(1)
	}
}

// RecordOperation records a local or remote operation
//
//go:inline
func (sm *SimpleMetrics) RecordOperation(isLocal bool) {
	if isLocal {
		sm.localOps.Add(1)
	} else {
		sm.remoteOps.Add(1)
	}
}

// MetricsStats contains metrics statistics (zero-allocation return type)
type MetricsStats struct {
	TotalRequests int64
	TotalErrors   int64
	ErrorRate     float64
	TotalProbes   int64
	FailedProbes  int64
	UptimeSeconds float64
	AvgLatencyNs  int64
	LocalOps      int64
	RemoteOps     int64
	LocalityRate  float64
}

// GetStatsStruct returns current statistics as struct (OPTIMIZED: zero boxing)
//
//go:inline
func (sm *SimpleMetrics) GetStatsStruct() MetricsStats {
	totalReqs := sm.totalRequests.Load()
	totalErrs := sm.totalErrors.Load()
	totalLatency := sm.totalLatencyNs.Load()
	localOps := sm.localOps.Load()
	remoteOps := sm.remoteOps.Load()
	totalOps := localOps + remoteOps

	var avgLatency int64
	if totalReqs > 0 {
		avgLatency = totalLatency / totalReqs
	}

	var localityRate float64
	if totalOps > 0 {
		localityRate = float64(localOps) / float64(totalOps)
	}

	return MetricsStats{
		TotalRequests: totalReqs,
		TotalErrors:   totalErrs,
		ErrorRate:     float64(totalErrs) / float64(maxInt64(totalReqs, 1)),
		TotalProbes:   sm.totalProbes.Load(),
		FailedProbes:  sm.failedProbes.Load(),
		UptimeSeconds: time.Since(sm.startTime).Seconds(),
		AvgLatencyNs:  avgLatency,
		LocalOps:      localOps,
		RemoteOps:     remoteOps,
		LocalityRate:  localityRate,
	}
}

// GetStats returns current statistics (DEPRECATED: use GetStatsStruct for better performance)
// This method is kept for backward compatibility but has high allocation cost
func (sm *SimpleMetrics) GetStats() map[string]interface{} {
	stats := sm.GetStatsStruct()

	// Convert to map (causes boxing allocations)
	return map[string]interface{}{
		"total_requests": stats.TotalRequests,
		"total_errors":   stats.TotalErrors,
		"error_rate":     stats.ErrorRate,
		"total_probes":   stats.TotalProbes,
		"failed_probes":  stats.FailedProbes,
		"uptime_seconds": stats.UptimeSeconds,
		"avg_latency_ns": stats.AvgLatencyNs,
		"local_ops":      stats.LocalOps,
		"remote_ops":     stats.RemoteOps,
		"locality_rate":  stats.LocalityRate,
	}
}

// Old GetStats implementation (REMOVED - use GetStatsStruct)
func (sm *SimpleMetrics) _oldGetStats() map[string]interface{} {
	totalReqs := sm.totalRequests.Load()
	totalErrs := sm.totalErrors.Load()
	totalLatency := sm.totalLatencyNs.Load()

	stats := map[string]interface{}{
		"total_requests": totalReqs,
		"total_errors":   totalErrs,
		"error_rate":     float64(totalErrs) / float64(maxInt64(totalReqs, 1)),
		"total_probes":   sm.totalProbes.Load(),
		"failed_probes":  sm.failedProbes.Load(),
		"uptime_seconds": time.Since(sm.startTime).Seconds(),
	}

	if totalReqs > 0 {
		avgLatency := time.Duration(totalLatency / totalReqs)
		stats["avg_latency"] = avgLatency.String()
	}

	localOps := sm.localOps.Load()
	remoteOps := sm.remoteOps.Load()
	if localOps > 0 || remoteOps > 0 {
		stats["local_operations"] = localOps
		stats["remote_operations"] = remoteOps
		stats["locality_ratio"] = float64(localOps) / float64(localOps+remoteOps)
	}

	return stats
}

// GetDashboard returns dashboard data (DEPRECATED: use GetStatsStruct)
func (sm *SimpleMetrics) GetDashboard() map[string]interface{} {
	return sm.GetStats() // Reuse GetStats for now
}

//go:inline
func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// Reset resets all counters (for testing)
func (sm *SimpleMetrics) Reset() {
	sm.totalRequests.Store(0)
	sm.totalErrors.Store(0)
	sm.totalLatencyNs.Store(0)
	sm.totalProbes.Store(0)
	sm.failedProbes.Store(0)
	sm.localOps.Store(0)
	sm.remoteOps.Store(0)
	sm.startTime = time.Now()
}
