package cluster

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/feellmoose/gridkv/internal/mem_storage"
	"github.com/feellmoose/gridkv/internal/utils/cache"
	"github.com/feellmoose/gridkv/internal/utils/executor"
)

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

	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	ring := newHashRing(128)
	ring.Update(1, []string{"node1"})

	member, _ := newMemberMgr(memberConfig{
		NodeID:   "node1",
		Address:  "localhost:8080",
		SendFunc: func(address string, msg interface{}) error { return nil },
	})

	cacheInst := cache.New(cache.Opts{
		Shards: 16,
		Size:   100,
	})
	exec, _ := executor.New(executor.Opts{Name: "test", Workers: 2})

	reader, err := newReader(readerConfig{
		NodeID:   "node1",
		Store:    store,
		Ring:     ring,
		Member:   member,
		Cache:    cacheInst,
		Executor: exec,
		CacheTTL: 10 * time.Millisecond,
		GetFunc:  nil,
	})
	if err != nil {
		t.Fatalf("newReader() error = %v", err)
	}

	// Pre-populate store
	item := &mem_storage.StoredItem{
		Version: 1,
		Value:   []byte("value1"),
	}
	_ = store.Set("key1", item)

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
