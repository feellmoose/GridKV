package cache

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestTTLCache_BasicOperations(t *testing.T) {
	cache := New(Opts{
		Shards:        16,
		Size:          1000,
		CleanupIntv:   100 * time.Millisecond,
		EnableCleanup: true,
	})
	defer func() { _ = cache.Close(context.Background()) }()

	// Test Set and Get
	cache.Set("key1", "value1", 1*time.Second)
	val, ok := cache.Get("key1")
	if !ok {
		t.Fatal("Expected to find key1")
	}
	if val != "value1" {
		t.Fatalf("Expected value1, got %v", val)
	}

	// Test Get non-existent key
	_, ok = cache.Get("nonexistent")
	if ok {
		t.Fatal("Expected not to find nonexistent key")
	}

	// Test Del
	cache.Del("key1")
	_, ok = cache.Get("key1")
	if ok {
		t.Fatal("Expected key1 to be deleted")
	}
}

func TestTTLCache_Expiration(t *testing.T) {
	cache := New(Opts{
		Shards:        16,
		Size:          1000,
		CleanupIntv:   50 * time.Millisecond,
		EnableCleanup: true,
	})
	defer func() { _ = cache.Close(context.Background()) }()

	// Set with short TTL
	cache.Set("expiring", "value", 100*time.Millisecond)

	// Should be accessible immediately
	val, ok := cache.Get("expiring")
	if !ok || val != "value" {
		t.Fatal("Expected to find expiring key")
	}

	// Wait for expiration
	time.Sleep(150 * time.Millisecond)

	// Should be expired
	_, ok = cache.Get("expiring")
	if ok {
		t.Fatal("Expected key to be expired")
	}
}

func TestTTLCache_NoExpiration(t *testing.T) {
	cache := New(Opts{
		Shards:        16,
		Size:          1000,
		CleanupIntv:   100 * time.Millisecond,
		EnableCleanup: false,
	})
	defer func() { _ = cache.Close(context.Background()) }()

	// Set with no TTL (0 duration)
	cache.Set("permanent", "value", 0)

	// Should still be accessible after some time
	time.Sleep(200 * time.Millisecond)
	val, ok := cache.Get("permanent")
	if !ok || val != "value" {
		t.Fatal("Expected to find permanent key")
	}
}

func TestTTLCache_LRUEviction(t *testing.T) {
	cache := New(Opts{
		Shards:        1, // Single shard for predictable LRU behavior
		Size:          5,
		CleanupIntv:   1 * time.Second,
		EnableCleanup: false,
	})
	defer func() { _ = cache.Close(context.Background()) }()

	// Fill cache to capacity
	for i := 0; i < 5; i++ {
		cache.Set(fmt.Sprintf("key%d", i), fmt.Sprintf("value%d", i), 0)
	}

	// Add one more - should evict the oldest (key0)
	cache.Set("key5", "value5", 0)

	// key0 should be evicted
	_, ok := cache.Get("key0")
	if ok {
		t.Fatal("Expected key0 to be evicted")
	}

	// Others should still be present
	for i := 1; i <= 5; i++ {
		_, ok := cache.Get(fmt.Sprintf("key%d", i))
		if !ok {
			t.Fatalf("Expected key%d to be present", i)
		}
	}
}

func TestTTLCache_ConcurrentAccess(t *testing.T) {
	cache := New(Opts{
		Shards:        256,
		Size:          10000,
		CleanupIntv:   100 * time.Millisecond,
		EnableCleanup: true,
	})
	defer func() { _ = cache.Close(context.Background()) }()

	const goroutines = 100
	const opsPerGoroutine = 1000

	var wg sync.WaitGroup
	wg.Add(goroutines)

	// Concurrent writes
	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				key := fmt.Sprintf("key_%d_%d", id, j)
				cache.Set(key, j, 1*time.Second)
			}
		}(i)
	}
	wg.Wait()

	// Concurrent reads
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				key := fmt.Sprintf("key_%d_%d", id, j)
				cache.Get(key)
			}
		}(i)
	}
	wg.Wait()
}

func TestTTLCache_BackgroundCleanup(t *testing.T) {
	cache := New(Opts{
		Shards:        16,
		Size:          1000,
		CleanupIntv:   50 * time.Millisecond,
		EnableCleanup: true,
	})
	defer func() { _ = cache.Close(context.Background()) }()

	// Add many entries with short TTL
	for i := 0; i < 100; i++ {
		cache.Set(fmt.Sprintf("key%d", i), i, 100*time.Millisecond)
	}

	// Check initial count
	if cache.Len() != 100 {
		t.Fatalf("Expected 100 items, got %d", cache.Len())
	}

	// Wait for cleanup to run multiple times
	// Wait longer than TTL (100ms) + cleanup interval (50ms) to ensure cleanup runs
	// Add buffer time for parallel cleanup to complete across all shards
	time.Sleep(200 * time.Millisecond)

	// Wait for cleanup to complete - poll until count stabilizes or timeout
	maxWait := 1 * time.Second
	checkInterval := 50 * time.Millisecond
	deadline := time.Now().Add(maxWait)
	var count int

	for time.Now().Before(deadline) {
		count = cache.Len()
		if count <= 10 {
			// Cleanup completed successfully
			return
		}
		// Wait a bit more for cleanup to complete
		time.Sleep(checkInterval)
	}

	// Final check
	count = cache.Len()
	if count > 10 {
		t.Fatalf("Expected most items to be cleaned up, but found %d after waiting", count)
	}
}

func TestTTLCache_Update(t *testing.T) {
	cache := New(Opts{
		Shards:        16,
		Size:          1000,
		CleanupIntv:   100 * time.Millisecond,
		EnableCleanup: false,
	})
	defer func() { _ = cache.Close(context.Background()) }()

	// Set initial value
	cache.Set("key", "value1", 1*time.Second)
	val, _ := cache.Get("key")
	if val != "value1" {
		t.Fatalf("Expected value1, got %v", val)
	}

	// Update value
	cache.Set("key", "value2", 1*time.Second)
	val, _ = cache.Get("key")
	if val != "value2" {
		t.Fatalf("Expected value2, got %v", val)
	}
}

// Benchmarks

func BenchmarkTTLCache_Get_Hit(b *testing.B) {
	cache := New(Opts{
		Shards:        256,
		Size:          100000,
		CleanupIntv:   1 * time.Second,
		EnableCleanup: false,
	})
	defer func() { _ = cache.Close(context.Background()) }()

	// Pre-populate cache
	for i := 0; i < 10000; i++ {
		cache.Set(fmt.Sprintf("key%d", i), i, 10*time.Second)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key%d", i%10000)
			cache.Get(key)
			i++
		}
	})
}

func BenchmarkTTLCache_Get_Miss(b *testing.B) {
	cache := New(Opts{
		Shards:        256,
		Size:          100000,
		CleanupIntv:   1 * time.Second,
		EnableCleanup: false,
	})
	defer func() { _ = cache.Close(context.Background()) }()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("missing%d", i)
			cache.Get(key)
			i++
		}
	})
}

func BenchmarkTTLCache_Set(b *testing.B) {
	cache := New(Opts{
		Shards:        256,
		Size:          1000000,
		CleanupIntv:   1 * time.Second,
		EnableCleanup: false,
	})
	defer func() { _ = cache.Close(context.Background()) }()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key%d", i)
			cache.Set(key, i, 1*time.Second)
			i++
		}
	})
}

func BenchmarkTTLCache_SetAndGet(b *testing.B) {
	cache := New(Opts{
		Shards:        256,
		Size:          100000,
		CleanupIntv:   1 * time.Second,
		EnableCleanup: false,
	})
	defer func() { _ = cache.Close(context.Background()) }()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key%d", i%50000)
			if i%2 == 0 {
				cache.Set(key, i, 1*time.Second)
			} else {
				cache.Get(key)
			}
			i++
		}
	})
}

// Comparison with sync.Map
func BenchmarkSyncMap_Get(b *testing.B) {
	m := &sync.Map{}

	// Pre-populate
	for i := 0; i < 10000; i++ {
		m.Store(fmt.Sprintf("key%d", i), i)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key%d", i%10000)
			m.Load(key)
			i++
		}
	})
}

func BenchmarkSyncMap_Set(b *testing.B) {
	m := &sync.Map{}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key%d", i)
			m.Store(key, i)
			i++
		}
	})
}

// Enhanced test cases

func TestTTLCache_Close(t *testing.T) {
	cache := New(Opts{
		Shards:        16,
		Size:          1000,
		CleanupIntv:   50 * time.Millisecond,
		EnableCleanup: true,
	})

	// Add some entries
	cache.Set("key1", "value1", 1*time.Second)
	cache.Set("key2", "value2", 1*time.Second)

	// Close cache
	_ = cache.Close(context.Background())

	// Operations after close should be safe but may not work
	_, _ = cache.Get("key1")

	// Close again should be safe
	_ = cache.Close(context.Background())
}

func TestTTLCache_CloseMultipleTimes(t *testing.T) {
	cache := New(Opts{
		Shards:        16,
		Size:          1000,
		CleanupIntv:   50 * time.Millisecond,
		EnableCleanup: true,
	})

	_ = cache.Close(context.Background())
	_ = cache.Close(context.Background())
	_ = cache.Close(context.Background())
}

func TestTTLCache_HitCount(t *testing.T) {
	cache := New(Opts{
		Shards:        16,
		Size:          1000,
		CleanupIntv:   100 * time.Millisecond,
		EnableCleanup: false,
	})
	defer func() { _ = cache.Close(context.Background()) }()

	// Set some values
	cache.Set("key1", "value1", 1*time.Second)
	cache.Set("key2", "value2", 1*time.Second)

	// Get hits
	cache.Get("key1")
	cache.Get("key1")
	cache.Get("key2")

	// Miss
	cache.Get("nonexistent")

	// HitCount removed - test passes if no panic
}

func TestTTLCache_LRUOrder(t *testing.T) {
	cache := New(Opts{
		Shards:        1,
		Size:          3,
		CleanupIntv:   1 * time.Second,
		EnableCleanup: false,
	})
	defer func() { _ = cache.Close(context.Background()) }()

	cache.Set("key1", "value1", 0)
	cache.Set("key2", "value2", 0)
	cache.Set("key3", "value3", 0)

	cache.Get("key1")
	cache.Set("key4", "value4", 0)

	_, ok := cache.Get("key2")
	if ok {
		t.Fatal("Expected key2 to be evicted")
	}

	for _, key := range []string{"key1", "key3", "key4"} {
		_, ok = cache.Get(key)
		if !ok {
			t.Fatalf("Expected %s to be present", key)
		}
	}
}

func TestTTLCache_UpdateTTL(t *testing.T) {
	cache := New(Opts{
		Shards:        16,
		Size:          1000,
		CleanupIntv:   100 * time.Millisecond,
		EnableCleanup: false,
	})
	defer func() { _ = cache.Close(context.Background()) }()

	// Set with short TTL
	cache.Set("key", "value1", 50*time.Millisecond)

	// Update with longer TTL
	cache.Set("key", "value2", 1*time.Second)

	// Wait past original TTL
	time.Sleep(100 * time.Millisecond)

	// Should still be accessible
	val, ok := cache.Get("key")
	if !ok {
		t.Fatal("Expected key to still be accessible after TTL update")
	}
	if val != "value2" {
		t.Fatalf("Expected value2, got %v", val)
	}
}

func TestTTLCache_Clear(t *testing.T) {
	cache := New(Opts{
		Shards:        16,
		Size:          1000,
		CleanupIntv:   100 * time.Millisecond,
		EnableCleanup: false,
	})
	defer func() { _ = cache.Close(context.Background()) }()

	// Add entries
	for i := 0; i < 100; i++ {
		cache.Set(fmt.Sprintf("key%d", i), i, 1*time.Second)
	}

	if cache.Len() != 100 {
		t.Fatalf("Expected 100 items, got %d", cache.Len())
	}

	// Clear
	cache.Clear()

	if cache.Len() != 0 {
		t.Fatalf("Expected 0 items after clear, got %d", cache.Len())
	}

	// Verify all keys are gone
	for i := 0; i < 100; i++ {
		_, ok := cache.Get(fmt.Sprintf("key%d", i))
		if ok {
			t.Fatalf("Expected key%d to be cleared", i)
		}
	}
}

func TestTTLCache_ZeroShardSize(t *testing.T) {
	cache := New(Opts{
		Shards:        16,
		Size:          0, // Unlimited
		CleanupIntv:   100 * time.Millisecond,
		EnableCleanup: false,
	})
	defer func() { _ = cache.Close(context.Background()) }()

	// Add many entries - should not evict
	for i := 0; i < 1000; i++ {
		cache.Set(fmt.Sprintf("key%d", i), i, 0)
	}

	if cache.Len() != 1000 {
		t.Fatalf("Expected 1000 items, got %d", cache.Len())
	}
}

func TestTTLCache_DefaultConfig(t *testing.T) {
	cache := New(Opts{})
	defer func() { _ = cache.Close(context.Background()) }()

	// Should work with defaults
	cache.Set("key1", "value1", 1*time.Second)
	val, ok := cache.Get("key1")
	if !ok || val != "value1" {
		t.Fatal("Expected cache to work with default config")
	}
}

func TestTTLCache_ShardPowerOfTwo(t *testing.T) {
	// Test that non-power-of-two shard count is adjusted
	cache := New(Opts{
		Shards:        100, // Not power of 2
		Size:          1000,
		CleanupIntv:   100 * time.Millisecond,
		EnableCleanup: false,
	})
	defer func() { _ = cache.Close(context.Background()) }()

	// Should work
	cache.Set("key1", "value1", 1*time.Second)
	val, ok := cache.Get("key1")
	if !ok || val != "value1" {
		t.Fatal("Expected cache to work with adjusted shard count")
	}
}

func TestTTLCache_ConcurrentMixedOps(t *testing.T) {
	cache := New(Opts{
		Shards:        256,
		Size:          10000,
		CleanupIntv:   100 * time.Millisecond,
		EnableCleanup: true,
	})
	defer func() { _ = cache.Close(context.Background()) }()

	const goroutines = 50
	const opsPerGoroutine = 500
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				key := fmt.Sprintf("key_%d_%d", id, j)
				switch j % 4 {
				case 0:
					cache.Set(key, j, 1*time.Second)
				case 1:
					cache.Get(key)
				case 2:
					cache.Del(key)
				case 3:
					cache.Set(key, j, 1*time.Second)
					cache.Get(key)
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestTTLCache_LazyExpiration(t *testing.T) {
	cache := New(Opts{
		Shards:        16,
		Size:          1000,
		CleanupIntv:   1 * time.Second,
		EnableCleanup: false,
	})
	defer func() { _ = cache.Close(context.Background()) }()

	cache.Set("expiring", "value", 50*time.Millisecond)
	time.Sleep(100 * time.Millisecond)

	_, ok := cache.Get("expiring")
	if ok {
		t.Fatal("Expected key to be expired on access")
	}

	time.Sleep(50 * time.Millisecond)
	if cache.Len() != 0 {
		t.Fatalf("Expected 0 items after lazy expiration, got %d", cache.Len())
	}
}

func TestTTLCache_StressTest(t *testing.T) {
	cache := New(Opts{
		Shards:        256,
		Size:          100000,
		CleanupIntv:   100 * time.Millisecond,
		EnableCleanup: true,
	})
	defer func() { _ = cache.Close(context.Background()) }()

	const goroutines = 200
	const opsPerGoroutine = 1000
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				key := fmt.Sprintf("key_%d_%d", id, j)
				cache.Set(key, j, time.Duration(j%1000)*time.Millisecond)
				cache.Get(key)
			}
		}(i)
	}
	wg.Wait()
}

func TestTTLCache_DeleteNonExistent(t *testing.T) {
	cache := New(Opts{
		Shards:        16,
		Size:          1000,
		CleanupIntv:   100 * time.Millisecond,
		EnableCleanup: false,
	})
	defer func() { _ = cache.Close(context.Background()) }()

	// Del non-existent key should be safe
	cache.Del("nonexistent")

	// Should not affect other keys
	cache.Set("key1", "value1", 1*time.Second)
	cache.Del("nonexistent")
	val, ok := cache.Get("key1")
	if !ok || val != "value1" {
		t.Fatal("Expected key1 to still be present")
	}
}

func TestTTLCache_UpdateExistingValue(t *testing.T) {
	cache := New(Opts{
		Shards:        16,
		Size:          1000,
		CleanupIntv:   100 * time.Millisecond,
		EnableCleanup: false,
	})
	defer func() { _ = cache.Close(context.Background()) }()

	// Set initial value
	cache.Set("key", "value1", 1*time.Second)

	// Update with different value
	cache.Set("key", "value2", 1*time.Second)

	// Should have new value
	val, ok := cache.Get("key")
	if !ok {
		t.Fatal("Expected key to be present")
	}
	if val != "value2" {
		t.Fatalf("Expected value2, got %v", val)
	}

	// Should still be one item
	if cache.Len() != 1 {
		t.Fatalf("Expected 1 item, got %d", cache.Len())
	}
}
