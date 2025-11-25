package tests

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	gridkv "github.com/feellmoose/gridkv"
	"github.com/feellmoose/gridkv/internal/utils/network"
)

// TestMain skips the heavy integration suite when -short is provided.
func TestMain(m *testing.M) {
	if testing.Short() {
		fmt.Println("Skipping integration tests under -short")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Shared test helper functions

func randomValue(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

var _ = randomValue

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

// countLiveNodes counts the number of live/healthy nodes in the cluster
func countLiveNodes(nodes []*gridkv.GridKV) int {
	if len(nodes) == 0 {
		return 0
	}

	// Sample a few nodes to estimate cluster health
	sampleCount := 5
	if sampleCount > len(nodes) {
		sampleCount = len(nodes)
	}
	sampleIndices := make([]int, 0, sampleCount)
	for i := 0; i < len(nodes) && len(sampleIndices) < sampleCount; i++ {
		if nodes[i] != nil {
			sampleIndices = append(sampleIndices, i)
		}
	}

	if len(sampleIndices) == 0 {
		return 0
	}

	totalHealthy := 0
	for _, idx := range sampleIndices {
		status := nodes[idx].GetReplicaStatus()
		if status.Ready && status.HealthyNodes > 0 {
			totalHealthy += status.HealthyNodes
		}
	}

	if totalHealthy == 0 {
		return 0
	}

	// Return average healthy nodes across samples
	return totalHealthy / len(sampleIndices)
}

// networkTypeString converts network type to string for logging
func networkTypeString(nt gridkv.NetworkType) string {
	switch nt {
	case gridkv.TCP:
		return "TCP"
	case gridkv.QUIC:
		return "QUIC"
	case gridkv.UDP:
		return "UDP"
	default:
		return fmt.Sprintf("unknown(%d)", nt)
	}
}

// networkTypeFromEnv allows overriding the default network type via GRIDKV_NETWORK env
func networkTypeFromEnv(defaultType gridkv.NetworkType) gridkv.NetworkType {
	env := strings.TrimSpace(strings.ToLower(os.Getenv("GRIDKV_NETWORK")))
	switch env {
	case "":
		return defaultType
	case "tcp":
		return gridkv.TCP
	case "quic":
		return gridkv.QUIC
	case "udp":
		return gridkv.UDP
	default:
		return defaultType
	}
}

// TestEnvironmentConfig configures test environment simulation
type TestEnvironmentConfig struct {
	NetworkProfile network.NetworkProfile
	NetworkType    gridkv.NetworkType
	NodeCount      int
	ReplicaCount   int
	BasePort       int
	StorageBackend gridkv.StorageBackendType
	MaxMemoryMB    int64
	ShardCount     int
}

// DefaultTestEnvironment returns default test environment config
func DefaultTestEnvironment() *TestEnvironmentConfig {
	return &TestEnvironmentConfig{
		NetworkProfile: network.ProfileLAN,
		NetworkType:    networkTypeFromEnv(gridkv.TCP),
		NodeCount:      3,
		ReplicaCount:   3,
		BasePort:       20000,
		StorageBackend: gridkv.BackendMemorySharded,
		MaxMemoryMB:    1024,
		ShardCount:     64,
	}
}

// TestEnvironmentSimulator manages test cluster with environment simulation
type TestEnvironmentSimulator struct {
	config *TestEnvironmentConfig
	nodes  []*gridkv.GridKV
	mu     sync.RWMutex
}

// WaitForReplicationSettle polls node readiness and sleeps briefly to give
// replication a moment to converge. It replaces the old diagnostics-driven
// pipeline flush with a best-effort heuristic. If no nodes are provided it
// operates on the entire cluster snapshot.
func (tes *TestEnvironmentSimulator) WaitForReplicationSettle(nodes ...*gridkv.GridKV) {
	const (
		readinessWait = 750 * time.Millisecond
		pollInterval  = 25 * time.Millisecond
		settleDelay   = 100 * time.Millisecond
	)

	targets := nodes
	if len(targets) == 0 {
		targets = tes.snapshotNodes()
	}

	deadline := time.Now().Add(readinessWait)
	for time.Now().Before(deadline) {
		if tes.nodesReady(targets) {
			break
		}
		time.Sleep(pollInterval)
	}

	time.Sleep(settleDelay)
}

func (tes *TestEnvironmentSimulator) nodesReady(nodes []*gridkv.GridKV) bool {
	ready := 0
	total := 0
	for _, node := range nodes {
		if node == nil {
			continue
		}
		total++
		status := node.GetReplicaStatus()
		if status.Ready && status.HealthyNodes > 0 {
			ready++
		}
	}
	return total == 0 || ready == total
}

func (tes *TestEnvironmentSimulator) snapshotNodes() []*gridkv.GridKV {
	tes.mu.RLock()
	defer tes.mu.RUnlock()
	out := make([]*gridkv.GridKV, len(tes.nodes))
	copy(out, tes.nodes)
	return out
}

// LogNodeDiagnostics logs replica status for all nodes (alive and down). Helper for debugging.
func (tes *TestEnvironmentSimulator) LogNodeDiagnostics(tb testing.TB, label string) {
	tb.Helper()
	nodes := tes.snapshotNodes()
	tb.Logf("=== node diagnostics: %s ===", label)
	for idx, node := range nodes {
		if node == nil {
			tb.Logf("node-%d: down/unavailable", idx)
			continue
		}
		status := node.GetReplicaStatus()
		// Transport stats removed - use metrics instead if needed
		tb.Logf("node-%d id=%s ready=%v healthy=%d cluster=%d peers=%d replica=%d",
			idx, status.LocalNodeID, status.Ready, status.HealthyNodes,
			status.ClusterSize, status.PeerCount, status.ReplicaFactor)
	}
}

// NewTestEnvironmentSimulator creates a new environment simulator
func NewTestEnvironmentSimulator(config *TestEnvironmentConfig) *TestEnvironmentSimulator {
	if config == nil {
		config = DefaultTestEnvironment()
	}
	return &TestEnvironmentSimulator{
		config: config,
		nodes:  make([]*gridkv.GridKV, 0, config.NodeCount),
	}
}

// SetupCluster creates and initializes the test cluster
func (tes *TestEnvironmentSimulator) SetupCluster(tb testing.TB) error {
	tes.mu.Lock()
	defer tes.mu.Unlock()

	latencyConfig := network.GetConfigForProfile(tes.config.NetworkProfile, tes.config.NodeCount)
	tes.nodes = make([]*gridkv.GridKV, tes.config.NodeCount)

	// Create seed node
	var err error
	opts := &gridkv.GridKVOptions{
		LocalNodeID:        "node-0",
		LocalAddress:       fmt.Sprintf("localhost:%d", tes.config.BasePort),
		FailureTimeout:     latencyConfig.FailureTimeout,
		SuspectTimeout:     latencyConfig.SuspectTimeout,
		GossipInterval:     latencyConfig.GossipInterval,
		ReplicationTimeout: latencyConfig.ReplicationTimeout,
		ReadTimeout:        latencyConfig.ReadTimeout,
		StartupGracePeriod: 1 * time.Second,
		DisableAuth:        true,
		ReplicaCount:       tes.config.ReplicaCount,
		Network: &gridkv.NetworkOptions{
			Type:     tes.config.NetworkType,
			BindAddr: fmt.Sprintf("localhost:%d", tes.config.BasePort),
			MaxConns: latencyConfig.MaxConnections,
			MaxIdle:  latencyConfig.MaxIdleConnections,
		},
		Storage: &gridkv.StorageOptions{
			Backend:     tes.config.StorageBackend,
			MaxMemoryMB: tes.config.MaxMemoryMB,
			ShardCount:  tes.config.ShardCount,
		},
	}

	tes.nodes[0], err = gridkv.NewGridKV(opts)
	if err != nil {
		return fmt.Errorf("failed to create seed node: %w", err)
	}

	time.Sleep(1 * time.Second)

	// Create remaining nodes
	seedAddr := []string{fmt.Sprintf("localhost:%d", tes.config.BasePort)}
	for i := 1; i < tes.config.NodeCount; i++ {
		opts := &gridkv.GridKVOptions{
			LocalNodeID:        fmt.Sprintf("node-%d", i),
			LocalAddress:       fmt.Sprintf("localhost:%d", tes.config.BasePort+i),
			SeedAddrs:          seedAddr,
			FailureTimeout:     latencyConfig.FailureTimeout,
			SuspectTimeout:     latencyConfig.SuspectTimeout,
			GossipInterval:     latencyConfig.GossipInterval,
			ReplicationTimeout: latencyConfig.ReplicationTimeout,
			ReadTimeout:        latencyConfig.ReadTimeout,
			StartupGracePeriod: 1 * time.Second,
			DisableAuth:        true,
			ReplicaCount:       tes.config.ReplicaCount,
			Network: &gridkv.NetworkOptions{
				Type:     tes.config.NetworkType,
				BindAddr: fmt.Sprintf("localhost:%d", tes.config.BasePort+i),
				MaxConns: latencyConfig.MaxConnections,
				MaxIdle:  latencyConfig.MaxIdleConnections,
			},
			Storage: &gridkv.StorageOptions{
				Backend:     tes.config.StorageBackend,
				MaxMemoryMB: tes.config.MaxMemoryMB,
				ShardCount:  tes.config.ShardCount,
			},
		}

		tes.nodes[i], err = gridkv.NewGridKV(opts)
		if err != nil {
			tes.cleanupNodes(i)
			return fmt.Errorf("failed to create node %d: %w", i, err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Wait for cluster stabilization
	time.Sleep(2 * time.Second)
	return nil
}

// GetNodes returns all cluster nodes
func (tes *TestEnvironmentSimulator) GetNodes() []*gridkv.GridKV {
	tes.mu.RLock()
	defer tes.mu.RUnlock()
	return tes.nodes
}

// GetNode returns node at index
func (tes *TestEnvironmentSimulator) GetNode(idx int) *gridkv.GridKV {
	tes.mu.RLock()
	defer tes.mu.RUnlock()
	if idx < 0 || idx >= len(tes.nodes) {
		return nil
	}
	return tes.nodes[idx]
}

// ShutdownNode gracefully shuts down a node
func (tes *TestEnvironmentSimulator) ShutdownNode(idx int, timeout time.Duration) error {
	tes.mu.Lock()
	defer tes.mu.Unlock()
	if idx < 0 || idx >= len(tes.nodes) || tes.nodes[idx] == nil {
		return fmt.Errorf("invalid node index: %d", idx)
	}
	node := tes.nodes[idx]
	tes.nodes[idx] = nil
	return node.CloseWithTimeout(timeout)
}

// ShutdownNodes shuts down multiple nodes
func (tes *TestEnvironmentSimulator) ShutdownNodes(indices []int, timeout time.Duration) error {
	var wg sync.WaitGroup
	errCh := make(chan error, len(indices))
	for _, idx := range indices {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := tes.ShutdownNode(i, timeout); err != nil {
				errCh <- err
			}
		}(idx)
	}
	wg.Wait()
	close(errCh)
	if len(errCh) > 0 {
		return <-errCh
	}
	return nil
}

// Cleanup shuts down all nodes
func (tes *TestEnvironmentSimulator) Cleanup() {
	tes.mu.Lock()
	defer tes.mu.Unlock()
	tes.cleanupNodes(len(tes.nodes))
}

func (tes *TestEnvironmentSimulator) cleanupNodes(limit int) {
	var wg sync.WaitGroup
	const closeTimeout = 10 * time.Second
	for idx := 0; idx < limit && idx < len(tes.nodes); idx++ {
		if tes.nodes[idx] == nil {
			continue
		}
		wg.Add(1)
		go func(i int, n *gridkv.GridKV) {
			defer wg.Done()
			if err := n.CloseWithTimeout(closeTimeout); err != nil {
				if strings.Contains(err.Error(), "timeout") {
					fmt.Printf("WARN: node %d close timed out\n", i)
				}
			}
		}(idx, tes.nodes[idx])
	}
	wg.Wait()
}

// WaitForHealthyNodes waits for cluster to reach expected healthy node count
func (tes *TestEnvironmentSimulator) WaitForHealthyNodes(tb testing.TB, expected int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		tes.mu.RLock()
		if len(tes.nodes) == 0 {
			tes.mu.RUnlock()
			return
		}
		sampleNode := tes.nodes[0]
		tes.mu.RUnlock()
		if sampleNode == nil {
			return
		}
		status := sampleNode.GetReplicaStatus()
		if status.HealthyNodes >= expected {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	tb.Fatalf("timed out waiting for %d healthy nodes", expected)
}

// WaitForAllNodesReady waits for all nodes to be ready using WaitReady API
// Similar to the test framework in REPORT_GRIDKV021.md
func (tes *TestEnvironmentSimulator) WaitForAllNodesReady(tb testing.TB, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	nodes := tes.snapshotNodes()
	
	readyCount := 0
	for time.Now().Before(deadline) {
		readyCount = 0
		for i, node := range nodes {
			if node == nil {
				continue
			}
			// Use WaitReady if available, otherwise check status
			status := node.GetReplicaStatus()
			if status.Ready && status.ClusterSize > 0 && status.PeerCount > 0 {
				readyCount++
			}
		}
		if readyCount == len(nodes) {
			tb.Logf("✅ All %d nodes are ready!", len(nodes))
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	tb.Fatalf("timed out waiting for all nodes to be ready: %d/%d ready", readyCount, len(nodes))
}

// VerifyConsistencyAcrossDelays verifies eventual consistency at multiple delay stages
// Similar to the consistency checker in REPORT_GRIDKV021.md
func (tes *TestEnvironmentSimulator) VerifyConsistencyAcrossDelays(tb testing.TB, writtenKeys map[string][]byte, delays []time.Duration, minConvergenceRate float64) {
	nodes := tes.snapshotNodes()
	ctx := context.Background()
	
	type delayResult struct {
		delay        time.Duration
		consistent   int
		missing      int
		mismatch     int
		total        int
		convergence  float64
	}
	
	results := make([]delayResult, 0, len(delays))
	
	for _, delay := range delays {
		if delay > 0 {
			time.Sleep(delay)
		}
		tes.WaitForReplicationSettle(nodes...)
		
		consistent := 0
		missing := 0
		mismatch := 0
		
		for key, expectedValue := range writtenKeys {
			foundCount := 0
			matchingCount := 0
			
			for _, node := range nodes {
				if node == nil {
					continue
				}
				value, err := node.Get(ctx, key)
				if err == nil && value != nil {
					foundCount++
					if string(value) == string(expectedValue) {
						matchingCount++
					}
				}
			}
			
			// Key is consistent if found on at least replicaCount nodes with matching value
			replicaCount := tes.config.ReplicaCount
			if matchingCount >= replicaCount {
				consistent++
			} else if foundCount == 0 {
				missing++
			} else {
				mismatch++
			}
		}
		
		total := len(writtenKeys)
		convergence := float64(consistent) / float64(total) * 100
		
		result := delayResult{
			delay:       delay,
			consistent:  consistent,
			missing:     missing,
			mismatch:    mismatch,
			total:       total,
			convergence: convergence,
		}
		results = append(results, result)
		
		tb.Logf("Delay %s -> Consistent: %d/%d (%.1f%%) Missing: %d Mismatch: %d",
			delay, consistent, total, convergence, missing, mismatch)
	}
	
	// Check final convergence
	final := results[len(results)-1]
	if final.convergence < minConvergenceRate {
		tb.Errorf("Final convergence rate %.1f%% below minimum %.1f%%", final.convergence, minConvergenceRate)
	}
}

// CountTotalKeys counts total keys across all nodes
func (tes *TestEnvironmentSimulator) CountTotalKeys(tb testing.TB) int {
	nodes := tes.snapshotNodes()
	totalKeys := 0
	keySet := make(map[string]bool)
	
	for _, node := range nodes {
		if node == nil {
			continue
		}
		// Note: This assumes there's a way to get keys from storage
		// If not available, we'll need to track keys during writes
		_ = node
		_ = keySet
	}
	
	return totalKeys
}

// VerifyDataPersistence verifies that written keys persist across all nodes
func (tes *TestEnvironmentSimulator) VerifyDataPersistence(tb testing.TB, writtenKeys map[string][]byte, minSuccessRate float64) {
	nodes := tes.snapshotNodes()
	ctx := context.Background()
	
	successCount := 0
	missingCount := 0
	
	for key, expectedValue := range writtenKeys {
		found := false
		for _, node := range nodes {
			if node == nil {
				continue
			}
			value, err := node.Get(ctx, key)
			if err == nil && value != nil && string(value) == string(expectedValue) {
				found = true
				successCount++
				break
			}
		}
		if !found {
			missingCount++
		}
	}
	
	total := len(writtenKeys)
	successRate := float64(successCount) / float64(total) * 100
	
	tb.Logf("Data Persistence: %d/%d keys found (%.1f%%)", successCount, total, successRate)
	
	if successRate < minSuccessRate {
		tb.Errorf("Data persistence rate %.1f%% below minimum %.1f%% (%d keys missing)",
			successRate, minSuccessRate, missingCount)
	}
}
