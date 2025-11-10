package gossip

import "time"

// EpidemicMetrics tracks gossip epidemic protocol statistics
// Simplified version for minimal overhead
type EpidemicMetrics struct {
	// Gossip efficiency
	TotalMessages int64 // Total gossip messages sent
	DedupHits     int64 // Messages deduplicated (already seen)

	// Fanout & coverage
	CurrentFanout int32 // Current fanout (nodes gossiped to per round)

	// Success tracking
	SendSuccess  int64   // Successful sends
	SendFailures int64   // Failed sends
	FailureRate  float64 // Current failure rate

	// Timing
	LastUpdate time.Time // Last metrics update
}

// GetEpidemicMetrics returns current epidemic gossip metrics.
// Simplified version - returns basic metrics without detailed tracking
func (gm *GossipManager) GetEpidemicMetrics() *EpidemicMetrics {
	gm.mu.RLock()
	liveNodeCount := len(gm.liveNodes)
	gm.mu.RUnlock()

	return &EpidemicMetrics{
		TotalMessages: 0, // Simplified: not tracking detailed messages
		DedupHits:     0,
		CurrentFanout: int32(liveNodeCount), // Use live node count as approximate fanout
		SendSuccess:   0,
		SendFailures:  0,
		FailureRate:   0.0,
		LastUpdate:    time.Now(),
	}
}

// ResetEpidemicMetrics resets all epidemic gossip metrics.
// Simplified version - no-op
func (gm *GossipManager) ResetEpidemicMetrics() {
	// Simplified: no metrics to reset
}

// GetAdaptiveFanout returns the current adaptive fanout value.
// Simplified version - returns cluster size / 3
func (gm *GossipManager) GetAdaptiveFanout() int {
	gm.mu.RLock()
	nodeCount := len(gm.liveNodes)
	gm.mu.RUnlock()

	fanout := nodeCount / 3
	if fanout < 3 {
		fanout = 3
	}
	if fanout > 10 {
		fanout = 10
	}
	return fanout
}

// GossipStats contains detailed gossip statistics
type GossipStats struct {
	Fanout        int
	MaxTTL        int
	SeenCacheSize int
	SuccessRate   float64
	FailureRate   float64
	CircuitOpen   map[string]int // nodeID -> failure count
}

// GetGossipStats returns detailed gossip statistics.
// Simplified version
func (gm *GossipManager) GetGossipStats() *GossipStats {
	gm.mu.RLock()
	defer gm.mu.RUnlock()

	return &GossipStats{
		Fanout:        gm.GetAdaptiveFanout(),
		MaxTTL:        3, // Default TTL
		SeenCacheSize: 0,
		SuccessRate:   1.0, // Simplified: assume healthy
		FailureRate:   0.0,
		CircuitOpen:   make(map[string]int),
	}
}
