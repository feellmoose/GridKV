package tests

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gridkv "github.com/feellmoose/gridkv"
	"github.com/feellmoose/gridkv/internal/utils/network"
)

// TestStressPressure tests system under various pressure scenarios
// Now uses runMixedClusterWorkload for consistent testing
func TestStressPressure(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}
	testCases := []struct {
		name           string
		envConfig      *TestEnvironmentConfig
		concurrentOps  int
		testDuration   time.Duration
		expectedMinQPS float64
	}{
		{
			name: "SmallCluster_LowLatency",
			envConfig: &TestEnvironmentConfig{
				NetworkProfile: network.ProfileLAN,
				NetworkType:    networkTypeFromEnv(gridkv.TCP),
				NodeCount:      5,
				ReplicaCount:   3,
				BasePort:       30000,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    2048,
				ShardCount:     128,
			},
			concurrentOps:  100,
			testDuration:   30 * time.Second,
			expectedMinQPS: 10000,
		},
		{
			name: "MediumCluster_WAN",
			envConfig: &TestEnvironmentConfig{
				NetworkProfile: network.ProfileWAN,
				NetworkType:    networkTypeFromEnv(gridkv.TCP),
				NodeCount:      10,
				ReplicaCount:   3,
				BasePort:       31000,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    4096,
				ShardCount:     256,
			},
			concurrentOps:  200,
			testDuration:   60 * time.Second,
			expectedMinQPS: 5000,
		},
		{
			name: "LargeCluster_Global",
			envConfig: &TestEnvironmentConfig{
				NetworkProfile: network.ProfileGlobal,
				NetworkType:    networkTypeFromEnv(gridkv.TCP),
				NodeCount:      20,
				ReplicaCount:   3,
				BasePort:       32000,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    8192,
				ShardCount:     512,
			},
			concurrentOps:  500,
			testDuration:   90 * time.Second,
			expectedMinQPS: 2000,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			stats := runMixedClusterWorkload(t, tc.envConfig, tc.concurrentOps, tc.testDuration)

			t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			t.Logf("   Stress Test Results (%s)", tc.name)
			t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			t.Logf("Writes:  %d submitted / %d completed (%.1f%%)",
				stats.writesSubmitted.Load(), stats.writesCompleted.Load(),
				stats.successRate(stats.writesSubmitted.Load(), stats.writesCompleted.Load()))
			t.Logf("Reads:   %d submitted / %d completed (%.1f%%)",
				stats.readsSubmitted.Load(), stats.readsCompleted.Load(),
				stats.successRate(stats.readsSubmitted.Load(), stats.readsCompleted.Load()))
			t.Logf("Deletes: %d submitted / %d completed (%.1f%%)",
				stats.deletesSubmitted.Load(), stats.deletesCompleted.Load(),
				stats.successRate(stats.deletesSubmitted.Load(), stats.deletesCompleted.Load()))
			t.Logf("Completed QPS: %.2f (writes: %.2f, reads: %.2f, deletes: %.2f)",
				stats.completedQPS(), stats.writeQPS(), stats.readQPS(), stats.deleteQPS())
			t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

			if stats.completedQPS() < tc.expectedMinQPS {
				t.Errorf("QPS %.2f below expected minimum %.2f", stats.completedQPS(), tc.expectedMinQPS)
			}
		})
	}
}

// TestStressGoroutineLeak tests for goroutine leaks under stress
func TestStressGoroutineLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping goroutine leak test in short mode")
	}
	testCases := []struct {
		name            string
		envConfig       *TestEnvironmentConfig
		concurrentOps   int
		testDuration    time.Duration
		maxGoroutines   int
		growthThreshold int
	}{
		{
			name: "SmallCluster_LeakTest",
			envConfig: &TestEnvironmentConfig{
				NetworkProfile: network.ProfileLAN,
				NetworkType:    networkTypeFromEnv(gridkv.TCP),
				NodeCount:      10,
				ReplicaCount:   3,
				BasePort:       40000,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    2048,
				ShardCount:     128,
			},
			concurrentOps:   100,
			testDuration:    60 * time.Second,
			maxGoroutines:   5000,
			growthThreshold: 2000,
		},
		{
			name: "MediumCluster_LeakTest",
			envConfig: &TestEnvironmentConfig{
				NetworkProfile: network.ProfileWAN,
				NetworkType:    networkTypeFromEnv(gridkv.TCP),
				NodeCount:      30,
				ReplicaCount:   3,
				BasePort:       41000,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    4096,
				ShardCount:     256,
			},
			concurrentOps:   300,
			testDuration:    90 * time.Second,
			maxGoroutines:   15000,
			growthThreshold: 5000,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			testStressGoroutineLeakScenario(t, tc.envConfig, tc.concurrentOps, tc.testDuration, tc.maxGoroutines, tc.growthThreshold)
		})
	}
}

func testStressGoroutineLeakScenario(t *testing.T, config *TestEnvironmentConfig, concurrentOps int, testDuration time.Duration, maxGoroutines, growthThreshold int) {
	// Baseline
	runtime.GC()
	time.Sleep(500 * time.Millisecond)
	baselineGoroutines := runtime.NumGoroutine()

	sim := NewTestEnvironmentSimulator(config)
	if err := sim.SetupCluster(t); err != nil {
		t.Fatalf("Failed to setup cluster: %v", err)
	}
	defer sim.Cleanup()

	nodes := sim.GetNodes()
	time.Sleep(5 * time.Second)
	runtime.GC()
	time.Sleep(500 * time.Millisecond)
	afterFormationGoroutines := runtime.NumGoroutine()

	t.Logf("Baseline: %d goroutines", baselineGoroutines)
	t.Logf("After formation: %d goroutines (+%d)", afterFormationGoroutines, afterFormationGoroutines-baselineGoroutines)

	// Monitor goroutines
	var peakGoroutines int64
	monitorStop := make(chan struct{})
	var monitorWg sync.WaitGroup

	monitorWg.Add(1)
	go func() {
		defer monitorWg.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-monitorStop:
				return
			case <-ticker.C:
				count := runtime.NumGoroutine()
				if int64(count) > atomic.LoadInt64(&peakGoroutines) {
					atomic.StoreInt64(&peakGoroutines, int64(count))
				}
			}
		}
	}()

	// Run workload
	ctx := context.Background()
	stopCh := make(chan struct{})
	var wg sync.WaitGroup
	var opsSubmitted atomic.Int64
	var opsCompleted atomic.Int64

	for w := 0; w < concurrentOps; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			nodeIdx := workerID % len(nodes)
			for {
				select {
				case <-stopCh:
					return
				default:
					key := fmt.Sprintf("leak-%d-%d", workerID, opsSubmitted.Load())
					opsSubmitted.Add(1)
					opCtx, opCancel := context.WithTimeout(ctx, 2*time.Second)
					err := nodes[nodeIdx].Set(opCtx, key, []byte(fmt.Sprintf("value-%d", workerID)))
					opCancel()
					if err == nil {
						opsCompleted.Add(1)
					}
					time.Sleep(100 * time.Microsecond)
				}
			}
		}(w)
	}

	time.Sleep(testDuration)
	close(stopCh)
	wg.Wait()
	close(monitorStop)
	monitorWg.Wait()

	time.Sleep(5 * time.Second)
	runtime.GC()
	time.Sleep(3 * time.Second)
	runtime.GC()
	time.Sleep(1 * time.Second)

	finalGoroutines := runtime.NumGoroutine()
	peak := int(atomic.LoadInt64(&peakGoroutines))
	growth := finalGoroutines - baselineGoroutines

	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("   Goroutine Leak Test Results")
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("Baseline:              %d", baselineGoroutines)
	t.Logf("After Formation:       %d (+%d)", afterFormationGoroutines, afterFormationGoroutines-baselineGoroutines)
	t.Logf("Peak During Test:      %d (+%d)", peak, peak-baselineGoroutines)
	t.Logf("Final After Cleanup:   %d (+%d)", finalGoroutines, growth)
	t.Logf("Operations:            %d submitted, %d completed", opsSubmitted.Load(), opsCompleted.Load())
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	leakDetected := false
	if finalGoroutines > maxGoroutines {
		leakDetected = true
		t.Errorf("Goroutine count %d exceeds maximum %d", finalGoroutines, maxGoroutines)
	}
	if growth > growthThreshold {
		leakDetected = true
		t.Errorf("Goroutine growth %d exceeds threshold %d", growth, growthThreshold)
	}

	if !leakDetected {
		t.Logf("✓ No goroutine leak detected")
	}
}

// TestStressMemoryLeak tests for memory leaks under sustained load
func TestStressMemoryLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping memory leak test in short mode")
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
	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	var memStatsBefore runtime.MemStats
	var memStatsAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memStatsBefore)

	var opsCount atomic.Int64
	for w := 0; w < 50; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			nodeIdx := workerID % len(nodes)
			for {
				select {
				case <-stopCh:
					return
				default:
					key := fmt.Sprintf("mem-%d-%d", workerID, opsCount.Load())
					value := make([]byte, 1024)
					rand.Read(value)
					nodes[nodeIdx].Set(ctx, key, value)
					opsCount.Add(1)
					time.Sleep(10 * time.Millisecond)
				}
			}
		}(w)
	}

	time.Sleep(120 * time.Second)
	close(stopCh)
	wg.Wait()

	time.Sleep(5 * time.Second)
	runtime.GC()
	runtime.ReadMemStats(&memStatsAfter)

	heapGrowth := memStatsAfter.HeapInuse - memStatsBefore.HeapInuse
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("   Memory Leak Test Results")
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("Operations:        %d", opsCount.Load())
	t.Logf("Heap growth:       %d MB", heapGrowth/(1024*1024))
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// Allow reasonable growth for sustained operations
	if heapGrowth > 500*1024*1024 {
		t.Errorf("Excessive heap growth: %d MB", heapGrowth/(1024*1024))
	}
}

// TestStressPerformanceOperations tests write/read/delete performance separately
func TestStressPerformanceOperations(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping performance operations test in short mode")
	}
	config := &TestEnvironmentConfig{
		NetworkProfile: network.ProfileLAN,
		NetworkType:    networkTypeFromEnv(gridkv.TCP),
		NodeCount:      3,
		ReplicaCount:   3,
		BasePort:       100000,
		StorageBackend: gridkv.BackendMemorySharded,
		MaxMemoryMB:    1024,
		ShardCount:     128,
	}

	sim := NewTestEnvironmentSimulator(config)
	if err := sim.SetupCluster(t); err != nil {
		t.Fatalf("Failed to setup cluster: %v", err)
	}
	defer sim.Cleanup()

	nodes := sim.GetNodes()
	ctx := context.Background()

	t.Run("WriteStress", func(t *testing.T) {
		numWrites := 5000
		start := time.Now()
		var wg sync.WaitGroup
		var successCount atomic.Int64

		for i := 0; i < numWrites; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				key := fmt.Sprintf("perf-write-%d", idx)
				value := []byte(fmt.Sprintf("perf-value-%d", idx))
				if err := nodes[0].Set(ctx, key, value); err == nil {
					successCount.Add(1)
				}
			}(i)
		}

		wg.Wait()
		duration := time.Since(start)
		success := successCount.Load()
		qps := float64(success) / duration.Seconds()

		t.Logf("Write stress: %d writes in %v (%.2f ops/s)", success, duration, qps)
		if success < int64(numWrites*9/10) {
			t.Errorf("Expected at least 90%% success rate, got %d/%d", success, numWrites)
		}
		if qps < 10000 {
			t.Errorf("Expected at least 10K ops/s, got %.2f", qps)
		}
	})

	time.Sleep(10 * time.Second)

	t.Run("ReadStress", func(t *testing.T) {
		numReads := 10000
		start := time.Now()
		var wg sync.WaitGroup
		var successCount atomic.Int64

		for i := 0; i < numReads; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				key := fmt.Sprintf("perf-write-%d", idx%5000)
				for nodeIdx := 0; nodeIdx < len(nodes); nodeIdx++ {
					value, err := nodes[nodeIdx].Get(ctx, key)
					if err == nil && len(value) > 0 {
						successCount.Add(1)
						return
					}
				}
			}(i)
		}

		wg.Wait()
		duration := time.Since(start)
		success := successCount.Load()
		qps := float64(success) / duration.Seconds()

		t.Logf("Read stress: %d reads in %v (%.2f ops/s)", success, duration, qps)
		if success < int64(numReads*8/10) {
			t.Errorf("Expected at least 80%% success rate, got %d/%d", success, numReads)
		}
		if qps < 50000 {
			t.Errorf("Expected at least 50K ops/s, got %.2f", qps)
		}
	})

	t.Run("DeleteStress", func(t *testing.T) {
		numDeletes := 2000
		start := time.Now()
		var wg sync.WaitGroup
		var successCount atomic.Int64

		for i := 0; i < numDeletes; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				key := fmt.Sprintf("perf-write-%d", idx)
				if err := nodes[0].Delete(ctx, key); err == nil {
					successCount.Add(1)
				}
			}(i)
		}

		wg.Wait()
		duration := time.Since(start)
		success := successCount.Load()
		qps := float64(success) / duration.Seconds()

		t.Logf("Delete stress: %d deletes in %v (%.2f ops/s)", success, duration, qps)
		if success < int64(numDeletes*9/10) {
			t.Errorf("Expected at least 90%% success rate, got %d/%d", success, numDeletes)
		}
		if qps < 50000 {
			t.Errorf("Expected at least 50K ops/s, got %.2f", qps)
		}
	})
}

// TestStressBatchOperations tests batch write/delete performance
func TestStressBatchOperations(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping batch operations test in short mode")
	}
	config := &TestEnvironmentConfig{
		NetworkProfile: network.ProfileLAN,
		NetworkType:    networkTypeFromEnv(gridkv.TCP),
		NodeCount:      3,
		ReplicaCount:   3,
		BasePort:       101000,
		StorageBackend: gridkv.BackendMemorySharded,
		MaxMemoryMB:    512,
		ShardCount:     128,
	}

	sim := NewTestEnvironmentSimulator(config)
	if err := sim.SetupCluster(t); err != nil {
		t.Fatalf("Failed to setup cluster: %v", err)
	}
	defer sim.Cleanup()

	nodes := sim.GetNodes()
	ctx := context.Background()
	time.Sleep(3 * time.Second)

	t.Run("BatchWrite", func(t *testing.T) {
		numWrites := 1000
		start := time.Now()
		var wg sync.WaitGroup
		var successCount atomic.Int64

		for i := 0; i < numWrites; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				key := fmt.Sprintf("batch-write-%d", idx)
				value := []byte(fmt.Sprintf("batch-value-%d", idx))
				if err := nodes[0].Set(ctx, key, value); err == nil {
					successCount.Add(1)
				}
			}(i)
		}

		wg.Wait()
		duration := time.Since(start)
		success := successCount.Load()
		t.Logf("Batch write: %d writes in %v (%.2f ops/s)", success, duration, float64(success)/duration.Seconds())

		if success < int64(numWrites*9/10) {
			t.Errorf("Expected at least 90%% success rate, got %d/%d", success, numWrites)
		}

		time.Sleep(500 * time.Millisecond)
		verifyCount := 0
		for i := 0; i < 100 && i < numWrites; i++ {
			key := fmt.Sprintf("batch-write-%d", i)
			expectedValue := []byte(fmt.Sprintf("batch-value-%d", i))
			value, err := nodes[1].Get(ctx, key)
			if err == nil && string(value) == string(expectedValue) {
				verifyCount++
			}
		}

		if verifyCount < 90 {
			t.Errorf("Expected at least 90 verified reads, got %d", verifyCount)
		}
	})

	t.Run("BatchDelete", func(t *testing.T) {
		numDeletes := 500
		start := time.Now()
		var wg sync.WaitGroup
		var successCount atomic.Int64

		for i := 0; i < numDeletes; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				key := fmt.Sprintf("batch-write-%d", idx)
				if err := nodes[0].Delete(ctx, key); err == nil {
					successCount.Add(1)
				}
			}(i)
		}

		wg.Wait()
		duration := time.Since(start)
		success := successCount.Load()
		t.Logf("Batch delete: %d deletes in %v (%.2f ops/s)", success, duration, float64(success)/duration.Seconds())

		if success < int64(numDeletes*8/10) {
			t.Errorf("Expected at least 80%% success rate, got %d/%d", success, numDeletes)
		}
	})
}

// TestStressAsyncRead tests asynchronous read operations performance
func TestStressAsyncRead(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping async read test in short mode")
	}
	config := &TestEnvironmentConfig{
		NetworkProfile: network.ProfileLAN,
		NetworkType:    networkTypeFromEnv(gridkv.TCP),
		NodeCount:      3,
		ReplicaCount:   3,
		BasePort:       102000,
		StorageBackend: gridkv.BackendMemorySharded,
		MaxMemoryMB:    512,
		ShardCount:     128,
	}

	sim := NewTestEnvironmentSimulator(config)
	if err := sim.SetupCluster(t); err != nil {
		t.Fatalf("Failed to setup cluster: %v", err)
	}
	defer sim.Cleanup()

	nodes := sim.GetNodes()
	ctx := context.Background()
	time.Sleep(3 * time.Second)

	// Pre-populate data
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("async-key-%d", i)
		value := []byte(fmt.Sprintf("async-value-%d", i))
		nodes[0].Set(ctx, key, value)
	}
	time.Sleep(2 * time.Second)

	t.Run("AsyncRead", func(t *testing.T) {
		numReads := 1000
		start := time.Now()
		var wg sync.WaitGroup
		var successCount atomic.Int64

		for i := 0; i < numReads; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				key := fmt.Sprintf("async-key-%d", idx%100)
				future, err := nodes[0].GetAsync(ctx, key)
				if err != nil {
					return
				}
				value, err := future.Get(ctx)
				if err == nil && len(value) > 0 {
					successCount.Add(1)
				}
			}(i)
		}

		wg.Wait()
		duration := time.Since(start)
		success := successCount.Load()
		qps := float64(success) / duration.Seconds()

		t.Logf("Async read: %d reads in %v (%.2f ops/s)", success, duration, qps)
		if success < int64(numReads*9/10) {
			t.Errorf("Expected at least 90%% success rate, got %d/%d", success, numReads)
		}
	})
}

// TestMemoryBackendStress exercises the Memory backend across cluster sizes.
func TestMemoryBackendStress(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping memory backend stress test in short mode")
	}

	testCases := []struct {
		name          string
		nodeCount     int
		basePort      int
		concurrentOps int
		duration      time.Duration
	}{
		{"Memory_LAN_3nodes", 3, 20000, 60, 15 * time.Second},
		{"Memory_LAN_5nodes", 5, 20100, 100, 20 * time.Second},
		{"Memory_LAN_10nodes", 10, 20200, 180, 25 * time.Second},
	}

	selected := os.Getenv("GRIDKV_MEM_CLUSTER_CASE")
	ran := false

	for _, tc := range testCases {
		if selected != "" && tc.name != selected {
			continue
		}
		ran = true

		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			config := &TestEnvironmentConfig{
				NetworkProfile: network.ProfileLAN,
				NetworkType:    networkTypeFromEnv(gridkv.TCP),
				NodeCount:      tc.nodeCount,
				ReplicaCount:   3,
				BasePort:       tc.basePort,
				StorageBackend: gridkv.BackendMemory,
				MaxMemoryMB:    2048,
			}

			stats := runMixedClusterWorkload(t, config, tc.concurrentOps, tc.duration)

			t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			t.Logf("   Memory Backend Stress Results (%s)", tc.name)
			t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			t.Logf("Nodes: %d, Duration: %.1fs, Workers: %d", tc.nodeCount, tc.duration.Seconds(), tc.concurrentOps)
			t.Logf("Writes:  %d submitted / %d completed (%.1f%%)",
				stats.writesSubmitted.Load(), stats.writesCompleted.Load(),
				stats.successRate(stats.writesSubmitted.Load(), stats.writesCompleted.Load()))
			t.Logf("Reads:   %d submitted / %d completed (%.1f%%)",
				stats.readsSubmitted.Load(), stats.readsCompleted.Load(),
				stats.successRate(stats.readsSubmitted.Load(), stats.readsCompleted.Load()))
			t.Logf("Deletes: %d submitted / %d completed (%.1f%%)",
				stats.deletesSubmitted.Load(), stats.deletesCompleted.Load(),
				stats.successRate(stats.deletesSubmitted.Load(), stats.deletesCompleted.Load()))
			t.Logf("Completed QPS: %.2f (writes: %.2f, reads: %.2f, deletes: %.2f)",
				stats.completedQPS(), stats.writeQPS(), stats.readQPS(), stats.deleteQPS())
			t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		})
	}

	if !ran {
		t.Fatalf("No test cases executed. Set GRIDKV_MEM_CLUSTER_CASE to one of: Memory_LAN_3nodes, Memory_LAN_5nodes, Memory_LAN_10nodes")
	}
}

// TestMemoryBackendPerformance captures single-node Memory backend perf metrics.
func TestMemoryBackendPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping memory backend performance test in short mode")
	}

	config := &TestEnvironmentConfig{
		NetworkProfile: network.ProfileLAN,
		NetworkType:    networkTypeFromEnv(gridkv.TCP),
		NodeCount:      3,
		ReplicaCount:   3,
		BasePort:       21000,
		StorageBackend: gridkv.BackendMemory,
		MaxMemoryMB:    1024,
	}

	sim := NewTestEnvironmentSimulator(config)
	if err := sim.SetupCluster(t); err != nil {
		t.Fatalf("Failed to setup cluster: %v", err)
	}
	defer sim.Cleanup()

	nodes := sim.GetNodes()
	ctx := context.Background()
	time.Sleep(3 * time.Second)

	t.Run("WritePerformance", func(t *testing.T) {
		numWrites := 5000
		start := time.Now()
		var wg sync.WaitGroup
		var successCount atomic.Int64

		for i := 0; i < numWrites; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				key := fmt.Sprintf("perf-write-%d", idx)
				value := make([]byte, 500)
				for j := range value {
					value[j] = byte(j % 128)
				}
				if err := nodes[0].Set(ctx, key, value); err == nil {
					successCount.Add(1)
				}
			}(i)
		}

		wg.Wait()
		duration := time.Since(start)
		success := successCount.Load()
		qps := float64(success) / duration.Seconds()

		t.Logf("Write Performance: %d writes in %v (%.2f ops/s)", success, duration, qps)
		if success < int64(numWrites*9/10) {
			t.Errorf("Expected at least 90%% success rate, got %d/%d", success, numWrites)
		}
		if qps < 8000 {
			t.Errorf("Expected at least 8K ops/s, got %.2f", qps)
		}
	})

	time.Sleep(5 * time.Second)

	t.Run("ReadPerformance", func(t *testing.T) {
		numReads := 10000
		start := time.Now()
		var wg sync.WaitGroup
		var successCount atomic.Int64

		for i := 0; i < numReads; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				key := fmt.Sprintf("perf-write-%d", idx%5000)
				for nodeIdx := 0; nodeIdx < len(nodes); nodeIdx++ {
					value, err := nodes[nodeIdx].Get(ctx, key)
					if err == nil && len(value) > 0 {
						successCount.Add(1)
						return
					}
				}
			}(i)
		}

		wg.Wait()
		duration := time.Since(start)
		success := successCount.Load()
		qps := float64(success) / duration.Seconds()

		t.Logf("Read Performance: %d reads in %v (%.2f ops/s)", success, duration, qps)
		if success < int64(numReads*8/10) {
			t.Errorf("Expected at least 80%% success rate, got %d/%d", success, numReads)
		}
		if qps < 40000 {
			t.Errorf("Expected at least 40K ops/s, got %.2f", qps)
		}
	})
}

// TestMemoryShardedClusterPerf captures MemorySharded cluster throughput tables.
func TestMemoryShardedClusterPerf(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping memory sharded cluster perf test in short mode")
	}

	testCases := []struct {
		name          string
		nodeCount     int
		basePort      int
		concurrentOps int
		duration      time.Duration
	}{
		{"Sharded_LAN_3nodes", 3, 30000, 80, 15 * time.Second},
		{"Sharded_LAN_5nodes", 5, 30100, 140, 20 * time.Second},
		{"Sharded_LAN_10nodes", 10, 30200, 220, 25 * time.Second},
		{"Sharded_LAN_15nodes", 15, 30350, 320, 30 * time.Second},
	}

	selected := os.Getenv("GRIDKV_SHARDED_CLUSTER_CASE")
	ran := false

	for _, tc := range testCases {
		if selected != "" && tc.name != selected {
			continue
		}
		ran = true

		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			config := &TestEnvironmentConfig{
				NetworkProfile: network.ProfileLAN,
				NetworkType:    networkTypeFromEnv(gridkv.TCP),
				NodeCount:      tc.nodeCount,
				ReplicaCount:   3,
				BasePort:       tc.basePort,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    4096,
				ShardCount:     256,
			}

			stats := runMixedClusterWorkload(t, config, tc.concurrentOps, tc.duration)

			t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			t.Logf("   MemorySharded Cluster Perf (%s)", tc.name)
			t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			t.Logf("Nodes: %d, Duration: %.1fs, Workers: %d", tc.nodeCount, tc.duration.Seconds(), tc.concurrentOps)
			t.Logf("Writes:  %d submitted / %d completed (%.1f%%)",
				stats.writesSubmitted.Load(), stats.writesCompleted.Load(),
				stats.successRate(stats.writesSubmitted.Load(), stats.writesCompleted.Load()))
			t.Logf("Reads:   %d submitted / %d completed (%.1f%%)",
				stats.readsSubmitted.Load(), stats.readsCompleted.Load(),
				stats.successRate(stats.readsSubmitted.Load(), stats.readsCompleted.Load()))
			t.Logf("Deletes: %d submitted / %d completed (%.1f%%)",
				stats.deletesSubmitted.Load(), stats.deletesCompleted.Load(),
				stats.successRate(stats.deletesSubmitted.Load(), stats.deletesCompleted.Load()))
			t.Logf("Completed QPS: %.2f (writes: %.2f, reads: %.2f, deletes: %.2f)",
				stats.completedQPS(), stats.writeQPS(), stats.readQPS(), stats.deleteQPS())
			t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		})
	}

	if !ran {
		t.Fatalf("No test cases executed. Set GRIDKV_SHARDED_CLUSTER_CASE to one of: Sharded_LAN_3nodes, Sharded_LAN_5nodes, Sharded_LAN_10nodes, Sharded_LAN_15nodes")
	}
}

// workloadStats captures per-workload metrics for mixed scenarios.
type workloadStats struct {
	writesSubmitted  atomic.Int64
	writesCompleted  atomic.Int64
	readsSubmitted   atomic.Int64
	readsCompleted   atomic.Int64
	deletesSubmitted atomic.Int64
	deletesCompleted atomic.Int64
	duration         time.Duration
}

func (s *workloadStats) successRate(submitted, completed int64) float64 {
	if submitted == 0 {
		return 0
	}
	return float64(completed) / float64(submitted) * 100
}

func (s *workloadStats) totalCompleted() int64 {
	return s.writesCompleted.Load() + s.readsCompleted.Load() + s.deletesCompleted.Load()
}

func (s *workloadStats) completedQPS() float64 {
	if s.duration <= 0 {
		return 0
	}
	return float64(s.totalCompleted()) / s.duration.Seconds()
}

func (s *workloadStats) writeQPS() float64 {
	if s.duration <= 0 {
		return 0
	}
	return float64(s.writesCompleted.Load()) / s.duration.Seconds()
}

func (s *workloadStats) readQPS() float64 {
	if s.duration <= 0 {
		return 0
	}
	return float64(s.readsCompleted.Load()) / s.duration.Seconds()
}

func (s *workloadStats) deleteQPS() float64 {
	if s.duration <= 0 {
		return 0
	}
	return float64(s.deletesCompleted.Load()) / s.duration.Seconds()
}

// runMixedClusterWorkload provisions a cluster and runs the mixed workload.
func runMixedClusterWorkload(t *testing.T, config *TestEnvironmentConfig, concurrentOps int, duration time.Duration) *workloadStats {
	sim := NewTestEnvironmentSimulator(config)
	if err := sim.SetupCluster(t); err != nil {
		t.Fatalf("Failed to setup cluster: %v", err)
	}
	defer sim.Cleanup()

	sim.WaitForHealthyNodes(t, config.NodeCount, 15*time.Second)

	nodes := sim.GetNodes()
	if len(nodes) == 0 {
		t.Fatalf("No nodes available in cluster")
	}

	ctx := context.Background()
	stats := &workloadStats{duration: duration}
	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	time.Sleep(2 * time.Second)

	cache := newKeyCache(50000)
	seedKeyCount := 500
	for i := 0; i < seedKeyCount; i++ {
		key := fmt.Sprintf("seed-%d", i)
		value := randomValue(128)
		node := nodes[i%len(nodes)]
		if err := node.Set(ctx, key, value); err == nil {
			cache.add(key)
		} else {
			t.Logf("seed write failed for %s: %v", key, err)
			time.Sleep(50 * time.Millisecond)
		}
	}

	writers := max(1, concurrentOps/3)
	readers := max(1, concurrentOps/2)
	deleters := max(1, concurrentOps/6)
	writerDelay, readerDelay, deleteDelay := workloadDelays()
	if writerDelay > 0 || readerDelay > 0 || deleteDelay > 0 {
		t.Logf("Workload throttles enabled (writers=%s, readers=%s, deleters=%s)", writerDelay, readerDelay, deleteDelay)
	} else {
		t.Logf("Workload throttles disabled (GRIDKV_NO_THROTTLE=%s)", os.Getenv("GRIDKV_NO_THROTTLE"))
	}

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			nodeIdx := workerID % len(nodes)
			for {
				select {
				case <-stopCh:
					return
				default:
					key := fmt.Sprintf("mixed-w-%d-%d", workerID, stats.writesSubmitted.Load())
					value := []byte(fmt.Sprintf("value-%d", time.Now().UnixNano()))
					stats.writesSubmitted.Add(1)
					if err := nodes[nodeIdx].Set(ctx, key, value); err == nil {
						stats.writesCompleted.Add(1)
						cache.add(key)
					}
					if writerDelay > 0 {
						time.Sleep(writerDelay)
					}
				}
			}
		}(w)
	}

	time.Sleep(2 * time.Second)
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			nodeIdx := workerID % len(nodes)
			for {
				select {
				case <-stopCh:
					return
				default:
					key, ok := cache.random()
					if !ok {
						if readerDelay > 0 {
							time.Sleep(readerDelay)
						}
						continue
					}
					stats.readsSubmitted.Add(1)
					if value, err := nodes[nodeIdx].Get(ctx, key); err == nil && len(value) > 0 {
						stats.readsCompleted.Add(1)
					}
					if readerDelay > 0 {
						time.Sleep(readerDelay)
					}
				}
			}
		}(r)
	}

	for d := 0; d < deleters; d++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			nodeIdx := workerID % len(nodes)
			for {
				select {
				case <-stopCh:
					return
				default:
					key, ok := cache.random()
					if !ok {
						if deleteDelay > 0 {
							time.Sleep(deleteDelay)
						}
						continue
					}
					stats.deletesSubmitted.Add(1)
					if err := nodes[nodeIdx].Delete(ctx, key); err == nil {
						stats.deletesCompleted.Add(1)
						cache.remove(key)
					}
					if deleteDelay > 0 {
						time.Sleep(deleteDelay)
					}
				}
			}
		}(d)
	}

	time.Sleep(duration)
	close(stopCh)
	wg.Wait()

	return stats
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

type keyCache struct {
	mu    sync.RWMutex
	keys  []string
	limit int
}

func newKeyCache(limit int) *keyCache {
	return &keyCache{limit: limit}
}

func (kc *keyCache) add(key string) {
	kc.mu.Lock()
	kc.keys = append(kc.keys, key)
	if kc.limit > 0 && len(kc.keys) > kc.limit {
		kc.keys = kc.keys[len(kc.keys)-kc.limit:]
	}
	kc.mu.Unlock()
}

func (kc *keyCache) remove(key string) {
	kc.mu.Lock()
	for i, k := range kc.keys {
		if k == key {
			kc.keys = append(kc.keys[:i], kc.keys[i+1:]...)
			break
		}
	}
	kc.mu.Unlock()
}

func (kc *keyCache) random() (string, bool) {
	kc.mu.RLock()
	defer kc.mu.RUnlock()
	if len(kc.keys) == 0 {
		return "", false
	}
	return kc.keys[rand.Intn(len(kc.keys))], true
}

func workloadDelays() (time.Duration, time.Duration, time.Duration) {
	if throttlingDisabled() {
		return 0, 0, 0
	}
	return 10 * time.Millisecond, 5 * time.Millisecond, 20 * time.Millisecond
}

func throttlingDisabled() bool {
	env := strings.TrimSpace(strings.ToLower(os.Getenv("GRIDKV_NO_THROTTLE")))
	switch env {
	case "1", "true", "yes", "peak", "off", "disable", "disabled":
		return true
	default:
		return false
	}
}
