package gossip

import (
	"sync"
	"sync/atomic"
	"time"
)

// PerformanceMetrics tracks performance-related metrics
type PerformanceMetrics struct {
	// Connection metrics
	ConnectionAttempts  atomic.Int64
	ConnectionSuccesses atomic.Int64
	ConnectionFailures  atomic.Int64
	ConnectionLatency   atomic.Int64 // nanoseconds

	// Pipeline metrics
	PipelineFlushes atomic.Int64
	PipelineDrops   atomic.Int64
	PipelineLatency atomic.Int64 // nanoseconds

	// Binary protocol metrics
	BinaryEncodes       atomic.Int64
	BinaryDecodes       atomic.Int64
	BinaryEncodeLatency atomic.Int64 // nanoseconds
	BinaryDecodeLatency atomic.Int64 // nanoseconds

	// Memory pool metrics
	PoolHits   atomic.Int64
	PoolMisses atomic.Int64

	// Batch metrics
	BatchSizes   atomic.Int64
	BatchCount   atomic.Int64
	AvgBatchSize atomic.Int64

	mu         sync.RWMutex
	lastUpdate time.Time
}

var globalPerfMetrics = &PerformanceMetrics{}

func GetPerformanceMetrics() *PerformanceMetrics {
	return globalPerfMetrics
}

func (pm *PerformanceMetrics) RecordConnectionAttempt() {
	pm.ConnectionAttempts.Add(1)
}

func (pm *PerformanceMetrics) RecordConnectionSuccess(latency time.Duration) {
	pm.ConnectionSuccesses.Add(1)
	pm.ConnectionLatency.Add(int64(latency))
}

func (pm *PerformanceMetrics) RecordConnectionFailure() {
	pm.ConnectionFailures.Add(1)
}

func (pm *PerformanceMetrics) RecordPipelineFlush(latency time.Duration) {
	pm.PipelineFlushes.Add(1)
	pm.PipelineLatency.Add(int64(latency))
}

func (pm *PerformanceMetrics) RecordPipelineDrop() {
	pm.PipelineDrops.Add(1)
}

func (pm *PerformanceMetrics) RecordBinaryEncode(latency time.Duration) {
	pm.BinaryEncodes.Add(1)
	pm.BinaryEncodeLatency.Add(int64(latency))
}

func (pm *PerformanceMetrics) RecordBinaryDecode(latency time.Duration) {
	pm.BinaryDecodes.Add(1)
	pm.BinaryDecodeLatency.Add(int64(latency))
}

func (pm *PerformanceMetrics) RecordPoolHit() {
	pm.PoolHits.Add(1)
}

func (pm *PerformanceMetrics) RecordPoolMiss() {
	pm.PoolMisses.Add(1)
}

func (pm *PerformanceMetrics) RecordBatch(size int) {
	pm.BatchCount.Add(1)
	pm.BatchSizes.Add(int64(size))

	// Update average
	count := pm.BatchCount.Load()
	if count > 0 {
		total := pm.BatchSizes.Load()
		pm.AvgBatchSize.Store(total / count)
	}
}

func (pm *PerformanceMetrics) GetStats() map[string]interface{} {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	stats := make(map[string]interface{})

	// Connection stats
	attempts := pm.ConnectionAttempts.Load()
	successes := pm.ConnectionSuccesses.Load()
	failures := pm.ConnectionFailures.Load()
	stats["connection_attempts"] = attempts
	stats["connection_successes"] = successes
	stats["connection_failures"] = failures
	if attempts > 0 {
		stats["connection_success_rate"] = float64(successes) / float64(attempts)
	}
	if successes > 0 {
		avgLatency := pm.ConnectionLatency.Load() / successes
		stats["avg_connection_latency_ns"] = avgLatency
		stats["avg_connection_latency_ms"] = float64(avgLatency) / 1e6
	}

	// Pipeline stats
	flushes := pm.PipelineFlushes.Load()
	drops := pm.PipelineDrops.Load()
	stats["pipeline_flushes"] = flushes
	stats["pipeline_drops"] = drops
	if flushes > 0 {
		avgLatency := pm.PipelineLatency.Load() / flushes
		stats["avg_pipeline_latency_ns"] = avgLatency
		stats["avg_pipeline_latency_us"] = float64(avgLatency) / 1e3
	}

	// Binary protocol stats
	encodes := pm.BinaryEncodes.Load()
	decodes := pm.BinaryDecodes.Load()
	stats["binary_encodes"] = encodes
	stats["binary_decodes"] = decodes
	if encodes > 0 {
		avgLatency := pm.BinaryEncodeLatency.Load() / encodes
		stats["avg_encode_latency_ns"] = avgLatency
		stats["avg_encode_latency_us"] = float64(avgLatency) / 1e3
	}
	if decodes > 0 {
		avgLatency := pm.BinaryDecodeLatency.Load() / decodes
		stats["avg_decode_latency_ns"] = avgLatency
		stats["avg_decode_latency_us"] = float64(avgLatency) / 1e3
	}

	// Pool stats
	hits := pm.PoolHits.Load()
	misses := pm.PoolMisses.Load()
	stats["pool_hits"] = hits
	stats["pool_misses"] = misses
	total := hits + misses
	if total > 0 {
		stats["pool_hit_rate"] = float64(hits) / float64(total)
	}

	// Batch stats
	stats["batch_count"] = pm.BatchCount.Load()
	stats["avg_batch_size"] = pm.AvgBatchSize.Load()

	pm.lastUpdate = time.Now()
	stats["last_update"] = pm.lastUpdate

	return stats
}

func (pm *PerformanceMetrics) Reset() {
	pm.ConnectionAttempts.Store(0)
	pm.ConnectionSuccesses.Store(0)
	pm.ConnectionFailures.Store(0)
	pm.ConnectionLatency.Store(0)
	pm.PipelineFlushes.Store(0)
	pm.PipelineDrops.Store(0)
	pm.PipelineLatency.Store(0)
	pm.BinaryEncodes.Store(0)
	pm.BinaryDecodes.Store(0)
	pm.BinaryEncodeLatency.Store(0)
	pm.BinaryDecodeLatency.Store(0)
	pm.PoolHits.Store(0)
	pm.PoolMisses.Store(0)
	pm.BatchSizes.Store(0)
	pm.BatchCount.Store(0)
	pm.AvgBatchSize.Store(0)
}
