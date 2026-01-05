package tests

import (
	"context"
	cryptorand "crypto/rand"
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gridkv "github.com/feellmoose/gridkv"
	"github.com/zeebo/xxh3"
)

// OperationProfile defines the ratio of read/write/delete operations
type OperationProfile struct {
	WriteRatio  float64 // 0.0-1.0, ratio of write operations
	ReadRatio   float64 // 0.0-1.0, ratio of read operations (write + read should be <= 1.0)
	DeleteRatio float64 // 0.0-1.0, ratio of delete operations (write + read + delete should be <= 1.0)
}

var (
	// Balanced profile: 40% write, 40% read, 20% delete
	ProfileBalanced = OperationProfile{WriteRatio: 0.4, ReadRatio: 0.4, DeleteRatio: 0.2}

	// ReadHeavy profile: 20% write, 70% read, 10% delete
	ProfileReadHeavy = OperationProfile{WriteRatio: 0.2, ReadRatio: 0.7, DeleteRatio: 0.1}

	// WriteHeavy profile: 70% write, 20% read, 10% delete
	ProfileWriteHeavy = OperationProfile{WriteRatio: 0.7, ReadRatio: 0.2, DeleteRatio: 0.1}

	// ReadOnly profile: 0% write, 90% read, 10% delete
	ProfileReadOnly = OperationProfile{WriteRatio: 0.0, ReadRatio: 0.9, DeleteRatio: 0.1}
)

// PerformanceMetrics holds performance measurement data
type PerformanceMetrics struct {
	StartTime       time.Time
	EndTime         time.Time
	StartGoroutines int
	EndGoroutines   int
	StartMemStats   runtime.MemStats
	EndMemStats     runtime.MemStats
	WriteOps        int64
	ReadOps         int64
	DeleteOps       int64
	TotalOps        int64
}

// captureMetrics captures current system metrics
func captureMetrics() PerformanceMetrics {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return PerformanceMetrics{
		StartTime:       time.Now(),
		StartGoroutines: runtime.NumGoroutine(),
		StartMemStats:   m,
	}
}

// finalizeMetrics finalizes and calculates performance metrics
func finalizeMetrics(metrics *PerformanceMetrics, writeOps, readOps, deleteOps int64) {
	metrics.EndTime = time.Now()
	metrics.EndGoroutines = runtime.NumGoroutine()
	runtime.ReadMemStats(&metrics.EndMemStats)

	metrics.WriteOps = writeOps
	metrics.ReadOps = readOps
	metrics.DeleteOps = deleteOps
	metrics.TotalOps = writeOps + readOps + deleteOps
}

// reportMetrics reports performance metrics to benchmark
func reportMetrics(b *testing.B, metrics *PerformanceMetrics, profile OperationProfile) {
	duration := metrics.EndTime.Sub(metrics.StartTime)
	if duration == 0 {
		return
	}

	// QPS metrics
	writeQPS := float64(metrics.WriteOps) / duration.Seconds()
	readQPS := float64(metrics.ReadOps) / duration.Seconds()
	deleteQPS := float64(metrics.DeleteOps) / duration.Seconds()
	totalQPS := float64(metrics.TotalOps) / duration.Seconds()

	// Report QPS metrics with better naming
	b.ReportMetric(writeQPS, "write-qps")
	b.ReportMetric(readQPS, "read-qps")
	b.ReportMetric(deleteQPS, "delete-qps")
	b.ReportMetric(totalQPS, "total-qps")

	// Memory metrics (in MB for readability)
	memIncreaseMB := float64(metrics.EndMemStats.Alloc-metrics.StartMemStats.Alloc) / (1024 * 1024)
	b.ReportMetric(memIncreaseMB, "mem-alloc-mb")

	// Goroutine metrics
	goroutineIncrease := metrics.EndGoroutines - metrics.StartGoroutines
	b.ReportMetric(float64(goroutineIncrease), "goroutines-delta")

	// CPU time (rough estimate via GC CPU fraction)
	gcCPUFraction := metrics.EndMemStats.GCCPUFraction - metrics.StartMemStats.GCCPUFraction
	b.ReportMetric(gcCPUFraction*100, "gc-cpu-percent")

	// Print formatted summary for better analysis
	b.Logf("Performance Summary:")
	b.Logf("  Duration: %v", duration.Round(time.Millisecond))
	b.Logf("  Operations: W=%.0f R=%.0f D=%.0f Total=%.0f",
		float64(metrics.WriteOps), float64(metrics.ReadOps), float64(metrics.DeleteOps), float64(metrics.TotalOps))
	b.Logf("  QPS: W=%.0f R=%.0f D=%.0f Total=%.0f",
		writeQPS, readQPS, deleteQPS, totalQPS)
	b.Logf("  Memory: +%.1f MB (GC CPU: %.2f%%)",
		memIncreaseMB, gcCPUFraction*100)
	b.Logf("  Goroutines: %+d", goroutineIncrease)

	// Operation ratios
	if metrics.TotalOps > 0 {
		b.ReportMetric(float64(metrics.WriteOps)/float64(metrics.TotalOps)*100, "write-ratio-percent")
		b.ReportMetric(float64(metrics.ReadOps)/float64(metrics.TotalOps)*100, "read-ratio-percent")
		b.ReportMetric(float64(metrics.DeleteOps)/float64(metrics.TotalOps)*100, "delete-ratio-percent")
	}

	// Log detailed metrics
	b.Logf("Performance Summary:")
	b.Logf("  Duration: %v", duration)
	b.Logf("  Total Operations: %d", metrics.TotalOps)
	b.Logf("  Write QPS: %.2f", writeQPS)
	b.Logf("  Read QPS: %.2f", readQPS)
	b.Logf("  Delete QPS: %.2f", deleteQPS)
	b.Logf("  Total QPS: %.2f", totalQPS)
	b.Logf("  Memory: +%.1f MB", memIncreaseMB)
	b.Logf("  Goroutines: %d -> %d (Δ%d)", metrics.StartGoroutines, metrics.EndGoroutines, goroutineIncrease)
	b.Logf("  Operation Profile: W:%.1f%% R:%.1f%% D:%.1f%%",
		profile.WriteRatio*100, profile.ReadRatio*100, profile.DeleteRatio*100)
}

// Shared cluster for benchmarks to avoid repeated initialization
var (
	sharedBenchCluster     *TestEnvironmentSimulator
	sharedBenchClusterOnce sync.Once
	sharedBenchClusterMu   sync.RWMutex
)

// getSharedBenchCluster returns a shared cluster for benchmarks
// Uses smaller cluster when -short flag is set, with environment variable overrides
func getSharedBenchCluster(b *testing.B) *TestEnvironmentSimulator {
	sharedBenchClusterOnce.Do(func() {
		nodeCount := GetEnvInt("BENCH_NODE_COUNT", 50)
		maxMemoryMB := GetEnvInt64("BENCH_MAX_MEMORY_MB", 256)
		if testing.Short() {
			nodeCount = GetEnvInt("BENCH_SHORT_NODE_COUNT", 5)
			maxMemoryMB = GetEnvInt64("BENCH_SHORT_MEMORY_MB", 128)
		}
		config := &TestEnvironmentConfig{
			NetworkProfile: ProfileLAN,
			NetworkType:    networkTypeFromEnv(gridkv.TCP),
			NodeCount:      nodeCount,
			ReplicaCount:   GetEnvInt("BENCH_REPLICA_COUNT", 3),
			BasePort:       GetEnvInt("BENCH_BASE_PORT", 26000),
			MaxMemoryMB:    maxMemoryMB,
			ShardCount:     GetEnvInt("BENCH_SHARD_COUNT", 64),
		}
		sim := NewTestEnvironmentSimulator(config)
		// Use SetupClusterOptimized which includes optimizations
		if err := sim.SetupClusterOptimized(b); err != nil {
			b.Fatalf("Failed to setup shared cluster: %v", err)
		}
		sharedBenchCluster = sim
	})
	return sharedBenchCluster
}

// getLargeBenchCluster returns a large cluster for benchmarks
func getLargeBenchCluster(b *testing.B) *TestEnvironmentSimulator {
	if testing.Short() {
		b.Skip("Skipping large cluster benchmark in short mode")
	}
	config := &TestEnvironmentConfig{
		NetworkProfile: ProfileLAN,
		NetworkType:    networkTypeFromEnv(gridkv.TCP),
		NodeCount:      1000,
		ReplicaCount:   5,
		BasePort:       27000,
		MaxMemoryMB:    2048,
		ShardCount:     256,
	}
	sim := NewTestEnvironmentSimulator(config)
	if err := sim.SetupClusterOptimized(b); err != nil {
		b.Fatalf("Failed to setup large cluster: %v", err)
	}
	return sim
}

// BenchmarkMixedOpsQPS benchmarks mixed operations with configurable ratios
func BenchmarkMixedOpsQPS(b *testing.B) {
	benchmarkMixedOpsWithProfile(b, ProfileBalanced, "balanced")
}

func BenchmarkMixedOpsQPS_ReadHeavy(b *testing.B) {
	benchmarkMixedOpsWithProfile(b, ProfileReadHeavy, "read-heavy")
}

func BenchmarkMixedOpsQPS_WriteHeavy(b *testing.B) {
	benchmarkMixedOpsWithProfile(b, ProfileWriteHeavy, "write-heavy")
}

func BenchmarkMixedOpsQPS_ReadOnly(b *testing.B) {
	benchmarkMixedOpsWithProfile(b, ProfileReadOnly, "read-only")
}

func benchmarkMixedOpsWithProfile(b *testing.B, profile OperationProfile, profileName string) {
	b.StopTimer()
	sim := getSharedBenchCluster(b)
	nodes := sim.GetNodes()
	ctx := context.Background()
	sim.WaitForHealthyNodes(b, len(nodes), 10*time.Second)
	time.Sleep(50 * time.Millisecond)

	testNode := nodes[0]
	keyGen := atomic.Int64{}
	readKeysCount := 5000
	if testing.Short() {
		readKeysCount = 500
	}

	// Pre-populate read keys
	readKeys := make([]string, 0, readKeysCount)
	for i := 0; i < readKeysCount; i++ {
		key := fmt.Sprintf("read-key-%d", i)
		value := make([]byte, 256)
		cryptorand.Read(value)
		if err := testNode.Set(ctx, key, value); err == nil {
			readKeys = append(readKeys, key)
		}
	}
	if len(readKeys) == 0 {
		b.Fatalf("Failed to pre-populate any read keys")
	}
	time.Sleep(100 * time.Millisecond)

	writtenKeys := make(map[string]bool)
	writtenKeysMu := sync.RWMutex{}

	// Capture initial metrics
	metrics := captureMetrics()

	b.ResetTimer()
	b.StartTimer()

	var writeCount, readCount, deleteCount atomic.Int64

	// Limit benchmark duration
	benchmarkDuration := 3 * time.Second
	if testing.Short() {
		benchmarkDuration = 1 * time.Second
	}
	timeout := time.After(benchmarkDuration)
	b.RunParallel(func(pb *testing.PB) {
		rng := rand.New(rand.NewSource(time.Now().UnixNano()))
		for pb.Next() {
			select {
			case <-timeout:
				return
			default:
				opType := rng.Float32()
				if opType < float32(profile.WriteRatio) {
					// Write operation
					keyIdx := keyGen.Add(1)
					key := fmt.Sprintf("mixed-write-%d", keyIdx)
					value := make([]byte, 256)
					cryptorand.Read(value)
					if err := testNode.Set(ctx, key, value); err == nil {
						writtenKeysMu.Lock()
						writtenKeys[key] = true
						writtenKeysMu.Unlock()
					}
					writeCount.Add(1)
				} else if opType < float32(profile.WriteRatio+profile.ReadRatio) {
					// Read operation
					if len(readKeys) > 0 {
						key := readKeys[rng.Intn(len(readKeys))]
						_, _ = testNode.Get(ctx, key)
						readCount.Add(1)
					}
				} else {
					// Delete operation
					writtenKeysMu.Lock()
					keys := make([]string, 0, 50)
					for k := range writtenKeys {
						keys = append(keys, k)
						if len(keys) >= 50 {
							break
						}
					}
					if len(keys) > 0 {
						key := keys[rng.Intn(len(keys))]
						delete(writtenKeys, key)
						writtenKeysMu.Unlock()
						_ = testNode.Delete(ctx, key)
						deleteCount.Add(1)
					} else {
						writtenKeysMu.Unlock()
					}
				}
			}
		}
	})

	b.StopTimer()

	// Finalize and report metrics
	finalizeMetrics(&metrics, writeCount.Load(), readCount.Load(), deleteCount.Load())
	reportMetrics(b, &metrics, profile)

	b.Logf("Profile: %s (W:%.1f%% R:%.1f%% D:%.1f%%)",
		profileName,
		profile.WriteRatio*100,
		profile.ReadRatio*100,
		profile.DeleteRatio*100)
}

// BenchmarkClusterMixedQPS benchmarks mixed operations from multiple nodes
func BenchmarkClusterMixedQPS(b *testing.B) {
	benchmarkClusterMixedOps(b, ProfileBalanced, "balanced")
}

func BenchmarkClusterMixedQPS_ReadHeavy(b *testing.B) {
	benchmarkClusterMixedOps(b, ProfileReadHeavy, "read-heavy")
}

func BenchmarkClusterMixedQPS_WriteHeavy(b *testing.B) {
	benchmarkClusterMixedOps(b, ProfileWriteHeavy, "write-heavy")
}

func benchmarkClusterMixedOps(b *testing.B, profile OperationProfile, profileName string) {
	nodeCount := 100
	maxMemoryMB := int64(512)
	readKeysCount := 10000
	if testing.Short() {
		nodeCount = 10
		maxMemoryMB = 256
		readKeysCount = 1000
	}
	b.StopTimer()
	config := &TestEnvironmentConfig{
		NetworkProfile: ProfileLAN,
		NetworkType:    networkTypeFromEnv(gridkv.TCP),
		NodeCount:      nodeCount,
		ReplicaCount:   3,
		BasePort:       24300,
		MaxMemoryMB:    maxMemoryMB,
		ShardCount:     64,
	}
	sim := NewTestEnvironmentSimulator(config)
	if err := sim.SetupClusterOptimized(b); err != nil {
		b.Fatalf("SetupClusterOptimized() error = %v", err)
	}
	defer func() {
		go sim.Cleanup()
		time.Sleep(100 * time.Millisecond)
	}()

	nodes := sim.GetNodes()
	ctx := context.Background()
	sim.WaitForHealthyNodes(b, len(nodes), 10*time.Second)
	time.Sleep(50 * time.Millisecond)

	testNode := nodes[0]
	readKeys := make([]string, 0, readKeysCount)
	for i := 0; i < readKeysCount; i++ {
		key := fmt.Sprintf("read-key-%d", i)
		value := make([]byte, 1024)
		cryptorand.Read(value)
		if err := testNode.Set(ctx, key, value); err == nil {
			readKeys = append(readKeys, key)
		}
	}
	time.Sleep(100 * time.Millisecond)

	clientNodeCount := len(nodes) / 10
	if clientNodeCount < 1 {
		clientNodeCount = 1
	}
	if clientNodeCount > 20 {
		clientNodeCount = 20
	}

	// Capture initial metrics
	metrics := captureMetrics()

	b.ResetTimer()
	b.StartTimer()

	var writeOps, readOps, deleteOps atomic.Int64
	var wg sync.WaitGroup
	keyGen := atomic.Int64{}
	writtenKeys := make(map[int]map[string]bool)
	writtenKeysMu := sync.RWMutex{}
	for c := 0; c < clientNodeCount; c++ {
		writtenKeys[c] = make(map[string]bool)
		wg.Add(1)
		go func(clientID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(clientID)))
			opsPerClient := b.N / clientNodeCount
			for i := 0; i < opsPerClient; i++ {
				opType := rng.Float32()
				node := nodes[clientID%len(nodes)]
				if opType < float32(profile.WriteRatio) {
					// Write operation
					keyIdx := keyGen.Add(1)
					key := fmt.Sprintf("cluster-mixed-write-%d-%d", clientID, keyIdx)
					value := make([]byte, 1024)
					cryptorand.Read(value)
					if err := node.Set(ctx, key, value); err == nil {
						writtenKeysMu.Lock()
						writtenKeys[clientID][key] = true
						writtenKeysMu.Unlock()
						writeOps.Add(1)
					}
				} else if opType < float32(profile.WriteRatio+profile.ReadRatio) {
					// Read operation
					if len(readKeys) > 0 {
						key := readKeys[rng.Intn(len(readKeys))]
						_, _ = node.Get(ctx, key)
						readOps.Add(1)
					}
				} else {
					// Delete operation
					writtenKeysMu.Lock()
					keys := make([]string, 0, 50)
					for k := range writtenKeys[clientID] {
						keys = append(keys, k)
						if len(keys) >= 50 {
							break
						}
					}
					if len(keys) > 0 {
						key := keys[rng.Intn(len(keys))]
						delete(writtenKeys[clientID], key)
						writtenKeysMu.Unlock()
						_ = node.Delete(ctx, key)
						deleteOps.Add(1)
					} else {
						writtenKeysMu.Unlock()
					}
				}
			}
		}(c)
	}
	wg.Wait()

	b.StopTimer()

	// Finalize and report metrics
	finalizeMetrics(&metrics, writeOps.Load(), readOps.Load(), deleteOps.Load())
	reportMetrics(b, &metrics, profile)

	b.Logf("Cluster Profile: %s (W:%.1f%% R:%.1f%% D:%.1f%%), %d client nodes",
		profileName,
		profile.WriteRatio*100,
		profile.ReadRatio*100,
		profile.DeleteRatio*100,
		clientNodeCount)
}

// BenchmarkConcurrentStress benchmarks concurrent stress test with high concurrency
// Tests: system behavior under extreme concurrent load
func BenchmarkConcurrentStress(b *testing.B) {
	benchmarkConcurrentStressWithProfile(b, ProfileBalanced, "balanced")
}

func BenchmarkConcurrentStress_ReadHeavy(b *testing.B) {
	benchmarkConcurrentStressWithProfile(b, ProfileReadHeavy, "read-heavy")
}

func BenchmarkConcurrentStress_WriteHeavy(b *testing.B) {
	benchmarkConcurrentStressWithProfile(b, ProfileWriteHeavy, "write-heavy")
}

func benchmarkConcurrentStressWithProfile(b *testing.B, profile OperationProfile, profileName string) {
	nodeCount := 100
	maxMemoryMB := int64(512)
	concurrentWorkers := 600 // Increased from 500 for more stress
	if testing.Short() {
		nodeCount = 10
		maxMemoryMB = 256
		concurrentWorkers = 50
	}
	config := &TestEnvironmentConfig{
		NetworkProfile: ProfileLAN,
		NetworkType:    networkTypeFromEnv(gridkv.TCP),
		NodeCount:      nodeCount,
		ReplicaCount:   3,
		BasePort:       28000,
		MaxMemoryMB:    maxMemoryMB,
		ShardCount:     64,
	}
	b.StopTimer()
	sim := NewTestEnvironmentSimulator(config)
	if err := sim.SetupClusterOptimized(b); err != nil {
		b.Fatalf("SetupClusterOptimized() error = %v", err)
	}
	defer func() {
		go sim.Cleanup()
		time.Sleep(100 * time.Millisecond)
	}()

	nodes := sim.GetNodes()
	ctx := context.Background()
	sim.WaitForHealthyNodes(b, len(nodes), 10*time.Second)
	time.Sleep(100 * time.Millisecond)

	// Pre-populate keys for reads
	testNode := nodes[0]
	prePopulateKeys := 10000
	if testing.Short() {
		prePopulateKeys = 1000
	}
	readKeys := make([]string, 0, prePopulateKeys)
	for i := 0; i < prePopulateKeys; i++ {
		key := fmt.Sprintf("stress-read-%d", i)
		value := make([]byte, 1024)
		cryptorand.Read(value)
		if err := testNode.Set(ctx, key, value); err == nil {
			readKeys = append(readKeys, key)
		}
	}
	time.Sleep(200 * time.Millisecond)

	// Capture initial metrics
	metrics := captureMetrics()

	b.ResetTimer()
	b.StartTimer()

	var writeOps, readOps, deleteOps atomic.Int64
	var wg sync.WaitGroup
	keyGen := atomic.Int64{}
	writtenKeys := make(map[string]bool)
	writtenKeysMu := sync.RWMutex{}

	// Launch concurrent workers
	for w := 0; w < concurrentWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)))
			node := nodes[workerID%len(nodes)]
			opsPerWorker := b.N / concurrentWorkers
			if opsPerWorker == 0 {
				opsPerWorker = 1
			}

			for i := 0; i < opsPerWorker; i++ {
				opType := rng.Float32()
				if opType < float32(profile.WriteRatio) {
					// Write operation
					keyIdx := keyGen.Add(1)
					key := fmt.Sprintf("stress-write-%d-%d", workerID, keyIdx)
					value := make([]byte, 1024)
					cryptorand.Read(value)
					if err := node.Set(ctx, key, value); err == nil {
						writtenKeysMu.Lock()
						writtenKeys[key] = true
						writtenKeysMu.Unlock()
						writeOps.Add(1)
					}
				} else if opType < float32(profile.WriteRatio+profile.ReadRatio) {
					// Read operation
					if len(readKeys) > 0 {
						key := readKeys[rng.Intn(len(readKeys))]
						_, _ = node.Get(ctx, key)
						readOps.Add(1)
					}
				} else {
					// Delete operation
					writtenKeysMu.Lock()
					keys := make([]string, 0, 50)
					for k := range writtenKeys {
						keys = append(keys, k)
						if len(keys) >= 50 {
							break
						}
					}
					if len(keys) > 0 {
						key := keys[rng.Intn(len(keys))]
						delete(writtenKeys, key)
						writtenKeysMu.Unlock()
						_ = node.Delete(ctx, key)
						deleteOps.Add(1)
					} else {
						writtenKeysMu.Unlock()
					}
				}
			}
		}(w)
	}
	wg.Wait()

	b.StopTimer()

	// Finalize and report metrics
	finalizeMetrics(&metrics, writeOps.Load(), readOps.Load(), deleteOps.Load())
	reportMetrics(b, &metrics, profile)

	b.Logf("Stress Test Profile: %s (W:%.1f%% R:%.1f%% D:%.1f%%), %d concurrent workers",
		profileName,
		profile.WriteRatio*100,
		profile.ReadRatio*100,
		profile.DeleteRatio*100,
		concurrentWorkers)
}

// BenchmarkMetricsFrameworkDemo demonstrates the performance monitoring framework
// Run with: go test -bench=BenchmarkMetricsFrameworkDemo -benchtime=1s ./tests/ -v
func BenchmarkMetricsFrameworkDemo(b *testing.B) {
	// Demonstrates the comprehensive performance monitoring capabilities
	// without requiring cluster setup

	b.StopTimer()

	// Capture initial system state
	metrics := captureMetrics()
	profile := ProfileBalanced // 40% write, 40% read, 20% delete

	var writes, reads, deletes int64

	b.ResetTimer()
	b.StartTimer()

	// Simulate parallel operations with realistic workload distribution
	b.RunParallel(func(pb *testing.PB) {
		localWrites, localReads, localDeletes := int64(0), int64(0), int64(0)

		for pb.Next() {
			// Distribute operations according to profile ratios
			totalOps := localWrites + localReads + localDeletes
			if totalOps%5 < 2 { // ~40% writes
				localWrites++
				// Simulate write operation (memory allocation, processing)
				_ = make([]byte, 1024)
				_ = make([]byte, 512) // Additional processing
			} else if totalOps%5 < 4 { // ~40% reads
				localReads++
				// Simulate read operation (lighter work)
				_ = make([]byte, 512)
			} else { // ~20% deletes
				localDeletes++
				// Simulate delete operation (cleanup work)
				_ = make([]byte, 256)
			}
		}

		atomic.AddInt64(&writes, localWrites)
		atomic.AddInt64(&reads, localReads)
		atomic.AddInt64(&deletes, localDeletes)
	})

	b.StopTimer()

	// Finalize and report comprehensive metrics
	finalizeMetrics(&metrics, writes, reads, deletes)
	reportMetrics(b, &metrics, profile)

	b.Logf("🎯 Performance Monitoring Framework Demo Complete")
	b.Logf("Metrics include: QPS, Memory, Goroutines, GC CPU, Operation Ratios")
	b.Logf("Configurable profiles: Balanced, ReadHeavy, WriteHeavy, ReadOnly")
	b.Logf("Keys include timestamp and random strings for uniqueness")
}

// BenchmarkHashConflictAnalysis analyzes hash conflicts between all components using xxhash
func BenchmarkHashConflictAnalysis(b *testing.B) {
	b.StopTimer()

	b.Logf("=== Comprehensive Hash Function Analysis ===")

	// Test keys that might cause conflicts
	testKeys := []string{
		"test-key-1",
		"test-key-2",
		"same-hash-different-keys-a",
		"same-hash-different-keys-b",
		"collision-test-123",
		"collision-test-456",
		"perf-test-key",
		"cache-test-key",
		"storage-test-key",
	}

	b.Logf("Component Hash Function Usage Analysis:")
	b.Logf("")

	// Analyze each component's hash usage
	for _, key := range testKeys {
		fullHash := xxh3.HashString128(key)
		memStorageHash := fullHash.Lo                 // memstorage uses .Lo
		hashRingHash := fullHash.Hi                   // hashring uses .Hi
		cacheHash := (fullHash.Lo << 1) ^ fullHash.Hi // ttl_cache uses bit rotation
		entropyHash := fullHash.Hi                    // entropy uses .Hi (same as hashring)

		b.Logf("Key: %s", key)
		b.Logf("  Full xxh3.HashString128: Lo=%x, Hi=%x", fullHash.Lo, fullHash.Hi)
		b.Logf("  MemStorage (.Lo): %x", memStorageHash)
		b.Logf("  HashRing (.Hi): %x", hashRingHash)
		b.Logf("  Cache (bit-rotated): %x", cacheHash)
		b.Logf("  Entropy (.Hi): %x", entropyHash)
		b.Logf("  Conflicts detected:")
		b.Logf("    HashRing vs Entropy: %t (should be same)", hashRingHash == entropyHash)
		b.Logf("    MemStorage == HashRing: %t (potential issue)", memStorageHash == uint64(hashRingHash))
		b.Logf("")
	}

	// Test shard distribution for memstorage
	b.Logf("MemStorage Shard Distribution Test:")
	memShardMask := uint32(15) // Assuming 16 shards (2^4 - 1)
	for _, key := range testKeys {
		hash := xxh3.HashString128(key).Lo
		shard := uint32(hash) & memShardMask
		b.Logf("  Key: %s -> Shard: %d", key, shard)
	}
	b.Logf("")

	// Test cache shard distribution
	b.Logf("Cache Shard Distribution Test:")
	cacheShardMask := uint32(63) // Assuming 64 shards (2^6 - 1)
	for _, key := range testKeys {
		hash := xxh3.HashString(key)
		shard := uint32(hash) & cacheShardMask
		b.Logf("  Key: %s -> Shard: %d", key, shard)
	}
	b.Logf("")

	// Test hash ring distribution
	b.Logf("HashRing Distribution Test:")
	ringSize := uint64(1024) // Assuming 1024 virtual nodes
	for _, key := range testKeys {
		hash := xxh3.HashString128(key).Hi
		position := hash % ringSize
		b.Logf("  Key: %s -> Ring Position: %d", key, position)
	}
	b.Logf("")

	// Test with a reasonable sample to find actual conflicts
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	conflictKeys := generateTestKeys("conflict", 1000, rng)

	memRingConflicts := 0
	cacheRingConflicts := 0
	totalTests := len(conflictKeys)

	for _, key := range conflictKeys {
		memHash := xxh3.HashString128(key).Lo
		ringHash := xxh3.HashString128(key).Hi
		cacheHash := xxh3.HashString(key)

		// Check for problematic conflicts
		if memHash == uint64(ringHash) {
			memRingConflicts++
		}
		if uint32(cacheHash) == uint32(ringHash) {
			cacheRingConflicts++
		}
	}

	b.Logf("=== Conflict Analysis Results ===")
	b.Logf("Total keys tested: %d", totalTests)
	b.Logf("MemStorage-Ring conflicts: %d (%.4f%%)", memRingConflicts, float64(memRingConflicts)/float64(totalTests)*100)
	b.Logf("Cache-Ring conflicts: %d (%.4f%%)", cacheRingConflicts, float64(cacheRingConflicts)/float64(totalTests)*100)
	b.Logf("")

	b.Logf("=== Performance Impact Analysis ===")
	if memRingConflicts > 0 {
		b.Logf("WARNING: MemStorage and HashRing use different hash parts!")
		b.Logf("Impact: Keys may be stored in different shards than expected by routing")
		b.Logf("Result: Increased cross-node traffic, higher latency, cache misses")
	} else {
		b.Logf("OK: MemStorage and HashRing hash functions are independent")
	}

	if cacheRingConflicts > 0 {
		b.Logf("WARNING: Cache and HashRing may have hash collisions!")
		b.Logf("Impact: Cached data may not match actual data location")
		b.Logf("Result: Cache invalidation, performance degradation")
	} else {
		b.Logf("OK: Cache and HashRing hash functions are independent")
	}

	b.Logf("")
	b.Logf("=== Design Philosophy Analysis ===")
	b.Logf("Different hash functions across components may be INTENTIONAL:")
	b.Logf("  • Load balancing: Prevents single key from overwhelming one shard")
	b.Logf("  • Fault isolation: Hot key in one component doesn't affect others")
	b.Logf("  • Performance optimization: Each component optimizes its distribution")
	b.Logf("")
	b.Logf("=== Alternative Recommendation ===")
	b.Logf("Consider the trade-offs:")
	b.Logf("  Option 1: Keep different hashes for load balancing (current)")
	b.Logf("  Option 2: Standardize hashes for consistency (our change)")
	b.Logf("  Option 3: Hybrid approach with configurable hash functions")
	b.Logf("")
	b.Logf("The choice depends on workload characteristics:")
	b.Logf("  - Hot key heavy: Different hashes better")
	b.Logf("  - Cache heavy: Same hashes better")
	b.Logf("  - Mixed workload: Need empirical testing")
}

// BenchmarkHashDesignComparison compares performance of same vs different hash functions
func BenchmarkHashDesignComparison(b *testing.B) {
	b.StopTimer()

	// Test different hash distribution strategies
	strategies := []struct {
		name        string
		description string
		memHash     func(string) uint64
		ringHash    func(string) uint64
		cacheHash   func(string) uint64
	}{
		{
			name:        "Current_Design",
			description: "MemStorage(.Lo) vs HashRing(.Hi) vs Cache(bit-rotated)",
			memHash:     func(k string) uint64 { return xxh3.HashString128(k).Lo },
			ringHash:    func(k string) uint64 { return xxh3.HashString128(k).Hi },
			cacheHash: func(k string) uint64 {
				fullHash := xxh3.HashString128(k)
				return (fullHash.Lo << 1) ^ fullHash.Hi // Bit rotation combination
			},
		},
		{
			name:        "Same_Hash_All",
			description: "All components use .Hi (consistent)",
			memHash:     func(k string) uint64 { return xxh3.HashString128(k).Hi },
			ringHash:    func(k string) uint64 { return xxh3.HashString128(k).Hi },
			cacheHash:   func(k string) uint64 { return xxh3.HashString128(k).Hi },
		},
		{
			name:        "Load_Balanced",
			description: "Layered: .Lo, .Hi, bit-rotated (all from same hash function)",
			memHash:     func(k string) uint64 { return xxh3.HashString128(k).Lo },
			ringHash:    func(k string) uint64 { return xxh3.HashString128(k).Hi },
			cacheHash: func(k string) uint64 {
				fullHash := xxh3.HashString128(k)
				return (fullHash.Lo << 1) ^ fullHash.Hi // Bit rotation combination
			},
		},
	}

	// Generate test keys - mix of hot keys and random keys
	hotKeys := []string{"hot-key-1", "hot-key-2", "hot-key-3"}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	randomKeys := generateTestKeys("random", 1000, rng)

	testKeys := append(hotKeys, randomKeys...)

	for _, strategy := range strategies {
		b.Run(strategy.name, func(b *testing.B) {
			// Simulate sharding
			shardMask := uint64(15)      // 16 shards
			cacheShardMask := uint64(63) // 64 cache shards

			memShards := make(map[uint64]int)
			ringNodes := make(map[uint64]int)
			cacheShards := make(map[uint64]int)

			// Count distribution
			for _, key := range testKeys {
				memShard := strategy.memHash(key) & shardMask
				ringNode := strategy.ringHash(key) % 5 // 5 nodes
				cacheShard := strategy.cacheHash(key) & cacheShardMask

				memShards[memShard]++
				ringNodes[ringNode]++
				cacheShards[cacheShard]++
			}

			// Calculate distribution metrics
			memVariance := calculateVariance(memShards)
			ringVariance := calculateVariance(ringNodes)
			cacheVariance := calculateVariance(cacheShards)

			// Calculate hot key isolation
			hotKeyIsolation := 0
			for _, hotKey := range hotKeys {
				memShard := strategy.memHash(hotKey) & shardMask
				ringNode := strategy.ringHash(hotKey) % 5
				cacheShard := strategy.cacheHash(hotKey) & cacheShardMask

				if memShard != ringNode && ringNode != uint64(cacheShard) {
					hotKeyIsolation++
				}
			}

			b.Logf("Strategy: %s", strategy.description)
			b.Logf("  MemStorage variance: %.2f", memVariance)
			b.Logf("  HashRing variance: %.2f", ringVariance)
			b.Logf("  Cache variance: %.2f", cacheVariance)
			b.Logf("  Hot key isolation: %d/3 keys isolated", hotKeyIsolation)
			b.Logf("  Load balancing score: %.2f (lower is better)", (memVariance+ringVariance+cacheVariance)/3)

			b.ReportMetric(memVariance, "mem-variance")
			b.ReportMetric(ringVariance, "ring-variance")
			b.ReportMetric(cacheVariance, "cache-variance")
			b.ReportMetric(float64(hotKeyIsolation), "hot-key-isolation")
			b.ReportMetric((memVariance+ringVariance+cacheVariance)/3, "avg-load-balance")
		})
	}
}

// calculateVariance calculates coefficient of variation for load distribution
func calculateVariance(distribution map[uint64]int) float64 {
	if len(distribution) == 0 {
		return 0
	}

	total := 0
	for _, count := range distribution {
		total += count
	}

	mean := float64(total) / float64(len(distribution))

	variance := 0.0
	for _, count := range distribution {
		diff := float64(count) - mean
		variance += diff * diff
	}
	variance /= float64(len(distribution))

	// Coefficient of variation (normalize by mean)
	if mean == 0 {
		return 0
	}
	return variance / (mean * mean)
}

// === HASH FUNCTION DESIGN SUMMARY ===
//
// GridKV implements a layered hash design for optimal load balancing:
//
// 1. MemStorage (.Lo): Primary data sharding
//    - Uses low 32 bits of xxh3.HashString128
//    - Ensures consistent data placement
//
// 2. HashRing (.Hi): Node routing decisions
//    - Uses high 32 bits of xxh3.HashString128
//    - Independent from storage for load balancing
//
// 3. Cache (bit-rotated): Cache entry distribution
//    - Uses (.Lo << 1) ^ .Hi combination
//    - Provides additional load balancing layer
//
// 4. Entropy (.Hi): Anti-entropy consistency
//    - Uses same as HashRing for consistency
//
// Benefits:
// - Hot key isolation across components
// - Fault containment (no cascading failures)
// - Consistent hash quality (all from xxh3)
// - Low performance overhead
//
// This design provides load balancing while avoiding
// the conflicts of completely different hash functions.

// BenchmarkSummary provides guidance for running benchmarks
func BenchmarkSummary(b *testing.B) {
	b.Skip("Summary benchmark - provides guidance for running tests")

	b.Log("GridKV Benchmark Suite:")
	b.Log("")
	b.Log("Quick Benchmarks (-short flag):")
	b.Log("  - BenchmarkMixedOpsQPS* : Mixed read/write/delete operations")
	b.Log("  - BenchmarkConcurrentStress* : High concurrency stress testing")
	b.Log("")
	b.Log("Full Benchmarks (no -short flag):")
	b.Log("  - BenchmarkCluster* : Distributed cluster performance")
	b.Log("  - BenchmarkMetricsFrameworkDemo : Framework capabilities")
	b.Log("  - BenchmarkHashDesignComparison : Hash function analysis")
	b.Log("")
	b.Log("Tips:")
	b.Log("  - Use -benchtime=2s for faster iteration")
	b.Log("  - Use -count=3 for result stability")
	b.Log("  - Check -benchmem for memory allocation metrics")
	b.Log("")
	b.Log("Performance targets (approximate):")
	b.Log("  - Single node: 10k-50k ops/sec")
	b.Log("  - 10-node cluster: 5k-20k ops/sec")
	b.Log("  - Memory: <50MB increase per 100k operations")
}
