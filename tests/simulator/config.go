// Package simulator provides test configurations for GridKV testing.
// This file contains predefined test configurations.
package simulator

import (
	"time"
)

// TestSuiteConfig defines configuration for different test scenarios
type TestSuiteConfig struct {
	Name         string        // Test suite name
	Target       TestTarget    // Primary test target
	NodeCount    int           // Number of nodes
	ReplicaCount int           // Replication factor
	WorkerCount  int           // Number of concurrent workers
	Duration     time.Duration // Test duration
	WriteRatio   float64       // Write operation ratio (0.0-1.0)
	KeySpaceSize int           // Size of key space
	ValueSize    int           // Size of values
}

// Predefined test configurations for different test scenarios
var (
	// BasicClusterTest - quick validation (consistency target)
	BasicClusterTest = TestSuiteConfig{
		Name:         "Basic Cluster",
		Target:       TargetConsistency,
		NodeCount:    7, // 7 nodes for cluster validation
		ReplicaCount: 3, // 3 replicas for redundancy
		WorkerCount:  10, // 10 concurrent workers
		Duration:     12 * time.Second, // 12 seconds test duration
		WriteRatio:   0.8,
		KeySpaceSize: 1000, // 1000 keys for testing
		ValueSize:    128,
	}

	// ReplicationTest - replication correctness
	ReplicationTest = TestSuiteConfig{
		Name:         "Replication",
		Target:       TargetReplication,
		NodeCount:    7, // 7 nodes for replication testing
		ReplicaCount: 3,
		WorkerCount:  15, // 15 concurrent workers
		Duration:     15 * time.Second,
		WriteRatio:   1.0, // 100% writes for replication testing
		KeySpaceSize: 2000, // 2000 keys for replication validation
		ValueSize:    256,
	}

	// PerformanceTest - performance validation
	PerformanceTest = TestSuiteConfig{
		Name:         "Performance",
		Target:       TargetPerformance,
		NodeCount:    7, // 7 nodes for performance testing
		ReplicaCount: 3, // 3 replicas
		WorkerCount:  30, // 30 concurrent workers for load testing
		Duration:     15 * time.Second,
		WriteRatio:   0.7,
		KeySpaceSize: 3000, // 3000 keys for performance testing
		ValueSize:    256,
	}

	// FaultToleranceTest - fault tolerance validation
	FaultToleranceTest = TestSuiteConfig{
		Name:         "Fault Tolerance",
		Target:       TargetFaultTolerance,
		NodeCount:    9, // 9 nodes for fault tolerance testing
		ReplicaCount: 3,
		WorkerCount:  20, // 20 concurrent workers
		Duration:     20 * time.Second,
		WriteRatio:   0.7,
		KeySpaceSize: 3000, // 3000 keys for fault tolerance testing
		ValueSize:    256,
	}

	// LargeScaleStressTest - large-scale cluster stress test with relaxed success rate
	LargeScaleStressTest = TestSuiteConfig{
		Name:         "Large Scale Stress",
		Target:       TargetExtremeStress,
		NodeCount:    10,
		ReplicaCount: 3,
		WorkerCount:  100,
		Duration:     2 * time.Minute,
		WriteRatio:   0.3,
		KeySpaceSize: 20000,
		ValueSize:    256,
	}
)