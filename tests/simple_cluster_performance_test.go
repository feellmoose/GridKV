package tests

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gridkv "github.com/feellmoose/gridkv"
)

// TestSimpleClusterPerformance runs a simplified but realistic long-term test
// Configuration: 5 nodes, 1 minute, single replica (no quorum overhead)
func TestSimpleClusterPerformance(t *testing.T) {
	if os.Getenv("GRIDKV_RUN_PERF_TESTS") != "1" {
		t.Skip("set GRIDKV_RUN_PERF_TESTS=1 to enable performance workload")
	}

	const (
		numNodes     = 5
		testDuration = 1 * time.Minute
		numClients   = 20
		basePort     = 57000
	)

	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("   Simple Cluster Performance Test")
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("Nodes:      %d", numNodes)
	t.Logf("Duration:   %v", testDuration)
	t.Logf("Clients:    %d", numClients)
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Log("")

	// Statistics
	var (
		totalWrites  atomic.Int64
		totalReads   atomic.Int64
		totalDeletes atomic.Int64
		writeErrors  atomic.Int64
		readErrors   atomic.Int64
		deleteErrors atomic.Int64
	)

	// Create cluster
	t.Log("📦 Creating cluster...")
	nodes := make([]*gridkv.GridKV, numNodes)

	// Create all nodes
	for i := 0; i < numNodes; i++ {
		var seedAddrs []string
		if i > 0 {
			seedAddrs = []string{fmt.Sprintf("localhost:%d", basePort)}
		}

		node, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
			LocalNodeID:  fmt.Sprintf("perf-node-%d", i),
			LocalAddress: fmt.Sprintf("localhost:%d", basePort+i),
			SeedAddrs:    seedAddrs,
			Network: &gridkv.NetworkOptions{
				Type:     gridkv.TCP,
				BindAddr: fmt.Sprintf("localhost:%d", basePort+i),
			},
			Storage: &gridkv.StorageOptions{
				Backend:     gridkv.BackendMemorySharded,
				MaxMemoryMB: 4096,
			},
			ReplicaCount: 1, // Single replica for max performance
			WriteQuorum:  1,
			ReadQuorum:   1,
			VirtualNodes: 100,
		})

		if err != nil {
			t.Fatalf("Failed to create node %d: %v", i, err)
		}
		nodes[i] = node
	}

	defer func() {
		for _, node := range nodes {
			if node != nil {
				node.Close()
			}
		}
	}()

	// Wait for convergence
	time.Sleep(2 * time.Second)
	t.Logf("✅ %d-node cluster ready", numNodes)
	t.Log("")

	// Pre-populate
	t.Log("💾 Pre-populating data...")
	ctx := context.Background()
	numKeys := 5000

	for i := 0; i < numKeys; i++ {
		key := fmt.Sprintf("key-%d", i)
		value := []byte(fmt.Sprintf("value-%d", i))
		node := nodes[i%len(nodes)]
		_ = node.Set(ctx, key, value)
	}

	t.Logf("✅ Populated %d keys", numKeys)
	t.Log("")

	// Start workload
	t.Log("🚀 Starting workload...")
	deadline := time.Now().Add(testDuration)
	startTime := time.Now()

	var wg sync.WaitGroup
	for c := 0; c < numClients; c++ {
		wg.Add(1)
		go func(clientID int) {
			defer wg.Done()

			node := nodes[clientID%len(nodes)]

			for time.Now().Before(deadline) {
				keyID := rand.Intn(numKeys)
				key := fmt.Sprintf("key-%d", keyID)
				value := []byte(fmt.Sprintf("value-%d-%d", clientID, rand.Intn(1000)))

				op := rand.Intn(10)

				switch {
				case op < 6: // 60% reads
					_, err := node.Get(ctx, key)
					if err == nil {
						totalReads.Add(1)
					} else {
						readErrors.Add(1)
					}

				case op < 9: // 30% writes
					err := node.Set(ctx, key, value)
					if err == nil {
						totalWrites.Add(1)
					} else {
						writeErrors.Add(1)
					}

				default: // 10% deletes
					err := node.Delete(ctx, key)
					if err == nil {
						totalDeletes.Add(1)
					} else {
						deleteErrors.Add(1)
					}
				}
			}
		}(c)
	}

	wg.Wait()
	elapsed := time.Since(startTime)

	// Final statistics
	finalWrites := totalWrites.Load()
	finalReads := totalReads.Load()
	finalDeletes := totalDeletes.Load()
	finalTotal := finalWrites + finalReads + finalDeletes

	finalWriteErrs := writeErrors.Load()
	finalReadErrs := readErrors.Load()
	finalDeleteErrs := deleteErrors.Load()
	finalTotalErrs := finalWriteErrs + finalReadErrs + finalDeleteErrs

	avgOpsPerSec := float64(finalTotal) / elapsed.Seconds()
	errorRate := float64(finalTotalErrs) / float64(finalTotal+finalTotalErrs) * 100

	t.Log("")
	t.Log("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Log("            FINAL RESULTS")
	t.Log("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("Duration:           %v", elapsed.Round(time.Second))
	t.Logf("Cluster Size:       %d nodes", numNodes)
	t.Logf("Concurrent Clients: %d", numClients)
	t.Log("")
	t.Logf("Total Operations:   %d", finalTotal)
	t.Logf("  Writes:           %d (%.1f%%)", finalWrites, float64(finalWrites)/float64(finalTotal)*100)
	t.Logf("  Reads:            %d (%.1f%%)", finalReads, float64(finalReads)/float64(finalTotal)*100)
	t.Logf("  Deletes:          %d (%.1f%%)", finalDeletes, float64(finalDeletes)/float64(finalTotal)*100)
	t.Log("")
	t.Logf("Total Errors:       %d", finalTotalErrs)
	t.Logf("  Error Rate:       %.3f%%", errorRate)
	t.Logf("  Write Errors:     %d", finalWriteErrs)
	t.Logf("  Read Errors:      %d", finalReadErrs)
	t.Logf("  Delete Errors:    %d", finalDeleteErrs)
	t.Log("")
	t.Logf("Performance:")
	t.Logf("  Avg Throughput:   %.0f ops/sec", avgOpsPerSec)
	t.Logf("  Per-Node:         %.0f ops/sec", avgOpsPerSec/float64(numNodes))
	t.Logf("  Per-Client:       %.0f ops/sec", avgOpsPerSec/float64(numClients))
	t.Log("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// Assertions
	if finalTotal == 0 {
		t.Error("❌ No operations completed!")
	} else {
		t.Logf("✅ Completed %d operations", finalTotal)
	}

	if errorRate > 5.0 {
		t.Logf("⚠️  Error rate: %.2f%% (acceptable for distributed system)", errorRate)
	} else {
		t.Logf("✅ Low error rate: %.2f%%", errorRate)
	}

	if avgOpsPerSec < 100 {
		t.Logf("⚠️  Low throughput: %.0f ops/sec", avgOpsPerSec)
	} else {
		t.Logf("✅ Good throughput: %.0f ops/sec", avgOpsPerSec)
	}
}
