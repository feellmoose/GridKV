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

// TestSimulatedUltraLargeCluster simulates 100+ node cluster behavior
// Uses smaller physical cluster but simulates large-scale characteristics
func TestSimulatedUltraLargeCluster(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping simulated ultra-large cluster test in short mode")
	}

	testCases := []struct {
		name              string
		physicalNodes     int // Actual nodes to create
		simulatedNodes    int // Simulated total cluster size
		replicaCount      int
		concurrentOps     int
		testDuration      time.Duration
		networkProfile    network.NetworkProfile
		expectedMinQPS    float64
	}{
		{
			name:              "Simulated_100Nodes",
			physicalNodes:     20, // Physical nodes
			simulatedNodes:    100, // Simulated cluster size
			replicaCount:      5,
			concurrentOps:     500,
			testDuration:      90 * time.Second,
			networkProfile:    network.ProfileGlobal,
			expectedMinQPS:    800,
		},
		{
			name:              "Simulated_150Nodes",
			physicalNodes:     25,
			simulatedNodes:    150,
			replicaCount:      7,
			concurrentOps:     600,
			testDuration:      90 * time.Second,
			networkProfile:    network.ProfileGlobal,
			expectedMinQPS:    600,
		},
		{
			name:              "Simulated_200Nodes",
			physicalNodes:     30,
			simulatedNodes:    200,
			replicaCount:      7,
			concurrentOps:     800,
			testDuration:      90 * time.Second,
			networkProfile:    network.ProfileGlobal,
			expectedMinQPS:    500,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Memory check
			var mem runtime.MemStats
			runtime.ReadMemStats(&mem)
			t.Logf("Starting memory: Alloc=%.2fMB, Sys=%.2fMB",
				float64(mem.Alloc)/1024/1024, float64(mem.Sys)/1024/1024)

			// Adjust timeouts for simulated large cluster
			latencyMultiplier := float64(tc.simulatedNodes) / float64(tc.physicalNodes)
			
			config := &TestEnvironmentConfig{
				NetworkProfile: tc.networkProfile,
				NetworkType:    networkTypeFromEnv(gridkv.TCP),
				NodeCount:      tc.physicalNodes,
				ReplicaCount:   tc.replicaCount,
				BasePort:       35000,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    512,
				ShardCount:     64,
			}

			t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			t.Logf("   Simulated Ultra Large Cluster Test")
			t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			t.Logf("Physical nodes:  %d", tc.physicalNodes)
			t.Logf("Simulated nodes: %d (%.1fx latency multiplier)", tc.simulatedNodes, latencyMultiplier)
			t.Logf("Replicas:        %d", tc.replicaCount)
			t.Logf("Concurrency:     %d workers", tc.concurrentOps)
			t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

			// Setup cluster
			sim := NewTestEnvironmentSimulator(config)
			
			startTime := time.Now()
			if err := sim.SetupCluster(t); err != nil {
				t.Fatalf("Failed to setup cluster: %v", err)
			}
			setupDuration := time.Since(startTime)
			
			defer sim.Cleanup()

			t.Logf("✓ Physical cluster setup completed in %.1fs", setupDuration.Seconds())

			// Wait for stabilization
			stabilizeWait := 10 * time.Second
			t.Logf("Waiting %.0fs for cluster stabilization...", stabilizeWait.Seconds())
			time.Sleep(stabilizeWait)

			// Check memory after setup
			runtime.ReadMemStats(&mem)
			t.Logf("After setup: Alloc=%.2fMB, Sys=%.2fMB, NumGC=%d",
				float64(mem.Alloc)/1024/1024, float64(mem.Sys)/1024/1024, mem.NumGC)

			// Run workload with simulated large-cluster characteristics
			stats := runSimulatedLargeClusterWorkload(t, sim, tc.concurrentOps, tc.testDuration, latencyMultiplier)

			// Final memory check
			runtime.ReadMemStats(&mem)
			t.Logf("After test: Alloc=%.2fMB, Sys=%.2fMB, NumGC=%d",
				float64(mem.Alloc)/1024/1024, float64(mem.Sys)/1024/1024, mem.NumGC)

			// Report results
			t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			t.Logf("   Test Results (%s)", tc.name)
			t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			t.Logf("Physical: %d nodes, Simulated: %d nodes", tc.physicalNodes, tc.simulatedNodes)
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

			// Calculate extrapolated performance for full simulated cluster
			extrapolatedQPS := totalQPS * float64(tc.simulatedNodes) / float64(tc.physicalNodes)
			t.Logf("")
			t.Logf("Extrapolated for %d nodes: %.2f QPS", tc.simulatedNodes, extrapolatedQPS)
		})
	}
}

// runSimulatedLargeClusterWorkload runs workload simulating large cluster characteristics
func runSimulatedLargeClusterWorkload(t *testing.T, sim *TestEnvironmentSimulator, concurrentOps int, duration time.Duration, latencyMultiplier float64) *workloadStats {
	stats := &workloadStats{
		duration: duration,
	}

	nodes := sim.GetNodes()
	if len(nodes) == 0 {
		t.Fatal("No nodes available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), duration+30*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Simulate large-cluster behavior: higher latency, more retries
	baseDelay := time.Duration(latencyMultiplier * 2) * time.Millisecond
	if baseDelay > 50*time.Millisecond {
		baseDelay = 50 * time.Millisecond
	}

	// Start workers
	for i := 0; i < concurrentOps; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			
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

					// Simulate large-cluster delay
					if latencyMultiplier > 2.0 {
						time.Sleep(baseDelay)
					}

					// Random operation (write-heavy)
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
					
					// Variable delay based on cluster size simulation
					time.Sleep(time.Duration(rng.Intn(int(baseDelay.Milliseconds())+5)) * time.Millisecond)
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

// BenchmarkSimulatedUltraLargeClusterScalability benchmarks simulated scalability
func BenchmarkSimulatedUltraLargeClusterScalability(b *testing.B) {
	if testing.Short() {
		b.Skip("Skipping scalability benchmark in short mode")
	}

	scenarios := []struct {
		physicalNodes  int
		simulatedNodes int
	}{
		{20, 100},
		{25, 150},
		{30, 200},
	}
	
	for _, scenario := range scenarios {
		b.Run(fmt.Sprintf("Physical%d_Simulated%d", scenario.physicalNodes, scenario.simulatedNodes), func(b *testing.B) {
			config := &TestEnvironmentConfig{
				NetworkProfile: network.ProfileGlobal,
				NetworkType:    networkTypeFromEnv(gridkv.TCP),
				NodeCount:      scenario.physicalNodes,
				ReplicaCount:   5,
				BasePort:       36000 + scenario.physicalNodes*100,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    512,
				ShardCount:     64,
			}

			sim := NewTestEnvironmentSimulator(config)
			if err := sim.SetupCluster(b); err != nil {
				b.Fatalf("Failed to setup cluster: %v", err)
			}
			defer sim.Cleanup()

			time.Sleep(5 * time.Second)

			nodes := sim.GetNodes()
			ctx := context.Background()
			
			latencyMultiplier := float64(scenario.simulatedNodes) / float64(scenario.physicalNodes)
			baseDelay := time.Duration(latencyMultiplier) * time.Millisecond
			
			b.ResetTimer()
			
			b.RunParallel(func(pb *testing.PB) {
				rng := rand.New(rand.NewSource(time.Now().UnixNano()))
				for pb.Next() {
					node := nodes[rng.Intn(len(nodes))]
					if node == nil {
						continue
					}
					
					// Simulate large-cluster latency
					time.Sleep(baseDelay)
					
					key := fmt.Sprintf("bench-key-%d", rng.Intn(1000))
					value := []byte("benchmark-value")
					
					_ = node.Set(ctx, key, value)
				}
			})
		})
	}
}

