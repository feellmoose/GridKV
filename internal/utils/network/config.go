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

	// 4. Connection pool sizing
	// MaxConnections: cluster_size × 4 (allows multiple concurrent operations per node)
	// MaxIdle: cluster_size × 2 (keeps warm connections to all nodes)
	config.MaxConnections = clusterSize * 4
	config.MaxIdleConnections = clusterSize * 2

	// Cap maximum connections to prevent resource exhaustion
	if config.MaxConnections > 1000 {
		config.MaxConnections = 1000
	}
	if config.MaxIdleConnections > 200 {
		config.MaxIdleConnections = 200
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
	switch profile {
	case ProfileLAN:
		// LAN: Low latency, fast detection
		return GetConfigForLatency(1*time.Millisecond, clusterSize)
	case ProfileWAN:
		// WAN: Moderate latency, balanced
		return GetConfigForLatency(50*time.Millisecond, clusterSize)
	case ProfileGlobal:
		// Global: High latency, patient timeouts
		return GetConfigForLatency(200*time.Millisecond, clusterSize)
	case ProfileSatellite:
		// Satellite: Very high latency, very patient timeouts
		return GetConfigForLatency(600*time.Millisecond, clusterSize)
	default:
		// Default to WAN profile
		return GetConfigForLatency(10*time.Millisecond, clusterSize)
	}
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

// EstimateGossipLoad estimates the Gossip message load for a given configuration.
// This helps predict network overhead before deployment.
//
// Returns estimated messages per second per node
func EstimateGossipLoad(clusterSize int, gossipInterval time.Duration) int {
	if gossipInterval == 0 {
		return 0
	}

	// Each node gossips to all other nodes
	messagesPerRound := clusterSize
	roundsPerSecond := float64(time.Second) / float64(gossipInterval)

	return int(float64(messagesPerRound) * roundsPerSecond)
}

// RecommendQuorumSettings recommends read/write quorum settings based on
// consistency requirements and cluster size.
type QuorumSettings struct {
	ReplicaCount int
	WriteQuorum  int
	ReadQuorum   int
	Description  string
}

// GetQuorumSettings returns recommended quorum settings for different scenarios
func GetQuorumSettings(scenario string, clusterSize int) *QuorumSettings {
	replicaCount := 3
	if clusterSize < 3 {
		replicaCount = clusterSize
	}

	switch scenario {
	case "strong":
		// Strong consistency: R + W > N
		return &QuorumSettings{
			ReplicaCount: replicaCount,
			WriteQuorum:  2,
			ReadQuorum:   2,
			Description:  "Strong consistency (R+W > N)",
		}

	case "balanced":
		// Balanced: R + W = N
		return &QuorumSettings{
			ReplicaCount: replicaCount,
			WriteQuorum:  2,
			ReadQuorum:   1,
			Description:  "Balanced consistency (R+W = N)",
		}

	case "eventual":
		// Eventual consistency: R + W < N (maximum performance)
		return &QuorumSettings{
			ReplicaCount: replicaCount,
			WriteQuorum:  1,
			ReadQuorum:   1,
			Description:  "Eventual consistency (R+W < N, fastest)",
		}

	default:
		// Default to strong consistency
		return GetQuorumSettings("strong", clusterSize)
	}
}
