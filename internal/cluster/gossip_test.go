package cluster

import (
	"bytes"
	"testing"
	"time"

	"github.com/feellmoose/gridkv/internal/mem_storage"
	"github.com/feellmoose/gridkv/internal/utils/executor"
)

// testGossipDeps holds common test dependencies for gossip tests
type testGossipDeps struct {
	store  *mem_storage.MemStorage
	member MemberMgr
	ring   HashRing
	exec   *executor.Exec
}

// setupGossipDeps creates common test dependencies
func setupGossipDeps(t *testing.T) *testGossipDeps {
	t.Helper()

	store, err := mem_storage.New(mem_storage.DefaultConfig())
	if err != nil {
		t.Fatalf("Failed to create mem storage: %v", err)
	}

	member, err := newMemberMgr(memberConfig{
		NodeID:         "node-local",
		Address:        "localhost:8080",
		PingInterval:   100 * time.Millisecond,
		FailureTimeout: 1 * time.Second,
		SuspectTimeout: 500 * time.Millisecond,
		SendFunc:       noOpSendFunc,
	})
	if err != nil {
		t.Fatalf("Failed to create member: %v", err)
	}

	ring := newHashRing(128)
	ring.Update(1, []string{"node-local"})

	exec, err := executor.New(executor.Opts{Name: "test", Workers: 2})
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	return &testGossipDeps{
		store:  store,
		member: member,
		ring:   ring,
		exec:   exec,
	}
}

// createTestGossip creates a gossip instance with test configuration
func createTestGossip(t *testing.T, deps *testGossipDeps, replicaCount int) *gossip {
	t.Helper()

	g, err := newGossip(gossipConfig{
		NodeID:       "node-local",
		Store:        deps.store,
		Member:       deps.member,
		Ring:         deps.ring,
		Executor:     deps.exec,
		ReplicaCount: replicaCount,
	})
	if err != nil {
		t.Fatalf("Failed to create gossip: %v", err)
	}
	return g
}

func TestGossip_HandlePullAppliesRemoteAndResponds(t *testing.T) {
	deps := setupGossipDeps(t)
	if mgr, ok := deps.member.(*memberMgr); ok {
		mgr.updateNode("remote", "localhost:8081", 1, NodeStateAlive)
	}

	g := createTestGossip(t, deps, 1)

	// Prepare remote ops to pull
	remoteOp := &mem_storage.SyncOperation{
		Key: "k1",
		Item: &mem_storage.StoredItem{
			Key:     "k1",
			Value:   []byte("v1"),
			Version: 100,
		},
	}
	data, err := SerializeSyncOps([]*mem_storage.SyncOperation{remoteOp})
	if err != nil {
		t.Fatalf("serializeOps() error = %v", err)
	}

	var sent [][]byte
	g.sendFunc = func(address string, data []byte) error {
		sent = append(sent, data)
		return nil
	}

	msg := append([]byte("PULL:remote:"), data...)
	if err := g.HandleMessage(msg); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	// Remote op should be applied locally
	item, err := deps.store.Get("k1")
	if err != nil || item == nil || string(item.Value) != "v1" {
		t.Fatalf("expected applied op, got item=%v err=%v", item, err)
	}

	// Pull response is now sent asynchronously via executor
	// Wait for executor to process the async task
	time.Sleep(100 * time.Millisecond)

	// Should respond with PUSH message
	if len(sent) == 0 {
		t.Fatalf("expected response push, got none")
	}
	if !bytes.HasPrefix(sent[0], []byte("PUSH:")) {
		t.Fatalf("expected PUSH response, got %s", string(sent[0]))
	}
}
