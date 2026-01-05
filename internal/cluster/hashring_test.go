package cluster

import (
	"testing"
)

func TestHashRing_Basic(t *testing.T) {
	ring := newHashRing(128)

	// Test empty ring
	node := ring.Get("key1")
	if node != "" {
		t.Errorf("Get() on empty ring = %v, want empty", node)
	}

	// Test Update
	nodes := []string{"node1", "node2", "node3"}
	if !ring.Update(1, nodes) {
		t.Error("Update() returned false")
	}

	// Test Get
	node = ring.Get("key1")
	if node == "" {
		t.Error("Get() returned empty after Update")
	}

	// Verify node is in the list
	found := false
	for _, n := range nodes {
		if n == node {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Get() returned %v, not in nodes %v", node, nodes)
	}
}

func TestHashRing_GetN(t *testing.T) {
	ring := newHashRing(128)

	nodes := []string{"node1", "node2", "node3", "node4", "node5"}
	if !ring.Update(1, nodes) {
		t.Fatal("Update() failed")
	}

	// Test GetN
	replicas := ring.GetN("key1", 3)
	if len(replicas) != 3 {
		t.Errorf("GetN() returned %d nodes, want 3", len(replicas))
	}

	// Verify no duplicates
	seen := make(map[string]bool)
	for _, n := range replicas {
		if seen[n] {
			t.Errorf("GetN() returned duplicate node: %v", n)
		}
		seen[n] = true
	}

	// Test GetN with n > available nodes
	replicas = ring.GetN("key1", 10)
	if len(replicas) > len(nodes) {
		t.Errorf("GetN() returned %d nodes, want <= %d", len(replicas), len(nodes))
	}
}

func TestHashRing_Version(t *testing.T) {
	ring := newHashRing(128)

	// Initial version should be 0
	version := ring.Version()
	if version != 0 {
		t.Errorf("Initial Version() = %v, want 0", version)
	}

	// Update should increment version
	if !ring.Update(1, []string{"node1"}) {
		t.Fatal("Update() failed")
	}

	version = ring.Version()
	if version != 1 {
		t.Errorf("Version() after Update = %v, want 1", version)
	}
}

func TestHashRing_UpdateRejectOldVersion(t *testing.T) {
	ring := newHashRing(128)

	// Update to version 5
	if !ring.Update(5, []string{"node1"}) {
		t.Fatal("Update() failed")
	}

	// Try to update with older version (should fail)
	if ring.Update(3, []string{"node2"}) {
		t.Error("Update() with older version should return false")
	}

	// Version should still be 5
	version := ring.Version()
	if version != 5 {
		t.Errorf("Version() = %v, want 5", version)
	}

	// Node should still be node1
	node := ring.Get("key1")
	if node != "node1" {
		t.Errorf("Get() = %v, want node1", node)
	}
}

func TestHashRing_ConsistentHashing(t *testing.T) {
	ring := newHashRing(128)

	nodes := []string{"node1", "node2", "node3"}
	if !ring.Update(1, nodes) {
		t.Fatal("Update() failed")
	}

	// Same key should map to same node
	node1 := ring.Get("key1")
	node2 := ring.Get("key1")
	if node1 != node2 {
		t.Errorf("Get() inconsistent: first = %v, second = %v", node1, node2)
	}
}

func TestHashRing_EmptyNodes(t *testing.T) {
	ring := newHashRing(128)

	// Update with empty nodes (clears ring, valid operation)
	if !ring.Update(1, []string{}) {
		t.Error("Update() with empty nodes should return true (clears ring)")
	}

	// Verify ring is empty
	if ring.Get("test-key") != "" {
		t.Error("Get() should return empty string after clearing ring")
	}
}

func TestHashRing_AddRemoveNodes(t *testing.T) {
	ring := newHashRing(128)

	// Initial nodes
	if !ring.Update(1, []string{"node1", "node2"}) {
		t.Fatal("Update() failed")
	}

	// Add node
	if !ring.Update(2, []string{"node1", "node2", "node3"}) {
		t.Fatal("Update() failed")
	}

	// Verify new node can be returned
	nodes := ring.GetN("key1", 3)
	if len(nodes) < 2 {
		t.Errorf("GetN() after add = %v, want at least 2 nodes", nodes)
	}

	// Remove node
	if !ring.Update(3, []string{"node1", "node2"}) {
		t.Fatal("Update() failed")
	}

	// Verify removed node not returned
	nodes = ring.GetN("key1", 3)
	for _, n := range nodes {
		if n == "node3" {
			t.Error("GetN() returned removed node node3")
		}
	}
}

