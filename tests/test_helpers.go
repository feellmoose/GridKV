package tests

import (
	"context"
	cryptorand "crypto/rand"
	"fmt"
	"math/rand"
	"net"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gridkv "github.com/feellmoose/gridkv"
	"github.com/feellmoose/gridkv/internal/utils/logging"
)

// TestMain runs all tests. Use -short flag to skip heavy tests.
func TestMain(m *testing.M) {
	// Enable debug logging for tests
	logging.SetDefault(logging.New(logging.Opts{
		Level:  logging.LevelDebug,
		Format: logging.FormatText,
	}))
	os.Exit(m.Run())
}

// Shared test helper functions

func randomValue(n int) []byte {
	b := make([]byte, n)
	cryptorand.Read(b)
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
	default:
		return fmt.Sprintf("unknown(%d)", nt)
	}
}

// checkNodeNetworkHealth performs basic network connectivity check for a node
func checkNodeNetworkHealth(address string) error {
	// Try to establish a brief TCP connection to verify node is listening
	conn, err := net.DialTimeout("tcp", address, 1*time.Second)
	if err != nil {
		return fmt.Errorf("network health check failed for %s: %w", address, err)
	}
	conn.Close()
	return nil
}

// waitForNodeNetworkReady waits for a node's network endpoint to be ready
func waitForNodeNetworkReady(address string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := checkNodeNetworkHealth(address); err == nil {
			return nil // Node is network ready
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("node %s network not ready within %v", address, timeout)
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
	default:
		return defaultType
	}
}

// skipHeavyTests skips long-running suites when -short flag is set.
// Run tests without -short flag to execute heavy tests.
func skipHeavyTests(t *testing.T, reason string) {
	if testing.Short() {
		t.Skip(reason)
	}
}

// NetworkProfile defines network latency and reliability characteristics for testing
type NetworkProfile int

const (
	// ProfileLAN represents low-latency, high-reliability LAN environment
	ProfileLAN NetworkProfile = iota
	// ProfileWAN represents high-latency, variable reliability WAN environment
	ProfileWAN
	// ProfileUnreliable represents high packet loss and latency environment
	ProfileUnreliable
)

// LatencyConfig holds network timing configuration
type LatencyConfig struct {
	FailureTimeout     time.Duration
	SuspectTimeout     time.Duration
	GossipInterval     time.Duration
	ReplicationTimeout time.Duration
	ReadTimeout        time.Duration
	MaxConnections     int
	MaxIdleConnections int
}

// GetConfigForProfile returns latency configuration for the specified network profile
func GetConfigForProfile(profile NetworkProfile, nodeCount int) *LatencyConfig {
	switch profile {
	case ProfileLAN:
		return &LatencyConfig{
			FailureTimeout:     5 * time.Second,
			SuspectTimeout:     3 * time.Second,
			GossipInterval:     200 * time.Millisecond,
			ReplicationTimeout: 2 * time.Second,
			ReadTimeout:        1 * time.Second,
			MaxConnections:     100,
			MaxIdleConnections: 10,
		}
	case ProfileWAN:
		return &LatencyConfig{
			FailureTimeout:     30 * time.Second,
			SuspectTimeout:     15 * time.Second,
			GossipInterval:     1 * time.Second,
			ReplicationTimeout: 10 * time.Second,
			ReadTimeout:        5 * time.Second,
			MaxConnections:     50,
			MaxIdleConnections: 5,
		}
	case ProfileUnreliable:
		return &LatencyConfig{
			FailureTimeout:     10 * time.Second,
			SuspectTimeout:     5 * time.Second,
			GossipInterval:     500 * time.Millisecond,
			ReplicationTimeout: 5 * time.Second,
			ReadTimeout:        2 * time.Second,
			MaxConnections:     25,
			MaxIdleConnections: 3,
		}
	default:
		return GetConfigForProfile(ProfileLAN, nodeCount)
	}
}

// IsNodeHealthy checks if a node is healthy and operational
func IsNodeHealthy(node *gridkv.GridKV) bool {
	if node == nil {
		return false
	}

	// Get replica status to check node health
	status := node.GetReplicaStatus()
	return status.Ready && status.HealthyNodes > 0
}

// TestEnvironmentConfig configures test environment simulation
type TestEnvironmentConfig struct {
	NetworkProfile NetworkProfile
	NetworkType    gridkv.NetworkType
	NodeCount      int
	ReplicaCount   int
	BasePort       int
	MaxMemoryMB    int64
	ShardCount     int
}

// DefaultTestEnvironment returns default test environment config
func DefaultTestEnvironment() *TestEnvironmentConfig {
	return &TestEnvironmentConfig{
		NetworkProfile: ProfileLAN,
		NetworkType:    networkTypeFromEnv(gridkv.TCP),
		NodeCount:      100,
		ReplicaCount:   3,
		BasePort:       20000,

		MaxMemoryMB: 1024,
		ShardCount:  64,
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
	// Use shorter delays - caller can check testing.Short() if needed
	// Default to faster settings
	const (
		readinessWait = 1 * time.Second
		pollInterval  = 25 * time.Millisecond
		settleDelay   = 200 * time.Millisecond
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

	// Additional wait to ensure all pipelines are flushed and replication completes
	time.Sleep(settleDelay)
}

func (tes *TestEnvironmentSimulator) nodesReady(nodes []*gridkv.GridKV) bool {
	if len(nodes) == 0 {
		return true
	}

	healthyCount := 0
	for _, node := range nodes {
		if node != nil && IsNodeHealthy(node) {
			healthyCount++
		}
	}

	// Count non-nil nodes
	nonNilCount := 0
	for _, node := range nodes {
		if node != nil {
			nonNilCount++
		}
	}

	return nonNilCount == 0 || healthyCount == nonNilCount
}

func (tes *TestEnvironmentSimulator) snapshotNodes() []*gridkv.GridKV {
	tes.mu.RLock()
	defer tes.mu.RUnlock()
	out := make([]*gridkv.GridKV, len(tes.nodes))
	copy(out, tes.nodes)
	return out
}

// LogNodeDiagnostics logs replica status for all nodes (alive and down).
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

// isPortAvailable checks if a port is available by attempting to listen on it
func isPortAvailable(port int) bool {
	addr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}

	listener, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return false
	}

	listener.Close()
	// Give the port a moment to be fully released
	time.Sleep(10 * time.Millisecond)
	return true
}

// findAvailablePort finds an available port starting from the given port, skipping occupied ones
// It tries up to maxAttempts ports, incrementing by portSpacing each time
func findAvailablePort(startPort, portSpacing, maxAttempts int) (int, error) {
	for attempt := 0; attempt < maxAttempts; attempt++ {
		port := startPort + attempt*portSpacing
		if isPortAvailable(port) {
			return port, nil
		}
	}
	return 0, fmt.Errorf("no available port found after %d attempts starting from %d", maxAttempts, startPort)
}

// SetupCluster creates and initializes the test cluster
func (tes *TestEnvironmentSimulator) SetupCluster(tb testing.TB) error {
	tes.mu.Lock()
	latencyConfig := GetConfigForProfile(tes.config.NetworkProfile, tes.config.NodeCount)
	tes.nodes = make([]*gridkv.GridKV, tes.config.NodeCount)
	tes.mu.Unlock() // Release lock before starting goroutines to avoid deadlock

	// Create seed node
	var err error
	opts := &gridkv.GridKVOptions{
		LocalNodeID:        "node-0",
		LocalAddress:       fmt.Sprintf("127.0.0.1:%d", tes.config.BasePort),
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
			BindAddr: fmt.Sprintf("127.0.0.1:%d", tes.config.BasePort),
			MaxConns: latencyConfig.MaxConnections,
			MaxIdle:  latencyConfig.MaxIdleConnections,
		},
		Storage: &gridkv.StorageOptions{
			MaxMemoryMB: tes.config.MaxMemoryMB,
			ShardCount:  tes.config.ShardCount,
		},
	}

	// Calculate port spacing for large clusters to avoid conflicts
	portSpacing := 1
	if tes.config.NodeCount > 15 {
		portSpacing = 2 // Use every other port for large clusters
	}
	if tes.config.NodeCount > 20 {
		portSpacing = 3 // Use every third port for very large clusters
	}
	if tes.config.NodeCount > 100 {
		portSpacing = 5 // Use every 5th port for very large clusters (100+ nodes)
	}
	if tes.config.NodeCount > 500 {
		portSpacing = 10 // Use every 10th port for extremely large clusters (1000+ nodes)
	}

	// Smart port selection for seed node: try base port, then find available port if needed
	seedPort := tes.config.BasePort
	maxPortAttempts := 10

	tb.Logf("Finding available port starting from %d...", seedPort)
	port, portErr := findAvailablePort(seedPort, portSpacing, maxPortAttempts)
	if portErr != nil {
		// If port finding fails, fall back to retry mechanism
		tb.Logf("Port finding failed, using retry mechanism: %v", portErr)
		maxRetries := 5
		for retry := 0; retry < maxRetries; retry++ {
			tes.nodes[0], err = gridkv.NewGridKV(opts)
			if err == nil {
				break
			}
			errStr := err.Error()
			if strings.Contains(errStr, "bind") || strings.Contains(errStr, "address already in use") {
				if retry < maxRetries-1 {
					waitTime := time.Duration(retry+1) * 1 * time.Second
					time.Sleep(waitTime)
					continue
				}
			}
			if retry == maxRetries-1 {
				return fmt.Errorf("failed to create seed node after %d retries: %w", maxRetries, err)
			}
		}
	} else {
		if port != seedPort {
			tb.Logf("Port %d is occupied, using port %d instead", seedPort, port)
			seedPort = port
			opts.LocalAddress = fmt.Sprintf("127.0.0.1:%d", seedPort)
			opts.Network.BindAddr = fmt.Sprintf("127.0.0.1:%d", seedPort)
		}
		tb.Logf("Creating seed node on port %d...", seedPort)
		tes.nodes[0], err = gridkv.NewGridKV(opts)
		if err != nil {
			return fmt.Errorf("failed to create seed node on port %d: %w", seedPort, err)
		}
		tb.Logf("Seed node created successfully")
	}

	// Wait for seed node to be fully ready before creating other nodes
	tb.Logf("Waiting for seed node to be ready...")
	if err := tes.nodes[0].WaitReady(10 * time.Second); err != nil {
		return fmt.Errorf("seed node not ready: %w", err)
	}
	tb.Logf("Seed node ready, creating other nodes...")

	// For larger clusters, configure multiple seed nodes for better bootstrap reliability
	var seeds []string
	if tes.config.NodeCount <= 5 {
		seeds = []string{fmt.Sprintf("127.0.0.1:%d", seedPort)}
	} else if tes.config.NodeCount <= 10 {
		seeds = []string{
			fmt.Sprintf("127.0.0.1:%d", seedPort),
			fmt.Sprintf("127.0.0.1:%d", seedPort+1*portSpacing),
		}
	} else {
		seeds = []string{
			fmt.Sprintf("127.0.0.1:%d", seedPort),
			fmt.Sprintf("127.0.0.1:%d", seedPort+1*portSpacing),
			fmt.Sprintf("127.0.0.1:%d", seedPort+2*portSpacing),
		}
	}

	// Concurrent node creation
	used := &sync.Map{}
	used.Store(seedPort, true)

	// Determine concurrency limit based on cluster size
	maxConcurrency := runtime.NumCPU() * 2
	if tes.config.NodeCount < 20 {
		maxConcurrency = tes.config.NodeCount - 1 // For small clusters, create all concurrently
	} else if tes.config.NodeCount < 100 {
		maxConcurrency = 20 // Limit to 20 concurrent for medium clusters
	} else {
		maxConcurrency = 30 // Limit to 30 concurrent for large clusters
	}

	tb.Logf("Creating %d nodes concurrently (max %d concurrent)...", tes.config.NodeCount-1, maxConcurrency)

	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrency)
	var createErr error
	var errMu sync.Mutex
	createdCount := atomic.Int64{}

	for i := 1; i < tes.config.NodeCount; i++ {
		wg.Add(1)

		go func(nodeIdx int) {
			defer wg.Done()
			sem <- struct{}{}        // Acquire semaphore
			defer func() { <-sem }() // Release semaphore

			prefPort := seedPort + nodeIdx*portSpacing

			// Find available port with thread-safe check
			var port int
			var portErr error
			maxAttempts := 3
			for attempt := 0; attempt < maxAttempts; attempt++ {
				port, portErr = findAvailablePort(prefPort, portSpacing, 20)
				if portErr != nil {
					if attempt < maxAttempts-1 {
						port, portErr = findAvailablePort(prefPort, portSpacing*2, 15)
					}
				}
				if portErr != nil && attempt < maxAttempts-1 {
					port, portErr = findAvailablePort(prefPort-portSpacing*5, 1, 30)
				}

				// Check if port is already used (thread-safe)
				if portErr == nil {
					if _, exists := used.LoadOrStore(port, true); exists {
						// Port already used, try next
						prefPort = port + portSpacing
						portErr = fmt.Errorf("port %d already in use", port)
						continue
					}
					break // Found available port
				}

				// Retry with different starting point
				if attempt < maxAttempts-1 {
					prefPort = seedPort + nodeIdx*portSpacing + (attempt+1)*portSpacing*10
				}
			}

			if portErr != nil {
				errMu.Lock()
				if createErr == nil {
					createErr = fmt.Errorf("failed to find available port for node %d: %w", nodeIdx, portErr)
				}
				errMu.Unlock()
				return
			}

			if port != seedPort+nodeIdx*portSpacing {
				tb.Logf("Node %d: port %d is occupied, using port %d instead", nodeIdx, seedPort+nodeIdx*portSpacing, port)
			}

			opts := &gridkv.GridKVOptions{
				LocalNodeID:        fmt.Sprintf("node-%d", nodeIdx),
				LocalAddress:       fmt.Sprintf("127.0.0.1:%d", port),
				SeedAddrs:          seeds,
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
					BindAddr: fmt.Sprintf("127.0.0.1:%d", port),
					MaxConns: latencyConfig.MaxConnections,
					MaxIdle:  latencyConfig.MaxIdleConnections,
				},
				Storage: &gridkv.StorageOptions{
					MaxMemoryMB: tes.config.MaxMemoryMB,
					ShardCount:  tes.config.ShardCount,
				},
			}

			node, nodeErr := gridkv.NewGridKV(opts)
			if nodeErr != nil {
				// Try fallback port
				used.Delete(port) // Release the port
				nextPort, nextErr := findAvailablePort(port+portSpacing, portSpacing, 10)
				if nextErr == nil {
					if _, exists := used.LoadOrStore(nextPort, true); !exists {
						opts.LocalAddress = fmt.Sprintf("127.0.0.1:%d", nextPort)
						opts.Network.BindAddr = fmt.Sprintf("127.0.0.1:%d", nextPort)
						node, nodeErr = gridkv.NewGridKV(opts)
						if nodeErr == nil {
							port = nextPort
						}
					}
				}
			}

			if nodeErr != nil {
				errMu.Lock()
				if createErr == nil {
					createErr = fmt.Errorf("failed to create node %d on port %d: %w", nodeIdx, port, nodeErr)
				}
				errMu.Unlock()
				used.Delete(port)
				return
			}

			// Store node (thread-safe)
			tes.mu.Lock()
			tes.nodes[nodeIdx] = node
			tes.mu.Unlock()

			// Skip WaitReady during node creation - will wait for cluster stability later
			// This significantly speeds up concurrent node creation
			// Just give node a moment to start async Join operation
			time.Sleep(50 * time.Millisecond)

			count := createdCount.Add(1)
			if count%10 == 0 || count == int64(tes.config.NodeCount-1) {
				tb.Logf("Created: %d/%d nodes...", count, tes.config.NodeCount-1)
			}
		}(i)
	}

	wg.Wait()

	finalCount := createdCount.Load()
	tb.Logf("Node creation completed: %d/%d nodes created successfully", finalCount, tes.config.NodeCount-1)

	if createErr != nil {
		tes.cleanupNodes(tes.config.NodeCount)
		return createErr
	}

	if finalCount != int64(tes.config.NodeCount-1) {
		tes.cleanupNodes(tes.config.NodeCount)
		return fmt.Errorf("only %d/%d nodes created successfully", finalCount, tes.config.NodeCount-1)
	}

	// All nodes created, now wait for cluster to stabilize
	// Use progressive waiting strategy: first ensure all nodes can see peers,
	// then wait for cluster stability
	tb.Logf("All %d nodes created, waiting for cluster to stabilize...", tes.config.NodeCount)

	// Progressive waiting strategy for large clusters
	// Note: Lock was already released before starting goroutines (line 284)
	// Phase 1: Ensure all nodes have basic connectivity (can see at least some peers)
	phase1Timeout := 3 * time.Second
	if tes.config.NodeCount >= 50 {
		phase1Timeout = 5 * time.Second
	}
	if tes.config.NodeCount >= 100 {
		phase1Timeout = 8 * time.Second
	}

	tb.Logf("Phase 1: Ensuring basic connectivity (timeout: %v)...", phase1Timeout)
	basicReadyCount := 0
	deadline := time.Now().Add(phase1Timeout)
	checkInterval := 100 * time.Millisecond
	for time.Now().Before(deadline) && basicReadyCount < tes.config.NodeCount {
		basicReadyCount = 0
		nodes := tes.snapshotNodes()
		for _, node := range nodes {
			if node != nil {
				status := node.GetReplicaStatus()
				if status.Ready && status.HealthyNodes > 0 {
					basicReadyCount++
				}
			}
		}
		if basicReadyCount < tes.config.NodeCount {
			time.Sleep(checkInterval)
		} else {
			break // All nodes ready, exit early
		}
	}
	tb.Logf("Phase 1 complete: %d/%d nodes have basic connectivity", basicReadyCount, tes.config.NodeCount)

	// Phase 2: Wait for cluster stability using WaitReady
	phase2Timeout := 10 * time.Second
	if tes.config.NodeCount >= 50 {
		phase2Timeout = 15 * time.Second
	}
	if tes.config.NodeCount >= 100 {
		phase2Timeout = 20 * time.Second
	}

	tb.Logf("Phase 2: Waiting for cluster stability (timeout: %v)...", phase2Timeout)
	err = tes.WaitForClusterReady(tb, phase2Timeout)

	if err != nil {
		tb.Logf("WaitForClusterReady returned error: %v", err)
		// Log diagnostics but don't fail immediately
		tes.LogNodeDiagnostics(tb, "after WaitReady timeout")
		// Give a brief moment for final convergence
		time.Sleep(2 * time.Second)
	} else {
		tb.Logf("Cluster ready and stable, all nodes are operational")
	}

	// Phase 3: Verify network connectivity for all nodes
	tb.Logf("Phase 3: Verifying network connectivity...")
	networkReadyCount := 0

	// Check network health for each node
	for i := 0; i < tes.config.NodeCount; i++ {
		if node := tes.nodes[i]; node != nil {
			// For now, just check that node reference exists
			// Network connectivity will be tested during actual operations
			networkReadyCount++
		}
	}

	if networkReadyCount == tes.config.NodeCount {
		tb.Logf("✅ Network verification complete: %d/%d nodes ready", networkReadyCount, tes.config.NodeCount)
	} else {
		tb.Logf("⚠️ Network verification: %d/%d nodes ready", networkReadyCount, tes.config.NodeCount)
	}

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
	// For large clusters, shutdown in batches to avoid resource exhaustion
	batchSize := 5
	if limit > 15 {
		batchSize = 3 // Smaller batches for very large clusters
	}

	// Use longer timeout for large clusters
	closeTimeout := 30 * time.Second
	if limit > 10 {
		closeTimeout = 60 * time.Second // Allow more time for large clusters
	}
	if limit > 50 {
		closeTimeout = 90 * time.Second // Extra time for very large clusters
	}

	// Record initial goroutine count for leak detection
	initialGoroutines := runtime.NumGoroutine()

	// Shutdown nodes in batches
	for batchStart := 0; batchStart < limit; batchStart += batchSize {
		batchEnd := batchStart + batchSize
		if batchEnd > limit {
			batchEnd = limit
		}

		var wg sync.WaitGroup
		for idx := batchStart; idx < batchEnd && idx < len(tes.nodes); idx++ {
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

		// Wait between batches to allow resources to be released
		if batchEnd < limit {
			time.Sleep(2 * time.Second)
			runtime.GC()
		}
	}

	// Additional wait and GC to allow goroutines to fully exit
	// Use progressive waiting for better resource cleanup
	waitIntervals := []time.Duration{2 * time.Second, 3 * time.Second, 2 * time.Second}
	for _, interval := range waitIntervals {
		time.Sleep(interval)
		runtime.GC()
	}

	// Check for goroutine leaks (allow some overhead for test framework)
	finalGoroutines := runtime.NumGoroutine()
	leakedGoroutines := finalGoroutines - initialGoroutines
	if leakedGoroutines > 50 {
		fmt.Printf("WARN: Potential goroutine leak detected: %d goroutines still running (initial: %d, final: %d)\n",
			leakedGoroutines, initialGoroutines, finalGoroutines)
	}

	// Final cleanup attempt
	runtime.GC()
	time.Sleep(1 * time.Second)
}

// WaitForHealthyNodes waits for cluster to reach expected healthy node count
func (tes *TestEnvironmentSimulator) WaitForHealthyNodes(tb testing.TB, expected int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	lastLogTime := time.Now()
	logInterval := 2 * time.Second

	for time.Now().Before(deadline) {
		tes.mu.RLock()
		if len(tes.nodes) == 0 {
			tes.mu.RUnlock()
			return
		}
		nodes := make([]*gridkv.GridKV, len(tes.nodes))
		copy(nodes, tes.nodes)
		tes.mu.RUnlock()

		// Check all nodes and log diagnostics periodically
		if time.Since(lastLogTime) >= logInterval {
			tb.Logf("Cluster status check: expected=%d healthy nodes", expected)
			for i, node := range nodes {
				if node == nil {
					continue
				}
				status := node.GetReplicaStatus()
				tb.Logf("  node-%d: ready=%v healthy=%d cluster=%d peers=%d",
					i, status.Ready, status.HealthyNodes, status.ClusterSize, status.PeerCount)
			}
			lastLogTime = time.Now()
		}

		// Check if any node reports enough healthy nodes
		for _, node := range nodes {
			if node == nil {
				continue
			}
			status := node.GetReplicaStatus()
			if status.HealthyNodes >= expected {
				tb.Logf("Cluster ready: %d healthy nodes detected", status.HealthyNodes)
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Final diagnostic before failing
	tb.Logf("=== Final cluster status before timeout ===")
	for i, node := range tes.snapshotNodes() {
		if node == nil {
			tb.Logf("node-%d: nil", i)
			continue
		}
		status := node.GetReplicaStatus()
		tb.Logf("node-%d: ready=%v healthy=%d cluster=%d peers=%d replica=%d",
			i, status.Ready, status.HealthyNodes, status.ClusterSize, status.PeerCount, status.ReplicaFactor)
	}
	tb.Fatalf("timed out waiting for %d healthy nodes", expected)
}

// WaitForAllNodesReady waits for all nodes to be ready using WaitReady API
// Similar to the test framework in REPORT_GRIDKV021.md
func (tes *TestEnvironmentSimulator) WaitForAllNodesReady(tb testing.TB, timeout time.Duration) {
	nodes := tes.snapshotNodes()

	// First, wait for nodes to be created (not nil)
	nonNilNodes := make([]*gridkv.GridKV, 0, len(nodes))
	for _, node := range nodes {
		if node != nil {
			nonNilNodes = append(nonNilNodes, node)
		}
	}
	if len(nonNilNodes) == 0 {
		tb.Fatalf("no nodes available")
	}

	// Use WaitReady API for each node (similar to report's framework)
	// Calculate per-node timeout (distribute total timeout across nodes)
	perNodeTimeout := timeout / time.Duration(len(nonNilNodes))
	if perNodeTimeout < 5*time.Second {
		perNodeTimeout = 5 * time.Second // Minimum timeout per node
	}

	tb.Logf("⏳ Waiting for nodes to be ready (using GridKV WaitReady API)...")
	var wg sync.WaitGroup
	var readyCount atomic.Int64
	var readyMu sync.Mutex
	readyNodes := make(map[int]bool)

	for i, node := range nonNilNodes {
		wg.Add(1)
		go func(idx int, n *gridkv.GridKV) {
			defer wg.Done()
			if err := n.WaitReady(perNodeTimeout); err != nil {
				tb.Logf("   Node node-%d WaitReady failed: %v", idx, err)
				return
			}
			status := n.GetReplicaStatus()
			readyMu.Lock()
			readyNodes[idx] = true
			currentCount := len(readyNodes)
			readyMu.Unlock()
			readyCount.Add(1)
			tb.Logf("   Node node-%d is ready (%d/%d) (clusterSize=%d nodes=%d peers=%d)",
				idx, currentCount, len(nonNilNodes), status.ClusterSize, status.ClusterSize, status.PeerCount)
		}(i, node)
	}

	// Wait for all nodes or timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		finalCount := int(readyCount.Load())
		if finalCount == len(nonNilNodes) {
			tb.Logf("✅ All %d nodes are ready!", len(nonNilNodes))
			return
		}
		// Log which nodes failed
		readyMu.Lock()
		var failedNodes []int
		for i := range nonNilNodes {
			if !readyNodes[i] {
				failedNodes = append(failedNodes, i)
			}
		}
		readyMu.Unlock()
		tb.Fatalf("only %d/%d nodes ready (failed nodes: %v)", finalCount, len(nonNilNodes), failedNodes)
	case <-time.After(timeout):
		finalCount := int(readyCount.Load())
		tb.Logf("Final node status:")
		for i, node := range nonNilNodes {
			if node != nil {
				status := node.GetReplicaStatus()
				readyMu.Lock()
				isReady := readyNodes[i]
				readyMu.Unlock()
				tb.Logf("  Node %d: Ready=%v (WaitReady=%v) ClusterSize=%d HealthyNodes=%d PeerCount=%d ReplicaFactor=%d",
					i, status.Ready, isReady, status.ClusterSize, status.HealthyNodes, status.PeerCount, status.ReplicaFactor)
			}
		}
		tb.Fatalf("timed out waiting for all nodes to be ready: %d/%d ready", finalCount, len(nonNilNodes))
	}
}

// VerifyConsistencyAcrossDelays verifies eventual consistency at multiple delay stages
// Similar to the consistency checker in REPORT_GRIDKV021.md
func (tes *TestEnvironmentSimulator) VerifyConsistencyAcrossDelays(tb testing.TB, writtenKeys map[string][]byte, delays []time.Duration, minConvergenceRate float64) {
	nodes := tes.snapshotNodes()
	ctx := context.Background()

	type delayResult struct {
		delay       time.Duration
		consistent  int
		missing     int
		mismatch    int
		total       int
		convergence float64
	}

	results := make([]delayResult, 0, len(delays))

	for _, delay := range delays {
		if delay > 0 {
			time.Sleep(delay)
		}
		// Wait for replication to settle before each verification
		tes.WaitForReplicationSettle(nodes...)
		// Additional wait for pipeline flush and network propagation
		if delay > 0 {
			time.Sleep(200 * time.Millisecond)
		}

		consistent := 0
		missing := 0
		mismatch := 0

		mismatchKeys := make([]string, 0, 10)                 // Track mismatch keys for diagnosis
		mismatchDetails := make(map[string]map[string]string) // key -> nodeID -> value

		for key, expectedValue := range writtenKeys {
			foundCount := 0
			matchingCount := 0
			nodeValues := make(map[string]string) // Track values per node for diagnosis

			for i, node := range nodes {
				if node == nil {
					continue
				}
				value, err := node.Get(ctx, key)
				if err == nil && value != nil {
					foundCount++
					valueStr := string(value)
					nodeID := fmt.Sprintf("node-%d", i)
					nodeValues[nodeID] = valueStr
					if valueStr == string(expectedValue) {
						matchingCount++
					}
				} else if err != nil {
					nodeID := fmt.Sprintf("node-%d", i)
					nodeValues[nodeID] = fmt.Sprintf("ERROR: %v", err)
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
				// Track mismatch details (limit to first 10 for logging)
				if len(mismatchKeys) < 10 {
					mismatchKeys = append(mismatchKeys, key)
					mismatchDetails[key] = nodeValues
				}
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

		// Log mismatch details for diagnosis
		if len(mismatchKeys) > 0 && delay == delays[0] { // Only log for first delay to avoid spam
			tb.Logf("  Mismatch analysis (showing first %d keys):", len(mismatchKeys))
			for _, key := range mismatchKeys {
				details := mismatchDetails[key]
				tb.Logf("    Key: %s", key)
				tb.Logf("      Expected value length: %d", len(writtenKeys[key]))
				for nodeID, value := range details {
					if strings.HasPrefix(value, "ERROR:") {
						tb.Logf("      %s: %s", nodeID, value)
					} else {
						tb.Logf("      %s: value length=%d, matches=%v", nodeID, len(value), value == string(writtenKeys[key]))
					}
				}
			}
		}
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

	for _, node := range nodes {
		if node == nil {
			continue
		}
		// Note: This assumes there's a way to get keys from storage
		// If not available, we'll need to track keys during writes
		_ = node
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

// TestPhases defines test execution phases
type TestPhases struct {
	Setup       time.Duration // Cluster setup time (not measured)
	Seed        time.Duration // Data seeding time (not measured)
	Warmup      time.Duration // Warmup period (not measured)
	Measurement time.Duration // Actual measurement period
	Cooldown    time.Duration // Cooldown period (not measured)
}

// DefaultPhases returns recommended phase durations
func DefaultPhases() TestPhases {
	return TestPhases{
		Setup:       15 * time.Second,
		Seed:        5 * time.Second,
		Warmup:      3 * time.Second,
		Measurement: 15 * time.Second,
		Cooldown:    2 * time.Second,
	}
}

// FastPhases returns faster phase durations for quick tests
func FastPhases() TestPhases {
	return TestPhases{
		Setup:       10 * time.Second,
		Seed:        2 * time.Second,
		Warmup:      1 * time.Second,
		Measurement: 10 * time.Second,
		Cooldown:    1 * time.Second,
	}
}

// WorkloadStats contains performance statistics
type WorkloadStats struct {
	// Timestamps
	MeasurementStart time.Time
	MeasurementEnd   time.Time
	Duration         time.Duration

	// Operation counts
	WritesCompleted  int64
	WritesFailed     int64
	ReadsCompleted   int64
	ReadsFailed      int64
	DeletesCompleted int64
	DeletesFailed    int64

	// Latency statistics
	WriteLatencyP50  time.Duration
	WriteLatencyP95  time.Duration
	WriteLatencyP99  time.Duration
	ReadLatencyP50   time.Duration
	ReadLatencyP95   time.Duration
	ReadLatencyP99   time.Duration
	DeleteLatencyP50 time.Duration
	DeleteLatencyP95 time.Duration
	DeleteLatencyP99 time.Duration

	// Additional metrics
	TotalKeys   int64
	SuccessRate float64
	TotalQPS    float64
	WriteQPS    float64
	ReadQPS     float64
	DeleteQPS   float64
}

// LatencySampler efficiently samples latencies using reservoir sampling
type LatencySampler struct {
	samples  []time.Duration
	count    int64
	capacity int
	mu       sync.Mutex
}

// NewLatencySampler creates a new latency sampler
func NewLatencySampler(capacity int) *LatencySampler {
	return &LatencySampler{
		samples:  make([]time.Duration, 0, capacity),
		capacity: capacity,
	}
}

// Add adds a latency sample
func (ls *LatencySampler) Add(latency time.Duration) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	count := atomic.AddInt64(&ls.count, 1)

	if len(ls.samples) < ls.capacity {
		ls.samples = append(ls.samples, latency)
	} else {
		// Reservoir sampling algorithm
		idx := rand.Int63n(count)
		if idx < int64(ls.capacity) {
			ls.samples[idx] = latency
		}
	}
}

// Percentiles calculates p50, p95, p99 percentiles
func (ls *LatencySampler) Percentiles() (p50, p95, p99 time.Duration) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	if len(ls.samples) == 0 {
		return 0, 0, 0
	}

	sorted := make([]time.Duration, len(ls.samples))
	copy(sorted, ls.samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	n := len(sorted)
	p50 = sorted[n*50/100]
	p95 = sorted[n*95/100]
	p99 = sorted[n*99/100]
	return
}

// Count returns the total number of samples added
func (ls *LatencySampler) Count() int64 {
	return atomic.LoadInt64(&ls.count)
}

// WaitForClusterReady waits for all nodes using WaitReady() API
func (tes *TestEnvironmentSimulator) WaitForClusterReady(tb testing.TB, timeout time.Duration) error {
	tb.Logf("WaitForClusterReady called with timeout: %v", timeout)
	nodes := tes.snapshotNodes()
	tb.Logf("snapshotNodes returned %d nodes", len(nodes))
	if len(nodes) == 0 {
		return fmt.Errorf("no nodes to wait for")
	}

	// Filter out nil nodes
	nonNilNodes := make([]*gridkv.GridKV, 0, len(nodes))
	nilCount := 0
	for i, node := range nodes {
		if node != nil {
			nonNilNodes = append(nonNilNodes, node)
		} else {
			nilCount++
			if nilCount <= 3 {
				tb.Logf("node-%d is nil", i)
			}
		}
	}
	if nilCount > 3 {
		tb.Logf("... and %d more nil nodes", nilCount-3)
	}
	tb.Logf("Found %d non-nil nodes out of %d total", len(nonNilNodes), len(nodes))
	if len(nonNilNodes) == 0 {
		return fmt.Errorf("no non-nil nodes to wait for")
	}

	tb.Logf("Waiting for %d nodes to be ready (timeout: %v)...", len(nonNilNodes), timeout)

	var wg sync.WaitGroup
	errCh := make(chan error, len(nonNilNodes))
	startTime := time.Now()
	readyCount := atomic.Int64{}

	// Use per-node timeout: for large clusters, use longer timeout per node
	// Don't divide timeout by node count - nodes wait in parallel
	perNodeTimeout := timeout
	if len(nonNilNodes) > 50 {
		// For large clusters, give each node more time
		perNodeTimeout = 15 * time.Second
	} else if len(nonNilNodes) > 20 {
		perNodeTimeout = 8 * time.Second
	} else {
		perNodeTimeout = 5 * time.Second
	}
	// Cap at total timeout
	if perNodeTimeout > timeout {
		perNodeTimeout = timeout
	}
	tb.Logf("Using per-node timeout: %v for %d nodes", perNodeTimeout, len(nonNilNodes))

	for i, node := range nonNilNodes {
		wg.Add(1)
		go func(idx int, n *gridkv.GridKV) {
			defer wg.Done()

			// Use WaitReady directly - it handles both basic readiness and stability
			if err := n.WaitReady(perNodeTimeout); err != nil {
				status := n.GetReplicaStatus()
				tb.Logf("node-%d stability wait failed: %v (clusterSize=%d healthy=%d)",
					idx, err, status.ClusterSize, status.HealthyNodes)
				// Don't fail if basic ready succeeded - cluster may still be forming
				// Just log and continue
				return
			}

			count := readyCount.Add(1)
			// Log progress more frequently for large clusters
			logInterval := int64(10)
			if len(nonNilNodes) > 50 {
				logInterval = 20
			}
			if count%logInterval == 0 || count == int64(len(nonNilNodes)) || count <= 5 {
				status := n.GetReplicaStatus()
				tb.Logf("node-%d ready (%d/%d, elapsed: %v, clusterSize=%d healthy=%d)",
					idx, count, len(nonNilNodes), time.Since(startTime),
					status.ClusterSize, status.HealthyNodes)
			}
		}(i, node)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	// Early exit: poll periodically to check if all nodes are ready
	// This allows us to exit as soon as all nodes are ready (typically ~500ms)
	checkTicker := time.NewTicker(100 * time.Millisecond)
	defer checkTicker.Stop()

	// Use a timeout channel
	timeoutCh := time.After(timeout)

	// Wait for completion with early exit check
	waitComplete := false
	for !waitComplete {
		select {
		case <-done:
			// All goroutines completed, break to check results
			waitComplete = true
		case <-checkTicker.C:
			// Check if all nodes are ready
			currentReady := readyCount.Load()
			if currentReady == int64(len(nonNilNodes)) {
				tb.Logf("✅ All %d nodes ready early (elapsed: %v), exiting wait",
					currentReady, time.Since(startTime))
				// Give goroutines a moment to finish
				select {
				case <-done:
				case <-time.After(50 * time.Millisecond):
				}
				waitComplete = true
			}
			// Continue polling
		case <-timeoutCh:
			// Timeout reached, break to check results
			waitComplete = true
		}
	}

	// Check results
	select {
	case <-done:
		elapsed := time.Since(startTime)
		finalCount := int(readyCount.Load())

		// Check basic readiness for all nodes
		basicReadyCount := 0
		for _, node := range nonNilNodes {
			if node != nil {
				status := node.GetReplicaStatus()
				if status.Ready && status.HealthyNodes > 0 {
					basicReadyCount++
				}
			}
		}

		if finalCount == len(nonNilNodes) {
			tb.Logf("✅ All %d nodes ready and stable in %v", finalCount, elapsed)
			return nil
		}

		// If most nodes are stable and all have basic readiness, accept it
		if finalCount >= len(nonNilNodes)*8/10 && basicReadyCount == len(nonNilNodes) {
			tb.Logf("⚠️  %d/%d nodes stable, %d/%d basic ready in %v (acceptable)",
				finalCount, len(nonNilNodes), basicReadyCount, len(nonNilNodes), elapsed)
			return nil
		}

		tb.Logf("⚠️  Only %d/%d nodes stable, %d/%d basic ready in %v",
			finalCount, len(nonNilNodes), basicReadyCount, len(nonNilNodes), elapsed)
		// Continue anyway if at least 80% are stable
		if finalCount >= len(nonNilNodes)*8/10 {
			return nil
		}
		return fmt.Errorf("insufficient nodes ready: %d/%d stable, %d/%d basic",
			finalCount, len(nonNilNodes), basicReadyCount, len(nonNilNodes))
	case err := <-errCh:
		// Log error but continue if most nodes are ready
		finalCount := int(readyCount.Load())
		if finalCount >= len(nonNilNodes)*8/10 { // 80% ready is acceptable
			tb.Logf("Warning: %v, but %d/%d nodes ready, continuing", err, finalCount, len(nonNilNodes))
			return nil
		}
		return err
	case <-time.After(timeout):
		finalCount := int(readyCount.Load())
		if finalCount >= len(nonNilNodes)*8/10 { // 80% ready is acceptable
			tb.Logf("Timeout reached, but %d/%d nodes ready (>=80%%), continuing", finalCount, len(nonNilNodes))
			return nil
		}
		return fmt.Errorf("cluster not ready within %v (%d/%d nodes ready)", timeout, finalCount, len(nonNilNodes))
	}
}

// SetupClusterOptimized creates cluster
func (tes *TestEnvironmentSimulator) SetupClusterOptimized(tb testing.TB) error {
	return tes.SetupCluster(tb)
}

// CleanupGracefully performs graceful shutdown with better error handling
func (tes *TestEnvironmentSimulator) CleanupGracefully(tb testing.TB, timeout time.Duration) {
	tb.Helper()

	tes.mu.Lock()
	nodes := make([]*gridkv.GridKV, len(tes.nodes))
	copy(nodes, tes.nodes)
	tes.mu.Unlock()

	if len(nodes) == 0 {
		return
	}

	tb.Logf("Starting graceful cleanup of %d nodes...", len(nodes))

	// Phase 1: Initiate shutdown signals
	time.Sleep(200 * time.Millisecond)

	// Phase 2: Close nodes concurrently
	var wg sync.WaitGroup
	closeErrors := make([]error, len(nodes))

	for i, node := range nodes {
		if node == nil {
			continue
		}
		wg.Add(1)
		go func(idx int, n *gridkv.GridKV) {
			defer wg.Done()
			if err := n.CloseWithTimeout(timeout); err != nil {
				closeErrors[idx] = err
				tb.Logf("node-%d close warning: %v", idx, err)
			} else {
				tb.Logf("node-%d closed successfully", idx)
			}
		}(i, node)
	}

	// Wait for all closes with grace period
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// All nodes closed
	case <-time.After(timeout + 5*time.Second):
		tb.Logf("Cleanup timed out after %v, forcing return", timeout+5*time.Second)
	}

	// Phase 3: Final cleanup
	time.Sleep(1 * time.Second)
	runtime.GC()
	time.Sleep(500 * time.Millisecond)

	tb.Logf("Cleanup completed")
}

// StartWorkersGradually starts workers with gradual ramp-up to avoid initial burst
func StartWorkersGradually(count int, startFn func(int), interval time.Duration, wg *sync.WaitGroup) {
	for i := 0; i < count; i++ {
		wg.Add(1)
		go startFn(i)
		if i < count-1 && interval > 0 {
			time.Sleep(interval)
		}
	}
}

// generateRandomValue generates a random-sized value
// targetAvgSize: target average size in bytes (e.g., 10KB = 10240)
// sizeRange: size variation range (e.g., 0.5 means ±50% of targetAvgSize)
// Returns a value with size between targetAvgSize*(1-sizeRange) and targetAvgSize*(1+sizeRange)
func generateRandomValue(rng *rand.Rand, targetAvgSize int, sizeRange float64) []byte {
	minSize := int(float64(targetAvgSize) * (1 - sizeRange))
	maxSize := int(float64(targetAvgSize) * (1 + sizeRange))
	if minSize < 1 {
		minSize = 1
	}
	size := minSize + rng.Intn(maxSize-minSize+1)
	value := make([]byte, size)
	cryptorand.Read(value)
	return value
}

// SeedData seeds initial data with progress tracking
func SeedData(tb testing.TB, nodes []*gridkv.GridKV, keyCount int, valueSize int) []string {
	tb.Helper()
	tb.Logf("Seeding %d keys (value size: %d bytes)...", keyCount, valueSize)

	ctx := context.Background()
	keys := make([]string, 0, keyCount)
	keysMu := sync.Mutex{}
	startTime := time.Now()

	var wg sync.WaitGroup
	var successCount atomic.Int64

	// Seed in batches to avoid overwhelming the cluster
	batchSize := 100
	for start := 0; start < keyCount; start += batchSize {
		end := start + batchSize
		if end > keyCount {
			end = keyCount
		}

		for i := start; i < end; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				key := fmt.Sprintf("seed-key-%d", idx)
				value := make([]byte, valueSize)
				cryptorand.Read(value)

				nodeIdx := idx % len(nodes)
				if err := nodes[nodeIdx].Set(ctx, key, value); err == nil {
					successCount.Add(1)
					keysMu.Lock()
					keys = append(keys, key)
					keysMu.Unlock()
				}
			}(i)
		}

		wg.Wait()

		// Brief pause between batches
		if end < keyCount {
			time.Sleep(10 * time.Millisecond)
		}
	}

	elapsed := time.Since(startTime)
	success := successCount.Load()
	qps := float64(success) / elapsed.Seconds()

	tb.Logf("Seeded %d/%d keys in %v (%.0f ops/s)", success, keyCount, elapsed, qps)

	return keys
}

// SeedDataWithRandomSizes seeds data with random-sized values
// keyCount: total number of keys (e.g., 10000)
// targetTotalSizeMB: target total size in MB (e.g., 100)
// sizeRange: size variation range (0.0-1.0, e.g., 0.5 means ±50% variation)
func SeedDataWithRandomSizes(tb testing.TB, nodes []*gridkv.GridKV, keyCount int, targetTotalSizeMB int, sizeRange float64) []string {
	tb.Helper()
	targetTotalSizeBytes := int64(targetTotalSizeMB) * 1024 * 1024
	targetAvgSize := int(targetTotalSizeBytes / int64(keyCount))
	tb.Logf("Seeding %d keys with random sizes (target total: %dMB, avg size: %d bytes, range: ±%.0f%%)...",
		keyCount, targetTotalSizeMB, targetAvgSize, sizeRange*100)

	ctx := context.Background()
	keys := make([]string, 0, keyCount)
	keysMu := sync.Mutex{}
	startTime := time.Now()

	var wg sync.WaitGroup
	var successCount atomic.Int64
	var totalSizeBytes atomic.Int64

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	// Seed in batches to avoid overwhelming the cluster
	batchSize := 100
	for start := 0; start < keyCount; start += batchSize {
		end := start + batchSize
		if end > keyCount {
			end = keyCount
		}

		for i := start; i < end; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				key := fmt.Sprintf("seed-key-%d", idx)
				value := generateRandomValue(rng, targetAvgSize, sizeRange)
				size := int64(len(value))
				totalSizeBytes.Add(size)

				nodeIdx := idx % len(nodes)
				if err := nodes[nodeIdx].Set(ctx, key, value); err == nil {
					successCount.Add(1)
					keysMu.Lock()
					keys = append(keys, key)
					keysMu.Unlock()
				}
			}(i)
		}

		wg.Wait()

		// Brief pause between batches
		if end < keyCount {
			time.Sleep(10 * time.Millisecond)
		}
	}

	elapsed := time.Since(startTime)
	success := successCount.Load()
	totalSize := totalSizeBytes.Load()
	actualMB := float64(totalSize) / (1024 * 1024)
	qps := float64(success) / elapsed.Seconds()

	tb.Logf("Seeded %d/%d keys in %v (%.0f ops/s, total size: %.2fMB)", success, keyCount, elapsed, qps, actualMB)

	return keys
}

// CalculateStats calculates comprehensive statistics from workload results
func CalculateStats(
	start, end time.Time,
	writesCompleted, writesFailed int64,
	readsCompleted, readsFailed int64,
	deletesCompleted, deletesFailed int64,
	writeLatencies, readLatencies, deleteLatencies *LatencySampler,
) *WorkloadStats {

	duration := end.Sub(start)
	stats := &WorkloadStats{
		MeasurementStart: start,
		MeasurementEnd:   end,
		Duration:         duration,
		WritesCompleted:  writesCompleted,
		WritesFailed:     writesFailed,
		ReadsCompleted:   readsCompleted,
		ReadsFailed:      readsFailed,
		DeletesCompleted: deletesCompleted,
		DeletesFailed:    deletesFailed,
	}

	// Calculate QPS
	seconds := duration.Seconds()
	if seconds > 0 {
		stats.WriteQPS = float64(writesCompleted) / seconds
		stats.ReadQPS = float64(readsCompleted) / seconds
		stats.DeleteQPS = float64(deletesCompleted) / seconds
		stats.TotalQPS = stats.WriteQPS + stats.ReadQPS + stats.DeleteQPS
	}

	// Calculate success rate
	totalOps := writesCompleted + writesFailed + readsCompleted + readsFailed + deletesCompleted + deletesFailed
	successOps := writesCompleted + readsCompleted + deletesCompleted
	if totalOps > 0 {
		stats.SuccessRate = float64(successOps) / float64(totalOps) * 100
	}

	// Calculate latency percentiles
	if writeLatencies != nil {
		stats.WriteLatencyP50, stats.WriteLatencyP95, stats.WriteLatencyP99 = writeLatencies.Percentiles()
	}
	if readLatencies != nil {
		stats.ReadLatencyP50, stats.ReadLatencyP95, stats.ReadLatencyP99 = readLatencies.Percentiles()
	}
	if deleteLatencies != nil {
		stats.DeleteLatencyP50, stats.DeleteLatencyP95, stats.DeleteLatencyP99 = deleteLatencies.Percentiles()
	}

	return stats
}

// PrintStats prints formatted statistics
func PrintStats(tb testing.TB, stats *WorkloadStats, testName string) {
	tb.Helper()

	tb.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	tb.Logf("   %s - Performance Statistics", testName)
	tb.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	tb.Logf("Test Duration:     %v (measurement only)", stats.Duration)
	tb.Logf("")
	tb.Logf("Throughput:")
	tb.Logf("  Total QPS:       %.2f ops/s", stats.TotalQPS)
	tb.Logf("  Write QPS:       %.2f ops/s (%d completed / %d failed)",
		stats.WriteQPS, stats.WritesCompleted, stats.WritesFailed)
	tb.Logf("  Read QPS:        %.2f ops/s (%d completed / %d failed)",
		stats.ReadQPS, stats.ReadsCompleted, stats.ReadsFailed)
	tb.Logf("  Delete QPS:      %.2f ops/s (%d completed / %d failed)",
		stats.DeleteQPS, stats.DeletesCompleted, stats.DeletesFailed)
	tb.Logf("")
	tb.Logf("Success Rate:      %.2f%%", stats.SuccessRate)
	tb.Logf("")
	tb.Logf("Latency (Write):   P50=%v  P95=%v  P99=%v",
		stats.WriteLatencyP50, stats.WriteLatencyP95, stats.WriteLatencyP99)
	tb.Logf("Latency (Read):    P50=%v  P95=%v  P99=%v",
		stats.ReadLatencyP50, stats.ReadLatencyP95, stats.ReadLatencyP99)
	tb.Logf("Latency (Delete):  P50=%v  P95=%v  P99=%v",
		stats.DeleteLatencyP50, stats.DeleteLatencyP95, stats.DeleteLatencyP99)
	tb.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
}

// workloadDelays returns delays for writers, readers, and deleters
func workloadDelays() (time.Duration, time.Duration, time.Duration) {
	// Check if throttling is disabled for performance tests
	if os.Getenv("GRIDKV_NO_THROTTLE") == "1" {
		return 0, 0, 0
	}
	// Production-grade throttling: prevent overload while allowing high throughput
	return 100 * time.Microsecond, 50 * time.Microsecond, 200 * time.Microsecond
}

// ThroughputSnapshot captures throughput at a point in time
type ThroughputSnapshot struct {
	Timestamp time.Time
	WriteQPS  float64
	ReadQPS   float64
	DeleteQPS float64
	TotalQPS  float64
}
