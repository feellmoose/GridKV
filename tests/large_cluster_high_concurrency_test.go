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

// TestLargeClusterHighConcurrency tests large cluster (10+ nodes) with very high concurrency
// This test verifies system stability and performance under extreme load
func TestLargeClusterHighConcurrency(t *testing.T) {
	if os.Getenv("GRIDKV_RUN_LARGE_CLUSTER_HC") != "1" {
		t.Skip("set GRIDKV_RUN_LARGE_CLUSTER_HC=1 to enable large cluster high concurrency test")
	}

	numNodes := 10
	if v := os.Getenv("GRIDKV_LC_NODES"); v != "" {
		if parsed, err := fmt.Sscanf(v, "%d", &numNodes); err == nil && parsed == 1 && numNodes >= 8 {
			// Use parsed value
		}
	}

	testDuration := 30 * time.Second
	if v := os.Getenv("GRIDKV_LC_DURATION"); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil && parsed >= 20*time.Second {
			testDuration = parsed
		}
	}

	writeWorkers := 200
	if v := os.Getenv("GRIDKV_LC_WRITE_WORKERS"); v != "" {
		if parsed, err := fmt.Sscanf(v, "%d", &writeWorkers); err == nil && parsed == 1 && parsed > 0 {
			// Use parsed value
		}
	}

	readWorkers := 100
	if v := os.Getenv("GRIDKV_LC_READ_WORKERS"); v != "" {
		if parsed, err := fmt.Sscanf(v, "%d", &readWorkers); err == nil && parsed == 1 && parsed > 0 {
			// Use parsed value
		}
	}

	basePort := 60000
	maxPendingOps := 20000

	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("   Large Cluster High Concurrency Test")
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("Nodes:         %d", numNodes)
	t.Logf("Duration:      %v", testDuration)
	t.Logf("Write Workers: %d (async)", writeWorkers)
	t.Logf("Read Workers:  %d", readWorkers)
	t.Logf("Max Pending:   %d", maxPendingOps)
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Log("")

	// Statistics
	var (
		writeSubmitted atomic.Int64
		writeCompleted atomic.Int64
		writeFailed    atomic.Int64
		readRequests   atomic.Int64
		readCompleted  atomic.Int64
		readFailed     atomic.Int64
		quorumFailures atomic.Int64
		ackTimeouts    atomic.Int64
	)

	// Create cluster
	t.Log("📦 Creating large cluster...")
	nodes := make([]*gridkv.GridKV, numNodes)
	for i := 0; i < numNodes; i++ {
		var seedAddrs []string
		if i > 0 {
			seedAddrs = []string{fmt.Sprintf("localhost:%d", basePort)}
		}

		node, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
			LocalNodeID:  fmt.Sprintf("large-cluster-node-%d", i),
			LocalAddress: fmt.Sprintf("localhost:%d", basePort+i),
			SeedAddrs:    seedAddrs,
			Network: &gridkv.NetworkOptions{
				Type:     gridkv.GNET,
				BindAddr: fmt.Sprintf("localhost:%d", basePort+i),
				MaxConns: 5000,
				MaxIdle:  500,
			},
			Storage: &gridkv.StorageOptions{
				Backend:     gridkv.BackendMemorySharded,
				ShardCount:  512,
				MaxMemoryMB: 4096,
			},
			ReplicaCount:       3,
			WriteQuorum:        2,
			ReadQuorum:         2,
			MaxReplicators:     512,
			ReplicationTimeout: 15 * time.Second,
			ReadTimeout:        10 * time.Second,
			VirtualNodes:       150,
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

	// Wait for cluster convergence
	t.Log("⏳ Waiting for cluster convergence...")
	time.Sleep(5 * time.Second)
	t.Logf("✅ %d-node cluster ready", numNodes)
	t.Log("")

	// Pre-populate data
	t.Log("💾 Pre-populating data...")
	ctx := context.Background()
	numKeys := 20000

	for i := 0; i < numKeys; i++ {
		key := fmt.Sprintf("key-%d", i)
		value := []byte(fmt.Sprintf("value-%d", i))
		node := nodes[i%len(nodes)]
		if err := node.Set(ctx, key, value); err != nil {
			t.Logf("Warning: Pre-populate failed for key %s: %v", key, err)
		}
		if (i+1)%5000 == 0 {
			t.Logf("  Populated %d keys...", i+1)
		}
	}

	t.Logf("✅ Populated %d keys", numKeys)
	t.Log("")

	// Start async write workers (fire-and-forget)
	t.Log("🚀 Starting high-concurrency workload...")
	startTime := time.Now()
	deadline := time.Now().Add(testDuration)

	var writeWg sync.WaitGroup
	pendingSem := make(chan struct{}, maxPendingOps)
	writeCtx, writeCancel := context.WithDeadline(ctx, deadline)
	defer writeCancel()

	// Async write workers
	for w := 0; w < writeWorkers; w++ {
		writeWg.Add(1)
		go func(workerID int) {
			defer writeWg.Done()

			node := nodes[workerID%len(nodes)]
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)))

			for writeCtx.Err() == nil {
				select {
				case <-writeCtx.Done():
					return
				case pendingSem <- struct{}{}:
					writeSubmitted.Add(1)
					keyID := rng.Intn(numKeys * 2) // Allow writes to new keys
					key := fmt.Sprintf("key-%d", keyID)
					value := []byte(fmt.Sprintf("value-%d-%d", workerID, rng.Intn(100000)))

					go func(k string, v []byte) {
						defer func() { <-pendingSem }()

						err := node.Set(ctx, k, v)
						if err != nil {
							writeFailed.Add(1)
							if err.Error() == "quorum not reached" {
								quorumFailures.Add(1)
							}
						} else {
							writeCompleted.Add(1)
						}
					}(key, value)

					// Small delay to prevent overwhelming
					time.Sleep(time.Microsecond * 100)
				default:
					// Pending operations limit reached, wait a bit
					time.Sleep(time.Millisecond)
				}
			}
		}(w)
	}

	// Read workers
	var readWg sync.WaitGroup
	for r := 0; r < readWorkers; r++ {
		readWg.Add(1)
		go func(workerID int) {
			defer readWg.Done()

			node := nodes[workerID%len(nodes)]
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID+1000)))

			for time.Now().Before(deadline) {
				readRequests.Add(1)
				keyID := rng.Intn(numKeys)
				key := fmt.Sprintf("key-%d", keyID)

				_, err := node.Get(ctx, key)
				if err != nil {
					readFailed.Add(1)
				} else {
					readCompleted.Add(1)
				}

				// Small delay for reads
				time.Sleep(time.Millisecond)
			}
		}(r)
	}

	// Metrics collector
	var metricsWg sync.WaitGroup
	metricsWg.Add(1)
	stopMetrics := make(chan struct{})
	go func() {
		defer metricsWg.Done()
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		var lastWriteSubmitted, lastWriteCompleted int64
		var lastReadCompleted int64
		lastTime := time.Now()

		for {
			select {
			case <-stopMetrics:
				return
			case <-ticker.C:
				now := time.Now()
				elapsed := now.Sub(lastTime).Seconds()

				currentSubmitted := writeSubmitted.Load()
				currentCompleted := writeCompleted.Load()
				currentReadCompleted := readCompleted.Load()

				submitRate := float64(currentSubmitted-lastWriteSubmitted) / elapsed
				completeRate := float64(currentCompleted-lastWriteCompleted) / elapsed
				readRate := float64(currentReadCompleted-lastReadCompleted) / elapsed

				pendingCount := writeSubmitted.Load() - writeCompleted.Load() - writeFailed.Load()
				submitSuccessRate := float64(currentCompleted) / float64(currentSubmitted) * 100.0

				t.Logf("[Metrics @ %ds] Writes: submit=%.0f/sec, complete=%.0f/sec, pending=%d (%.1f%% success)",
					int(elapsed), submitRate, completeRate, pendingCount, submitSuccessRate)
				t.Logf("[Metrics @ %ds] Reads: %.0f/sec (%.1f%% success)",
					int(elapsed), readRate, float64(currentReadCompleted)/float64(readRequests.Load())*100.0)
				t.Logf("[Metrics @ %ds] Errors: quorum=%d, ack_timeout=%d",
					int(elapsed), quorumFailures.Load(), ackTimeouts.Load())

				lastWriteSubmitted = currentSubmitted
				lastWriteCompleted = currentCompleted
				lastReadCompleted = currentReadCompleted
				lastTime = now
			}
		}
	}()

	// Wait for deadline
	time.Sleep(testDuration)

	// Wait for pending writes to complete
	t.Log("⏳ Waiting for pending writes to complete...")
	writeCancel()
	waitStart := time.Now()
	waitDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(waitDeadline) {
		pending := writeSubmitted.Load() - writeCompleted.Load() - writeFailed.Load()
		if pending <= 0 {
			break
		}
		if time.Since(waitStart) > 10*time.Second {
			t.Logf("⚠️  Still waiting for %d writes...", pending)
		}
		time.Sleep(500 * time.Millisecond)
	}

	close(stopMetrics)
	writeWg.Wait()
	readWg.Wait()
	metricsWg.Wait()

	elapsed := time.Since(startTime)

	// Final statistics
	finalWritesSubmitted := writeSubmitted.Load()
	finalWritesCompleted := writeCompleted.Load()
	finalWritesFailed := writeFailed.Load()
	finalReadsCompleted := readCompleted.Load()
	finalReadsFailed := readFailed.Load()
	finalQuorumFailures := quorumFailures.Load()
	finalAckTimeouts := ackTimeouts.Load()

	t.Log("")
	t.Log("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Log("         LARGE CLUSTER TEST RESULTS")
	t.Log("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("Duration:           %v", elapsed.Round(time.Second))
	t.Logf("Cluster Size:       %d nodes", numNodes)
	t.Logf("Write Workers:      %d (async)", writeWorkers)
	t.Logf("Read Workers:       %d", readWorkers)
	t.Log("")
	t.Logf("Write Performance:")
	t.Logf("  Submitted:        %d", finalWritesSubmitted)
	t.Logf("  Completed:        %d (%.1f%%)", finalWritesCompleted,
		float64(finalWritesCompleted)/float64(finalWritesSubmitted)*100.0)
	t.Logf("  Failed:           %d (%.1f%%)", finalWritesFailed,
		float64(finalWritesFailed)/float64(finalWritesSubmitted)*100.0)
	t.Logf("  Submit Rate:      %.0f ops/sec", float64(finalWritesSubmitted)/elapsed.Seconds())
	t.Logf("  Complete Rate:    %.0f ops/sec", float64(finalWritesCompleted)/elapsed.Seconds())
	t.Log("")
	t.Logf("Read Performance:")
	t.Logf("  Completed:        %d", finalReadsCompleted)
	t.Logf("  Failed:           %d (%.1f%%)", finalReadsFailed,
		float64(finalReadsFailed)/float64(finalReadsCompleted+finalReadsFailed)*100.0)
	t.Logf("  Read Rate:        %.0f ops/sec", float64(finalReadsCompleted)/elapsed.Seconds())
	t.Log("")
	t.Logf("Total Throughput:")
	t.Logf("  Combined:         %.0f ops/sec", float64(finalWritesCompleted+finalReadsCompleted)/elapsed.Seconds())
	t.Log("")
	t.Logf("Error Statistics:")
	t.Logf("  Quorum Failures:  %d", finalQuorumFailures)
	t.Logf("  ACK Timeouts:     %d", finalAckTimeouts)
	t.Log("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// Assertions
	successRate := float64(finalWritesCompleted) / float64(finalWritesSubmitted) * 100.0
	if successRate < 90.0 {
		t.Errorf("⚠️  Low write success rate: %.1f%%", successRate)
	} else {
		t.Logf("✅ Write success rate: %.1f%%", successRate)
	}

	combinedThroughput := float64(finalWritesCompleted+finalReadsCompleted) / elapsed.Seconds()
	if combinedThroughput < 50000 {
		t.Logf("⚠️  Throughput lower than expected: %.0f ops/sec", combinedThroughput)
	} else {
		t.Logf("✅ High throughput: %.0f ops/sec", combinedThroughput)
	}

	if finalQuorumFailures > finalWritesSubmitted/100 {
		t.Logf("⚠️  High quorum failure rate: %d failures", finalQuorumFailures)
	} else {
		t.Logf("✅ Quorum failures within acceptable range: %d", finalQuorumFailures)
	}
}
