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

// TestAsyncPerformance tests throughput without blocking on writes
// This test demonstrates true system capacity by fire-and-forget writes
func TestAsyncPerformance(t *testing.T) {
	if os.Getenv("GRIDKV_RUN_PERF_TESTS") != "1" {
		t.Skip("set GRIDKV_RUN_PERF_TESTS=1 to enable performance workload")
	}

	const (
		numNodes      = 5
		testDuration  = 30 * time.Second
		writeWorkers  = 100 // Increased for async writes
		readWorkers   = 50
		basePort      = 58000
		maxPendingOps = 10000 // Limit pending operations to prevent memory overflow
	)

	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("   Async Performance Test (Fire-and-Forget Writes)")
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("Nodes:         %d", numNodes)
	t.Logf("Duration:      %v", testDuration)
	t.Logf("Write Workers: %d (async)", writeWorkers)
	t.Logf("Read Workers:  %d", readWorkers)
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Log("")

	// Statistics
	var (
		writeRequests  atomic.Int64 // Total write requests submitted
		writeSubmitted atomic.Int64 // Successfully submitted (no blocking)
		writeCompleted atomic.Int64 // Completed writes (acknowledged)
		writeFailed    atomic.Int64 // Failed writes
		readRequests   atomic.Int64
		readCompleted  atomic.Int64
		readFailed     atomic.Int64
	)

	// Create cluster
	t.Log("📦 Creating cluster...")
	nodes := make([]*gridkv.GridKV, numNodes)
	for i := 0; i < numNodes; i++ {
		var seedAddrs []string
		if i > 0 {
			seedAddrs = []string{fmt.Sprintf("localhost:%d", basePort)}
		}

		node, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
			LocalNodeID:  fmt.Sprintf("async-perf-node-%d", i),
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
			MaxReplicators:     256,              // Increased for high concurrency
			ReplicationTimeout: 10 * time.Second, // Increased for high concurrency
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

	// Wait for convergence
	time.Sleep(3 * time.Second)
	t.Logf("✅ %d-node cluster ready", numNodes)
	t.Log("")

	// Pre-populate
	t.Log("💾 Pre-populating data...")
	ctx := context.Background()
	numKeys := 10000

	var prePopWg sync.WaitGroup
	for i := 0; i < numKeys; i++ {
		prePopWg.Add(1)
		go func(keyID int) {
			defer prePopWg.Done()
			key := fmt.Sprintf("key-%d", keyID)
			value := []byte(fmt.Sprintf("value-%d", keyID))
			node := nodes[keyID%len(nodes)]
			_ = node.Set(ctx, key, value)
		}(i)
	}
	prePopWg.Wait()

	t.Logf("✅ Populated %d keys", numKeys)
	t.Log("")

	// Start workload
	t.Log("🚀 Starting async workload...")
	deadline := time.Now().Add(testDuration)
	startTime := time.Now()

	// Semaphore to limit pending operations
	pendingWriteSem := make(chan struct{}, maxPendingOps)

	// Async write workers (fire-and-forget)
	var writeWg sync.WaitGroup
	for w := 0; w < writeWorkers; w++ {
		writeWg.Add(1)
		go func(workerID int) {
			defer writeWg.Done()

			node := nodes[workerID%len(nodes)]
			keyCounter := int64(0)

			for time.Now().Before(deadline) {
				// Try to acquire semaphore (non-blocking check)
				select {
				case pendingWriteSem <- struct{}{}:
					// Got slot, proceed
				default:
					// Too many pending, skip this iteration
					time.Sleep(100 * time.Microsecond)
					continue
				}

				writeRequests.Add(1)
				keyNum := atomic.AddInt64(&keyCounter, 1)
				key := fmt.Sprintf("async-key-%d-%d", workerID, keyNum)
				value := []byte(fmt.Sprintf("async-value-%d-%d-%d", workerID, keyNum, time.Now().UnixNano()))

				// ASYNC WRITE: Fire and forget - don't wait for completion
				writeSubmitted.Add(1)
				go func(k string, v []byte, sem chan struct{}) {
					defer func() { <-sem }() // Release semaphore

					writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					err := node.Set(writeCtx, k, v)
					cancel()

					if err != nil {
						writeFailed.Add(1)
					} else {
						writeCompleted.Add(1)
					}
				}(key, value, pendingWriteSem)

				// No delay - maximum fire rate
			}
		}(w)
	}

	// Read workers (synchronous for verification)
	var readWg sync.WaitGroup
	for r := 0; r < readWorkers; r++ {
		readWg.Add(1)
		go func(workerID int) {
			defer readWg.Done()

			node := nodes[workerID%len(nodes)]

			for time.Now().Before(deadline) {
				keyID := rand.Intn(numKeys)
				key := fmt.Sprintf("key-%d", keyID)

				readRequests.Add(1)
				readCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_, err := node.Get(readCtx, key)
				cancel()

				if err != nil {
					readFailed.Add(1)
				} else {
					readCompleted.Add(1)
				}

				// Small delay for reads
				time.Sleep(1 * time.Millisecond)
			}
		}(r)
	}

	// Metrics collector
	var metricsWg sync.WaitGroup
	metricsWg.Add(1)
	stopMetrics := make(chan struct{})
	go func() {
		defer metricsWg.Done()
		ticker := time.NewTicker(5 * time.Second)
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

				t.Logf("[Metrics] Writes: submit=%.0f/sec, complete=%.0f/sec, pending=%d (%.1f%% success)",
					submitRate, completeRate, pendingCount, submitSuccessRate)
				t.Logf("[Metrics] Reads: %.0f/sec (%.1f%% success)",
					readRate, float64(currentReadCompleted)/float64(readRequests.Load())*100.0)

				lastWriteSubmitted = currentSubmitted
				lastWriteCompleted = currentCompleted
				lastReadCompleted = currentReadCompleted
				lastTime = now
			}
		}
	}()

	// Wait for deadline
	time.Sleep(testDuration)

	// Wait for pending writes to complete (with timeout)
	t.Log("⏳ Waiting for pending writes to complete...")
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

	t.Log("")
	t.Log("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Log("            ASYNC TEST RESULTS")
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
	t.Logf("  Pending:          %d", finalWritesSubmitted-finalWritesCompleted-finalWritesFailed)
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
	t.Log("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// Assertions
	if finalWritesSubmitted == 0 {
		t.Error("❌ No writes submitted!")
	} else {
		t.Logf("✅ Submitted %d writes", finalWritesSubmitted)
	}

	successRate := float64(finalWritesCompleted) / float64(finalWritesSubmitted) * 100.0
	if successRate < 80.0 {
		t.Errorf("⚠️  Low write success rate: %.1f%%", successRate)
	} else {
		t.Logf("✅ Write success rate: %.1f%%", successRate)
	}

	submitRate := float64(finalWritesSubmitted) / elapsed.Seconds()
	if submitRate < 1000 {
		t.Logf("⚠️  Low submit rate: %.0f ops/sec", submitRate)
	} else {
		t.Logf("✅ High submit rate: %.0f ops/sec", submitRate)
	}
}
