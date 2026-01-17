// Package tests provides stress testing for GridKV distributed system
package tests

import (
	"testing"
	"time"

	"github.com/feellmoose/gridkv/tests/simulator"
)

// TestLargeScaleStress runs large-scale cluster stress test
func TestLargeScaleStress(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping large-scale stress test in short mode")
	}

	// Use the predefined large-scale stress test configuration
	simulator.RunTestSuite(t, simulator.LargeScaleStressTest)
}

// TestLongRunningStress runs extended duration stress test (5 minutes)
func TestLongRunningStress(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping long-running stress test in short mode")
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

// TestStabilityLongRun runs 10-node cluster for 30 minutes for stability validation
func TestStabilityLongRun(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping 30-minute stability test in short mode")
	}

	// 30-minute extended duration test for stability and reliability validation
	config := simulator.TestSuiteConfig{
		Name:         "Stability Long Run",
		Target:       simulator.TargetPerformance,
		NodeCount:    10,
		ReplicaCount: 3,
		WorkerCount:  50,
		Duration:     30 * time.Minute,
		WriteRatio:   0.3,
		KeySpaceSize: 10000,
		ValueSize:    256,
	}

	simulator.RunTestSuite(t, config)
}
