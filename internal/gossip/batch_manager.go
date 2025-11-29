package gossip

import (
	"math"
	"sync"
	"time"
)

// BatchRole represents a logical batching domain (writes, read requests, etc.).
type BatchRole string

const (
	BatchRoleWrite        BatchRole = "write"
	BatchRoleReadRequest  BatchRole = "read_request"
	BatchRoleReadResponse BatchRole = "read_response"
)

type batchProfile struct {
	maxClusterSize int
	batchSize      int
	flushInterval  time.Duration
}

type batchSettings struct {
	profiles     []batchProfile
	minBatchSize int
	minInterval  time.Duration
}

// BatchManager centralizes adaptive batch configuration for all gossip pipelines.
type BatchManager struct {
	mu          sync.RWMutex
	clusterSize int
	configs     map[BatchRole]*batchSettings
}

func newBatchManager() *BatchManager {
	// Stage 2.3: Adjusted flush intervals - min 500µs, max 2-5ms
	baseProfiles := []batchProfile{
		{maxClusterSize: 10, batchSize: 20000, flushInterval: 500 * time.Microsecond},
		{maxClusterSize: 20, batchSize: 35000, flushInterval: 1 * time.Millisecond},
		{maxClusterSize: 50, batchSize: 50000, flushInterval: 2 * time.Millisecond},
		{maxClusterSize: 100, batchSize: 65000, flushInterval: 3 * time.Millisecond},
		{maxClusterSize: math.MaxInt32, batchSize: 80000, flushInterval: 5 * time.Millisecond},
	}

	return &BatchManager{
		clusterSize: 1,
		configs: map[BatchRole]*batchSettings{
			BatchRoleWrite: {
				profiles:     cloneProfiles(baseProfiles, 1.0, 1.0),
				minBatchSize: 10000,
				minInterval:  500 * time.Microsecond, // Stage 2.3: Fixed minimum at 500µs
			},
			BatchRoleReadRequest: {
				profiles:     cloneProfiles(baseProfiles, 0.3, 0.4), // Smaller batches, shorter intervals for requests
				minBatchSize: 2000,                                  // Smaller batches for lower latency
				minInterval:  20 * time.Microsecond,                 // Shorter interval for requests
			},
			BatchRoleReadResponse: {
				profiles:     cloneProfiles(baseProfiles, 0.3, 0.4), // Smaller batches, shorter intervals for responses
				minBatchSize: 2000,                                  // Smaller batches for lower latency
				minInterval:  20 * time.Microsecond,                 // Shorter interval for responses
			},
		},
	}
}

func cloneProfiles(base []batchProfile, sizeMultiplier, intervalMultiplier float64) []batchProfile {
	cloned := make([]batchProfile, len(base))
	for i, profile := range base {
		batch := int(float64(profile.batchSize) * sizeMultiplier)
		if batch < 1 {
			batch = 1
		}
		interval := time.Duration(float64(profile.flushInterval) * intervalMultiplier)
		if interval < time.Microsecond {
			interval = time.Microsecond
		}
		cloned[i] = batchProfile{
			maxClusterSize: profile.maxClusterSize,
			batchSize:      batch,
			flushInterval:  interval,
		}
	}
	return cloned
}

func (bm *BatchManager) UpdateClusterSize(size int) {
	if size <= 0 {
		size = 1
	}
	bm.mu.Lock()
	bm.clusterSize = size
	bm.mu.Unlock()
}

func (bm *BatchManager) Get(role BatchRole) (int, time.Duration) {
	bm.mu.RLock()
	size := bm.clusterSize
	settings := bm.configs[role]
	bm.mu.RUnlock()

	if settings == nil {
		return pipelineBatchSize, pipelineFlushTick
	}

	for _, profile := range settings.profiles {
		if size <= profile.maxClusterSize {
			batch := profile.batchSize
			if batch < settings.minBatchSize {
				batch = settings.minBatchSize
			}
			interval := profile.flushInterval
			if interval < settings.minInterval {
				interval = settings.minInterval
			}
			return batch, interval
		}
	}

	// fallback (should never happen)
	return settings.minBatchSize, settings.minInterval
}
