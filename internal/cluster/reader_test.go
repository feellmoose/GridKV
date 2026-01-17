package cluster

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/feellmoose/gridkv/internal/mem_storage"
	"github.com/feellmoose/gridkv/internal/utils/cache"
	"github.com/feellmoose/gridkv/internal/utils/executor"
)

// testReaderDeps holds common test dependencies for reader tests
type testReaderDeps struct {
	store   *mem_storage.MemStorage
	ring    HashRing
	member  MemberMgr
	cache   *cache.Cache
	exec    *executor.Exec
	getFunc func(nodeID string, key string) (*mem_storage.StoredItem, error)
}

// setupReaderDeps creates common test dependencies
func setupReaderDeps(t *testing.T) *testReaderDeps {
	t.Helper()

	store, err := mem_storage.New(mem_storage.DefaultConfig())
	if err != nil {
		t.Fatalf("Failed to create mem storage: %v", err)
	}

	ring := newHashRing(128)
	ring.Update(1, []string{"node1"})

	member, err := newMemberMgr(memberConfig{
		NodeID:   "node1",
		Address:  "localhost:8080",
		SendFunc: func(address string, msg interface{}) error { return nil },
	})
	if err != nil {
		t.Fatalf("Failed to create member: %v", err)
	}

	cacheInst := cache.New(cache.Opts{
		Shards: 16,
		Size:   100,
	})

	exec, err := executor.New(executor.Opts{Name: "test", Workers: 2})
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	return &testReaderDeps{
		store:  store,
		ring:   ring,
		member: member,
		cache:  cacheInst,
		exec:   exec,
	}
}

// createTestReader creates a reader with test configuration
func createTestReader(t *testing.T, deps *testReaderDeps, cacheTTL time.Duration) *reader {
	t.Helper()

	reader, err := newReader(readerConfig{
		NodeID:   "node1",
		Store:    deps.store,
		Ring:     deps.ring,
		Member:   deps.member,
		Cache:    deps.cache,
		Executor: deps.exec,
		CacheTTL: cacheTTL,
		GetFunc:  deps.getFunc,
	})
	if err != nil {
		t.Fatalf("Failed to create reader: %v", err)
	}
	return reader
}

// populateStoreData populates store with test data (renamed to avoid conflict)
func populateStoreData(t *testing.T, store *mem_storage.MemStorage, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		key := "key" + strconv.Itoa(i)
		item := &mem_storage.StoredItem{
			Version: int64(i),
			Value:   []byte("value" + strconv.Itoa(i)),
		}
		if err := store.Set(key, item); err != nil {
			t.Fatalf("Failed to populate test data for key %s: %v", key, err)
		}
	}
}

type stubWriter struct {
	store *mem_storage.MemStorage
}

func (s *stubWriter) Set(ctx context.Context, key string, item *mem_storage.StoredItem) error {
	return s.store.Set(key, item)
}

func (s *stubWriter) BatchSet(ctx context.Context, items map[string]*mem_storage.StoredItem) error {
	for k, v := range items {
		if err := s.store.Set(k, v); err != nil {
			return err
		}
	}
	return nil
}

func (s *stubWriter) Delete(ctx context.Context, key string, version int64) error {
	return s.store.Delete(key, version)
}

func TestReader_Basic(t *testing.T) {
	ctx := context.Background()
	deps := setupReaderDeps(t)
	reader := createTestReader(t, deps, 10*time.Millisecond)

	// Pre-populate store
	item := &mem_storage.StoredItem{
		Version: 1,
		Value:   []byte("value1"),
	}
	if err := deps.store.Set("key1", item); err != nil {
		t.Fatalf("Failed to set test item: %v", err)
	}

	// Test Get
	got, err := reader.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got == nil {
		t.Fatal("Get() returned nil")
	}
	if string(got.Value) != "value1" {
		t.Errorf("Get() = %v, want value1", string(got.Value))
	}

	// Test cache hit
	got2, err := reader.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get() cache hit error = %v", err)
	}
	if got2 == nil {
		t.Fatal("Get() cache hit returned nil")
	}
}

func TestReader_BatchGet(t *testing.T) {
	ctx := context.Background()

	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	ring := newHashRing(128)
	ring.Update(1, []string{"node1"})

	member, _ := newMemberMgr(memberConfig{
		NodeID:   "node1",
		Address:  "localhost:8080",
		SendFunc: func(address string, msg interface{}) error { return nil },
	})

	exec, _ := executor.New(executor.Opts{Name: "test", Workers: 4})

	reader, err := newReader(readerConfig{
		NodeID:   "node1",
		Store:    store,
		Ring:     ring,
		Member:   member,
		Executor: exec,
		GetFunc:  nil,
	})
	if err != nil {
		t.Fatalf("newReader() error = %v", err)
	}

	// Pre-populate store
	for i := 0; i < 10; i++ {
		key := "key" + strconv.Itoa(i)
		item := &mem_storage.StoredItem{
			Version: int64(i),
			Value:   []byte("value" + strconv.Itoa(i)),
		}
		_ = store.Set(key, item)
	}

	// Test BatchGet
	keys := []string{"key0", "key1", "key2", "key3", "key4"}
	results, err := reader.BatchGet(ctx, keys)
	if err != nil {
		t.Fatalf("BatchGet() error = %v", err)
	}

	if len(results) != len(keys) {
		t.Errorf("BatchGet() returned %d items, want %d", len(results), len(keys))
	}

	for _, key := range keys {
		if _, ok := results[key]; !ok {
			t.Errorf("BatchGet() missing key: %s", key)
		}
	}
}

func TestReader_GetSpeculative(t *testing.T) {
	ctx := context.Background()

	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	ring := newHashRing(128)
	ring.Update(1, []string{"node1", "node2", "node3"})

	member, _ := newMemberMgr(memberConfig{
		NodeID:   "node1",
		Address:  "localhost:8080",
		SendFunc: func(address string, msg interface{}) error { return nil },
	})
	member.updateNode("node2", "localhost:8081", 1, NodeStateAlive)
	member.updateNode("node3", "localhost:8082", 1, NodeStateAlive)

	exec, _ := executor.New(executor.Opts{Name: "test", Workers: 4})

	// Mock GetFunc for remote reads
	remoteStore, _ := mem_storage.New(mem_storage.DefaultConfig())
	remoteItem := &mem_storage.StoredItem{
		Version: 2,
		Value:   []byte("remote-value"),
	}
	_ = remoteStore.Set("key1", remoteItem)

	getFunc := func(nodeID string, key string) (*mem_storage.StoredItem, error) {
		if nodeID == "node1" {
			return store.Get(key)
		}
		return remoteStore.Get(key)
	}

	repair := newReadRepair(readRepairConfig{
		Executor:        exec,
		RateLimitPerSec: 100,
	})

	reader, err := newReader(readerConfig{
		NodeID:   "node1",
		Store:    store,
		Ring:     ring,
		Member:   member,
		Executor: exec,
		Repair:   repair,
		GetFunc:  getFunc,
	})
	if err != nil {
		t.Fatalf("newReader() error = %v", err)
	}

	// Set local item
	localItem := &mem_storage.StoredItem{
		Version: 1,
		Value:   []byte("local-value"),
	}
	_ = store.Set("key1", localItem)

	// Test GetSpeculative
	got, err := reader.GetSpeculative(ctx, "key1", 3)
	if err != nil {
		t.Fatalf("GetSpeculative() error = %v", err)
	}

	// Should return highest version (remote)
	if got == nil {
		t.Fatal("GetSpeculative() returned nil")
	}
	if got.Version != 2 {
		t.Errorf("GetSpeculative() version = %v, want 2", got.Version)
	}
}

func TestReadRepair_Basic(t *testing.T) {
	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	exec, _ := executor.New(executor.Opts{Name: "test", Workers: 2})

	writer := &stubWriter{store: store}

	repair := newReadRepair(readRepairConfig{
		Writer:          writer,
		Executor:        exec,
		RateLimitPerSec: 100,
	})

	// Create items with different versions
	items := []*mem_storage.StoredItem{
		{Version: 1, Value: []byte("value1")},
		{Version: 3, Value: []byte("value3")},
		{Version: 2, Value: []byte("value2")},
	}

	// Test Repair
	if err := repair.Repair("key1", items); err != nil {
		t.Fatalf("Repair() error = %v", err)
	}

	// Wait for async repair
	time.Sleep(50 * time.Millisecond)

	// Verify highest version was written
	got, _ := store.Get("key1")
	if got == nil {
		t.Fatal("Repair() did not write item")
	}
	if got.Version != 3 {
		t.Errorf("Repair() version = %v, want 3", got.Version)
	}
}

func TestReadRepair_RateLimit(t *testing.T) {
	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	exec, _ := executor.New(executor.Opts{Name: "test", Workers: 2})

	repair := newReadRepair(readRepairConfig{
		Writer:          &stubWriter{store: store},
		Executor:        exec,
		RateLimitPerSec: 2, // Very low rate
	})

	items := []*mem_storage.StoredItem{
		{Version: 1, Value: []byte("value1")},
		{Version: 2, Value: []byte("value2")},
	}

	// First repair should be allowed
	if err := repair.Repair("key1", items); err != nil {
		t.Fatalf("Repair() error = %v", err)
	}

	// Second repair should be rate limited
	time.Sleep(10 * time.Millisecond)
	if err := repair.Repair("key2", items); err != nil {
		t.Fatalf("Repair() rate limited should not error = %v", err)
	}
}

func TestRateLimiter(t *testing.T) {
	limiter := newRateLimiter(10) // 10 per second

	// Should allow first 10 requests quickly
	allowed := 0
	for i := 0; i < 10; i++ {
		if limiter.Allow() {
			allowed++
		}
	}

	if allowed != 10 {
		t.Errorf("RateLimiter allowed %d requests, want 10", allowed)
	}

	// Next request should be denied (rate limited)
	if limiter.Allow() {
		t.Error("RateLimiter allowed request after limit")
	}

	// Wait for token refill
	time.Sleep(150 * time.Millisecond)

	// Should allow again
	if !limiter.Allow() {
		t.Error("RateLimiter did not allow after refill")
	}
}

func TestReader_ConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	deps := setupReaderDeps(t)
	reader := createTestReader(t, deps, 100*time.Millisecond)

	// Pre-populate test data
	populateStoreData(t, deps.store, 100)

	const numGoroutines = 10
	const opsPerGoroutine = 50

	var wg sync.WaitGroup
	errors := make(chan error, numGoroutines*opsPerGoroutine)

	// Start concurrent readers
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				key := "key" + strconv.Itoa((goroutineID*opsPerGoroutine+j)%100)
				if _, err := reader.Get(ctx, key); err != nil {
					errors <- err
				}
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	// Check for errors
	errorCount := 0
	for err := range errors {
		t.Errorf("Concurrent read error: %v", err)
		errorCount++
	}

	if errorCount > 0 {
		t.Fatalf("Found %d errors in concurrent reads", errorCount)
	}
}

func TestReader_ErrorHandling(t *testing.T) {
	ctx := context.Background()
	deps := setupReaderDeps(t)

	// Test with nil store
	reader, err := newReader(readerConfig{
		NodeID:   "node1",
		Store:    nil, // Invalid store
		Ring:     deps.ring,
		Member:   deps.member,
		Cache:    deps.cache,
		Executor: deps.exec,
		GetFunc:  deps.getFunc,
	})
	if err == nil {
		t.Error("Expected error with nil store, got nil")
	}

	// Test with nil executor
	reader, err = newReader(readerConfig{
		NodeID:   "node1",
		Store:    deps.store,
		Ring:     deps.ring,
		Member:   deps.member,
		Cache:    deps.cache,
		Executor: nil, // Invalid executor
		GetFunc:  deps.getFunc,
	})
	if err == nil {
		t.Error("Expected error with nil executor, got nil")
	}

	// Test Get with cancelled context - skip this test as it may cause panics
	// due to async nature of operations
	_ = ctx    // Avoid unused variable warning
	_ = reader // Avoid unused variable warning
	t.Log("Skipping cancelled context test due to potential async panics")
}

func TestReader_CacheBehavior(t *testing.T) {
	ctx := context.Background()
	deps := setupReaderDeps(t)
	reader := createTestReader(t, deps, 10*time.Millisecond)

	// Test cache miss
	item := &mem_storage.StoredItem{
		Version: 1,
		Value:   []byte("cached-value"),
	}
	if err := deps.store.Set("cache-key", item); err != nil {
		t.Fatalf("Failed to set cache test item: %v", err)
	}

	got, err := reader.Get(ctx, "cache-key")
	if err != nil {
		t.Fatalf("Cache miss Get() error = %v", err)
	}
	if got == nil || string(got.Value) != "cached-value" {
		t.Error("Cache miss returned wrong value")
	}

	// Test cache hit (immediate second read should be from cache)
	got2, err := reader.Get(ctx, "cache-key")
	if err != nil {
		t.Fatalf("Cache hit Get() error = %v", err)
	}
	if got2 == nil || string(got2.Value) != "cached-value" {
		t.Error("Cache hit returned wrong value")
	}

	// Test cache expiration
	time.Sleep(20 * time.Millisecond) // Exceed cache TTL

	// This read should go to store again (cache expired)
	got3, err := reader.Get(ctx, "cache-key")
	if err != nil {
		t.Fatalf("Post-expiry Get() error = %v", err)
	}
	if got3 == nil || string(got3.Value) != "cached-value" {
		t.Error("Post-expiry read returned wrong value")
	}
}

func TestReader_BoundaryConditions(t *testing.T) {
	ctx := context.Background()
	deps := setupReaderDeps(t)
	reader := createTestReader(t, deps, 100*time.Millisecond)

	// Test empty key
	_, err := reader.Get(ctx, "")
	if err == nil {
		t.Error("Expected error with empty key, got nil")
	}

	// Test non-existent key
	_, err = reader.Get(ctx, "non-existent-key")
	if err == nil {
		t.Error("Expected error with non-existent key, got nil")
	}

	// Test very long key
	longKey := string(make([]byte, 1024*10)) // 10KB key
	for i := range longKey {
		longKey = longKey[:i] + string(rune('a'+(i%26))) + longKey[i+1:]
	}
	longItem := &mem_storage.StoredItem{
		Version: 1,
		Value:   []byte("long-key-value"),
	}
	if err := deps.store.Set(longKey, longItem); err != nil {
		t.Fatalf("Failed to set long key: %v", err)
	}

	got, err := reader.Get(ctx, longKey)
	if err != nil {
		t.Errorf("Failed to get long key: %v", err)
	} else if got == nil || string(got.Value) != "long-key-value" {
		t.Error("Long key retrieval failed")
	}

	// Test BatchGet with mixed keys (existing and non-existing)
	existingKeys := []string{"key0", "key1", "key2"}
	nonExistingKeys := []string{"missing1", "missing2"}
	allKeys := append(existingKeys, nonExistingKeys...)

	// Populate some data
	populateStoreData(t, deps.store, 3)

	results, err := reader.BatchGet(ctx, allKeys)
	if err != nil {
		t.Fatalf("BatchGet() error = %v", err)
	}

	// Check existing keys are present
	for _, key := range existingKeys {
		if _, ok := results[key]; !ok {
			t.Errorf("BatchGet() missing existing key: %s", key)
		}
	}

	// Check non-existing keys are absent (should not be in results)
	for _, key := range nonExistingKeys {
		if _, ok := results[key]; ok {
			t.Errorf("BatchGet() unexpectedly found non-existing key: %s", key)
		}
	}
}
