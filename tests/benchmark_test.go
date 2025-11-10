package tests

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	gridkv "github.com/feellmoose/gridkv"
)

// ============================================================================
// Helper Functions
// ============================================================================

func setupSingleNode(b *testing.B, backend gridkv.StorageBackendType, basePort int) (*gridkv.GridKV, error) {
	// Use random port offset to avoid conflicts
	port := basePort + rand.Intn(1000)
	opts := &gridkv.GridKVOptions{
		LocalNodeID:  fmt.Sprintf("bench-node-%d", port),
		LocalAddress: fmt.Sprintf("localhost:%d", port),
		Network: &gridkv.NetworkOptions{
			Type:         gridkv.TCP,
			BindAddr:     fmt.Sprintf("localhost:%d", port),
			MaxConns:     2000,
			MaxIdle:      200,
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 5 * time.Second,
		},
		Storage: &gridkv.StorageOptions{
			Backend:     backend,
			MaxMemoryMB: 4096, // 4GB limit
		},
		ReplicaCount:       1,
		WriteQuorum:        1,
		ReadQuorum:         1,
		VirtualNodes:       150,
		MaxReplicators:     8,
		ReplicationTimeout: 2 * time.Second,
		ReadTimeout:        2 * time.Second,
	}

	return gridkv.NewGridKV(opts)
}

// ============================================================================
// Core Single-Node Benchmarks
// ============================================================================

// BenchmarkSet tests Set operation with different value sizes
func BenchmarkSet(b *testing.B) {
	sizes := []struct {
		name string
		size int
	}{
		{"100B", 100},
		{"1KB", 1024},
		{"10KB", 10240},
	}

	for _, size := range sizes {
		b.Run(size.name, func(b *testing.B) {
			kv, err := setupSingleNode(b, gridkv.BackendMemorySharded, 19000+size.size)
			if err != nil {
				b.Fatal(err)
			}
			defer kv.Close()

			ctx := context.Background()
			value := randomValue(size.size)

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				key := fmt.Sprintf("key-%d", i)
				if err := kv.Set(ctx, key, value); err != nil {
					b.Fatal(err)
				}
			}

			b.StopTimer()
			ops := float64(b.N) / b.Elapsed().Seconds()
			b.ReportMetric(ops, "ops/sec")
		})
	}
}

// BenchmarkGet tests Get operation with different value sizes
func BenchmarkGet(b *testing.B) {
	sizes := []struct {
		name string
		size int
	}{
		{"100B", 100},
		{"1KB", 1024},
	}

	for _, size := range sizes {
		b.Run(size.name, func(b *testing.B) {
			kv, err := setupSingleNode(b, gridkv.BackendMemorySharded, 19100+size.size)
			if err != nil {
				b.Fatal(err)
			}
			defer kv.Close()

			ctx := context.Background()
			value := randomValue(size.size)

			// Pre-populate data
			numKeys := 10000
			for i := 0; i < numKeys; i++ {
				key := fmt.Sprintf("key-%d", i)
				if err := kv.Set(ctx, key, value); err != nil {
					b.Fatal(err)
				}
			}

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				key := fmt.Sprintf("key-%d", i%numKeys)
				if _, err := kv.Get(ctx, key); err != nil {
					b.Fatal(err)
				}
			}

			b.StopTimer()
			ops := float64(b.N) / b.Elapsed().Seconds()
			b.ReportMetric(ops, "ops/sec")
		})
	}
}

// ============================================================================
// Concurrent Benchmarks
// ============================================================================

// BenchmarkConcurrentSet tests concurrent Set operations
func BenchmarkConcurrentSet(b *testing.B) {
	goroutineCounts := []int{10, 100}

	for _, numGoroutines := range goroutineCounts {
		b.Run(fmt.Sprintf("%dGoroutines", numGoroutines), func(b *testing.B) {
			kv, err := setupSingleNode(b, gridkv.BackendMemorySharded, 19300+numGoroutines)
			if err != nil {
				b.Fatal(err)
			}
			defer kv.Close()

			ctx := context.Background()
			value := randomValue(100)

			b.ResetTimer()
			b.ReportAllocs()

			var wg sync.WaitGroup
			opsPerGoroutine := b.N / numGoroutines

			for g := 0; g < numGoroutines; g++ {
				wg.Add(1)
				go func(gid int) {
					defer wg.Done()
					for i := 0; i < opsPerGoroutine; i++ {
						key := fmt.Sprintf("key-%d-%d", gid, i)
						if err := kv.Set(ctx, key, value); err != nil {
							b.Error(err)
							return
						}
					}
				}(g)
			}

			wg.Wait()
			b.StopTimer()

			ops := float64(b.N) / b.Elapsed().Seconds()
			b.ReportMetric(ops, "ops/sec")
		})
	}
}

// BenchmarkConcurrentGet tests concurrent Get operations
func BenchmarkConcurrentGet(b *testing.B) {
	goroutineCounts := []int{10, 100}

	for _, numGoroutines := range goroutineCounts {
		b.Run(fmt.Sprintf("%dGoroutines", numGoroutines), func(b *testing.B) {
			kv, err := setupSingleNode(b, gridkv.BackendMemorySharded, 19400+numGoroutines)
			if err != nil {
				b.Fatal(err)
			}
			defer kv.Close()

			ctx := context.Background()
			value := randomValue(100)

			// Pre-populate data
			numKeys := 10000
			for i := 0; i < numKeys; i++ {
				key := fmt.Sprintf("key-%d", i)
				if err := kv.Set(ctx, key, value); err != nil {
					b.Fatal(err)
				}
			}

			b.ResetTimer()
			b.ReportAllocs()

			var wg sync.WaitGroup
			opsPerGoroutine := b.N / numGoroutines

			for g := 0; g < numGoroutines; g++ {
				wg.Add(1)
				go func(gid int) {
					defer wg.Done()
					for i := 0; i < opsPerGoroutine; i++ {
						key := fmt.Sprintf("key-%d", i%numKeys)
						if _, err := kv.Get(ctx, key); err != nil {
							b.Error(err)
							return
						}
					}
				}(g)
			}

			wg.Wait()
			b.StopTimer()

			ops := float64(b.N) / b.Elapsed().Seconds()
			b.ReportMetric(ops, "ops/sec")
		})
	}
}

// ============================================================================
// Mixed Workload Benchmarks
// ============================================================================

// BenchmarkMixedWorkload tests realistic mixed read/write workloads
func BenchmarkMixedWorkload(b *testing.B) {
	workloads := []struct {
		name        string
		readPercent float32
	}{
		{"ReadHeavy_90_10", 0.9},
		{"Balanced_50_50", 0.5},
		{"WriteHeavy_30_70", 0.3},
	}

	for _, workload := range workloads {
		b.Run(workload.name, func(b *testing.B) {
			kv, err := setupSingleNode(b, gridkv.BackendMemorySharded, 19500+int(workload.readPercent*100))
			if err != nil {
				b.Fatal(err)
			}
			defer kv.Close()

			ctx := context.Background()
			value := randomValue(100)
			numGoroutines := 50

			// Pre-populate data
			numKeys := 10000
			for i := 0; i < numKeys; i++ {
				key := fmt.Sprintf("key-%d", i)
				if err := kv.Set(ctx, key, value); err != nil {
					b.Fatal(err)
				}
			}

			b.ResetTimer()
			b.ReportAllocs()

			var wg sync.WaitGroup
			opsPerGoroutine := b.N / numGoroutines

			for g := 0; g < numGoroutines; g++ {
				wg.Add(1)
				go func(gid int) {
					defer wg.Done()
					for i := 0; i < opsPerGoroutine; i++ {
						key := fmt.Sprintf("key-%d", rand.Intn(numKeys))

						if rand.Float32() < workload.readPercent {
							if _, err := kv.Get(ctx, key); err != nil {
								b.Error(err)
								return
							}
						} else {
							if err := kv.Set(ctx, key, value); err != nil {
								b.Error(err)
								return
							}
						}
					}
				}(g)
			}

			wg.Wait()
			b.StopTimer()

			ops := float64(b.N) / b.Elapsed().Seconds()
			b.ReportMetric(ops, "ops/sec")
		})
	}
}

// ============================================================================
// Latency Benchmarks
// ============================================================================

// BenchmarkLatency measures operation latency distribution
func BenchmarkLatency(b *testing.B) {
	operations := []string{"Set", "Get"}

	for _, op := range operations {
		b.Run(op, func(b *testing.B) {
			kv, err := setupSingleNode(b, gridkv.BackendMemorySharded, 19600)
			if err != nil {
				b.Fatal(err)
			}
			defer kv.Close()

			ctx := context.Background()
			value := randomValue(100)

			if op == "Get" {
				// Pre-populate for Get benchmark
				numKeys := 10000
				for i := 0; i < numKeys; i++ {
					key := fmt.Sprintf("key-%d", i)
					if err := kv.Set(ctx, key, value); err != nil {
						b.Fatal(err)
					}
				}
			}

			latencies := make([]time.Duration, b.N)

			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				key := fmt.Sprintf("key-%d", i)
				start := time.Now()

				if op == "Set" {
					if err := kv.Set(ctx, key, value); err != nil {
						b.Fatal(err)
					}
				} else {
					getKey := fmt.Sprintf("key-%d", i%10000)
					if _, err := kv.Get(ctx, getKey); err != nil {
						b.Fatal(err)
					}
				}

				latencies[i] = time.Since(start)
			}

			b.StopTimer()

			// Calculate percentiles
			p50, p95, p99 := calculatePercentiles(latencies)
			b.ReportMetric(float64(p50.Microseconds()), "p50_us")
			b.ReportMetric(float64(p95.Microseconds()), "p95_us")
			b.ReportMetric(float64(p99.Microseconds()), "p99_us")
		})
	}
}

// ============================================================================
// Storage Backend Comparison
// ============================================================================

// BenchmarkStorageBackends compares different storage backends
func BenchmarkStorageBackends(b *testing.B) {
	backends := []struct {
		name    string
		backend gridkv.StorageBackendType
	}{
		{"Memory", gridkv.BackendMemory},
		{"MemorySharded", gridkv.BackendMemorySharded},
	}

	for _, be := range backends {
		b.Run(be.name+"_Set", func(b *testing.B) {
			kv, err := setupSingleNode(b, be.backend, 19700)
			if err != nil {
				b.Fatal(err)
			}
			defer kv.Close()

			ctx := context.Background()
			value := randomValue(100)

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				key := fmt.Sprintf("key-%d", i)
				if err := kv.Set(ctx, key, value); err != nil {
					b.Fatal(err)
				}
			}

			b.StopTimer()
			ops := float64(b.N) / b.Elapsed().Seconds()
			b.ReportMetric(ops, "ops/sec")
		})

		b.Run(be.name+"_Get", func(b *testing.B) {
			kv, err := setupSingleNode(b, be.backend, 19710)
			if err != nil {
				b.Fatal(err)
			}
			defer kv.Close()

			ctx := context.Background()
			value := randomValue(100)

			// Pre-populate data
			numKeys := 1000
			for i := 0; i < numKeys; i++ {
				key := fmt.Sprintf("key-%d", i)
				if err := kv.Set(ctx, key, value); err != nil {
					b.Fatal(err)
				}
			}

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				key := fmt.Sprintf("key-%d", i%numKeys)
				if _, err := kv.Get(ctx, key); err != nil {
					b.Fatal(err)
				}
			}

			b.StopTimer()
			ops := float64(b.N) / b.Elapsed().Seconds()
			b.ReportMetric(ops, "ops/sec")
		})

		b.Run(be.name+"_Mixed", func(b *testing.B) {
			kv, err := setupSingleNode(b, be.backend, 19720)
			if err != nil {
				b.Fatal(err)
			}
			defer kv.Close()

			ctx := context.Background()
			value := randomValue(100)

			// Pre-populate
			numKeys := 1000
			for i := 0; i < numKeys; i++ {
				key := fmt.Sprintf("key-%d", i)
				if err := kv.Set(ctx, key, value); err != nil {
					b.Fatal(err)
				}
			}

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				key := fmt.Sprintf("key-%d", i%numKeys)

				// 80% reads, 20% writes
				if i%5 == 0 {
					if err := kv.Set(ctx, key, value); err != nil {
						b.Fatal(err)
					}
				} else {
					if _, err := kv.Get(ctx, key); err != nil {
						b.Fatal(err)
					}
				}
			}

			b.StopTimer()
			ops := float64(b.N) / b.Elapsed().Seconds()
			b.ReportMetric(ops, "ops/sec")
		})
	}
}
