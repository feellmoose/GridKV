package tests

import (
	"context"
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gridkv "github.com/feellmoose/gridkv"
	"github.com/feellmoose/gridkv/internal/utils/network"
)

// TestStressPressure tests system under various pressure scenarios
func TestStressPressure(t *testing.T) {
	testCases := []struct {
		name           string
		envConfig      *TestEnvironmentConfig
		concurrentOps  int
		testDuration   time.Duration
		opsPerSecond   int
		expectedMinQPS float64
	}{
		{
			name: "SmallCluster_LowLatency",
			envConfig: &TestEnvironmentConfig{
				NetworkProfile: network.ProfileLAN,
				NetworkType:    gridkv.TCP,
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
				NetworkType:    gridkv.TCP,
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
				NetworkType:    gridkv.TCP,
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
			testStressScenario(t, tc.envConfig, tc.concurrentOps, tc.testDuration, tc.expectedMinQPS)
		})
	}
}

func testStressScenario(t *testing.T, config *TestEnvironmentConfig, concurrentOps int, testDuration time.Duration, expectedMinQPS float64) {
	sim := NewTestEnvironmentSimulator(config)
	if err := sim.SetupCluster(t); err != nil {
		t.Fatalf("Failed to setup cluster: %v", err)
	}
	defer sim.Cleanup()

	nodes := sim.GetNodes()
	ctx := context.Background()
	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	var (
		writesSubmitted  atomic.Int64
		writesCompleted  atomic.Int64
		readsSubmitted   atomic.Int64
		readsCompleted   atomic.Int64
		deletesSubmitted atomic.Int64
		deletesCompleted atomic.Int64
	)

	// Writer workers
	for w := 0; w < concurrentOps/3; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			nodeIdx := workerID % len(nodes)
			for {
				select {
				case <-stopCh:
					return
				default:
					key := fmt.Sprintf("stress-w-%d-%d", workerID, writesSubmitted.Load())
					value := []byte(fmt.Sprintf("value-%d", time.Now().UnixNano()))
					writesSubmitted.Add(1)
					if err := nodes[nodeIdx].Set(ctx, key, value); err == nil {
						writesCompleted.Add(1)
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
		}(w)
	}

	// Reader workers
	time.Sleep(2 * time.Second)
	for r := 0; r < concurrentOps/2; r++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			nodeIdx := workerID % len(nodes)
			for {
				select {
				case <-stopCh:
					return
				default:
					key := fmt.Sprintf("stress-w-%d-%d", workerID%len(nodes), rand.Intn(1000))
					readsSubmitted.Add(1)
					_, err := nodes[nodeIdx].Get(ctx, key)
					if err == nil {
						readsCompleted.Add(1)
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
		}(r)
	}

	// Delete workers
	for d := 0; d < concurrentOps/6; d++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			nodeIdx := workerID % len(nodes)
			for {
				select {
				case <-stopCh:
					return
				default:
					key := fmt.Sprintf("stress-w-%d-%d", workerID%len(nodes), rand.Intn(1000))
					deletesSubmitted.Add(1)
					if err := nodes[nodeIdx].Delete(ctx, key); err == nil {
						deletesCompleted.Add(1)
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
		}(d)
	}

	time.Sleep(testDuration)
	close(stopCh)
	wg.Wait()

	elapsed := testDuration.Seconds()
	totalOps := writesSubmitted.Load() + readsSubmitted.Load() + deletesSubmitted.Load()
	qps := float64(totalOps) / elapsed

	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("   Stress Test Results")
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("Writes:  %d submitted, %d completed", writesSubmitted.Load(), writesCompleted.Load())
	t.Logf("Reads:   %d submitted, %d completed", readsSubmitted.Load(), readsCompleted.Load())
	t.Logf("Deletes: %d submitted, %d completed", deletesSubmitted.Load(), deletesCompleted.Load())
	t.Logf("Total QPS: %.2f (expected: %.2f)", qps, expectedMinQPS)
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	if qps < expectedMinQPS {
		t.Errorf("QPS %.2f below expected minimum %.2f", qps, expectedMinQPS)
	}

	writeSuccessRate := float64(writesCompleted.Load()) / float64(writesSubmitted.Load()) * 100
	if writeSuccessRate < 80 {
		t.Errorf("Write success rate %.2f%% too low", writeSuccessRate)
	}
}

// TestStressGoroutineLeak tests for goroutine leaks under stress
func TestStressGoroutineLeak(t *testing.T) {
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
				NetworkType:    gridkv.TCP,
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
				NetworkType:    gridkv.TCP,
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
		NetworkType:    gridkv.TCP,
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
	config := &TestEnvironmentConfig{
		NetworkProfile: network.ProfileLAN,
		NetworkType:    gridkv.TCP,
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
	config := &TestEnvironmentConfig{
		NetworkProfile: network.ProfileLAN,
		NetworkType:    gridkv.TCP,
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
	config := &TestEnvironmentConfig{
		NetworkProfile: network.ProfileLAN,
		NetworkType:    gridkv.TCP,
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
