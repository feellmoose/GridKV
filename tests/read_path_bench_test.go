package tests

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gridkv "github.com/feellmoose/gridkv"
	"github.com/feellmoose/gridkv/internal/utils/network"
)

// TestReadPathBenchmark focuses on read path performance (Stage 1 optimization)
// Tests read success rate, latency, and identifies bottlenecks
func TestReadPathBenchmark(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping read path benchmark in short mode")
	}

	config := &TestEnvironmentConfig{
		NetworkProfile: network.ProfileLAN,
		NetworkType:    networkTypeFromEnv(gridkv.TCP),
		NodeCount:      5,
		ReplicaCount:   3,
		BasePort:       50000,
		StorageBackend: gridkv.BackendMemorySharded,
		MaxMemoryMB:    2048,
		ShardCount:     128,
	}

	sim := NewTestEnvironmentSimulator(config)
	if err := sim.SetupCluster(t); err != nil {
		t.Fatalf("Failed to setup cluster: %v", err)
	}
	defer sim.Cleanup()

	nodes := sim.GetNodes()
	ctx := context.Background()
	sim.WaitForHealthyNodes(t, config.NodeCount, 10*time.Second)

	// Pre-populate data for reads
	t.Log("Pre-populating data for read benchmark...")
	seedKeys := 10000
	writtenKeys := make([]string, 0, seedKeys)
	writtenKeysMu := sync.RWMutex{}

	var seedWg sync.WaitGroup
	for i := 0; i < seedKeys; i++ {
		seedWg.Add(1)
		go func(idx int) {
			defer seedWg.Done()
			key := fmt.Sprintf("read-bench-key-%d", idx)
			value := []byte(fmt.Sprintf("read-bench-value-%d-%d", idx, time.Now().UnixNano()))
			nodeIdx := idx % len(nodes)
			if err := nodes[nodeIdx].Set(ctx, key, value); err == nil {
				writtenKeysMu.Lock()
				writtenKeys = append(writtenKeys, key)
				writtenKeysMu.Unlock()
			}
		}(i)
	}
	seedWg.Wait()

	// Wait for replication to settle
	time.Sleep(2 * time.Second)
	t.Logf("Pre-populated %d keys, starting read benchmark...", len(writtenKeys))

	// Read path benchmark
	testDuration := 30 * time.Second
	concurrentReaders := 200 // High concurrency to stress read path

	var (
		readsSubmitted   atomic.Int64
		readsCompleted   atomic.Int64
		readsFailed      atomic.Int64
		readsTimeout     atomic.Int64
		readsFastFail    atomic.Int64
		readsFallback    atomic.Int64
		readLatencies    []time.Duration
		readLatenciesMu  sync.Mutex
	)

	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	// Start readers
	for r := 0; r < concurrentReaders; r++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			nodeIdx := readerID % len(nodes)
			for {
				select {
				case <-stopCh:
					return
				default:
					writtenKeysMu.RLock()
					if len(writtenKeys) == 0 {
						writtenKeysMu.RUnlock()
						time.Sleep(10 * time.Millisecond)
						continue
					}
					targetIdx := rand.Intn(len(writtenKeys))
					key := writtenKeys[targetIdx]
					writtenKeysMu.RUnlock()

					readsSubmitted.Add(1)
					start := time.Now()
					value, err := nodes[nodeIdx].Get(ctx, key)
					latency := time.Since(start)

					readLatenciesMu.Lock()
					readLatencies = append(readLatencies, latency)
					if len(readLatencies) > 10000 {
						// Keep only last 10K samples
						readLatencies = readLatencies[len(readLatencies)-10000:]
					}
					readLatenciesMu.Unlock()

					if err != nil {
						readsFailed.Add(1)
						errStr := err.Error()
						if strings.Contains(errStr, "timeout") || strings.Contains(errStr, "deadline") {
							readsTimeout.Add(1)
						}
						if strings.Contains(errStr, "pending reads") || strings.Contains(errStr, "resource limit") {
							readsFastFail.Add(1)
						}
						if strings.Contains(errStr, "fallback") {
							readsFallback.Add(1)
						}
					} else if len(value) > 0 {
						readsCompleted.Add(1)
					} else {
						readsFailed.Add(1)
					}
				}
			}
		}(r)
	}

	// Run for test duration
	time.Sleep(testDuration)
	close(stopCh)
	wg.Wait()

	// Calculate statistics
	totalReads := readsSubmitted.Load()
	completed := readsCompleted.Load()
	failed := readsFailed.Load()
	timeouts := readsTimeout.Load()
	fastFails := readsFastFail.Load()
	fallbacks := readsFallback.Load()

	successRate := float64(completed) / float64(totalReads) * 100
	timeoutRate := float64(timeouts) / float64(totalReads) * 100
	fastFailRate := float64(fastFails) / float64(totalReads) * 100
	readQPS := float64(completed) / testDuration.Seconds()

	// Calculate latency percentiles
	readLatenciesMu.Lock()
	p50, p95, p99 := calculatePercentiles(readLatencies)
	readLatenciesMu.Unlock()

	// Report results
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("   Read Path Benchmark Results (Stage 1)")
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("Test Duration: %v", testDuration)
	t.Logf("Concurrent Readers: %d", concurrentReaders)
	t.Logf("Pre-populated Keys: %d", len(writtenKeys))
	t.Logf("")
	t.Logf("Read Statistics:")
	t.Logf("  Total Reads: %d", totalReads)
	t.Logf("  Completed: %d (%.2f%%)", completed, successRate)
	t.Logf("  Failed: %d (%.2f%%)", failed, float64(failed)/float64(totalReads)*100)
	t.Logf("  Timeouts: %d (%.2f%%)", timeouts, timeoutRate)
	t.Logf("  Fast-Fails: %d (%.2f%%)", fastFails, fastFailRate)
	t.Logf("  Fallbacks: %d (%.2f%%)", fallbacks, float64(fallbacks)/float64(totalReads)*100)
	t.Logf("")
	t.Logf("Performance:")
	t.Logf("  Read QPS: %.2f", readQPS)
	t.Logf("  P50 Latency: %v", p50)
	t.Logf("  P95 Latency: %v", p95)
	t.Logf("  P99 Latency: %v", p99)
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// Compare with baseline
	baselineSuccessRate := 69.5 // From baseline_report.md
	if successRate < baselineSuccessRate {
		t.Errorf("Read success rate %.2f%% is below baseline %.2f%%", successRate, baselineSuccessRate)
	} else {
		t.Logf("✓ Read success rate improved: %.2f%% (baseline: %.2f%%)", successRate, baselineSuccessRate)
	}

	baselineTimeoutRate := 30.5 // From baseline_report.md
	if timeoutRate > baselineTimeoutRate {
		t.Errorf("Read timeout rate %.2f%% is above baseline %.2f%%", timeoutRate, baselineTimeoutRate)
	} else {
		t.Logf("✓ Read timeout rate improved: %.2f%% (baseline: %.2f%%)", timeoutRate, baselineTimeoutRate)
	}
}


