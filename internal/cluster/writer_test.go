package cluster

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feellmoose/gridkv/internal/mem_storage"
	"github.com/feellmoose/gridkv/internal/utils/executor"
	"github.com/feellmoose/gridkv/internal/utils/hlc"
)

// testWriterDeps holds common test dependencies for writer tests
type testWriterDeps struct {
	store   *mem_storage.MemStorage
	hlcInst *hlc.HLC
	exec    *executor.Exec
	ring    HashRing
	gossip  Gossip
	member  MemberMgr
}

// setupWriterDeps creates common test dependencies
func setupWriterDeps(t *testing.T) *testWriterDeps {
	t.Helper()

	store, err := mem_storage.New(mem_storage.DefaultConfig())
	if err != nil {
		t.Fatalf("Failed to create mem storage: %v", err)
	}

	hlcInst := hlc.NewHLC("node1")
	exec, err := executor.New(executor.Opts{Name: "test", Workers: 2})
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	ring := newHashRing(128)
	ring.Update(1, []string{"node1"})

	gossip, err := newGossip(gossipConfig{
		NodeID:   "node1",
		Store:    store,
		Executor: exec,
		SendFunc: func(address string, data []byte) error { return nil },
	})
	if err != nil {
		t.Fatalf("Failed to create gossip: %v", err)
	}

	member, err := newMemberMgr(memberConfig{
		NodeID:         "node1",
		Address:        "localhost:8080",
		PingInterval:   100 * time.Millisecond,
		FailureTimeout: 1 * time.Second,
		SuspectTimeout: 500 * time.Millisecond,
		SendFunc:       noOpSendFunc,
	})
	if err != nil {
		t.Fatalf("Failed to create member: %v", err)
	}

	return &testWriterDeps{
		store:   store,
		hlcInst: hlcInst,
		exec:    exec,
		ring:    ring,
		gossip:  gossip,
		member:  member,
	}
}

// createTestWriter creates a writer with test configuration
func createTestWriter(t *testing.T, deps *testWriterDeps, batchThreshold int, batchWindow time.Duration) *writer {
	t.Helper()

	writer, err := newWriter(writerConfig{
		NodeID:         "node1",
		HLC:            deps.hlcInst,
		Store:          deps.store,
		Ring:           deps.ring,
		Gossip:         deps.gossip,
		Member:         deps.member,
		Executor:       deps.exec,
		BatchThreshold: batchThreshold,
		BatchWindow:    batchWindow,
		ReplicaCount:   3,
	})
	if err != nil {
		t.Fatalf("Failed to create writer: %v", err)
	}
	return writer
}

func TestWriter_Basic(t *testing.T) {
	ctx := context.Background()
	deps := setupWriterDeps(t)
	writer := createTestWriter(t, deps, 10, 100*time.Millisecond)

	// Test Set
	item := &mem_storage.StoredItem{
		Value: []byte("value1"),
	}
	if err := writer.Set(ctx, "key1", item); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	// Verify stored
	got, err := deps.store.Get("key1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got == nil {
		t.Fatal("Get() returned nil")
	}
	if string(got.Value) != "value1" {
		t.Errorf("Get() = %v, want value1", string(got.Value))
	}
}

func TestWriter_BatchSet(t *testing.T) {
	ctx := context.Background()
	deps := setupWriterDeps(t)
	writer := createTestWriter(t, deps, 10, 100*time.Millisecond)

	// Test BatchSet
	items := map[string]*mem_storage.StoredItem{
		"key1": {Value: []byte("value1")},
		"key2": {Value: []byte("value2")},
		"key3": {Value: []byte("value3")},
	}

	if err := writer.BatchSet(ctx, items); err != nil {
		t.Fatalf("BatchSet() error = %v", err)
	}

	// Verify all stored
	for key, expected := range items {
		got, err := deps.store.Get(key)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", key, err)
		}
		if got == nil {
			t.Fatalf("Get(%s) returned nil", key)
		}
		if string(got.Value) != string(expected.Value) {
			t.Errorf("Get(%s) = %v, want %v", key, string(got.Value), string(expected.Value))
		}
	}
}

func TestWriter_Delete(t *testing.T) {
	ctx := context.Background()

	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	hlcInst := hlc.NewHLC("node1")
	exec, _ := executor.New(executor.Opts{Name: "test", Workers: 2})
	ring := newHashRing(128)
	ring.Update(1, []string{"node1"})

	gossip, _ := newGossip(gossipConfig{
		NodeID:   "node1",
		Store:    store,
		Executor: exec,
		SendFunc: func(address string, data []byte) error { return nil },
	})

	writer, err := newWriter(writerConfig{
		NodeID:         "node1",
		HLC:            hlcInst,
		Store:          store,
		Ring:           ring,
		Gossip:         gossip,
		Executor:       exec,
		BatchThreshold: 1,                    // Force immediate batching for test
		BatchWindow:    1 * time.Millisecond, // Very short window
		ReplicaCount:   3,
	})
	if err != nil {
		t.Fatalf("newWriter() error = %v", err)
	}

	// Set a key first
	item := &mem_storage.StoredItem{
		Value: []byte("value1"),
	}
	if err := writer.Set(ctx, "key1", item); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	// Get version
	existing, _ := store.Get("key1")
	version := int64(0)
	if existing != nil {
		version = existing.Version
	}

	// Delete
	if err := writer.Delete(ctx, "key1", version); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	// Wait for batch processing to complete
	time.Sleep(10 * time.Millisecond)

	// Verify deleted (tombstone)
	got, err := store.Get("key1")
	if err != nil {
		t.Fatalf("Get() after delete error = %v", err)
	}
	gotExists := got != nil
	var value []byte
	var itemVersion int64
	var isTombstone bool
	if gotExists {
		value = got.Value
		itemVersion = got.Version
		isTombstone = got.IsTombstone()
	}
	t.Logf("After delete: got=%v, Value=%v, Version=%d, IsTombstone=%v", gotExists, value, itemVersion, isTombstone)
	if got != nil && !got.IsTombstone() {
		t.Errorf("Item after Delete() is not a tombstone: Value=%v, Version=%d, len(Value)=%d", got.Value, got.Version, len(got.Value))
	}
}

func TestWriter_BatchTrigger(t *testing.T) {
	ctx := context.Background()

	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	hlcInst := hlc.NewHLC("node1")
	exec, _ := executor.New(executor.Opts{Name: "test", Workers: 2})
	ring := newHashRing(128)
	ring.Update(1, []string{"node1", "node2", "node3"})

	var pushed int32
	gossip, _ := newGossip(gossipConfig{
		NodeID:   "node1",
		Store:    store,
		Executor: exec,
		SendFunc: func(address string, data []byte) error {
			atomic.StoreInt32(&pushed, 1)
			return nil
		},
	})

	// Create a simple member mock for testing
	member, _ := newMemberMgr(memberConfig{
		NodeID:         "node1",
		Address:        "localhost:8080",
		PingInterval:   100 * time.Millisecond,
		FailureTimeout: 1 * time.Second,
		SuspectTimeout: 500 * time.Millisecond,
		SendFunc:       noOpSendFunc,
	})
	// Add test nodes to member list
	member.updateNode("node2", "localhost:8081", 1, NodeStateAlive)
	member.updateNode("node3", "localhost:8082", 1, NodeStateAlive)

	writer, err := newWriter(writerConfig{
		NodeID:         "node1",
		HLC:            hlcInst,
		Store:          store,
		Ring:           ring,
		Gossip:         gossip,
		Member:         member,
		Executor:       exec,
		BatchThreshold: 5,
		BatchWindow:    50 * time.Millisecond,
		ReplicaCount:   3,
	})
	if err != nil {
		t.Fatalf("newWriter() error = %v", err)
	}

	// Trigger batch by threshold
	for i := 0; i < 6; i++ {
		item := &mem_storage.StoredItem{
			Value: []byte("value"),
		}
		_ = writer.Set(ctx, "key"+string(rune(i)), item)
	}

	// Wait for batch processing
	time.Sleep(100 * time.Millisecond)

	if atomic.LoadInt32(&pushed) == 0 {
		t.Error("Batch threshold did not trigger gossip push")
	}

	// Reset
	atomic.StoreInt32(&pushed, 0)

	// Trigger batch by time window
	item := &mem_storage.StoredItem{
		Value: []byte("value"),
	}
	_ = writer.Set(ctx, "key-window", item)

	// Wait for window timeout
	time.Sleep(100 * time.Millisecond)

	if atomic.LoadInt32(&pushed) == 0 {
		t.Error("Batch window did not trigger gossip push")
	}
}

func TestWriter_ConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	deps := setupWriterDeps(t)
	writer := createTestWriter(t, deps, 100, 1*time.Second) // Large threshold to avoid batching

	const numGoroutines = 10
	const opsPerGoroutine = 50

	var wg sync.WaitGroup
	errors := make(chan error, numGoroutines*opsPerGoroutine)

	// Start concurrent writers
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				key := fmt.Sprintf("concurrent-key-%d-%d", goroutineID, j)
				item := &mem_storage.StoredItem{
					Value: []byte(fmt.Sprintf("concurrent-value-%d-%d", goroutineID, j)),
				}
				if err := writer.Set(ctx, key, item); err != nil {
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
		t.Errorf("Concurrent write error: %v", err)
		errorCount++
	}

	if errorCount > 0 {
		t.Fatalf("Found %d errors in concurrent writes", errorCount)
	}

	// Verify all operations were stored
	totalExpected := numGoroutines * opsPerGoroutine
	storedCount := 0
	for i := 0; i < numGoroutines; i++ {
		for j := 0; j < opsPerGoroutine; j++ {
			key := fmt.Sprintf("concurrent-key-%d-%d", i, j)
			if got, err := deps.store.Get(key); err == nil && got != nil {
				expectedValue := fmt.Sprintf("concurrent-value-%d-%d", i, j)
				if string(got.Value) != expectedValue {
					t.Errorf("Key %s has wrong value: got %s, want %s", key, string(got.Value), expectedValue)
				}
				storedCount++
			}
		}
	}

	if storedCount != totalExpected {
		t.Errorf("Expected %d stored operations, got %d", totalExpected, storedCount)
	}
}

func TestWriter_ErrorHandling(t *testing.T) {
	deps := setupWriterDeps(t)

	// Test with nil store
	_, err := newWriter(writerConfig{
		NodeID:         "node1",
		HLC:            deps.hlcInst,
		Store:          nil, // Invalid store
		Ring:           deps.ring,
		Gossip:         deps.gossip,
		Member:         deps.member,
		Executor:       deps.exec,
		BatchThreshold: 10,
		BatchWindow:    100 * time.Millisecond,
		ReplicaCount:   3,
	})
	if err == nil {
		t.Error("Expected error with nil store, got nil")
	}

	// Test with nil HLC
	_, err = newWriter(writerConfig{
		NodeID:         "node1",
		HLC:            nil, // Invalid HLC
		Store:          deps.store,
		Ring:           deps.ring,
		Gossip:         deps.gossip,
		Member:         deps.member,
		Executor:       deps.exec,
		BatchThreshold: 10,
		BatchWindow:    100 * time.Millisecond,
		ReplicaCount:   3,
	})
	if err == nil {
		t.Error("Expected error with nil HLC, got nil")
	}

	// Test with nil executor
	_, err = newWriter(writerConfig{
		NodeID:         "node1",
		HLC:            deps.hlcInst,
		Store:          deps.store,
		Ring:           deps.ring,
		Gossip:         deps.gossip,
		Member:         deps.member,
		Executor:       nil, // Invalid executor
		BatchThreshold: 10,
		BatchWindow:    100 * time.Millisecond,
		ReplicaCount:   3,
	})
	if err == nil {
		t.Error("Expected error with nil executor, got nil")
	}
}

func TestWriter_BoundaryConditions(t *testing.T) {
	ctx := context.Background()
	deps := setupWriterDeps(t)
	writer := createTestWriter(t, deps, 10, 100*time.Millisecond)

	// Test empty key
	item := &mem_storage.StoredItem{Value: []byte("value")}
	err := writer.Set(ctx, "", item)
	if err == nil {
		t.Error("Expected error with empty key, got nil")
	}

	// Test very large value
	largeValue := make([]byte, 1024*1024) // 1MB
	for i := range largeValue {
		largeValue[i] = byte(i % 256)
	}
	largeItem := &mem_storage.StoredItem{Value: largeValue}
	err = writer.Set(ctx, "large-key", largeItem)
	if err != nil {
		t.Fatalf("Set with large value failed: %v", err)
	}

	// Verify large value was stored
	got, err := deps.store.Get("large-key")
	if err != nil {
		t.Fatalf("Get large value failed: %v", err)
	}
	if got == nil || len(got.Value) != len(largeValue) {
		t.Errorf("Large value not stored correctly: got len=%d, want len=%d", len(got.Value), len(largeValue))
	}

	// Test special characters in key
	specialKeys := []string{
		"key with spaces",
		"key-with-dashes",
		"key_with_underscores",
		"key/with/slashes",
		"key:with:colons",
		"key@with@symbols",
		"unicode-key-中文",
	}

	for _, key := range specialKeys {
		value := fmt.Sprintf("value-for-%s", key)
		item := &mem_storage.StoredItem{Value: []byte(value)}
		err := writer.Set(ctx, key, item)
		if err != nil {
			t.Errorf("Set with special key '%s' failed: %v", key, err)
		}

		got, err := deps.store.Get(key)
		if err != nil {
			t.Errorf("Get with special key '%s' failed: %v", key, err)
		} else if got == nil || string(got.Value) != value {
			t.Errorf("Special key '%s' not stored correctly", key)
		}
	}
}
