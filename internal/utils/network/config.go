package network

import (
	"time"

	"github.com/feellmoose/gridkv"
)

// NetworkProfile defines the network environment type
type NetworkProfile int

const (
	ProfileLAN       NetworkProfile = iota // LAN environment (RTT < 5ms)
	ProfileWAN                             // WAN environment (RTT 50-100ms)
	ProfileGlobal                          // Global/cross-region (RTT 150-300ms)
	ProfileSatellite                       // Satellite link (RTT 500-800ms)
)

// LatencyConfig contains optimized configuration based on network latency
type LatencyConfig struct {
	GossipInterval     time.Duration
	FailureTimeout     time.Duration
	SuspectTimeout     time.Duration
	ReplicationTimeout time.Duration
	ReadTimeout        time.Duration
	MaxConnections     int
	MaxIdleConnections int
}

// GetConfigForLatency returns optimized configuration based on average RTT and cluster size.
// This function calculates optimal timeouts and connection pool sizes to maximize
// performance in high-latency network environments.
//
// Parameters:
//   - avgRTT: Average round-trip time between nodes
//   - clusterSize: Number of nodes in the cluster
//
// Returns optimized LatencyConfig with all timing and connection parameters
func GetConfigForLatency(avgRTT time.Duration, clusterSize int) *LatencyConfig {
	config := &LatencyConfig{}

	// 1. Calculate Gossip interval based on RTT and cluster size
	// Formula: RTT × multiplier (multiplier increases with cluster size)
	multiplier := 10
	if clusterSize > 10 {
		multiplier = 20
	}
	if clusterSize > 30 {
		multiplier = 30
	}

	config.GossipInterval = avgRTT * time.Duration(multiplier)

	// Ensure minimum of 1 second for stability
	if config.GossipInterval < 1*time.Second {
		config.GossipInterval = 1 * time.Second
	}

	// 2. Failure detection timeouts: Gossip interval × 3
	// This gives enough time for multiple gossip rounds before marking as suspect
	config.FailureTimeout = config.GossipInterval * 3
	config.SuspectTimeout = config.FailureTimeout * 2

	// 3. Replication and read timeouts: RTT × 5 (sufficient headroom)
	// Factor of 5 accounts for: network jitter, processing time, and retries
	config.ReplicationTimeout = avgRTT * 5
	config.ReadTimeout = avgRTT * 5

	// Ensure minimum timeouts for stability
	if config.ReplicationTimeout < 500*time.Millisecond {
		config.ReplicationTimeout = 500 * time.Millisecond
	}
	if config.ReadTimeout < 500*time.Millisecond {
		config.ReadTimeout = 500 * time.Millisecond
	}

	// 4. Connection pool sizing - dynamically scaled for large clusters
	// For small/medium clusters: cluster_size × 4 (allows multiple concurrent operations per node)
	// For large clusters (50-100 nodes): more aggressive scaling to handle connection storms
	switch {
	case clusterSize <= 10:
		// Small clusters: standard scaling
		config.MaxConnections = clusterSize * 4
		config.MaxIdleConnections = clusterSize * 2
	case clusterSize <= 30:
		// Medium clusters: moderate scaling
		config.MaxConnections = clusterSize * 5
		config.MaxIdleConnections = clusterSize * 3
	case clusterSize <= 50:
		// Large clusters: higher scaling for connection volume
		config.MaxConnections = clusterSize * 6
		config.MaxIdleConnections = clusterSize * 4
	case clusterSize <= 100:
		// Very large clusters: aggressive scaling (50-100 nodes)
		config.MaxConnections = clusterSize * 8
		config.MaxIdleConnections = clusterSize * 6
	default:
		// Huge clusters (>100 nodes): maximum practical scaling
		config.MaxConnections = clusterSize * 10
		config.MaxIdleConnections = clusterSize * 8
	}

	// Cap maximum connections to prevent resource exhaustion
	// Higher caps for large clusters to handle message volume
	if config.MaxConnections > 2000 {
		config.MaxConnections = 2000 // Increased from 1000 for 50-100 node clusters
	}
	if config.MaxIdleConnections > 500 {
		config.MaxIdleConnections = 500 // Increased from 200 for large clusters
	}

	return config
}

// GetConfigForProfile returns optimized configuration for a predefined network profile.
// This is a convenience function for common deployment scenarios.
//
// Parameters:
//   - profile: The network environment profile (LAN/WAN/Global/Satellite)
//   - clusterSize: Number of nodes in the cluster
//
// Returns optimized LatencyConfig for the specified profile
func GetConfigForProfile(profile NetworkProfile, clusterSize int) *LatencyConfig {
	var cfg *LatencyConfig

	switch profile {
	case ProfileLAN:
		// LAN: Low latency, fast detection
		cfg = GetConfigForLatency(1*time.Millisecond, clusterSize)
	case ProfileWAN:
		// WAN: Moderate latency, balanced
		cfg = GetConfigForLatency(50*time.Millisecond, clusterSize)
	case ProfileGlobal:
		// Global: High latency, patient timeouts
		cfg = GetConfigForLatency(200*time.Millisecond, clusterSize)
	case ProfileSatellite:
		// Satellite: Very high latency, very patient timeouts
		cfg = GetConfigForLatency(600*time.Millisecond, clusterSize)
	default:
		// Default to WAN profile
		cfg = GetConfigForLatency(10*time.Millisecond, clusterSize)
	}

	// Stage 1.3 alignment: tighten timeouts by profile with clear bands.
	// This ensures tests using NetworkProfile align with ClusterProfile defaults.
	switch profile {
	case ProfileLAN:
		// LAN: 0.5–0.8s
		if cfg.ReadTimeout < 800*time.Millisecond {
			cfg.ReadTimeout = 800 * time.Millisecond
		}
		if cfg.ReplicationTimeout < 800*time.Millisecond {
			cfg.ReplicationTimeout = 800 * time.Millisecond
		}
		// Slightly enlarge connection pool for high-concurrency tests.
		if clusterSize <= 10 {
			if cfg.MaxConnections < clusterSize*6 {
				cfg.MaxConnections = clusterSize * 6
			}
			if cfg.MaxIdleConnections < clusterSize*3 {
				cfg.MaxIdleConnections = clusterSize * 3
			}
		}
	case ProfileWAN:
		// WAN: 1–2s
		if cfg.ReadTimeout < 1500*time.Millisecond {
			cfg.ReadTimeout = 1500 * time.Millisecond
		}
		if cfg.ReplicationTimeout < 1500*time.Millisecond {
			cfg.ReplicationTimeout = 1500 * time.Millisecond
		}
	case ProfileGlobal, ProfileSatellite:
		// Global/Satellite: 2–3s+
		if cfg.ReadTimeout < 2500*time.Millisecond {
			cfg.ReadTimeout = 2500 * time.Millisecond
		}
		if cfg.ReplicationTimeout < 2500*time.Millisecond {
			cfg.ReplicationTimeout = 2500 * time.Millisecond
		}
	}

	return cfg
}

// ApplyToOptions applies the latency configuration to GridKVOptions.
// This modifies the provided options object with optimized parameters.
func (c *LatencyConfig) ApplyToOptions(opts *gridkv.GridKVOptions) {
	opts.GossipInterval = c.GossipInterval
	opts.FailureTimeout = c.FailureTimeout
	opts.SuspectTimeout = c.SuspectTimeout
	opts.ReplicationTimeout = c.ReplicationTimeout
	opts.ReadTimeout = c.ReadTimeout

	// Apply network connection pool settings if network options exist
	if opts.Network != nil {
		opts.Network.MaxConns = c.MaxConnections
		opts.Network.MaxIdle = c.MaxIdleConnections
	}
}

// String returns a human-readable description of the network profile
func (p NetworkProfile) String() string {
	switch p {
	case ProfileLAN:
		return "LAN (< 5ms RTT)"
	case ProfileWAN:
		return "WAN (50-100ms RTT)"
	case ProfileGlobal:
		return "Global (150-300ms RTT)"
	case ProfileSatellite:
		return "Satellite (500-800ms RTT)"
	default:
		return "Unknown"
	}
}

// CalculateOptimalShardCount calculates the optimal number of shards
// based on CPU cores and expected concurrency level.
//
// Parameters:
//   - cpuCores: Number of CPU cores available
//   - concurrencyLevel: Expected concurrent operations (1=low, 2=medium, 3=high, 4=very high)
//
// Returns optimal shard count (always a power of 2)
func CalculateOptimalShardCount(cpuCores int, concurrencyLevel int) int {
	// Base formula: cores × (2^concurrencyLevel)
	shardCount := cpuCores * (1 << uint(concurrencyLevel))

	// Ensure it's within reasonable bounds
	if shardCount < 16 {
		shardCount = 16
	}
	if shardCount > 1024 {
		shardCount = 1024
	}

	// Round to next power of 2 for efficient masking
	return nextPowerOf2(shardCount)
}

// nextPowerOf2 returns the next power of 2 greater than or equal to n
func nextPowerOf2(n int) int {
	if n <= 1 {
		return 1
	}
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	n++
	return n
}

// EstimateGossipLoad estimates the Gossip message load for a given configuration (Stage 2.5).
// This helps predict network overhead before deployment.
//
// Improvements:
// - Considers actual fan-out (not all nodes, typically 1-3 targets)
// - Accounts for retry attempts (assumes 5-10% failure rate)
// - Uses actual gossip interval (may vary by cluster size)
//
// Returns estimated messages per second per node
func EstimateGossipLoad(clusterSize int, gossipInterval time.Duration) int {
	if gossipInterval == 0 || clusterSize <= 1 {
		return 0
	}

	// Stage 2.5: Calculate actual fan-out based on cluster size
	// Small clusters gossip to all, medium to 3, large to 1
	var fanOut int
	switch {
	case clusterSize <= 3:
		fanOut = clusterSize - 1 // All other nodes
	case clusterSize <= 10:
		fanOut = 3 // Fixed fan-out
	case clusterSize <= 50:
		fanOut = 2 // Reduced fan-out
	default:
		fanOut = 1 // Single target for large clusters
	}

	// Base messages per round
	messagesPerRound := fanOut
	roundsPerSecond := float64(time.Second) / float64(gossipInterval)

	// Stage 2.5: Account for retry attempts (assume 8% failure rate with 1 retry)
	retryFactor := 1.08

	// Stage 2.5: Account for CACHE_SYNC messages (roughly 10-20% of gossip messages)
	cacheSyncFactor := 1.15

	return int(float64(messagesPerRound) * roundsPerSecond * retryFactor * cacheSyncFactor)
}
