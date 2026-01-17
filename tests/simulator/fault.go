// Package simulator provides fault injection for testing fault tolerance.
// This file contains fault injection utilities.
package simulator

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// FaultInjector manages fault injection for testing
type FaultInjector struct {
	simulator *Simulator
	mu        sync.Mutex
	failedNodes map[int]bool
}

// NewFaultInjector creates a new fault injector
func NewFaultInjector(simulator *Simulator) *FaultInjector {
	return &FaultInjector{
		simulator:   simulator,
		failedNodes: make(map[int]bool),
	}
}

// InjectNodeFailure simulates a node failure by shutting it down
func (fi *FaultInjector) InjectNodeFailure(t *testing.T, nodeIndex int) error {
	fi.mu.Lock()
	defer fi.mu.Unlock()

	nodes := fi.simulator.GetNodes()
	if nodeIndex < 0 || nodeIndex >= len(nodes) {
		return fmt.Errorf("invalid node index: %d", nodeIndex)
	}

	if nodes[nodeIndex] == nil {
		return fmt.Errorf("node %d is already nil", nodeIndex)
	}

	// Shutdown the node
	if err := nodes[nodeIndex].Close(5 * time.Second); err != nil {
		return fmt.Errorf("failed to shutdown node %d: %w", nodeIndex, err)
	}

	fi.failedNodes[nodeIndex] = true
	t.Logf("🔴 Injected failure: node %d shutdown", nodeIndex)
	return nil
}

// RestoreNodeFailure restores a failed node (not implemented - nodes can't be restarted)
func (fi *FaultInjector) RestoreNodeFailure(nodeIndex int) error {
	// GridKV doesn't support node restart in current implementation
	return fmt.Errorf("node restart not supported")
}

// GetFailedNodes returns list of failed node indices
func (fi *FaultInjector) GetFailedNodes() []int {
	fi.mu.Lock()
	defer fi.mu.Unlock()

	failed := make([]int, 0, len(fi.failedNodes))
	for idx := range fi.failedNodes {
		failed = append(failed, idx)
	}
	return failed
}

// TestFaultToleranceWithFailures tests fault tolerance by injecting failures
func TestFaultToleranceWithFailures(t *testing.T, sim *Simulator, failureCount int) {
	if failureCount <= 0 {
		return
	}

	nodes := sim.GetNodes()
	if failureCount >= len(nodes) {
		t.Fatalf("Cannot fail all nodes: %d failures requested for %d nodes", failureCount, len(nodes))
	}

	// Pre-populate some data before failures
	ctx := context.Background()
	testKeys := make([]string, 20)
	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("fault-test-key-%d", i)
		value := []byte(fmt.Sprintf("fault-test-value-%d", i))
		nodeIdx := i % len(nodes)
		if err := nodes[nodeIdx].Set(ctx, key, value); err == nil {
			testKeys[i] = key
		}
	}

	// Wait for replication
	time.Sleep(2 * time.Second)

	// Inject failures
	injector := NewFaultInjector(sim)
	for i := 0; i < failureCount; i++ {
		if err := injector.InjectNodeFailure(t, i); err != nil {
			t.Logf("Warning: failed to inject failure for node %d: %v", i, err)
		}
	}

	// Wait for cluster to adapt
	time.Sleep(3 * time.Second)

	// Test data availability after failures
	criteria := GetCriteria(TargetFaultTolerance)
	accessibleCount := 0
	for _, key := range testKeys {
		if key == "" {
			continue
		}
		// Try to read from surviving nodes
		for i := failureCount; i < len(nodes); i++ {
			if nodes[i] != nil {
				if _, err := nodes[i].Get(ctx, key); err == nil {
					accessibleCount++
					break
				}
			}
		}
	}

	accessibilityRate := float64(accessibleCount) / float64(len(testKeys))
	if accessibilityRate < criteria.MinReplicationRate {
		t.Errorf("❌ Data accessibility %.1f%% < required %.1f%% after failures",
			accessibilityRate*100, criteria.MinReplicationRate*100)
	} else {
		t.Logf("✅ Data accessibility: %.1f%% (required: %.1f%%) after %d failures",
			accessibilityRate*100, criteria.MinReplicationRate*100, failureCount)
	}
}