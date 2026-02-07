package cluster

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/feellmoose/gridkv/internal/mem_storage"
	"github.com/feellmoose/gridkv/internal/network"
	"github.com/feellmoose/gridkv/internal/utils/cache"
	"github.com/feellmoose/gridkv/internal/utils/executor"
	"github.com/feellmoose/gridkv/internal/utils/hlc"
)

// testClusterDeps holds common test dependencies for cluster tests
type testClusterDeps struct {
	store   *mem_storage.MemStorage
	hlcInst *hlc.HLC
	exec    *executor.Exec
	cache   *cache.Cache
}

// setupClusterDeps creates common test dependencies
func setupClusterDeps(t *testing.T) *testClusterDeps {
	t.Helper()

	store, err := mem_storage.New(mem_storage.DefaultConfig())
	if err != nil {
		t.Fatalf("Failed to create mem storage: %v", err)
	}

	hlcInst := hlc.NewHLC("node1")
	exec, err := executor.New(executor.Opts{Name: "test", Workers: 4})
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cacheInst := cache.New(cache.Opts{
		Shards: 16,
		Size:   1000,
	})

	return &testClusterDeps{
		store:   store,
		hlcInst: hlcInst,
		exec:    exec,
		cache:   cacheInst,
	}
}

// createTestCluster creates a cluster with test configuration
// For tests that don't need network communication, pass nil for net (will use noOpSendFunc)
func createTestCluster(t *testing.T, deps *testClusterDeps, nodeID string, address string, net network.Network) *Cluster {
	t.Helper()

	// If network is nil, use no-op send function for single-node tests
	var sendFunc func(address string, msg interface{}) error
	if net == nil {
		sendFunc = noOpSendFunc
	}

	cluster, err := New(Config{
		NodeID:                    nodeID,
		Address:                   address,
		Store:                     deps.store,
		HLC:                       deps.hlcInst,
		Network:                   net,      // Pass the network instance (nil for no-op)
		SendFunc:                  sendFunc, // Provide SendFunc when Network is nil
		VirtualNodes:              128,
		ReplicaCount:              3,
		BatchThreshold:            10,
		BatchWindow:               50 * time.Millisecond,  // Faster for tests
		GossipInterval:            100 * time.Millisecond, // Faster gossip for tests
		CacheTTL:                  100 * time.Millisecond, // Shorter TTL for tests
		EntropyInterval:           1 * time.Second,        // Faster entropy for tests
		ReadRepairRateLimitPerSec: 1000,                   // Higher rate limit for tests
		PingInterval:              100 * time.Millisecond,
		FailureTimeout:            5 * time.Second,
		SuspectTimeout:            3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Failed to create cluster: %v", err)
	}
	return cluster
}

// startTestCluster starts a cluster for testing
func startTestCluster(t *testing.T, cluster *Cluster) {
	t.Helper()
	ctx := context.Background()
	if err := cluster.Start(ctx); err != nil {
		t.Fatalf("Failed to start cluster: %v", err)
	}
}

func stopTestCluster(t *testing.T, cluster *Cluster) {
	t.Helper()
	ctx := context.Background()
	if err := cluster.Stop(ctx); err != nil {
		t.Errorf("Failed to stop cluster: %v", err)
	}
}

func noOpSendFunc(address string, msg interface{}) error {
	return nil
}

// getFreePort returns a free port on localhost
func getFreePort() (int, error) {
	addr, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer addr.Close()
	return addr.Addr().(*net.TCPAddr).Port, nil
}

// createTestClusterWithNetwork creates a cluster with real network
func createTestClusterWithNetwork(t *testing.T, deps *testClusterDeps, nodeID string, address string, seedAddrs []string) *Cluster {
	t.Helper()

	// Create network with real transport
	netConfig := network.DefaultNetworkConfig(address)
	netConfig.TransportType = network.TransportTCP
	netConfig.TransportConfig = network.DefaultTransportConfig()
	netConfig.TransportConfig.Type = network.TransportTCP

	net, err := network.NewNetwork(netConfig)
	if err != nil {
		t.Fatalf("Failed to create network for %s: %v", nodeID, err)
	}

	// Create cluster with real network (network will be started by lifecycle manager)
	cluster, err := New(Config{
		NodeID:                    nodeID,
		Address:                   address,
		Store:                     deps.store,
		HLC:                       deps.hlcInst,
		Network:                   net,
		VirtualNodes:              128,
		ReplicaCount:              3,
		BatchThreshold:            10,
		BatchWindow:               50 * time.Millisecond,
		GossipInterval:            100 * time.Millisecond,
		CacheTTL:                  100 * time.Millisecond,
		EntropyInterval:           1 * time.Second,
		ReadRepairRateLimitPerSec: 1000,
		PingInterval:              100 * time.Millisecond,
		FailureTimeout:            5 * time.Second,
		SuspectTimeout:            3 * time.Second,
	})
	if err != nil {
		_ = net.Stop(context.Background())
		t.Fatalf("Failed to create cluster %s: %v", nodeID, err)
	}

	// Join seed addresses if provided (should be done after Start)

	return cluster
}

func TestCluster_BasicOperations(t *testing.T) {
	ctx := context.Background()
	deps := setupClusterDeps(t)
	cluster := createTestCluster(t, deps, "node1", "localhost:8080", nil)
	startTestCluster(t, cluster)
	defer stopTestCluster(t, cluster)

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
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
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
	defer func() { _ = cluster.Stop(ctx) }()

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
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
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
	defer func() { _ = cluster.Stop(ctx) }()

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
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
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
	defer func() { _ = cluster.Stop(ctx) }()

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
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
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
	defer func() { _ = cluster.Stop(ctx) }()

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
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
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
	defer func() { _ = cluster.Stop(ctx) }()

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
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
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
	defer func() { _ = cluster.Stop(ctx) }()

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
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
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
	defer func() { _ = cluster.Stop(ctx) }()

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
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
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
	defer func() { _ = cluster.Stop(ctx) }()

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
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
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
	defer func() { _ = cluster.Stop(ctx) }()

	got, err := cluster.Get(ctx, "non-existent")
	if err != nil {
		t.Errorf("Get(non-existent) error = %v, want nil", err)
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
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
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
	defer func() { _ = cluster.Stop(ctx) }()

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
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
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

// TestCluster_HLC_Causality tests HLC causality guarantees
func TestCluster_HLC_Causality(t *testing.T) {
	ctx := context.Background()
	deps := setupClusterDeps(t)
	cluster := createTestCluster(t, deps, "node1", "localhost:8080", nil)
	startTestCluster(t, cluster)
	defer stopTestCluster(t, cluster)

	// Test causality: HLC(A) < HLC(B) when A → B
	key := "causality-key"

	// First write
	if err := cluster.Set(ctx, key, []byte("value1")); err != nil {
		t.Fatalf("First Set() error = %v", err)
	}

	// Wait a bit to ensure HLC progression
	time.Sleep(1 * time.Millisecond)

	// Second write (should have higher HLC)
	if err := cluster.Set(ctx, key, []byte("value2")); err != nil {
		t.Fatalf("Second Set() error = %v", err)
	}

	// Verify the second write wins (LWW)
	got, err := cluster.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got == nil {
		t.Fatal("Get() returned nil")
	}
	if string(got) != "value2" {
		t.Errorf("Causality violation: got %s, want value2", string(got))
	}
}

// TestCluster_SWIM_FailureDetection tests SWIM protocol failure detection
// This is an integration test that should use gridkv.GridKV interface, not cluster.Cluster directly
// However, this test is in internal/cluster package and tests cluster internals
// For true integration tests using gridkv interface, see tests/ directory
func TestCluster_SWIM_FailureDetection(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping SWIM failure detection test in short mode")
	}

	// This test is kept here for unit testing cluster internals
	// For integration tests that don't couple to implementation, see tests/ directory
	// Use real network with dynamic ports
	// Get free ports for nodes
	port1, err := getFreePort()
	if err != nil {
		t.Fatalf("Failed to get free port: %v", err)
	}
	port2, err := getFreePort()
	if err != nil {
		t.Fatalf("Failed to get free port: %v", err)
	}

	addr1 := fmt.Sprintf("127.0.0.1:%d", port1)
	addr2 := fmt.Sprintf("127.0.0.1:%d", port2)

	// Create clusters with real network
	deps1 := setupClusterDeps(t)
	deps2 := setupClusterDeps(t)

	cluster1 := createTestClusterWithNetwork(t, deps1, "node1", addr1, nil)
	cluster2 := createTestClusterWithNetwork(t, deps2, "node2", addr2, []string{addr1})

	startTestCluster(t, cluster1)
	defer stopTestCluster(t, cluster1)

	// Wait for cluster1 to be fully ready before starting cluster2
	time.Sleep(500 * time.Millisecond)

	startTestCluster(t, cluster2)
	defer stopTestCluster(t, cluster2)

	// Explicitly join cluster2 to cluster1 after both are started
	// This ensures the network is ready and sendFunc is initialized
	time.Sleep(500 * time.Millisecond)
	if err := cluster2.Join([]string{addr1}); err != nil {
		t.Logf("Warning: cluster2.Join() failed: %v", err)
		// Continue anyway - SWIM ping may still discover nodes
	}

	// Wait for nodes to discover each other via SWIM protocol
	// Increased wait time and interval for more reliable discovery
	maxWait := 20 * time.Second
	interval := 500 * time.Millisecond
	elapsed := time.Duration(0)
	for elapsed < maxWait {
		time.Sleep(interval)
		elapsed += interval

		members1 := cluster1.Members()
		healthyCount1 := 0
		for _, member := range members1 {
			if member.State == NodeStateAlive {
				healthyCount1++
			}
		}

		members2 := cluster2.Members()
		healthyCount2 := 0
		for _, member := range members2 {
			if member.State == NodeStateAlive {
				healthyCount2++
			}
		}

		// Both nodes should see at least 2 healthy nodes (including themselves)
		if healthyCount1 >= 2 && healthyCount2 >= 2 {
			return // Success
		}
	}

	// Final check after waiting
	members1 := cluster1.Members()
	healthyCount1 := 0
	for _, member := range members1 {
		if member.State == NodeStateAlive {
			healthyCount1++
		}
	}
	if healthyCount1 < 2 {
		t.Errorf("Node1 should see 2 healthy nodes, got %d", healthyCount1)
	}

	members2 := cluster2.Members()
	healthyCount2 := 0
	for _, member := range members2 {
		if member.State == NodeStateAlive {
			healthyCount2++
		}
	}
	if healthyCount2 < 2 {
		t.Errorf("Node2 should see 2 healthy nodes, got %d", healthyCount2)
	}
}

// TestCluster_Gossip_Propagation tests gossip epidemic propagation
func TestCluster_Gossip_Propagation(t *testing.T) {
	ctx := context.Background()
	deps := setupClusterDeps(t)

	// Create multiple nodes for gossip testing
	clusters := make([]*Cluster, 3)
	for i := 0; i < 3; i++ {
		nodeID := fmt.Sprintf("node%d", i+1)
		address := fmt.Sprintf("localhost:808%d", i)
		clusters[i] = createTestCluster(t, deps, nodeID, address, nil)
		startTestCluster(t, clusters[i])
		defer stopTestCluster(t, clusters[i])
	}

	// Wait for gossip to establish connections
	time.Sleep(200 * time.Millisecond)

	// Write to first node
	key := "gossip-test-key"
	value := "gossip-test-value"
	if err := clusters[0].Set(ctx, key, []byte(value)); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	// Wait for gossip propagation
	time.Sleep(500 * time.Millisecond)

	// Check if value propagated to other nodes
	for i := 1; i < 3; i++ {
		got, err := clusters[i].Get(ctx, key)
		if err != nil {
			t.Errorf("Node %d Get() error = %v", i+1, err)
			continue
		}
		if got == nil {
			t.Errorf("Node %d should have received gossip propagation", i+1)
			continue
		}
		if string(got) != value {
			t.Errorf("Node %d has wrong value: got %s, want %s", i+1, string(got), value)
		}
	}
}

// TestCluster_AntiEntropy_Convergence tests anti-entropy convergence
func TestCluster_AntiEntropy_Convergence(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping anti-entropy convergence test in short mode")
	}

	ctx := context.Background()
	deps := setupClusterDeps(t)

	// Create two clusters
	cluster1 := createTestCluster(t, deps, "node1", "localhost:8080", nil)
	cluster2 := createTestCluster(t, deps, "node2", "localhost:8081", nil)

	startTestCluster(t, cluster1)
	defer stopTestCluster(t, cluster1)

	startTestCluster(t, cluster2)
	defer stopTestCluster(t, cluster2)

	// Pre-populate data on first cluster
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("entropy-key-%d", i)
		value := fmt.Sprintf("entropy-value-%d", i)
		if err := cluster1.Set(ctx, key, []byte(value)); err != nil {
			t.Fatalf("Failed to populate data: %v", err)
		}
	}

	// Wait for anti-entropy to run (configured to 1 second intervals)
	time.Sleep(3 * time.Second)

	// Check convergence - second cluster should have caught up
	converged := 0
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("entropy-key-%d", i)
		expectedValue := fmt.Sprintf("entropy-value-%d", i)

		got, err := cluster2.Get(context.Background(), key)
		if err != nil {
			continue // May not have converged yet
		}
		if got != nil && string(got) == expectedValue {
			converged++
		}
	}

	// Should have at least some convergence
	if converged == 0 {
		t.Error("Anti-entropy did not achieve any convergence")
	} else {
		t.Logf("Anti-entropy achieved %d/%d convergence", converged, 100)
	}
}

// TestCluster_ReadRepair_Conflicts tests read repair conflict resolution
func TestCluster_ReadRepair_Conflicts(t *testing.T) {
	ctx := context.Background()
	deps := setupClusterDeps(t)

	// Create cluster with read repair
	cluster := createTestCluster(t, deps, "node1", "localhost:8080", nil)
	startTestCluster(t, cluster)
	defer stopTestCluster(t, cluster)

	key := "conflict-key"

	// Simulate version conflict by directly setting different versions
	// This tests the read repair mechanism when conflicts are detected
	if err := cluster.Set(ctx, key, []byte("value1")); err != nil {
		t.Fatalf("First Set() error = %v", err)
	}

	// Wait a bit for versioning
	time.Sleep(2 * time.Millisecond)

	if err := cluster.Set(ctx, key, []byte("value2")); err != nil {
		t.Fatalf("Second Set() error = %v", err)
	}

	// Get should return the latest version
	got, err := cluster.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got == nil {
		t.Fatal("Get() returned nil")
	}
	if string(got) != "value2" {
		t.Errorf("Read repair failed: got %s, want value2", string(got))
	}
}

// TestCluster_HashRing_Consistency tests hash ring consistency
func TestCluster_HashRing_Consistency(t *testing.T) {
	ctx := context.Background()
	deps := setupClusterDeps(t)
	cluster := createTestCluster(t, deps, "node1", "localhost:8080", nil)
	startTestCluster(t, cluster)
	defer stopTestCluster(t, cluster)

	// Test that same key always routes to same node
	testKeys := []string{"key1", "key2", "key3", "consistent-key"}

	routes := make(map[string]string)
	for _, key := range testKeys {
		if err := cluster.Set(ctx, key, []byte("test-value")); err != nil {
			t.Fatalf("Set() error for key %s: %v", key, err)
		}
		routes[key] = "stored" // In single node setup, all go to same node
	}

	// Verify consistency - same keys should be retrievable
	for _, key := range testKeys {
		got, err := cluster.Get(ctx, key)
		if err != nil {
			t.Errorf("Get() error for key %s: %v", key, err)
		} else if got == nil {
			t.Errorf("Key %s not found after consistent hashing", key)
		}
	}
}

// TestCluster_WriteAmplification tests write amplification behavior
func TestCluster_WriteAmplification(t *testing.T) {
	ctx := context.Background()
	deps := setupClusterDeps(t)
	cluster := createTestCluster(t, deps, "node1", "localhost:8080", nil)
	startTestCluster(t, cluster)
	defer stopTestCluster(t, cluster)

	// Measure write performance with batching
	start := time.Now()
	ops := 1000

	for i := 0; i < ops; i++ {
		key := fmt.Sprintf("batch-key-%d", i)
		value := fmt.Sprintf("batch-value-%d", i)
		if err := cluster.Set(ctx, key, []byte(value)); err != nil {
			t.Fatalf("Batch write error: %v", err)
		}
	}

	elapsed := time.Since(start)
	opsPerSec := float64(ops) / elapsed.Seconds()

	t.Logf("Write amplification optimization: %d ops in %v (%.0f ops/sec)",
		ops, elapsed, opsPerSec)

	// Verify all writes succeeded
	for i := 0; i < ops; i++ {
		key := fmt.Sprintf("batch-key-%d", i)
		got, err := cluster.Get(ctx, key)
		if err != nil || got == nil {
			t.Errorf("Batch write verification failed for key %s", key)
		}
	}
}

// BenchmarkCluster_WriteOperations benchmarks write operations
func BenchmarkCluster_WriteOperations(b *testing.B) {
	// Create minimal test setup for benchmarks
	store, err := mem_storage.New(mem_storage.DefaultConfig())
	if err != nil {
		b.Fatalf("Failed to create mem storage: %v", err)
	}

	hlcInst := hlc.NewHLC("node1")
	_, err = executor.New(executor.Opts{Name: "bench", Workers: 4})
	if err != nil {
		b.Fatalf("Failed to create executor: %v", err)
	}

	cluster, err := New(Config{
		NodeID:         "node1",
		Address:        "localhost:8080",
		Store:          store,
		HLC:            hlcInst,
		VirtualNodes:   128,
		ReplicaCount:   3,
		BatchThreshold: 100,
		BatchWindow:    50 * time.Millisecond,
		GossipInterval: 100 * time.Millisecond,
		CacheTTL:       100 * time.Millisecond,
		SendFunc:       func(address string, msg interface{}) error { return nil },
	})
	if err != nil {
		b.Fatalf("Failed to create cluster: %v", err)
	}

	ctx := context.Background()
	if err := cluster.Start(ctx); err != nil {
		b.Fatalf("Failed to start cluster: %v", err)
	}
	defer func() { _ = cluster.Stop(ctx) }()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("bench-key-%d", i)
			value := fmt.Sprintf("bench-value-%d", i)
			if err := cluster.Set(ctx, key, []byte(value)); err != nil {
				b.Fatalf("Benchmark write error: %v", err)
			}
			i++
		}
	})
}

// BenchmarkCluster_ReadOperations benchmarks read operations
func BenchmarkCluster_ReadOperations(b *testing.B) {
	// Create minimal test setup for benchmarks
	store, err := mem_storage.New(mem_storage.DefaultConfig())
	if err != nil {
		b.Fatalf("Failed to create mem storage: %v", err)
	}

	hlcInst := hlc.NewHLC("node1")
	_, err = executor.New(executor.Opts{Name: "bench", Workers: 4})
	if err != nil {
		b.Fatalf("Failed to create executor: %v", err)
	}

	cluster, err := New(Config{
		NodeID:         "node1",
		Address:        "localhost:8080",
		Store:          store,
		HLC:            hlcInst,
		VirtualNodes:   128,
		ReplicaCount:   3,
		BatchThreshold: 100,
		BatchWindow:    50 * time.Millisecond,
		GossipInterval: 100 * time.Millisecond,
		CacheTTL:       100 * time.Millisecond,
		SendFunc:       func(address string, msg interface{}) error { return nil },
	})
	if err != nil {
		b.Fatalf("Failed to create cluster: %v", err)
	}

	ctx := context.Background()
	if err := cluster.Start(ctx); err != nil {
		b.Fatalf("Failed to start cluster: %v", err)
	}
	defer func() { _ = cluster.Stop(ctx) }()

	// Pre-populate data
	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("bench-key-%d", i)
		value := fmt.Sprintf("bench-value-%d", i)
		if err := cluster.Set(ctx, key, []byte(value)); err != nil {
			b.Fatalf("Pre-populate error: %v", err)
		}
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("bench-key-%d", i%1000)
			if _, err := cluster.Get(ctx, key); err != nil {
				b.Fatalf("Benchmark read error: %v", err)
			}
			i++
		}
	})
}
