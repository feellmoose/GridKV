package tests

import (
	"context"
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"testing"
	"time"

	gridkv "github.com/feellmoose/gridkv"
	"github.com/feellmoose/gridkv/internal/utils/network"
)

// TestUltraLargeCluster tests GridKV with 100+ nodes
// Simulates real large-scale production deployment
func TestUltraLargeCluster(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping ultra-large cluster test in short mode")
	}

	testCases := []struct {
		name           string
		nodeCount      int
		replicaCount   int
		concurrentOps  int
		testDuration   time.Duration
		networkProfile network.NetworkProfile
		expectedMinQPS float64
	}{
		{
			name:           "UltraLarge_100Nodes",
			nodeCount:      100,
			replicaCount:   5,  // 5 replicas for better availability
			concurrentOps:  500, // Controlled concurrency
			testDuration:   120 * time.Second,
			networkProfile: network.ProfileGlobal,
			expectedMinQPS: 1000,
		},
		{
			name:           "UltraLarge_150Nodes",
			nodeCount:      150,
			replicaCount:   7,
			concurrentOps:  600,
			testDuration:   120 * time.Second,
			networkProfile: network.ProfileGlobal,
			expectedMinQPS: 800,
		},
		{
			name:           "UltraLarge_200Nodes",
			nodeCount:      200,
			replicaCount:   7,
			concurrentOps:  800,
			testDuration:   120 * time.Second,
			networkProfile: network.ProfileGlobal,
			expectedMinQPS: 600,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Memory check
			var mem runtime.MemStats
			runtime.ReadMemStats(&mem)
			t.Logf("Starting memory: Alloc=%.2fMB, Sys=%.2fMB",
				float64(mem.Alloc)/1024/1024, float64(mem.Sys)/1024/1024)

			config := &TestEnvironmentConfig{
				NetworkProfile: tc.networkProfile,
				NetworkType:    networkTypeFromEnv(gridkv.TCP),
				NodeCount:      tc.nodeCount,
				ReplicaCount:   tc.replicaCount,
				BasePort:       45000, // New port range to avoid conflicts
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    512, // Lower per-node memory for large cluster
				ShardCount:     64,  // Reduced shards per node
			}

			t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			t.Logf("   Ultra Large Cluster Test: %d nodes", tc.nodeCount)
			t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

			// Setup cluster with progressive initialization
			sim := NewTestEnvironmentSimulator(config)
			
			startTime := time.Now()
			if err := sim.SetupClusterProgressive(t, 10); err != nil {
				t.Fatalf("Failed to setup cluster: %v", err)
			}
			setupDuration := time.Since(startTime)
			
			defer sim.Cleanup()

			t.Logf("✓ Cluster setup completed in %.1fs", setupDuration.Seconds())

			// Check memory after setup
			runtime.ReadMemStats(&mem)
			t.Logf("After setup: Alloc=%.2fMB, Sys=%.2fMB, NumGC=%d",
				float64(mem.Alloc)/1024/1024, float64(mem.Sys)/1024/1024, mem.NumGC)

			// Wait for cluster stabilization (proportional to cluster size)
			stabilizeWait := time.Duration(tc.nodeCount/10) * time.Second
			if stabilizeWait < 10*time.Second {
				stabilizeWait = 10 * time.Second
			}
			if stabilizeWait > 60*time.Second {
				stabilizeWait = 60 * time.Second
			}
			t.Logf("Waiting %.0fs for cluster stabilization...", stabilizeWait.Seconds())
			time.Sleep(stabilizeWait)

			// Run workload
			stats := runUltraLargeClusterWorkload(t, sim, tc.concurrentOps, tc.testDuration)

			// Final memory check
			runtime.ReadMemStats(&mem)
			t.Logf("After test: Alloc=%.2fMB, Sys=%.2fMB, NumGC=%d",
				float64(mem.Alloc)/1024/1024, float64(mem.Sys)/1024/1024, mem.NumGC)

			// Report results
			t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			t.Logf("   Test Results (%s)", tc.name)
			t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			t.Logf("Cluster: %d nodes, %d replicas", tc.nodeCount, tc.replicaCount)
			t.Logf("Setup time: %.1fs", setupDuration.Seconds())
			t.Logf("")
			t.Logf("Writes:  %d submitted / %d completed (%.1f%%)",
				stats.writesSubmitted.Load(), stats.writesCompleted.Load(),
				stats.successRate(stats.writesSubmitted.Load(), stats.writesCompleted.Load()))
			t.Logf("Reads:   %d submitted / %d completed (%.1f%%)",
				stats.readsSubmitted.Load(), stats.readsCompleted.Load(),
				stats.successRate(stats.readsSubmitted.Load(), stats.readsCompleted.Load()))
			t.Logf("Deletes: %d submitted / %d completed (%.1f%%)",
				stats.deletesSubmitted.Load(), stats.deletesCompleted.Load(),
				stats.successRate(stats.deletesSubmitted.Load(), stats.deletesCompleted.Load()))
			t.Logf("")
			t.Logf("Total QPS: %.2f (writes: %.2f, reads: %.2f, deletes: %.2f)",
				stats.completedQPS(), stats.writeQPS(), stats.readQPS(), stats.deleteQPS())
			t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

			// Validate performance
			totalQPS := stats.completedQPS()
			if totalQPS < tc.expectedMinQPS {
				t.Logf("⚠️  QPS below expected minimum (%.2f < %.2f)", totalQPS, tc.expectedMinQPS)
			} else {
				t.Logf("✓ QPS meets expectations (%.2f >= %.2f)", totalQPS, tc.expectedMinQPS)
			}
		})
	}
}

// SetupClusterProgressive sets up large cluster progressively to avoid resource spike
func (tes *TestEnvironmentSimulator) SetupClusterProgressive(tb testing.TB, batchSize int) error {
	tes.mu.Lock()
	defer tes.mu.Unlock()

	latencyConfig := network.GetConfigForProfile(tes.config.NetworkProfile, tes.config.NodeCount)
	tes.nodes = make([]*gridkv.GridKV, tes.config.NodeCount)

	// Create seed node
	tb.Logf("Creating seed node (node-0)...")
	opts := &gridkv.GridKVOptions{
		LocalNodeID:        "node-0",
		LocalAddress:       fmt.Sprintf("localhost:%d", tes.config.BasePort),
		FailureTimeout:     latencyConfig.FailureTimeout,
		SuspectTimeout:     latencyConfig.SuspectTimeout,
		GossipInterval:     latencyConfig.GossipInterval,
		ReplicationTimeout: latencyConfig.ReplicationTimeout,
		ReadTimeout:        latencyConfig.ReadTimeout,
		StartupGracePeriod: 2 * time.Second,
		DisableAuth:        true,
		ReplicaCount:       tes.config.ReplicaCount,
		Network: &gridkv.NetworkOptions{
			Type:     tes.config.NetworkType,
			BindAddr: fmt.Sprintf("localhost:%d", tes.config.BasePort),
			MaxConns: latencyConfig.MaxConnections,
			MaxIdle:  latencyConfig.MaxIdleConnections,
		},
		Storage: &gridkv.StorageOptions{
			Backend:     tes.config.StorageBackend,
			MaxMemoryMB: tes.config.MaxMemoryMB,
			ShardCount:  tes.config.ShardCount,
		},
	}

	var err error
	tes.nodes[0], err = gridkv.NewGridKV(opts)
	if err != nil {
		return fmt.Errorf("failed to create seed node: %w", err)
	}

	time.Sleep(2 * time.Second)

	// Create remaining nodes in batches
	seedAddr := []string{fmt.Sprintf("localhost:%d", tes.config.BasePort)}
	
	for batch := 0; batch < (tes.config.NodeCount-1+batchSize-1)/batchSize; batch++ {
		startIdx := batch*batchSize + 1
		endIdx := startIdx + batchSize
		if endIdx > tes.config.NodeCount {
			endIdx = tes.config.NodeCount
		}

		tb.Logf("Creating nodes %d-%d (batch %d)...", startIdx, endIdx-1, batch+1)

		for i := startIdx; i < endIdx; i++ {
			opts := &gridkv.GridKVOptions{
				LocalNodeID:        fmt.Sprintf("node-%d", i),
				LocalAddress:       fmt.Sprintf("localhost:%d", tes.config.BasePort+i),
				SeedAddrs:          seedAddr,
				FailureTimeout:     latencyConfig.FailureTimeout,
				SuspectTimeout:     latencyConfig.SuspectTimeout,
				GossipInterval:     latencyConfig.GossipInterval,
				ReplicationTimeout: latencyConfig.ReplicationTimeout,
				ReadTimeout:        latencyConfig.ReadTimeout,
				StartupGracePeriod: 2 * time.Second,
				DisableAuth:        true,
				ReplicaCount:       tes.config.ReplicaCount,
				Network: &gridkv.NetworkOptions{
					Type:     tes.config.NetworkType,
					BindAddr: fmt.Sprintf("localhost:%d", tes.config.BasePort+i),
					MaxConns: latencyConfig.MaxConnections,
					MaxIdle:  latencyConfig.MaxIdleConnections,
				},
				Storage: &gridkv.StorageOptions{
					Backend:     tes.config.StorageBackend,
					MaxMemoryMB: tes.config.MaxMemoryMB,
					ShardCount:  tes.config.ShardCount,
				},
			}

			tes.nodes[i], err = gridkv.NewGridKV(opts)
			if err != nil {
				tes.cleanupNodes(i)
				return fmt.Errorf("failed to create node %d: %w", i, err)
			}
			
			// Minimal delay between nodes in same batch
			time.Sleep(50 * time.Millisecond)
		}

		// Pause between batches to let gossip protocol stabilize
		if endIdx < tes.config.NodeCount {
			tb.Logf("Batch %d complete, pausing for gossip propagation...", batch+1)
			time.Sleep(2 * time.Second)
		}
	}

	tb.Logf("All %d nodes created", tes.config.NodeCount)
	return nil
}

// runUltraLargeClusterWorkload runs mixed workload on ultra-large cluster
func runUltraLargeClusterWorkload(t *testing.T, sim *TestEnvironmentSimulator, concurrentOps int, duration time.Duration) *workloadStats {
	stats := &workloadStats{
		duration: duration,
	}

	nodes := sim.GetNodes()
	if len(nodes) == 0 {
		t.Fatal("No nodes available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), duration+10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Start workers
	for i := 0; i < concurrentOps; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			
			// Each worker uses local random source
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)))
			
			for {
				select {
				case <-stop:
					return
				default:
					// Pick random node
					node := nodes[rng.Intn(len(nodes))]
					if node == nil {
						continue
					}

					// Random operation (write-heavy for large cluster)
					op := rng.Float64()
					key := fmt.Sprintf("key-%d-%d", workerID, rng.Intn(10000))
					
					switch {
					case op < 0.5: // 50% writes
						stats.writesSubmitted.Add(1)
						value := make([]byte, 256)
						rng.Read(value)
						if err := node.Set(ctx, key, value); err == nil {
							stats.writesCompleted.Add(1)
						}
						
					case op < 0.85: // 35% reads
						stats.readsSubmitted.Add(1)
						if _, err := node.Get(ctx, key); err == nil {
							stats.readsCompleted.Add(1)
						}
						
					default: // 15% deletes
						stats.deletesSubmitted.Add(1)
						if err := node.Delete(ctx, key); err == nil {
							stats.deletesCompleted.Add(1)
						}
					}
					
					// Small delay to avoid overwhelming
					time.Sleep(time.Duration(rng.Intn(10)) * time.Millisecond)
				}
			}
		}(i)
	}

	// Run for specified duration
	time.Sleep(duration)
	close(stop)
	wg.Wait()

	return stats
}

// BenchmarkUltraLargeClusterScalability benchmarks scalability with increasing nodes
func BenchmarkUltraLargeClusterScalability(b *testing.B) {
	if testing.Short() {
		b.Skip("Skipping scalability benchmark in short mode")
	}

	nodeCounts := []int{50, 100, 150}
	
	for _, nodeCount := range nodeCounts {
		b.Run(fmt.Sprintf("Nodes_%d", nodeCount), func(b *testing.B) {
			config := &TestEnvironmentConfig{
				NetworkProfile: network.ProfileGlobal,
				NetworkType:    networkTypeFromEnv(gridkv.TCP),
				NodeCount:      nodeCount,
				ReplicaCount:   5,
				BasePort:       50000 + nodeCount*1000,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    512,
				ShardCount:     64,
			}

			sim := NewTestEnvironmentSimulator(config)
			if err := sim.SetupClusterProgressive(b, 10); err != nil {
				b.Fatalf("Failed to setup cluster: %v", err)
			}
			defer sim.Cleanup()

			// Wait for stabilization
			time.Sleep(10 * time.Second)

			nodes := sim.GetNodes()
			ctx := context.Background()
			
			b.ResetTimer()
			
			// Benchmark operations
			b.RunParallel(func(pb *testing.PB) {
				rng := rand.New(rand.NewSource(time.Now().UnixNano()))
				for pb.Next() {
					node := nodes[rng.Intn(len(nodes))]
					if node == nil {
						continue
					}
					
					key := fmt.Sprintf("bench-key-%d", rng.Intn(1000))
					value := []byte("benchmark-value")
					
					_ = node.Set(ctx, key, value)
				}
			})
		})
	}
}

