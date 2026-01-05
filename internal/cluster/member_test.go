package cluster

import (
	"testing"
	"time"
)

func TestMemberMgr_Basic(t *testing.T) {
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
