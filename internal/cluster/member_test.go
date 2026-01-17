package cluster

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/feellmoose/gridkv/internal/mem_storage"
	"github.com/feellmoose/gridkv/internal/network"
	"github.com/feellmoose/gridkv/internal/utils/cache"
	"github.com/feellmoose/gridkv/internal/utils/executor"
	"github.com/feellmoose/gridkv/internal/utils/hlc"
)

func TestMemberMgr_Basic(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	mgr, err := newMemberMgr(memberConfig{
		NodeID:         "node1",
		Address:        "localhost:8080",
		PingInterval:   100 * time.Millisecond,
		FailureTimeout: 500 * time.Millisecond,
		SuspectTimeout: 200 * time.Millisecond,
		SendFunc:       func(address string, msg interface{}) error { return nil },
	})
	if err != nil {
		t.Fatalf("newMemberMgr() error = %v", err)
	}

	// Test Members
	members := mgr.Members()
	if len(members) == 0 {
		t.Error("Members() returned empty list")
	}

	// Verify self is included
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
}

func TestMemberMgr_State(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	mgr, err := newMemberMgr(memberConfig{
		NodeID:         "node1",
		Address:        "localhost:8080",
		PingInterval:   100 * time.Millisecond,
		FailureTimeout: 500 * time.Millisecond,
		SuspectTimeout: 200 * time.Millisecond,
		SendFunc:       func(address string, msg interface{}) error { return nil },
	})
	if err != nil {
		t.Fatalf("newMemberMgr() error = %v", err)
	}

	// Test self state
	state := mgr.State("node1")
	if state != NodeStateAlive {
		t.Errorf("State(node1) = %v, want %v", state, NodeStateAlive)
	}

	// Test unknown node
	state = mgr.State("unknown")
	if state != NodeStateUnknown {
		t.Errorf("State(unknown) = %v, want %v", state, NodeStateUnknown)
	}
}

func TestMemberMgr_Join(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	mgr, err := newMemberMgr(memberConfig{
		NodeID:         "node1",
		Address:        "localhost:8080",
		PingInterval:   100 * time.Millisecond,
		FailureTimeout: 500 * time.Millisecond,
		SuspectTimeout: 200 * time.Millisecond,
		SendFunc:       func(address string, msg interface{}) error { return nil },
	})
	if err != nil {
		t.Fatalf("newMemberMgr() error = %v", err)
	}

	// Test Join with empty seed
	if err := mgr.Join([]string{}); err != nil {
		t.Errorf("Join([]) error = %v", err)
	}

	// Test Join with seed nodes
	sent := false
	mgr, err = newMemberMgr(memberConfig{
		NodeID:  "node1",
		Address: "localhost:8080",
		SendFunc: func(address string, msg interface{}) error {
			sent = true
			return nil
		},
	})
	if err != nil {
		t.Fatalf("newMemberMgr() error = %v", err)
	}

	if err := mgr.Join([]string{"localhost:8081", "localhost:8082"}); err != nil {
		t.Errorf("Join() error = %v", err)
	}

	// Give some time for async send
	time.Sleep(50 * time.Millisecond)
	if !sent {
		t.Error("Join() did not send connect messages")
	}
}

func TestMemberMgr_Leave(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	sent := false
	mgr, err := newMemberMgr(memberConfig{
		NodeID:  "node1",
		Address: "localhost:8080",
		SendFunc: func(address string, msg interface{}) error {
			sent = true
			return nil
		},
	})
	if err != nil {
		t.Fatalf("newMemberMgr() error = %v", err)
	}

	// Add a member manually for testing
	mgr.updateNode("node2", "localhost:8081", 1, NodeStateAlive)

	if err := mgr.Leave(); err != nil {
		t.Errorf("Leave() error = %v", err)
	}

	// Give some time for async send
	time.Sleep(50 * time.Millisecond)
	if !sent {
		t.Error("Leave() did not send leave messages")
	}

	// Verify self is marked as dead
	state := mgr.State("node1")
	if state != NodeStateDead {
		t.Errorf("State(node1) after Leave = %v, want %v", state, NodeStateDead)
	}
}

func TestMemberMgr_UpdateNode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	mgr, err := newMemberMgr(memberConfig{
		NodeID:   "node1",
		Address:  "localhost:8080",
		SendFunc: func(address string, msg interface{}) error { return nil },
	})
	if err != nil {
		t.Fatalf("newMemberMgr() error = %v", err)
	}

	// Add new node
	mgr.updateNode("node2", "localhost:8081", 1, NodeStateAlive)

	// Verify node is added
	state := mgr.State("node2")
	if state != NodeStateAlive {
		t.Errorf("State(node2) = %v, want %v", state, NodeStateAlive)
	}

	// Update with newer incarnation
	mgr.updateNode("node2", "localhost:8081", 2, NodeStateSuspect)

	// Verify update
	state = mgr.State("node2")
	if state != NodeStateSuspect {
		t.Errorf("State(node2) after update = %v, want %v", state, NodeStateSuspect)
	}

	// Try to update with older incarnation (should be ignored)
	mgr.updateNode("node2", "localhost:8081", 1, NodeStateAlive)

	// Verify state unchanged
	state = mgr.State("node2")
	if state != NodeStateSuspect {
		t.Errorf("State(node2) after old update = %v, want %v", state, NodeStateSuspect)
	}
}

func TestMemberMgr_ClusterSync(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	called := false
	mgr, err := newMemberMgr(memberConfig{
		NodeID:   "node1",
		Address:  "localhost:8080",
		SendFunc: func(address string, msg interface{}) error { return nil },
		OnMembershipChange: func() {
			called = true
		},
	})
	if err != nil {
		t.Fatalf("newMemberMgr() error = %v", err)
	}

	msg := &clusterSyncMsg{
		From: "node2",
		Members: []NodeInfo{
			{NodeID: "node2", Address: "localhost:8081", State: NodeStateAlive, Incarnation: 1},
			{NodeID: "node3", Address: "localhost:8082", State: NodeStateSuspect, Incarnation: 2},
		},
	}

	if err := mgr.handleClusterSync(msg); err != nil {
		t.Fatalf("handleClusterSync() error = %v", err)
	}

	if !called {
		t.Fatal("onMembershipChange not called on cluster sync")
	}

	if mgr.State("node2") != NodeStateAlive {
		t.Fatalf("node2 state = %v, want %v", mgr.State("node2"), NodeStateAlive)
	}
	if mgr.State("node3") != NodeStateSuspect {
		t.Fatalf("node3 state = %v, want %v", mgr.State("node3"), NodeStateSuspect)
	}
}

func TestMemberMgr_MarkSuspect(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	mgr, err := newMemberMgr(memberConfig{
		NodeID:         "node1",
		Address:        "localhost:8080",
		SuspectTimeout: 100 * time.Millisecond,
		FailureTimeout: 200 * time.Millisecond,
		SendFunc:       func(address string, msg interface{}) error { return nil },
	})
	if err != nil {
		t.Fatalf("newMemberMgr() error = %v", err)
	}

	// Add node
	mgr.updateNode("node2", "localhost:8081", 1, NodeStateAlive)

	// Mark as suspect
	mgr.markSuspect("node2")

	// Verify state
	state := mgr.State("node2")
	if state != NodeStateSuspect {
		t.Errorf("State(node2) = %v, want %v", state, NodeStateSuspect)
	}

	// Wait for suspect timeout
	time.Sleep(150 * time.Millisecond)

	// Should be marked as dead
	state = mgr.State("node2")
	if state != NodeStateDead {
		t.Errorf("State(node2) after timeout = %v, want %v", state, NodeStateDead)
	}
}

func TestMemberMgr_MarkDead(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	mgr, err := newMemberMgr(memberConfig{
		NodeID:   "node1",
		Address:  "localhost:8080",
		SendFunc: func(address string, msg interface{}) error { return nil },
	})
	if err != nil {
		t.Fatalf("newMemberMgr() error = %v", err)
	}

	// Add node
	mgr.updateNode("node2", "localhost:8081", 1, NodeStateAlive)

	// Mark as dead
	mgr.markDead("node2")

	// Verify state
	state := mgr.State("node2")
	if state != NodeStateDead {
		t.Errorf("State(node2) = %v, want %v", state, NodeStateDead)
	}

	// Mark dead again (should be idempotent)
	mgr.markDead("node2")
	state = mgr.State("node2")
	if state != NodeStateDead {
		t.Errorf("State(node2) after double mark = %v, want %v", state, NodeStateDead)
	}
}

// TestSWIM_FailureDetection_Protocol tests SWIM failure detection protocol mechanics
func TestSWIM_FailureDetection_Protocol(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping SWIM protocol test in short mode")
	}

	mgr, err := newMemberMgr(memberConfig{
		NodeID:         "node1",
		Address:        "localhost:8080",
		PingInterval:   50 * time.Millisecond, // Faster for testing
		FailureTimeout: 200 * time.Millisecond,
		SuspectTimeout: 100 * time.Millisecond,
		SendFunc:       func(address string, msg interface{}) error { return nil },
	})
	if err != nil {
		t.Fatalf("newMemberMgr() error = %v", err)
	}

	// Start the manager
	ctx := context.Background()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("start() error = %v", err)
	}
	defer func() { _ = mgr.Close(ctx) }()

	// Add test nodes
	mgr.updateNode("node2", "localhost:8081", 1, NodeStateAlive)
	mgr.updateNode("node3", "localhost:8082", 1, NodeStateAlive)

	// Test initial state
	if state := mgr.State("node2"); state != NodeStateAlive {
		t.Errorf("Initial state of node2 = %v, want %v", state, NodeStateAlive)
	}

	// Test suspect mechanism - simulate indirect ping failure
	mgr.markSuspect("node2")
	if state := mgr.State("node2"); state != NodeStateSuspect {
		t.Errorf("State after markSuspect = %v, want %v", state, NodeStateSuspect)
	}

	// Wait for suspect timeout to trigger dead state
	time.Sleep(150 * time.Millisecond)

	if state := mgr.State("node2"); state != NodeStateDead {
		t.Errorf("State after suspect timeout = %v, want %v", state, NodeStateDead)
	}
}

// TestSWIM_Incarnaation_PreventsStaleUpdates tests incarnation version prevents stale updates
func TestSWIM_Incarnaation_PreventsStaleUpdates(t *testing.T) {
	mgr, err := newMemberMgr(memberConfig{
		NodeID:   "node1",
		Address:  "localhost:8080",
		SendFunc: func(address string, msg interface{}) error { return nil },
	})
	if err != nil {
		t.Fatalf("newMemberMgr() error = %v", err)
	}

	// Add node with incarnation 1
	mgr.updateNode("node2", "localhost:8081", 1, NodeStateAlive)

	// Try to update with older incarnation (should be ignored)
	mgr.updateNode("node2", "localhost:8081", 0, NodeStateDead)

	// Node should still be alive (higher incarnation wins)
	if state := mgr.State("node2"); state != NodeStateAlive {
		t.Errorf("State after stale update = %v, want %v", state, NodeStateAlive)
	}

	// Update with newer incarnation (should succeed)
	mgr.updateNode("node2", "localhost:8081", 2, NodeStateSuspect)

	if state := mgr.State("node2"); state != NodeStateSuspect {
		t.Errorf("State after newer incarnation = %v, want %v", state, NodeStateSuspect)
	}
}

// TestSWIM_ConcurrentOperations tests concurrent SWIM operations
func TestSWIM_ConcurrentOperations(t *testing.T) {
	mgr, err := newMemberMgr(memberConfig{
		NodeID:   "node1",
		Address:  "localhost:8080",
		SendFunc: func(address string, msg interface{}) error { return nil },
	})
	if err != nil {
		t.Fatalf("newMemberMgr() error = %v", err)
	}

	const numGoroutines = 10
	const opsPerGoroutine = 100

	var wg sync.WaitGroup
	errors := make(chan error, numGoroutines*opsPerGoroutine)

	// Start concurrent operations
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				nodeID := fmt.Sprintf("concurrent-node-%d-%d", goroutineID, j)
				address := fmt.Sprintf("localhost:9%03d", goroutineID*opsPerGoroutine+j)

				// Mix of operations
				switch j % 4 {
				case 0:
					mgr.updateNode(nodeID, address, int64(j), NodeStateAlive)
				case 1:
					mgr.markSuspect(nodeID)
				case 2:
					mgr.markDead(nodeID)
				case 3:
					_ = mgr.State(nodeID) // Read operation
				}
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	// Check for errors
	errorCount := 0
	for err := range errors {
		t.Errorf("Concurrent operation error: %v", err)
		errorCount++
	}

	if errorCount > 0 {
		t.Fatalf("Found %d errors in concurrent operations", errorCount)
	}
}

// TestSWIM_PingAck_Reliability tests ping/ack reliability using real network
// This is an integration test that should use gridkv.GridKV interface, not cluster.Cluster directly
// However, this test is in internal/cluster package and tests cluster internals
// For true integration tests using gridkv interface, see tests/ directory
func TestSWIM_PingAck_Reliability(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping ping/ack reliability test in short mode")
	}

	// This test is kept here for unit testing cluster internals
	// For integration tests that don't couple to implementation, see tests/ directory
	// Use real network with dynamic ports
	getFreePort := func() (int, error) {
		addr, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		defer addr.Close()
		return addr.Addr().(*net.TCPAddr).Port, nil
	}

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
	store1, _ := mem_storage.New(mem_storage.DefaultConfig())
	hlc1 := hlc.NewHLC("node1")
	exec1, _ := executor.New(executor.Opts{Name: "test1", Workers: 4})
	cache1 := cache.New(cache.Opts{Shards: 16, Size: 1000})

	store2, _ := mem_storage.New(mem_storage.DefaultConfig())
	hlc2 := hlc.NewHLC("node2")
	exec2, _ := executor.New(executor.Opts{Name: "test2", Workers: 4})
	cache2 := cache.New(cache.Opts{Shards: 16, Size: 1000})

	cluster1 := createTestClusterWithNetworkForMember(t, store1, hlc1, exec1, cache1, "node1", addr1, nil)
	cluster2 := createTestClusterWithNetworkForMember(t, store2, hlc2, exec2, cache2, "node2", addr2, nil)

	ctx := context.Background()
	if err := cluster1.Start(ctx); err != nil {
		t.Fatalf("Failed to start cluster1: %v", err)
	}
	defer func() { _ = cluster1.Stop(ctx) }()

	if err := cluster2.Start(ctx); err != nil {
		t.Fatalf("Failed to start cluster2: %v", err)
	}
	defer func() { _ = cluster2.Stop(ctx) }()

	// Join cluster2 to cluster1 after both are started
	if err := cluster2.Join([]string{addr1}); err != nil {
		t.Logf("Warning: Failed to join cluster2 to cluster1: %v", err)
	}

	// Wait for SWIM ping/ack cycles
	time.Sleep(1 * time.Second)

	// Verify both nodes see each other as healthy
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

	if healthyCount1 < 2 {
		t.Errorf("Node1 should see at least 2 healthy nodes (including itself), got %d", healthyCount1)
	}
	if healthyCount2 < 2 {
		t.Errorf("Node2 should see at least 2 healthy nodes (including itself), got %d", healthyCount2)
	}

	t.Logf("SWIM ping/ack test: node1 healthy=%d, node2 healthy=%d", healthyCount1, healthyCount2)
}

// createTestClusterWithNetworkForMember creates a cluster with real network (for member_test.go)
func createTestClusterWithNetworkForMember(t *testing.T, store *mem_storage.MemStorage, hlcInst *hlc.HLC, exec *executor.Exec, cacheInst *cache.Cache, nodeID string, address string, seedAddrs []string) *Cluster {
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

	// Create cluster with real network (network will be started by lifecycle manager in cluster.Start())
	cluster, err := New(Config{
		NodeID:                    nodeID,
		Address:                   address,
		Store:                     store,
		HLC:                       hlcInst,
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

	// Join seed addresses if provided
	if len(seedAddrs) > 0 {
		if err := cluster.Join(seedAddrs); err != nil {
			t.Logf("Warning: Failed to join seed addresses for %s: %v", nodeID, err)
		}
	}

	return cluster
}

// BenchmarkSWIM_StateTransitions benchmarks SWIM state transition performance
func BenchmarkSWIM_StateTransitions(b *testing.B) {
	mgr, _ := newMemberMgr(memberConfig{
		NodeID:   "bench-node",
		Address:  "localhost:8080",
		SendFunc: func(address string, msg interface{}) error { return nil },
	})

	// Pre-populate nodes
	for i := 0; i < 100; i++ {
		nodeID := fmt.Sprintf("bench-node-%d", i)
		address := fmt.Sprintf("localhost:8%03d", i)
		mgr.updateNode(nodeID, address, 1, NodeStateAlive)
	}

	b.ResetTimer()

	b.Run("StateTransitions", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				nodeID := fmt.Sprintf("bench-node-%d", i%100)
				switch i % 3 {
				case 0:
					mgr.markSuspect(nodeID)
				case 1:
					mgr.markDead(nodeID)
				case 2:
					mgr.updateNode(nodeID, fmt.Sprintf("localhost:8%03d", i%100), int64(i), NodeStateAlive)
				}
				i++
			}
		})
	})
}
