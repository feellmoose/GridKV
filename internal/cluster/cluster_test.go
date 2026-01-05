package cluster

import (
	"context"
	"testing"
	"time"

	"github.com/feellmoose/gridkv/internal/mem_storage"
	"github.com/feellmoose/gridkv/internal/utils/hlc"
)

// noOpSendFunc is a no-op send function for single-node tests
func noOpSendFunc(address string, msg interface{}) error {
	return nil
}

func TestCluster_BasicOperations(t *testing.T) {
	ctx := context.Background()

	// Create cluster
	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	hlcInst := hlc.NewHLC("node1")

	cluster, err := New(Config{
		NodeID:                    "node1",
		Address:                   "localhost:8080",
		Store:                     store,
		HLC:                       hlcInst,
		VirtualNodes:              128,
		ReplicaCount:              3,
		BatchThreshold:            10,
		BatchWindow:               100 * time.Millisecond,
		GossipInterval:            200 * time.Millisecond,
		CacheTTL:                  10 * time.Millisecond,
		EntropyInterval:           5 * time.Minute,
		ReadRepairRateLimitPerSec: 100,
		SendFunc:                  noOpSendFunc,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Start cluster
	if err := cluster.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer cluster.Stop(ctx)

	// Test Set
	key := "test-key"
	value := []byte("test-value")
	if err := cluster.Set(ctx, key, value); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	// Test Get
	got, err := cluster.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if string(got) != string(value) {
		t.Errorf("Get() = %v, want %v", string(got), string(value))
	}

	// Test Delete
	if err := cluster.Delete(ctx, key); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	// Verify deleted
	got, err = cluster.Get(ctx, key)
	if err == nil && got != nil {
		t.Errorf("Get() after delete = %v, want nil", got)
	}
}

func TestCluster_MemberManagement(t *testing.T) {
	ctx := context.Background()

	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	hlcInst := hlc.NewHLC("node1")

	cluster, err := New(Config{
		NodeID:       "node1",
		Address:      "localhost:8080",
		Store:        store,
		HLC:          hlcInst,
		VirtualNodes: 128,
		ReplicaCount: 3,
		SendFunc:     noOpSendFunc,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := cluster.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer cluster.Stop(ctx)

	// Test Members
	members := cluster.Members()
	if len(members) == 0 {
		t.Error("Members() returned empty list")
	}

	// Verify self is in members
	found := false
	for _, m := range members {
		if m.NodeID == "node1" {
			found = true
			if m.State != NodeStateAlive {
				t.Errorf("Self state = %v, want %v", m.State, NodeStateAlive)
			}
			break
		}
	}
	if !found {
		t.Error("Self not found in members")
	}

	// Test MemberMgr
	mgr := cluster.MemberMgr()
	if mgr == nil {
		t.Fatal("MemberMgr() returned nil")
	}

	state := mgr.State("node1")
	if state != NodeStateAlive {
		t.Errorf("State(node1) = %v, want %v", state, NodeStateAlive)
	}
}

func TestCluster_HashRing(t *testing.T) {
	ctx := context.Background()

	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	hlcInst := hlc.NewHLC("node1")

	cluster, err := New(Config{
		NodeID:       "node1",
		Address:      "localhost:8080",
		Store:        store,
		HLC:          hlcInst,
		VirtualNodes: 128,
		ReplicaCount: 3,
		SendFunc:     noOpSendFunc,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := cluster.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer cluster.Stop(ctx)

	ring := cluster.HashRing()
	if ring == nil {
		t.Fatal("HashRing() returned nil")
	}

	// Test Get
	node := ring.Get("test-key")
	if node == "" {
		t.Error("Get() returned empty string")
	}

	// Test GetN
	nodes := ring.GetN("test-key", 3)
	if len(nodes) == 0 {
		t.Error("GetN() returned empty list")
	}

	// Test Version
	version := ring.Version()
	if version < 0 {
		t.Errorf("Version() = %v, want >= 0", version)
	}
}

func TestCluster_BatchOperations(t *testing.T) {
	ctx := context.Background()

	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	hlcInst := hlc.NewHLC("node1")

	cluster, err := New(Config{
		NodeID:         "node1",
		Address:        "localhost:8080",
		Store:          store,
		HLC:            hlcInst,
		BatchThreshold: 5,
		BatchWindow:    50 * time.Millisecond,
		SendFunc:       noOpSendFunc,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := cluster.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer cluster.Stop(ctx)

	// Test batch Set
	writer := cluster.Writer()
	if writer == nil {
		t.Fatal("Writer() returned nil")
	}

	items := make(map[string]*mem_storage.StoredItem)
	for i := 0; i < 10; i++ {
		key := "batch-key-" + string(rune(i))
		items[key] = &mem_storage.StoredItem{
			Value: []byte("value-" + string(rune(i))),
		}
	}

	if err := writer.BatchSet(ctx, items); err != nil {
		t.Fatalf("BatchSet() error = %v", err)
	}

	// Wait for batch processing
	time.Sleep(100 * time.Millisecond)

	// Verify batch Get
	reader := cluster.Reader()
	if reader == nil {
		t.Fatal("Reader() returned nil")
	}

	keys := make([]string, 0, len(items))
	for k := range items {
		keys = append(keys, k)
	}

	results, err := reader.BatchGet(ctx, keys)
	if err != nil {
		t.Fatalf("BatchGet() error = %v", err)
	}

	if len(results) != len(items) {
		t.Errorf("BatchGet() returned %d items, want %d", len(results), len(items))
	}
}

func TestCluster_ReadRepair(t *testing.T) {
	ctx := context.Background()

	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	hlcInst := hlc.NewHLC("node1")

	cluster, err := New(Config{
		NodeID:                    "node1",
		Address:                   "localhost:8080",
		Store:                     store,
		HLC:                       hlcInst,
		ReadRepairRateLimitPerSec: 100,
		SendFunc:                  noOpSendFunc,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := cluster.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer cluster.Stop(ctx)

	reader := cluster.Reader()
	if reader == nil {
		t.Fatal("Reader() returned nil")
	}

	// Test speculative read
	key := "repair-key"
	item, err := reader.GetSpeculative(ctx, key, 3)
	if err != nil {
		t.Fatalf("GetSpeculative() error = %v", err)
	}

	// Item may be nil if not found, which is OK
	_ = item
}

func TestCluster_ConcurrentWrites(t *testing.T) {
	ctx := context.Background()

	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	hlcInst := hlc.NewHLC("node1")

	cluster, err := New(Config{
		NodeID:         "node1",
		Address:        "localhost:8080",
		Store:          store,
		HLC:            hlcInst,
		BatchThreshold: 10,
		BatchWindow:    50 * time.Millisecond,
		SendFunc:       noOpSendFunc,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := cluster.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer cluster.Stop(ctx)

	// Concurrent writes
	done := make(chan error, 10)
	for i := 0; i < 10; i++ {
		i := i
		go func() {
			key := "concurrent-key-" + string(rune(i))
			value := []byte("value-" + string(rune(i)))
			done <- cluster.Set(ctx, key, value)
		}()
	}

	// Wait for all writes
	for i := 0; i < 10; i++ {
		if err := <-done; err != nil {
			t.Errorf("Concurrent Set() error = %v", err)
		}
	}

	// Wait for batch processing
	time.Sleep(100 * time.Millisecond)

	// Verify all writes
	for i := 0; i < 10; i++ {
		key := "concurrent-key-" + string(rune(i))
		got, err := cluster.Get(ctx, key)
		if err != nil {
			t.Errorf("Get(%s) error = %v", key, err)
			continue
		}
		if got == nil {
			t.Errorf("Get(%s) returned nil", key)
		}
	}
}

func TestCluster_JoinLeave(t *testing.T) {
	ctx := context.Background()

	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	hlcInst := hlc.NewHLC("node1")

	cluster, err := New(Config{
		NodeID:   "node1",
		Address:  "localhost:8080",
		Store:    store,
		HLC:      hlcInst,
		SendFunc: noOpSendFunc,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := cluster.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer cluster.Stop(ctx)

	// Test Join (with empty seed, should not error)
	if err := cluster.Join([]string{}); err != nil {
		t.Errorf("Join([]) error = %v", err)
	}

	// Test Leave
	if err := cluster.Leave(); err != nil {
		t.Errorf("Leave() error = %v", err)
	}
}

func TestCluster_WriteReadFlow(t *testing.T) {
	ctx := context.Background()

	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	hlcInst := hlc.NewHLC("node1")

	cluster, err := New(Config{
		NodeID:         "node1",
		Address:        "localhost:8080",
		Store:          store,
		HLC:            hlcInst,
		BatchThreshold: 5,
		BatchWindow:    50 * time.Millisecond,
		CacheTTL:       10 * time.Millisecond,
		SendFunc:       noOpSendFunc,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := cluster.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer cluster.Stop(ctx)

	// Write sequence
	keys := []string{"key1", "key2", "key3", "key4", "key5"}
	for i, key := range keys {
		value := []byte("value-" + string(rune(i)))
		if err := cluster.Set(ctx, key, value); err != nil {
			t.Fatalf("Set(%s) error = %v", key, err)
		}
	}

	// Wait for batch processing
	time.Sleep(100 * time.Millisecond)

	// Read sequence
	for i, key := range keys {
		got, err := cluster.Get(ctx, key)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", key, err)
		}
		expected := "value-" + string(rune(i))
		if string(got) != expected {
			t.Errorf("Get(%s) = %v, want %v", key, string(got), expected)
		}
	}
}

func TestCluster_UpdateValue(t *testing.T) {
	ctx := context.Background()

	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	hlcInst := hlc.NewHLC("node1")

	cluster, err := New(Config{
		NodeID:   "node1",
		Address:  "localhost:8080",
		Store:    store,
		HLC:      hlcInst,
		SendFunc: noOpSendFunc,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := cluster.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer cluster.Stop(ctx)

	key := "update-key"

	// Initial value
	if err := cluster.Set(ctx, key, []byte("value1")); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	// Update value
	if err := cluster.Set(ctx, key, []byte("value2")); err != nil {
		t.Fatalf("Set() update error = %v", err)
	}

	// Verify update
	got, err := cluster.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if string(got) != "value2" {
		t.Errorf("Get() = %v, want value2", string(got))
	}
}

func TestCluster_EmptyOperations(t *testing.T) {
	ctx := context.Background()

	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	hlcInst := hlc.NewHLC("node1")

	cluster, err := New(Config{
		NodeID:   "node1",
		Address:  "localhost:8080",
		Store:    store,
		HLC:      hlcInst,
		SendFunc: noOpSendFunc,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := cluster.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer cluster.Stop(ctx)

	// Get non-existent key
	got, err := cluster.Get(ctx, "non-existent")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got != nil {
		t.Errorf("Get(non-existent) = %v, want nil", got)
	}

	// Delete non-existent key (should not error)
	if err := cluster.Delete(ctx, "non-existent"); err != nil {
		t.Errorf("Delete(non-existent) error = %v", err)
	}
}

func TestCluster_ComponentAccess(t *testing.T) {
	ctx := context.Background()

	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	hlcInst := hlc.NewHLC("node1")

	cluster, err := New(Config{
		NodeID:   "node1",
		Address:  "localhost:8080",
		Store:    store,
		HLC:      hlcInst,
		SendFunc: noOpSendFunc,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := cluster.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer cluster.Stop(ctx)

	// Test component accessors
	if cluster.MemberMgr() == nil {
		t.Error("MemberMgr() returned nil")
	}

	if cluster.HashRing() == nil {
		t.Error("HashRing() returned nil")
	}

	if cluster.Writer() == nil {
		t.Error("Writer() returned nil")
	}

	if cluster.Reader() == nil {
		t.Error("Reader() returned nil")
	}
}

func TestCluster_Lifecycle(t *testing.T) {
	ctx := context.Background()

	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	hlcInst := hlc.NewHLC("node1")

	cluster, err := New(Config{
		NodeID:   "node1",
		Address:  "localhost:8080",
		Store:    store,
		HLC:      hlcInst,
		SendFunc: noOpSendFunc,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Test Start
	if err := cluster.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	// Test Stop
	if err := cluster.Stop(ctx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	// Test double Stop (should not error)
	if err := cluster.Stop(ctx); err != nil {
		t.Errorf("Double Stop() error = %v", err)
	}
}
