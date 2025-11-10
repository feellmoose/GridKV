package tests

import (
	"context"
	"testing"

	"github.com/feellmoose/gridkv"
	"github.com/feellmoose/gridkv/internal/gossip"
)

// TestCoreBasicOperations tests the fundamental KV operations
func TestCoreBasicOperations(t *testing.T) {
	ctx := context.Background()

	// Setup minimal cluster
	kv, err := createTestGridKV("test-node-1", "localhost:19001", nil)
	if err != nil {
		t.Fatalf("Failed to create GridKV: %v", err)
	}
	defer kv.Close()

	t.Run("Set", func(t *testing.T) {
		err := kv.Set(ctx, "test-key", []byte("test-value"))
		if err != nil {
			t.Errorf("Set failed: %v", err)
		}
	})

	t.Run("Get", func(t *testing.T) {
		value, err := kv.Get(ctx, "test-key")
		if err != nil {
			t.Errorf("Get failed: %v", err)
		}
		if string(value) != "test-value" {
			t.Errorf("Expected 'test-value', got '%s'", string(value))
		}
	})

	t.Run("Delete", func(t *testing.T) {
		err := kv.Delete(ctx, "test-key")
		if err != nil {
			t.Errorf("Delete failed: %v", err)
		}

		_, err = kv.Get(ctx, "test-key")
		if err == nil {
			t.Error("Expected error for deleted key")
		}
	})
}

// TestCoreConsistentHash tests consistent hashing
func TestCoreConsistentHash(t *testing.T) {
	hash := gossip.NewConsistentHash(150, nil)

	// Add nodes
	nodes := []string{"node1", "node2", "node3"}
	for _, node := range nodes {
		hash.Add(node)
	}

	t.Run("GetNode", func(t *testing.T) {
		node := hash.Get("test-key")
		if node == "" {
			t.Error("Expected a node, got empty string")
		}
	})

	t.Run("GetNReplicas", func(t *testing.T) {
		replicas := hash.GetN("test-key", 3)
		if len(replicas) != 3 {
			t.Errorf("Expected 3 replicas, got %d", len(replicas))
		}

		// Should be unique
		seen := make(map[string]bool)
		for _, r := range replicas {
			if seen[r] {
				t.Errorf("Duplicate replica: %s", r)
			}
			seen[r] = true
		}
	})

	t.Run("Members", func(t *testing.T) {
		members := hash.Members()
		if len(members) != 3 {
			t.Errorf("Expected 3 members, got %d", len(members))
		}
	})
}

// TestCoreMultipleOperations tests multiple operations
func TestCoreMultipleOperations(t *testing.T) {
	ctx := context.Background()

	kv, err := createTestGridKV("test-node-multi", "localhost:19101", nil)
	if err != nil {
		t.Fatalf("Failed to create GridKV: %v", err)
	}
	defer kv.Close()

	t.Run("MultipleSetGet", func(t *testing.T) {
		// Set multiple keys
		for i := 0; i < 10; i++ {
			key := "multi-" + string(rune('0'+i))
			value := []byte("value-" + string(rune('0'+i)))

			err := kv.Set(ctx, key, value)
			if err != nil {
				t.Errorf("Set %s failed: %v", key, err)
			}
		}

		// Get all keys
		for i := 0; i < 10; i++ {
			key := "multi-" + string(rune('0'+i))
			value, err := kv.Get(ctx, key)
			if err != nil {
				t.Errorf("Get %s failed: %v", key, err)
			}
			if value == nil {
				t.Errorf("Got nil value for %s", key)
			}
		}
	})
}

// Helper function to create test GridKV instance
func createTestGridKV(nodeID, addr string, seeds []string) (*gridkv.GridKV, error) {
	return gridkv.NewGridKV(&gridkv.GridKVOptions{
		LocalNodeID:  nodeID,
		LocalAddress: addr,
		SeedAddrs:    seeds,
		ReplicaCount: 1, // Single node test
		WriteQuorum:  1,
		ReadQuorum:   1,
		Network: &gridkv.NetworkOptions{
			BindAddr: addr,
			Type:     gridkv.TCP,
		},
		Storage: &gridkv.StorageOptions{
			Backend: gridkv.BackendMemory,
		},
	})
}
