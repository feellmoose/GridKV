package cluster

import (
	"bytes"
	"testing"
	"time"

	"github.com/feellmoose/gridkv/internal/mem_storage"
)

func TestGossip_HandlePullAppliesRemoteAndResponds(t *testing.T) {
	store, _ := mem_storage.New(mem_storage.DefaultConfig())

	// Create member manager for address resolution
	member, _ := newMemberMgr(memberConfig{
		NodeID:         "node-local",
		Address:        "localhost:8080",
		PingInterval:   100 * time.Millisecond,
		FailureTimeout: 1 * time.Second,
		SuspectTimeout: 500 * time.Millisecond,
		SendFunc:       noOpSendFunc,
	})
	// Add remote node to member list
	member.updateNode("remote", "localhost:8081", 1, NodeStateAlive)

	// Create hash ring and add local node as replica
	ring := newHashRing(128)
	ring.Update(1, []string{"node-local"})

	g, err := newGossip(gossipConfig{
		NodeID:       "node-local",
		Store:        store,
		Member:       member,
		Ring:         ring,
		ReplicaCount: 1,
	})
	if err != nil {
		t.Fatalf("newGossip() error = %v", err)
	}

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
	item, err := store.Get("k1")
	if err != nil || item == nil || string(item.Value) != "v1" {
		t.Fatalf("expected applied op, got item=%v err=%v", item, err)
	}

	// Should respond with PUSH message
	if len(sent) == 0 {
		t.Fatalf("expected response push, got none")
	}
	if !bytes.HasPrefix(sent[0], []byte("PUSH:")) {
		t.Fatalf("expected PUSH response, got %s", string(sent[0]))
	}
}
