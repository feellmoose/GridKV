package tests

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	gridkv "github.com/feellmoose/gridkv"
)

// Shared test helper functions

func randomValue(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func calculatePercentiles(latencies []time.Duration) (p50, p95, p99 time.Duration) {
	n := len(latencies)
	if n == 0 {
		return 0, 0, 0
	}

	sort.Slice(latencies, func(i, j int) bool {
		return latencies[i] < latencies[j]
	})

	p50 = latencies[n*50/100]
	p95 = latencies[n*95/100]
	p99 = latencies[n*99/100]
	return
}

func setupTestCluster(tb testing.TB, nodeCount, basePort int) []*gridkv.GridKV {
	nodes := make([]*gridkv.GridKV, nodeCount)

	// Create seed node
	var err error
	nodes[0], err = gridkv.NewGridKV(&gridkv.GridKVOptions{
		LocalNodeID:        "node-0",
		LocalAddress:       fmt.Sprintf("localhost:%d", basePort),
		FailureTimeout:     2 * time.Second,
		SuspectTimeout:     4 * time.Second,
		GossipInterval:     200 * time.Millisecond,
		StartupGracePeriod: 1 * time.Second,
		Network: &gridkv.NetworkOptions{
			Type:     gridkv.TCP,
			BindAddr: fmt.Sprintf("localhost:%d", basePort),
		},
		Storage: &gridkv.StorageOptions{
			Backend:     gridkv.BackendMemory,
			MaxMemoryMB: 512,
		},
		ReplicaCount: minInt(3, nodeCount),
		WriteQuorum:  minInt(2, nodeCount),
		ReadQuorum:   minInt(2, nodeCount),
	})
	if err != nil {
		tb.Fatalf("Failed to create seed node: %v", err)
	}

	// Create remaining nodes
	seedAddr := []string{fmt.Sprintf("localhost:%d", basePort)}
	for i := 1; i < nodeCount; i++ {
		nodes[i], err = gridkv.NewGridKV(&gridkv.GridKVOptions{
			LocalNodeID:        fmt.Sprintf("node-%d", i),
			LocalAddress:       fmt.Sprintf("localhost:%d", basePort+i),
			SeedAddrs:          seedAddr,
			FailureTimeout:     2 * time.Second,
			SuspectTimeout:     4 * time.Second,
			GossipInterval:     200 * time.Millisecond,
			StartupGracePeriod: 1 * time.Second,
			Network: &gridkv.NetworkOptions{
				Type:     gridkv.TCP,
				BindAddr: fmt.Sprintf("localhost:%d", basePort+i),
			},
			Storage: &gridkv.StorageOptions{
				Backend:     gridkv.BackendMemory,
				MaxMemoryMB: 512,
			},
			ReplicaCount: minInt(3, nodeCount),
			WriteQuorum:  minInt(2, nodeCount),
			ReadQuorum:   minInt(2, nodeCount),
		})
		if err != nil {
			tb.Fatalf("Failed to create node %d: %v", i, err)
		}
	}

	return nodes
}

func cleanupTestCluster(nodes []*gridkv.GridKV) {
	for _, node := range nodes {
		if node != nil {
			node.Close()
		}
	}
}

func waitForHealthyNodes(tb testing.TB, node *gridkv.GridKV, expected int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status := node.GetReplicaStatus()
		if status.HealthyNodes == expected {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	status := node.GetReplicaStatus()
	tb.Fatalf("timed out waiting for %d healthy nodes (have %d)", expected, status.HealthyNodes)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
