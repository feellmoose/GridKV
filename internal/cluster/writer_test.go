package cluster

import (
	"context"
	"testing"
	"time"

	"github.com/feellmoose/gridkv/internal/mem_storage"
	"github.com/feellmoose/gridkv/internal/utils/executor"
	"github.com/feellmoose/gridkv/internal/utils/hlc"
)

func TestWriter_Basic(t *testing.T) {
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
		BatchThreshold: 10,
		BatchWindow:    100 * time.Millisecond,
		ReplicaCount:   3,
	})
	if err != nil {
		t.Fatalf("newWriter() error = %v", err)
	}

	// Test Set
	item := &mem_storage.StoredItem{
		Value: []byte("value1"),
	}
	if err := writer.Set(ctx, "key1", item); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	// Verify stored
	got, err := store.Get("key1")
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
		BatchThreshold: 10,
		BatchWindow:    100 * time.Millisecond,
		ReplicaCount:   3,
	})
	if err != nil {
		t.Fatalf("newWriter() error = %v", err)
	}

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
		got, err := store.Get(key)
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
		BatchThreshold: 1, // Force immediate batching for test
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
	t.Logf("After delete: got=%v, Value=%v, Version=%d, IsTombstone=%v", got != nil, got.Value, got.Version, got.IsTombstone())
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

	var pushed bool
	gossip, _ := newGossip(gossipConfig{
		NodeID:   "node1",
		Store:    store,
		Executor: exec,
		SendFunc: func(address string, data []byte) error {
			pushed = true
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
		writer.Set(ctx, "key"+string(rune(i)), item)
	}

	// Wait for batch processing
	time.Sleep(100 * time.Millisecond)

	if !pushed {
		t.Error("Batch threshold did not trigger gossip push")
	}

	// Reset
	pushed = false

	// Trigger batch by time window
	item := &mem_storage.StoredItem{
		Value: []byte("value"),
	}
	writer.Set(ctx, "key-window", item)

	// Wait for window timeout
	time.Sleep(100 * time.Millisecond)

	if !pushed {
		t.Error("Batch window did not trigger gossip push")
	}
}
