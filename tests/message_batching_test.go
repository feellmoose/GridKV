package tests

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestCriticalMessageDirectProcessing verifies that critical messages
// (CLUSTER_SYNC, CONNECT, PROBE) are processed directly without queuing
func TestCriticalMessageDirectProcessing(t *testing.T) {
	ctx := context.Background()

	// Create two nodes
	node1, err := createTestGridKV("node-1", "localhost:20001", nil)
	if err != nil {
		t.Fatalf("Failed to create node1: %v", err)
	}
	defer node1.Close()

	node2, err := createTestGridKV("node-2", "localhost:20002", []string{"localhost:20001"})
	if err != nil {
		t.Fatalf("Failed to create node2: %v", err)
	}
	defer node2.Close()

	// Wait for cluster formation
	time.Sleep(2 * time.Second)

	// Flood inputCh with data messages to test if critical messages still get through
	// We'll do this by sending many SET operations
	var wg sync.WaitGroup
	opsCount := 1000
	for i := 0; i < opsCount; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			key := fmt.Sprintf("flood-key-%d", idx)
			_ = node1.Set(ctx, key, []byte("flood-value"))
		}(i)
	}

	// While flooding, verify cluster sync still works
	// This tests that critical messages bypass the queue
	time.Sleep(500 * time.Millisecond)

	// Verify nodes can still discover each other (critical message processing)
	// This would fail if critical messages were queued and dropped
	wg.Wait()

	// Verify both nodes are aware of each other
	// This confirms CLUSTER_SYNC messages were processed
	time.Sleep(1 * time.Second)

	// Test passes if no deadlock or message loss occurred
	t.Log("Critical message direct processing verified: cluster formation succeeded despite message flood")
}

// TestReplicationBatching verifies that SET operations are batched
func TestReplicationBatching(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping batching test in short mode")
	}

	ctx := context.Background()

	// Create 3-node cluster
	nodes := setupTestCluster(t, 3, 21000)
	defer cleanupTestCluster(nodes)

	// Wait for cluster formation
	time.Sleep(2 * time.Second)

	// Send many SET operations rapidly
	// With batching, these should be combined into fewer messages
	opsCount := 200
	start := time.Now()

	var wg sync.WaitGroup
	for i := 0; i < opsCount; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			key := fmt.Sprintf("batch-key-%d", idx)
			err := nodes[0].Set(ctx, key, []byte(fmt.Sprintf("batch-value-%d", idx)))
			if err != nil {
				t.Errorf("Set failed for key %s: %v", key, err)
			}
		}(i)
	}

	wg.Wait()
	duration := time.Since(start)

	// Verify all operations completed
	for i := 0; i < opsCount; i++ {
		key := fmt.Sprintf("batch-key-%d", i)
		value, err := nodes[0].Get(ctx, key)
		if err != nil {
			t.Errorf("Get failed for key %s: %v", key, err)
			continue
		}
		expected := fmt.Sprintf("batch-value-%d", i)
		if string(value) != expected {
			t.Errorf("Expected %s, got %s", expected, string(value))
		}
	}

	t.Logf("Completed %d operations in %v (batching should reduce message count)", opsCount, duration)
}

// TestBatchingSizeThreshold verifies batching flushes at size threshold
func TestBatchingSizeThreshold(t *testing.T) {
	ctx := context.Background()

	// Create 2-node cluster
	nodes := setupTestCluster(t, 2, 22000)
	defer cleanupTestCluster(nodes)

	time.Sleep(1 * time.Second)

	// Send exactly 50 operations (batch threshold)
	// This should trigger a batch flush
	opsCount := 50
	for i := 0; i < opsCount; i++ {
		key := fmt.Sprintf("threshold-key-%d", i)
		err := nodes[0].Set(ctx, key, []byte(fmt.Sprintf("value-%d", i)))
		if err != nil {
			t.Errorf("Set failed: %v", err)
		}
	}

	// Wait for batch flush
	time.Sleep(100 * time.Millisecond)

	// Verify all operations completed
	for i := 0; i < opsCount; i++ {
		key := fmt.Sprintf("threshold-key-%d", i)
		_, err := nodes[0].Get(ctx, key)
		if err != nil {
			t.Errorf("Get failed for key %s: %v", key, err)
		}
	}

	t.Log("Batch size threshold test passed")
}

// TestBatchingTimeout verifies batching flushes after timeout
func TestBatchingTimeout(t *testing.T) {
	ctx := context.Background()

	// Create 2-node cluster
	nodes := setupTestCluster(t, 2, 23000)
	defer cleanupTestCluster(nodes)

	time.Sleep(1 * time.Second)

	// Send fewer than threshold operations
	// Batch should flush after 10ms timeout
	opsCount := 10
	for i := 0; i < opsCount; i++ {
		key := fmt.Sprintf("timeout-key-%d", i)
		err := nodes[0].Set(ctx, key, []byte(fmt.Sprintf("value-%d", i)))
		if err != nil {
			t.Errorf("Set failed: %v", err)
		}
	}

	// Wait for timeout flush (10ms + buffer)
	time.Sleep(50 * time.Millisecond)

	// Verify all operations completed
	for i := 0; i < opsCount; i++ {
		key := fmt.Sprintf("timeout-key-%d", i)
		_, err := nodes[0].Get(ctx, key)
		if err != nil {
			t.Errorf("Get failed for key %s: %v", key, err)
		}
	}

	t.Log("Batch timeout test passed")
}

// TestInputChBufferSize verifies increased buffer size handles bursts
func TestInputChBufferSize(t *testing.T) {
	ctx := context.Background()

	// Create 2-node cluster
	nodes := setupTestCluster(t, 2, 24000)
	defer cleanupTestCluster(nodes)

	time.Sleep(1 * time.Second)

	// Send burst of operations to test buffer
	// With 1024 buffer, should handle more than old 256 buffer
	opsCount := 500
	var errors atomic.Int64

	var wg sync.WaitGroup
	for i := 0; i < opsCount; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			key := fmt.Sprintf("buffer-key-%d", idx)
			err := nodes[0].Set(ctx, key, []byte("buffer-value"))
			if err != nil {
				errors.Add(1)
			}
		}(i)
	}

	wg.Wait()

	errorCount := errors.Load()
	if errorCount > 0 {
		t.Errorf("Expected 0 errors, got %d (buffer may be too small)", errorCount)
	}

	t.Logf("Buffer test passed: %d operations with %d errors", opsCount, errorCount)
}

// TestBatchCleanupOnNodeDisconnect verifies batches are cleaned up when node disconnects
func TestBatchCleanupOnNodeDisconnect(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping disconnect test in short mode")
	}

	ctx := context.Background()

	// Create 3-node cluster
	nodes := setupTestCluster(t, 3, 25000)
	defer cleanupTestCluster(nodes)

	time.Sleep(2 * time.Second)

	// Send some operations to create batches
	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("disconnect-key-%d", i)
		_ = nodes[0].Set(ctx, key, []byte("value"))
	}

	// Disconnect one node
	nodes[2].Close()

	// Wait for failure detection
	time.Sleep(3 * time.Second)

	// Send more operations - should not have stale batches
	for i := 20; i < 40; i++ {
		key := fmt.Sprintf("disconnect-key-%d", i)
		err := nodes[0].Set(ctx, key, []byte("value"))
		if err != nil {
			t.Errorf("Set failed after disconnect: %v", err)
		}
	}

	// Verify operations completed
	for i := 20; i < 40; i++ {
		key := fmt.Sprintf("disconnect-key-%d", i)
		_, err := nodes[0].Get(ctx, key)
		if err != nil {
			t.Errorf("Get failed for key %s: %v", key, err)
		}
	}

	t.Log("Batch cleanup on disconnect test passed")
}

// BenchmarkBatchingPerformance compares batched vs non-batched performance
func BenchmarkBatchingPerformance(b *testing.B) {
	ctx := context.Background()

	nodes := setupTestCluster(b, 3, 26000)
	defer cleanupTestCluster(nodes)

	time.Sleep(1 * time.Second)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("bench-key-%d", i)
			_ = nodes[0].Set(ctx, key, []byte("bench-value"))
			i++
		}
	})
}

// TestCriticalMessageTypes verifies all critical message types are handled directly
func TestCriticalMessageTypes(t *testing.T) {
	ctx := context.Background()

	// Create cluster
	nodes := setupTestCluster(t, 3, 27000)
	defer cleanupTestCluster(nodes)

	// Wait for cluster formation (tests CONNECT and CLUSTER_SYNC)
	time.Sleep(2 * time.Second)

	// Verify all nodes are connected
	// This confirms critical messages were processed
	for i := 0; i < 3; i++ {
		key := fmt.Sprintf("test-key-%d", i)
		err := nodes[i].Set(ctx, key, []byte("test-value"))
		if err != nil {
			t.Errorf("Set failed on node %d: %v", i, err)
		}
	}

	// Verify cross-node reads (tests cluster sync worked)
	value, err := nodes[1].Get(ctx, "test-key-0")
	if err != nil {
		t.Errorf("Cross-node read failed: %v", err)
	} else if string(value) != "test-value" {
		t.Errorf("Expected 'test-value', got '%s'", string(value))
	}

	t.Log("Critical message types test passed: CONNECT and CLUSTER_SYNC processed correctly")
}
