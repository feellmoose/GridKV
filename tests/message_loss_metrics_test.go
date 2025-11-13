package tests

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/feellmoose/gridkv/internal/gossip"
	"github.com/feellmoose/gridkv/internal/utils/logging"
)

// TestMessageLossProbability measures the drop probability when the gossip queue is saturated.
func TestMessageLossProbability(t *testing.T) {
	logging.Log = logging.NewLogger(&logging.LogOptions{Level: "error", Format: "text"})

	kv, err := createTestGridKV("loss-node", "localhost:36010", nil)
	if err != nil {
		t.Fatalf("Failed to create GridKV: %v", err)
	}
	defer kv.Close()

	// Stop gossip processing so the queue remains saturated and messages drop after capacity.
	kv.StopGossip()

	totalMessages := 2048
	for i := 0; i < totalMessages; i++ {
		msg := &gossip.GossipMessage{Type: gossip.GossipMessageType_MESSAGE_TYPE_CACHE_SYNC}
		kv.InjectGossipMessage(msg)
	}

	total, dropped := kv.MessageStats()
	if total == 0 {
		t.Fatalf("Expected messages to be recorded")
	}

	// Expected drop probability equals messages beyond buffer size.
	expectedDrop := float64(totalMessages-1024) / float64(totalMessages)
	actualDrop := float64(dropped) / float64(total)
	if math.Abs(actualDrop-expectedDrop) > 0.05 {
		t.Errorf("Drop probability mismatch: expected %.2f, got %.2f (total=%d dropped=%d)", expectedDrop, actualDrop, total, dropped)
	}

	t.Logf("Message stats: total=%d dropped=%d drop_probability=%.3f", total, dropped, actualDrop)
}

// TestMessageLossProbabilityUnderLoad ensures drop probability remains near zero during normal operation.
func TestMessageLossProbabilityUnderLoad(t *testing.T) {
	kv, err := createTestGridKV("loss-node-2", "localhost:36020", nil)
	if err != nil {
		t.Fatalf("Failed to create GridKV: %v", err)
	}
	defer kv.Close()

	// Allow gossip manager to run normally.
	time.Sleep(500 * time.Millisecond)

	// Perform a burst of Set operations (will generate gossip traffic).
	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("loss-key-%03d", i)
		if err := kv.Set(context.Background(), key, []byte("value")); err != nil {
			t.Fatalf("Set failed: %v", err)
		}
	}

	// Allow processing to catch up.
	time.Sleep(1 * time.Second)

	total, dropped := kv.MessageStats()
	if total == 0 {
		t.Fatalf("No messages recorded during load test")
	}

	dropProbability := float64(dropped) / float64(total)
	if dropProbability > 0.01 {
		t.Errorf("Drop probability too high under normal load: %.3f", dropProbability)
	}

	t.Logf("Under load: total=%d dropped=%d drop_probability=%.4f", total, dropped, dropProbability)
}
