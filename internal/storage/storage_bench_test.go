package storage

import (
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
)

// Benchmark Memory vs MemorySharded with different scenarios

// BenchmarkMemory_SmallValues benchmarks Memory with small values
func BenchmarkMemory_SmallValues(b *testing.B) {
	storage, _ := NewMemoryStorage(1024) // 1GB limit
	defer storage.Close()

	value := make([]byte, 100) // 100 bytes (small, below compression threshold)
	item := &StoredItem{
		Version: 1,
		Value:   value,
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key-%d", i%10000)
			storage.Set(key, item)
			storage.Get(key)
			i++
		}
	})

	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "ops/sec")
}

// BenchmarkMemory_LargeValues benchmarks Memory with large values (benefits from compression)
func BenchmarkMemory_LargeValues(b *testing.B) {
	storage, _ := NewMemoryStorage(2048) // 2GB limit
	defer storage.Close()

	value := make([]byte, 4096) // 4KB (large, will be compressed)
	// Fill with compressible data
	for i := range value {
		value[i] = byte(i % 10) // Highly compressible pattern
	}
	item := &StoredItem{
		Version: 1,
		Value:   value,
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key-%d", i%1000)
			storage.Set(key, item)
			storage.Get(key)
			i++
		}
	})

	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "ops/sec")

	stats := storage.Stats()
	compressed := storage.compressedBytes.Load()
	original := storage.originalBytes.Load()
	if original > 0 {
		compressionRatio := float64(compressed) / float64(original)
		b.ReportMetric(compressionRatio*100, "compression%")
		b.Logf("Compression: %d -> %d bytes (%.1f%%)", original, compressed, compressionRatio*100)
	}
	b.Logf("Keys: %d, Memory: %d MB", stats.KeyCount, stats.DBSize/1024/1024)
}

// BenchmarkMemorySharded_SmallValues benchmarks MemorySharded with small values
func BenchmarkMemorySharded_SmallValues(b *testing.B) {
	storage, _ := NewShardedMemoryStorageV2(V2Config{
		MaxMemoryMB: 1024,
		ShardCount:  256,
	})
	defer storage.Close()

	value := make([]byte, 100) // 100 bytes
	item := &StoredItem{
		Version: 1,
		Value:   value,
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key-%d", i%10000)
			storage.Set(key, item)
			storage.Get(key)
			i++
		}
	})

	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "ops/sec")
	b.Logf("Shards: %d (CPU: %d)", storage.shardCount, runtime.NumCPU())
}

// BenchmarkMemory_WriteOnly benchmarks Memory write-only workload
func BenchmarkMemory_WriteOnly(b *testing.B) {
	storage, _ := NewMemoryStorage(1024)
	defer storage.Close()

	value := make([]byte, 500) // 500 bytes (will compress)
	for i := range value {
		value[i] = byte(i % 128)
	}
	item := &StoredItem{
		Version: 1,
		Value:   value,
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key-%d", i)
			storage.Set(key, item)
			i++
		}
	})

	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "ops/sec")
}

// BenchmarkMemorySharded_WriteOnly benchmarks MemorySharded write-only workload
func BenchmarkMemorySharded_WriteOnly(b *testing.B) {
	storage, _ := NewShardedMemoryStorageV2(V2Config{
		MaxMemoryMB: 1024,
		ShardCount:  256,
	})
	defer storage.Close()

	value := make([]byte, 500)
	item := &StoredItem{
		Version: 1,
		Value:   value,
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key-%d", i)
			storage.Set(key, item)
			i++
		}
	})

	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "ops/sec")
}

// BenchmarkMemory_ReadHeavy benchmarks Memory read-heavy workload (90% reads)
func BenchmarkMemory_ReadHeavy(b *testing.B) {
	storage, _ := NewMemoryStorage(1024)
	defer storage.Close()

	// Pre-populate
	value := make([]byte, 100)
	item := &StoredItem{
		Version: 1,
		Value:   value,
	}
	for i := 0; i < 1000; i++ {
		storage.Set(fmt.Sprintf("key-%d", i), item)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key-%d", i%1000)
			if i%10 == 0 {
				storage.Set(key, item) // 10% writes
			} else {
				storage.Get(key) // 90% reads
			}
			i++
		}
	})

	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "ops/sec")
	stats := storage.Stats()
	b.Logf("Hit rate: %.2f%%", stats.CacheHitRate*100)
}

// BenchmarkMemorySharded_ReadHeavy benchmarks MemorySharded read-heavy workload
func BenchmarkMemorySharded_ReadHeavy(b *testing.B) {
	storage, _ := NewShardedMemoryStorageV2(V2Config{
		MaxMemoryMB: 1024,
		ShardCount:  256,
	})
	defer storage.Close()

	// Pre-populate
	value := make([]byte, 100)
	item := &StoredItem{
		Version: 1,
		Value:   value,
	}
	for i := 0; i < 1000; i++ {
		storage.Set(fmt.Sprintf("key-%d", i), item)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key-%d", i%1000)
			if i%10 == 0 {
				storage.Set(key, item) // 10% writes
			} else {
				storage.Get(key) // 90% reads
			}
			i++
		}
	})

	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "ops/sec")
	stats := storage.Stats()
	b.Logf("Hit rate: %.2f%%, Shards: %d", stats.CacheHitRate*100, storage.shardCount)
}

// BenchmarkMemory_MemoryEfficiency tests memory efficiency with LRU eviction
func BenchmarkMemory_MemoryEfficiency(b *testing.B) {
	storage, _ := NewMemoryStorage(100) // Only 100MB limit
	defer storage.Close()

	value := make([]byte, 1024) // 1KB values
	for i := range value {
		value[i] = byte(i % 256)
	}
	item := &StoredItem{
		Version: 1,
		Value:   value,
	}

	b.ResetTimer()
	successCount := atomic.Int64{}
	evictionCount := atomic.Int64{}

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key-%d", i)
			err := storage.Set(key, item)
			if err != nil {
				evictionCount.Add(1)
			} else {
				successCount.Add(1)
			}
			i++
		}
	})

	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "ops/sec")
	stats := storage.Stats()
	b.Logf("Keys: %d, Memory: %d MB, Evictions: %d, Success: %d",
		stats.KeyCount, stats.DBSize/1024/1024, evictionCount.Load(), successCount.Load())
	b.Logf("Eviction rate: %.2f%%", float64(evictionCount.Load())/float64(b.N)*100)
}

// BenchmarkComparisonMatrix runs a comprehensive comparison
func BenchmarkComparisonMatrix(b *testing.B) {
	scenarios := []struct {
		name      string
		valueSize int
		keyCount  int
	}{
		{"Small_100B_10K", 100, 10000},
		{"Medium_1KB_1K", 1024, 1000},
		{"Large_4KB_500", 4096, 500},
	}

	for _, scenario := range scenarios {
		value := make([]byte, scenario.valueSize)
		for i := range value {
			value[i] = byte(i % 128)
		}
		item := &StoredItem{
			Version: 1,
			Value:   value,
		}

		// Memory (with compression)
		b.Run(fmt.Sprintf("Memory_%s", scenario.name), func(b *testing.B) {
			storage, _ := NewMemoryStorage(2048)
			defer storage.Close()

			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					key := fmt.Sprintf("key-%d", i%scenario.keyCount)
					storage.Set(key, item)
					storage.Get(key)
					i++
				}
			})

			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "ops/sec")
			compressed := storage.compressedBytes.Load()
			original := storage.originalBytes.Load()
			if original > 0 {
				compressionRatio := float64(compressed) / float64(original)
				b.ReportMetric(compressionRatio*100, "compression%")
			}
		})

		// MemorySharded (high throughput)
		b.Run(fmt.Sprintf("MemorySharded_%s", scenario.name), func(b *testing.B) {
			storage, _ := NewShardedMemoryStorageV2(V2Config{
				MaxMemoryMB: 2048,
				ShardCount:  256,
			})
			defer storage.Close()

			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					key := fmt.Sprintf("key-%d", i%scenario.keyCount)
					storage.Set(key, item)
					storage.Get(key)
					i++
				}
			})

			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "ops/sec")
		})
	}
}
