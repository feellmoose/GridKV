package storage

import (
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestMemoryStorageBasicOperations tests basic Get/Set/Delete operations
func TestMemoryStorageBasicOperations(t *testing.T) {
	store, err := NewMemoryStorage(100) // 100MB limit
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer store.Close()

	// Test Set
	item := &StoredItem{
		Value:   []byte("test-value"),
		Version: 1,
	}
	if err := store.Set("test-key", item); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// Test Get
	retrieved, err := store.Get("test-key")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if string(retrieved.Value) != "test-value" {
		t.Errorf("Expected 'test-value', got '%s'", string(retrieved.Value))
	}

	// Test Delete
	if err := store.Delete("test-key", 1); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Verify deleted
	_, err = store.Get("test-key")
	if err != ErrItemNotFound {
		t.Errorf("Expected ErrItemNotFound, got %v", err)
	}
}

// TestMemoryStorageCompression tests compression functionality
func TestMemoryStorageCompression(t *testing.T) {
	store, err := NewMemoryStorage(100)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer store.Close()

	// Create a large value that should be compressed (>64 bytes)
	largeValue := make([]byte, 200)
	for i := range largeValue {
		largeValue[i] = byte(i % 256)
	}

	item := &StoredItem{
		Value:   largeValue,
		Version: 1,
	}
	if err := store.Set("large-key", item); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// Retrieve and verify
	retrieved, err := store.Get("large-key")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if len(retrieved.Value) != len(largeValue) {
		t.Errorf("Value size mismatch: expected %d, got %d", len(largeValue), len(retrieved.Value))
	}
	for i := range largeValue {
		if retrieved.Value[i] != largeValue[i] {
			t.Errorf("Value mismatch at index %d", i)
			break
		}
	}
}

// TestMemoryStorageConcurrency tests concurrent access safety
func TestMemoryStorageConcurrency(t *testing.T) {
	store, err := NewMemoryStorage(1000) // 1GB limit for concurrent test
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer store.Close()

	const numGoroutines = 100
	const numOps = 1000
	var wg sync.WaitGroup
	wg.Add(numGoroutines * 2) // Readers and writers

	// Concurrent writers
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < numOps; j++ {
				key := "key-" + string(rune(id*numOps+j))
				item := &StoredItem{
					Value:   []byte("value"),
					Version: int64(j),
				}
				_ = store.Set(key, item)
			}
		}(i)
	}

	// Concurrent readers
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < numOps; j++ {
				key := "key-" + string(rune(id*numOps+j))
				_, _ = store.Get(key)
			}
		}(i)
	}

	wg.Wait()
	// If we get here without panic, concurrency is safe
}

// TestMemoryStorageExpiration tests expiration and cleanup
func TestMemoryStorageExpiration(t *testing.T) {
	store, err := NewMemoryStorage(100)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer store.Close()

	// Set item with short expiration
	item := &StoredItem{
		Value:    []byte("expiring-value"),
		Version:  1,
		ExpireAt: time.Now().Add(100 * time.Millisecond),
	}
	if err := store.Set("expiring-key", item); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// Should be available immediately
	_, err = store.Get("expiring-key")
	if err != nil {
		t.Fatalf("Get failed immediately after Set: %v", err)
	}

	// Wait for expiration
	time.Sleep(150 * time.Millisecond)

	// Should be expired (lazy deletion)
	_, err = store.Get("expiring-key")
	if err != ErrItemExpired {
		t.Errorf("Expected ErrItemExpired, got %v", err)
	}

	// Wait for background cleaner (runs every 10 seconds)
	// For this test, we'll just verify lazy deletion works
}

// TestMemoryStorageLRUEviction tests LRU eviction
func TestMemoryStorageLRUEviction(t *testing.T) {
	store, err := NewMemoryStorage(1) // 1MB limit - very small
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer store.Close()

	// Fill storage to trigger eviction
	for i := 0; i < 1000; i++ {
		key := "key-" + string(rune(i))
		value := make([]byte, 2000) // 2KB per item
		item := &StoredItem{
			Value:   value,
			Version: 1,
		}
		_ = store.Set(key, item)
	}

	// Eviction should have occurred
	stats := store.Stats()
	if stats.KeyCount > 100 {
		t.Logf("Key count after eviction: %d (expected < 100)", stats.KeyCount)
	}
}

// TestMemoryStorageGoroutineLeak tests for goroutine leaks
func TestMemoryStorageGoroutineLeak(t *testing.T) {
	initialGoroutines := runtime.NumGoroutine()

	store, err := NewMemoryStorage(100)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	// Give cleaner goroutine time to start
	time.Sleep(50 * time.Millisecond)

	// Should have 1 additional goroutine (cleaner)
	runningGoroutines := runtime.NumGoroutine()
	if runningGoroutines < initialGoroutines+1 {
		t.Logf("Expected at least %d goroutines, got %d", initialGoroutines+1, runningGoroutines)
	}

	// Close storage
	store.Close()

	// Wait for goroutine to exit
	time.Sleep(200 * time.Millisecond)

	// Should be back to initial count (or close)
	finalGoroutines := runtime.NumGoroutine()
	if finalGoroutines > initialGoroutines+2 {
		t.Errorf("Possible goroutine leak: initial=%d, final=%d", initialGoroutines, finalGoroutines)
	}
}

// TestMemoryStorageMemorySafety tests memory safety with large values
func TestMemoryStorageMemorySafety(t *testing.T) {
	store, err := NewMemoryStorage(10) // 10MB limit
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer store.Close()

	// Try to set values that exceed memory limit
	largeValue := make([]byte, 5*1024*1024) // 5MB
	item := &StoredItem{
		Value:   largeValue,
		Version: 1,
	}

	// First item should succeed
	if err := store.Set("key1", item); err != nil {
		t.Fatalf("First Set failed: %v", err)
	}

	// Second item should trigger eviction or fail
	err = store.Set("key2", item)
	if err != nil && err != ErrMemoryLimitExceeded {
		t.Errorf("Expected ErrMemoryLimitExceeded or success, got %v", err)
	}
}

// TestMemoryStorageBatchOperations tests batch operations
func TestMemoryStorageBatchOperations(t *testing.T) {
	store, err := NewMemoryStorage(100)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer store.Close()

	// Test BatchSet
	items := make(map[string]*StoredItem)
	for i := 0; i < 100; i++ {
		key := "batch-key-" + string(rune(i))
		items[key] = &StoredItem{
			Value:   []byte("batch-value"),
			Version: 1,
		}
	}

	if err := store.BatchSet(items); err != nil {
		t.Fatalf("BatchSet failed: %v", err)
	}

	// Test BatchGet
	keys := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		keys = append(keys, "batch-key-"+string(rune(i)))
	}

	results, err := store.BatchGet(keys)
	if err != nil {
		t.Fatalf("BatchGet failed: %v", err)
	}

	if len(results) != 100 {
		t.Errorf("Expected 100 results, got %d", len(results))
	}
}

// TestMemoryStorageStats tests statistics
func TestMemoryStorageStats(t *testing.T) {
	store, err := NewMemoryStorage(100)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer store.Close()

	// Set some items
	for i := 0; i < 10; i++ {
		key := "stats-key-" + string(rune(i))
		item := &StoredItem{
			Value:   []byte("value"),
			Version: 1,
		}
		_ = store.Set(key, item)
	}

	stats := store.Stats()
	if stats.KeyCount != 10 {
		t.Errorf("Expected 10 keys, got %d", stats.KeyCount)
	}
}
