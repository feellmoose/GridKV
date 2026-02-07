// Package tests provides stress testing for GridKV distributed system
package tests

import (
	"os"
	"testing"
	"time"

	"github.com/feellmoose/gridkv/tests/simulator"
)

// shouldRunStressTests returns true if long-running stress tests are enabled via env.
// By default we skip them to keep "go test ./..." fast and within the default timeout.
func shouldRunStressTests() bool {
	return os.Getenv("RUN_STRESS_TESTS") == "1"
}

// TestLargeScaleStress runs large-scale cluster stress test
func TestLargeScaleStress(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping large-scale stress test in short mode")
	}
	if !shouldRunStressTests() {
		t.Skip("Skipping large-scale stress test; set RUN_STRESS_TESTS=1 to enable")
	}

	// Use the predefined large-scale stress test configuration
	simulator.RunTestSuite(t, simulator.LargeScaleStressTest)
}

// TestLongRunningStress runs extended duration stress test (5 minutes)
func TestLongRunningStress(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping long-running stress test in short mode")
	}
	if !shouldRunStressTests() {
		t.Skip("Skipping long-running stress test; set RUN_STRESS_TESTS=1 to enable")
	}

	// Extended duration test for stability validation
	config := simulator.TestSuiteConfig{
		Name:         "Long Running Stress",
		Target:       simulator.TargetPerformance,
		NodeCount:    10,
		ReplicaCount: 3,
		WorkerCount:  50,
		Duration:     5 * time.Minute,
		WriteRatio:   0.3,
		KeySpaceSize: 10000,
		ValueSize:    256,
	}

	simulator.RunTestSuite(t, config)
}

// TestExtendedLongRunningStress runs extended 10-minute stress test
func TestExtendedLongRunningStress(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping extended long-running stress test in short mode")
	}
	if !shouldRunStressTests() {
		t.Skip("Skipping extended long-running stress test; set RUN_STRESS_TESTS=1 to enable")
	}

	// 10-minute extended duration test for stability and reliability validation
	config := simulator.TestSuiteConfig{
		Name:         "Extended Long Running Stress",
		Target:       simulator.TargetPerformance,
		NodeCount:    10,
		ReplicaCount: 3,
		WorkerCount:  50,
		Duration:     10 * time.Minute,
		WriteRatio:   0.3,
		KeySpaceSize: 10000,
		ValueSize:    256,
	}

	simulator.RunTestSuite(t, config)
}

// TestHighConcurrencyStress tests with high worker count
func TestHighConcurrencyStress(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping high concurrency stress test in short mode")
	}
	if !shouldRunStressTests() {
		t.Skip("Skipping high concurrency stress test; set RUN_STRESS_TESTS=1 to enable")
	}

	config := simulator.TestSuiteConfig{
		Name:         "High Concurrency Stress",
		Target:       simulator.TargetPerformance,
		NodeCount:    5,
		ReplicaCount: 3,
		WorkerCount:  200, // High concurrency
		Duration:     1 * time.Minute,
		WriteRatio:   0.4,
		KeySpaceSize: 5000,
		ValueSize:    128,
	}

	simulator.RunTestSuite(t, config)
}

// TestVeryLargeClusterStress runs 20-node cluster for 10 minutes
func TestVeryLargeClusterStress(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping very large cluster stress test in short mode")
	}
	if !shouldRunStressTests() {
		t.Skip("Skipping very large cluster stress test; set RUN_STRESS_TESTS=1 to enable")
	}

	// 20-node cluster with 10-minute duration for maximum scale testing
	config := simulator.TestSuiteConfig{
		Name:         "Very Large Cluster Stress",
		Target:       simulator.TargetPerformance,
		NodeCount:    20,
		ReplicaCount: 3,
		WorkerCount:  80, // Moderate workers for larger cluster
		Duration:     10 * time.Minute,
		WriteRatio:   0.3,
		KeySpaceSize: 20000, // Larger key space for more nodes
		ValueSize:    256,
	}

	simulator.RunTestSuite(t, config)
}

// TestStabilityLongRun runs 10-node cluster for stability validation.
// Duration and workload can be tuned via env vars while keeping node/replica counts:
//
//	TEST_DURATION       - test duration (default: "30m", e.g. "6h", "2h30m")
//	TEST_WORKER_COUNT   - concurrent workers (default: 50)
//	TEST_KEYSPACE_SIZE  - key space size (default: 10000)
//	TEST_VALUE_SIZE     - value size in bytes (default: 256)
func TestStabilityLongRun(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stability test in short mode")
	}
	if !shouldRunStressTests() {
		t.Skip("Skipping stability long run test; set RUN_STRESS_TESTS=1 to enable")
	}

	// Get duration and workload parameters from environment or use defaults
	duration := simulator.GetEnvDuration("TEST_DURATION", 30*time.Minute)
	workerCount := simulator.GetEnvInt("TEST_WORKER_COUNT", 50)
	keySpaceSize := simulator.GetEnvInt("TEST_KEYSPACE_SIZE", 10000)
	valueSize := simulator.GetEnvInt("TEST_VALUE_SIZE", 256)

	config := simulator.TestSuiteConfig{
		Name:         "Stability Long Run",
		Target:       simulator.TargetPerformance,
		NodeCount:    10,
		ReplicaCount: 3,
		WorkerCount:  workerCount,
		Duration:     duration,
		WriteRatio:   0.3,
		KeySpaceSize: keySpaceSize,
		ValueSize:    valueSize,
	}

	simulator.RunTestSuite(t, config)
}
