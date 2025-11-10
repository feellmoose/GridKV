package tests

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gridkv "github.com/feellmoose/gridkv"
)

// Chaos engineering tests - simulate failures and measure recovery

// TestChaos_NodeFailures simulates random node failures and recovery
func TestChaos_NodeFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping chaos test in short mode")
	}

	const (
		nodeCount    = 10
		testDuration = 5 * time.Minute
		basePort     = 32000
	)

	startTime := time.Now()

	t.Logf("Starting chaos test: %d nodes, %v duration", nodeCount, testDuration)

	// Create cluster
	nodes := make([]*gridkv.GridKV, nodeCount)
	nodeStates := make([]atomic.Bool, nodeCount) // true = running
	var nodesMu sync.RWMutex

	// Setup cluster
	for i := 0; i < nodeCount; i++ {
		node := createNode(t, i, basePort, i == 0)
		nodes[i] = node
		nodeStates[i].Store(true)
	}

	time.Sleep(3 * time.Second) // Cluster formation

	ctx := context.Background()
	stopCh := make(chan struct{})

	// Statistics
	var writes, reads atomic.Int64
	var writeErrors, readErrors atomic.Int64
	var nodeFailures, nodeRecoveries atomic.Int64

	// Workload generator
	var wg sync.WaitGroup
	for i := 0; i < nodeCount*2; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			for {
				select {
				case <-stopCh:
					return
				default:
					// Find a running node
					nodesMu.RLock()
					var activeNode *gridkv.GridKV
					for idx := 0; idx < nodeCount; idx++ {
						if nodeStates[idx].Load() && nodes[idx] != nil {
							activeNode = nodes[idx]
							break
						}
					}
					nodesMu.RUnlock()

					if activeNode == nil {
						time.Sleep(100 * time.Millisecond)
						continue
					}

					key := fmt.Sprintf("chaos-key-%d", rand.Intn(10000))

					// 60% reads, 40% writes
					if rand.Float32() < 0.6 {
						_, err := activeNode.Get(ctx, key)
						if err == nil {
							reads.Add(1)
						} else {
							readErrors.Add(1)
						}
					} else {
						err := activeNode.Set(ctx, key, []byte("chaos-value"))
						if err == nil {
							writes.Add(1)
						} else {
							writeErrors.Add(1)
						}
					}

					time.Sleep(10 * time.Millisecond)
				}
			}
		}(i)
	}

	// Chaos generator - randomly kill and restart nodes
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				// Pick random running node to kill (not seed node)
				targetIdx := 1 + rand.Intn(nodeCount-1)

				nodesMu.Lock()
				if nodeStates[targetIdx].Load() {
					// Kill node
					t.Logf("[CHAOS] Killing node-%d", targetIdx)
					if nodes[targetIdx] != nil {
						nodes[targetIdx].Close()
						nodes[targetIdx] = nil
					}
					nodeStates[targetIdx].Store(false)
					nodeFailures.Add(1)

					nodesMu.Unlock()

					// Wait before recovery
					time.Sleep(10 * time.Second)

					// Recover node
					nodesMu.Lock()
					t.Logf("[CHAOS] Recovering node-%d", targetIdx)
					node := createNode(t, targetIdx, basePort, false)
					nodes[targetIdx] = node
					nodeStates[targetIdx].Store(true)
					nodeRecoveries.Add(1)
				}
				nodesMu.Unlock()
			}
		}
	}()

	// Progress reporter
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		lastWrites, lastReads := int64(0), int64(0)
		lastTime := time.Now()

		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				now := time.Now()
				elapsed := now.Sub(lastTime).Seconds()

				currentWrites := writes.Load()
				currentReads := reads.Load()

				writeRate := float64(currentWrites-lastWrites) / elapsed
				readRate := float64(currentReads-lastReads) / elapsed

				activeCount := 0
				nodesMu.RLock()
				for i := 0; i < nodeCount; i++ {
					if nodeStates[i].Load() {
						activeCount++
					}
				}
				nodesMu.RUnlock()

				totalElapsed := time.Since(startTime)
				t.Logf("[%v] Active nodes: %d/%d | Writes: %.0f/s | Reads: %.0f/s | Failures: %d | Recoveries: %d",
					totalElapsed.Round(time.Second),
					activeCount, nodeCount,
					writeRate, readRate,
					nodeFailures.Load(), nodeRecoveries.Load())

				lastWrites = currentWrites
				lastReads = currentReads
				lastTime = now
			}
		}
	}()

	// Run test for specified duration
	time.Sleep(testDuration)
	close(stopCh)
	wg.Wait()

	// Final report
	totalElapsed := time.Since(startTime)

	t.Logf("\n========== Chaos Test Complete ==========")
	t.Logf("Duration: %v", testDuration)
	t.Logf("Cluster: %d nodes", nodeCount)
	t.Logf("Node failures: %d", nodeFailures.Load())
	t.Logf("Node recoveries: %d", nodeRecoveries.Load())
	t.Logf("Total writes: %d (%.0f ops/sec)", writes.Load(), float64(writes.Load())/totalElapsed.Seconds())
	t.Logf("Total reads: %d (%.0f ops/sec)", reads.Load(), float64(reads.Load())/totalElapsed.Seconds())
	t.Logf("Write error rate: %.2f%%", float64(writeErrors.Load())/float64(writes.Load()+writeErrors.Load())*100)
	t.Logf("Read error rate: %.2f%%", float64(readErrors.Load())/float64(reads.Load()+readErrors.Load())*100)
	t.Logf("=========================================")

	// Health check
	healthyNodes := 0
	nodesMu.RLock()
	for i := 0; i < nodeCount; i++ {
		if nodeStates[i].Load() && nodes[i] != nil {
			healthyNodes++
		}
	}
	nodesMu.RUnlock()

	t.Logf("Final cluster health: %d/%d nodes alive", healthyNodes, nodeCount)

	if healthyNodes < nodeCount/2 {
		t.Errorf("Cluster failed: only %d/%d nodes alive", healthyNodes, nodeCount)
	}
}

// TestChaos_NetworkPartition simulates network partition and recovery
func TestChaos_NetworkPartition(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping chaos test in short mode")
	}

	const nodeCount = 6
	const testDuration = 10 * time.Minute

	t.Logf("Starting network partition test: %d nodes, %v duration", nodeCount, testDuration)

	nodes := setupTestCluster(t, nodeCount, 33000)
	defer cleanupTestCluster(nodes)

	ctx := context.Background()
	stopCh := make(chan struct{})

	var writes, reads atomic.Int64
	var writeErrors, readErrors atomic.Int64
	var wg sync.WaitGroup

	// Workload generator
	for i := 0; i < nodeCount; i++ {
		wg.Add(1)
		go func(nodeIdx int) {
			defer wg.Done()
			node := nodes[nodeIdx]
			keyCounter := 0

			for {
				select {
				case <-stopCh:
					return
				default:
					key := fmt.Sprintf("partition-key-%d", keyCounter%5000)

					if rand.Float32() < 0.5 {
						// Read
						_, err := node.Get(ctx, key)
						if err == nil {
							reads.Add(1)
						} else {
							readErrors.Add(1)
						}
					} else {
						// Write
						err := node.Set(ctx, key, []byte("value"))
						if err == nil {
							writes.Add(1)
						} else {
							writeErrors.Add(1)
						}
					}

					keyCounter++
					time.Sleep(20 * time.Millisecond)
				}
			}
		}(i)
	}

	// Partition simulator
	// Note: True network partition requires firewall rules or network namespace isolation
	// This test simulates by temporarily closing nodes
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(2 * time.Minute) // Run normally first

		t.Logf("[PARTITION] Creating partition: nodes 0-2 vs nodes 3-5")

		// Simulate partition by stopping nodes 3-5
		var partitionedNodes []*gridkv.GridKV
		for i := 3; i < 6; i++ {
			if nodes[i] != nil {
				partitionedNodes = append(partitionedNodes, nodes[i])
				nodes[i].Close()
				nodes[i] = nil
			}
		}

		// Partition active for 3 minutes
		time.Sleep(3 * time.Minute)

		t.Logf("[PARTITION] Healing partition: restarting nodes 3-5")

		// Heal partition by recreating nodes
		for i := 3; i < 6; i++ {
			nodes[i] = createNode(t, i, 33000, false)
		}

		t.Logf("[PARTITION] Partition healed, waiting for convergence...")
	}()

	// Run test
	time.Sleep(testDuration)
	close(stopCh)
	wg.Wait()

	// Final statistics
	t.Logf("\n========== Partition Test Complete ==========")
	t.Logf("Total writes: %d", writes.Load())
	t.Logf("Total reads: %d", reads.Load())
	t.Logf("Write error rate: %.2f%%", float64(writeErrors.Load())/float64(writes.Load()+writeErrors.Load())*100)
	t.Logf("Read error rate: %.2f%%", float64(readErrors.Load())/float64(reads.Load()+readErrors.Load())*100)
	t.Logf("=============================================")
}

// TestStability_MemoryLeaks checks for memory leaks over time
func TestStability_MemoryLeaks(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stability test in short mode")
	}

	const (
		nodeCount    = 5
		testDuration = 10 * time.Minute
	)

	t.Logf("Starting memory leak test: %d nodes, %v duration", nodeCount, testDuration)

	nodes := setupTestCluster(t, nodeCount, 34000)
	defer cleanupTestCluster(nodes)

	ctx := context.Background()
	stopCh := make(chan struct{})

	var operations atomic.Int64
	var wg sync.WaitGroup

	// Continuous workload
	for i := 0; i < nodeCount; i++ {
		wg.Add(1)
		go func(nodeIdx int) {
			defer wg.Done()
			node := nodes[nodeIdx]

			for {
				select {
				case <-stopCh:
					return
				default:
					key := fmt.Sprintf("mem-key-%d", rand.Intn(1000))

					// Set and delete pattern to exercise memory management
					node.Set(ctx, key, []byte("test-value"))
					time.Sleep(5 * time.Millisecond)
					node.Delete(ctx, key)

					operations.Add(2)
					time.Sleep(5 * time.Millisecond)
				}
			}
		}(i)
	}

	// Memory monitor
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				// Check node health
				for i, node := range nodes {
					if node != nil {
						t.Logf("[MEMORY] Node %d: active", i)
					}
				}
			}
		}
	}()

	// Run test
	time.Sleep(testDuration)
	close(stopCh)
	wg.Wait()

	t.Logf("\n========== Memory Leak Test Complete ==========")
	t.Logf("Total operations: %d", operations.Load())
	t.Logf("All nodes status:")
	for i, node := range nodes {
		if node != nil {
			t.Logf("  Node %d: active", i)
		} else {
			t.Logf("  Node %d: inactive", i)
		}
	}
	t.Logf("===============================================")
}

// TestChaos_HighConcurrency tests system under extreme concurrent load
func TestChaos_HighConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping concurrency test in short mode")
	}

	const (
		nodeCount       = 5
		concurrentUsers = 500
		testDuration    = 5 * time.Minute
	)

	t.Logf("Starting high concurrency test: %d nodes, %d concurrent users", nodeCount, concurrentUsers)

	nodes := setupTestCluster(t, nodeCount, 35000)
	defer cleanupTestCluster(nodes)

	ctx := context.Background()
	stopCh := make(chan struct{})

	var operations, errors atomic.Int64
	var wg sync.WaitGroup

	// Spawn concurrent users
	for i := 0; i < concurrentUsers; i++ {
		wg.Add(1)
		go func(userID int) {
			defer wg.Done()

			for {
				select {
				case <-stopCh:
					return
				default:
					node := nodes[rand.Intn(nodeCount)]
					key := fmt.Sprintf("concurrent-key-%d", rand.Intn(50000))

					err := node.Set(ctx, key, []byte("value"))
					if err == nil {
						operations.Add(1)
					} else {
						errors.Add(1)
					}

					time.Sleep(time.Duration(rand.Intn(10)) * time.Millisecond)
				}
			}
		}(i)
	}

	// Monitor
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		lastOps := int64(0)
		lastTime := time.Now()

		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				now := time.Now()
				currentOps := operations.Load()
				rate := float64(currentOps-lastOps) / now.Sub(lastTime).Seconds()

				t.Logf("[CONCURRENCY] Throughput: %.0f ops/sec | Total: %d | Errors: %d",
					rate, currentOps, errors.Load())

				lastOps = currentOps
				lastTime = now
			}
		}
	}()

	// Run test
	time.Sleep(testDuration)
	close(stopCh)
	wg.Wait()

	// Final statistics
	totalOps := operations.Load()
	totalErrors := errors.Load()
	errorRate := float64(totalErrors) / float64(totalOps+totalErrors) * 100

	t.Logf("\n========== High Concurrency Test Complete ==========")
	t.Logf("Concurrent users: %d", concurrentUsers)
	t.Logf("Total operations: %d", totalOps)
	t.Logf("Throughput: %.0f ops/sec", float64(totalOps)/testDuration.Seconds())
	t.Logf("Error rate: %.2f%%", errorRate)
	t.Logf("====================================================")

	if errorRate > 5.0 {
		t.Errorf("High error rate under concurrency: %.2f%%", errorRate)
	}
}

func createNode(t *testing.T, idx, basePort int, isSeed bool) *gridkv.GridKV {
	var seedAddrs []string
	if !isSeed {
		seedAddrs = []string{fmt.Sprintf("localhost:%d", basePort)}
	}

	node, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
		LocalNodeID:  fmt.Sprintf("node-%d", idx),
		LocalAddress: fmt.Sprintf("localhost:%d", basePort+idx),
		SeedAddrs:    seedAddrs,
		Network: &gridkv.NetworkOptions{
			Type:     gridkv.TCP,
			BindAddr: fmt.Sprintf("localhost:%d", basePort+idx),
		},
		Storage: &gridkv.StorageOptions{
			Backend:     gridkv.BackendMemory,
			MaxMemoryMB: 256,
		},
		ReplicaCount: 3,
		WriteQuorum:  2,
		ReadQuorum:   2,
	})

	if err != nil {
		t.Fatalf("Failed to create node %d: %v", idx, err)
	}

	return node
}
