package cluster

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/feellmoose/gridkv/internal/mem_storage"
	"github.com/feellmoose/gridkv/internal/utils/executor"
	"github.com/feellmoose/gridkv/internal/utils/hlc"
)

// BenchmarkWriter_Set benchmarks single write operations
func BenchmarkWriter_Set(b *testing.B) {
	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	defer func() { _ = store.Close(context.Background()) }()

	hlcInstance := hlc.NewHLC("bench-node")
	ring := newHashRing(128)
	ring.Update(1, []string{"node-1", "node-2", "node-3"})

	executor, _ := executor.New(executor.Opts{
		Workers:   10,
		QueueSize: 1000,
		NoStats:   true,
	})
	defer executor.Stop(5 * time.Second)

	// Create minimal writer (no gossip for benchmark)
	writer := &writer{
		nodeID:         "bench-node",
		hlc:            hlcInstance,
		store:          store,
		ring:           ring,
		executor:       executor,
		batchThreshold: 100,
		batchWindow:    20 * time.Millisecond,
		replicaCount:   3,
		stopCh:         make(chan struct{}),
		lastVersions:   make(map[string]int64, 1024),               // Initialize map to prevent panic
		pendingOps:     make([]*mem_storage.SyncOperation, 0, 100), // Initialize slice
		flushTimer:     time.NewTimer(20 * time.Millisecond),       // Initialize timer
	}

	ctx := context.Background()
	item := &mem_storage.StoredItem{
		Value: make([]byte, 256),
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := int64(0)
		for pb.Next() {
			key := fmt.Sprintf("bench-key-%d", i)
			_ = writer.Set(ctx, key, item)
			i++
		}
	})
}

// BenchmarkStorage_Set benchmarks storage Set operations
func BenchmarkStorage_Set(b *testing.B) {
	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	defer func() { _ = store.Close(context.Background()) }()

	item := &mem_storage.StoredItem{
		Version: time.Now().UnixNano(),
		Value:   make([]byte, 256),
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := int64(0)
		for pb.Next() {
			key := fmt.Sprintf("bench-key-%d", i)
			_ = store.Set(key, item)
			i++
		}
	})
}

// BenchmarkStorage_Get benchmarks storage Get operations
func BenchmarkStorage_Get(b *testing.B) {
	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	defer func() { _ = store.Close(context.Background()) }()

	// Pre-populate
	item := &mem_storage.StoredItem{
		Version: time.Now().UnixNano(),
		Value:   make([]byte, 256),
	}
	for i := 0; i < 1000; i++ {
		key := "bench-key-" + string(rune(i))
		_ = store.Set(key, item)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := int64(0)
		for pb.Next() {
			key := fmt.Sprintf("bench-key-%d", i%1000)
			_, _ = store.Get(key)
			i++
		}
	})
}

// BenchmarkStorage_ConflictResolution benchmarks conflict resolution performance
func BenchmarkStorage_ConflictResolution(b *testing.B) {
	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	defer func() { _ = store.Close(context.Background()) }()

	key := "conflict-key"
	baseItem := &mem_storage.StoredItem{
		Version: time.Now().UnixNano(),
		Value:   make([]byte, 256),
	}
	_ = store.Set(key, baseItem)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := int64(0)
		for pb.Next() {
			item := &mem_storage.StoredItem{
				Version: time.Now().UnixNano() + i,
				Value:   make([]byte, 256),
			}
			_ = store.Set(key, item)
			i++
		}
	})
}

// BenchmarkGossip_ApplyOps benchmarks gossip applyOps performance
func BenchmarkGossip_ApplyOps(b *testing.B) {
	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	defer func() { _ = store.Close(context.Background()) }()

	ring := newHashRing(128)
	ring.Update(1, []string{"node-1"})

	executor, _ := executor.New(executor.Opts{
		Workers:   10,
		QueueSize: 1000,
		NoStats:   true,
	})
	defer executor.Stop(5 * time.Second)

	g := &gossip{
		nodeID:       "node-1",
		store:        store,
		ring:         ring,
		executor:     executor,
		replicaCount: 3,
	}

	// Pre-create ops
	ops := make([]*mem_storage.SyncOperation, 100)
	for i := 0; i < 100; i++ {
		ops[i] = &mem_storage.SyncOperation{
			Key:    fmt.Sprintf("bench-key-%d", i),
			OpType: mem_storage.OpSet,
			Item: &mem_storage.StoredItem{
				Version: time.Now().UnixNano() + int64(i),
				Value:   make([]byte, 256),
			},
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = g.applyOps(ops)
	}
}
