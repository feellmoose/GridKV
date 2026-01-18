// Package simulator defines test targets and validation criteria for GridKV testing.
// This file contains test target definitions based on system guarantees.
package simulator

import "time"

// TestTarget defines what aspect of the system is being tested
type TestTarget string

const (
	// TargetConsistency - eventual consistency guarantee
	TargetConsistency TestTarget = "consistency"
	// TargetReplication - data replication correctness
	TargetReplication TestTarget = "replication"
	// TargetFaultTolerance - system resilience under failures
	TargetFaultTolerance TestTarget = "fault_tolerance"
	// TargetPerformance - throughput and latency
	TargetPerformance TestTarget = "performance"
	// TargetConflictResolution - LWW conflict resolution
	TargetConflictResolution TestTarget = "conflict_resolution"
	// TargetExtremeStress - extreme load stress test with relaxed success rate
	TargetExtremeStress TestTarget = "extreme_stress"
)

// ValidationCriteria defines pass/fail criteria for test targets
type ValidationCriteria struct {
	MinSuccessRate     float64       // Minimum operation success rate (0.0-1.0)
	MinConsistencyRate float64       // Minimum consistency rate (0.0-1.0)
	MinReplicationRate float64       // Minimum replication rate (0.0-1.0)
	MinQPS             float64       // Minimum QPS threshold
	MaxLatencyP99      time.Duration // Maximum P99 latency
}

// GetCriteria returns validation criteria for a test target
func GetCriteria(target TestTarget) ValidationCriteria {
	switch target {
	case TargetConsistency:
		return ValidationCriteria{
			MinSuccessRate:     0.90,
			MinConsistencyRate: 0.65, // 65% eventual consistency acceptable for high-concurrency async replication
			MinReplicationRate: 0.80,
			MinQPS:             100,
		}
	case TargetReplication:
		return ValidationCriteria{
			MinSuccessRate:     0.95,
			MinConsistencyRate: 0.90,
			MinReplicationRate: 0.90, // 90% replication required
			MinQPS:             50,
		}
	case TargetFaultTolerance:
		return ValidationCriteria{
			MinSuccessRate:     0.80, // Lower during failures
			MinConsistencyRate: 0.70, // Lower during failures
			MinReplicationRate: 0.75,
			MinQPS:             50,
		}
	case TargetPerformance:
		return ValidationCriteria{
			MinSuccessRate:     0.85, // Slightly lower to account for eventual consistency under high load
			MinConsistencyRate: 0.80,
			MinReplicationRate: 0.80,
			MinQPS:             1000, // Higher QPS requirement
			MaxLatencyP99:      100 * time.Millisecond,
		}
	case TargetConflictResolution:
		return ValidationCriteria{
			MinSuccessRate:     0.95,
			MinConsistencyRate: 0.95, // High consistency for conflict resolution
			MinReplicationRate: 0.90,
			MinQPS:             100,
		}
	case TargetExtremeStress:
		return ValidationCriteria{
			MinSuccessRate:     0.35, // Lower success rate acceptable for extreme stress (eventual consistency)
			MinConsistencyRate: 0.95, // High consistency required (final state correctness)
			MinReplicationRate: 0.90,
			MinQPS:             5000, // Higher QPS requirement for extreme stress
		}
	default:
		return ValidationCriteria{
			MinSuccessRate:     0.90,
			MinConsistencyRate: 0.80,
			MinReplicationRate: 0.80,
			MinQPS:             100,
		}
	}
}
