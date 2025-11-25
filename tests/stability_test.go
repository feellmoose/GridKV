package tests

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gridkv "github.com/feellmoose/gridkv"
	"github.com/feellmoose/gridkv/internal/utils/network"
)

// TestStabilityExtremeLatency tests system stability under extreme network latency
func TestStabilityExtremeLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping extreme latency test in short mode")
	}
	testCases := []struct {
		name           string
		envConfig      *TestEnvironmentConfig
		duration       time.Duration
		minSuccessRate float64
	}{
		{
			name: "Satellite_Link",
			envConfig: &TestEnvironmentConfig{
				NetworkProfile: network.ProfileSatellite,
				NetworkType:    networkTypeFromEnv(gridkv.TCP),
				NodeCount:      5,
				ReplicaCount:   3,
				BasePort:       80000,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    2048,
				ShardCount:     128,
			},
			duration:       60 * time.Second,
			minSuccessRate: 70,
		},
		{
			name: "Global_HighLatency",
			envConfig: &TestEnvironmentConfig{
				NetworkProfile: network.ProfileGlobal,
				NetworkType:    networkTypeFromEnv(gridkv.TCP),
				NodeCount:      10,
				ReplicaCount:   3,
				BasePort:       81000,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    4096,
				ShardCount:     256,
			},
			duration:       90 * time.Second,
			minSuccessRate: 80,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			testExtremeLatencyScenario(t, tc.envConfig, tc.duration, tc.minSuccessRate)
		})
	}
}

func testExtremeLatencyScenario(t *testing.T, config *TestEnvironmentConfig, testDuration time.Duration, minSuccessRate float64) {
	sim := NewTestEnvironmentSimulator(config)
	if err := sim.SetupCluster(t); err != nil {
		t.Fatalf("Failed to setup cluster: %v", err)
	}
	defer sim.Cleanup()

	nodes := sim.GetNodes()
	ctx := context.Background()
	stopCh := make(chan struct{})

	var (
		writesCompleted atomic.Int64
		writesFailed    atomic.Int64
		readsCompleted  atomic.Int64
		readsFailed     atomic.Int64
	)

	var wg sync.WaitGroup

	// Writers
	for w := 0; w < 20; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			nodeIdx := workerID % len(nodes)
			for {
				select {
				case <-stopCh:
					return
				default:
					key := fmt.Sprintf("latency-w-%d-%d", workerID, time.Now().UnixNano())
					value := []byte(fmt.Sprintf("value-%d", time.Now().UnixNano()))
					opCtx, opCancel := context.WithTimeout(ctx, 10*time.Second)
					err := nodes[nodeIdx].Set(opCtx, key, value)
					opCancel()
					if err != nil {
						writesFailed.Add(1)
					} else {
						writesCompleted.Add(1)
					}
					time.Sleep(100 * time.Millisecond)
				}
			}
		}(w)
	}

	// Readers
	time.Sleep(5 * time.Second)
	for r := 0; r < 30; r++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			nodeIdx := workerID % len(nodes)
			for {
				select {
				case <-stopCh:
					return
				default:
					key := fmt.Sprintf("latency-w-%d-%d", workerID%len(nodes), rand.Intn(1000))
					opCtx, opCancel := context.WithTimeout(ctx, 10*time.Second)
					_, err := nodes[nodeIdx].Get(opCtx, key)
					opCancel()
					if err != nil {
						readsFailed.Add(1)
					} else {
						readsCompleted.Add(1)
					}
					time.Sleep(50 * time.Millisecond)
				}
			}
		}(r)
	}

	time.Sleep(testDuration)
	close(stopCh)
	wg.Wait()

	totalOps := writesCompleted.Load() + readsCompleted.Load()
	totalFailed := writesFailed.Load() + readsFailed.Load()
	successRate := float64(totalOps) / float64(totalOps+totalFailed) * 100

	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("   Extreme Latency Test Results")
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("Writes: completed=%d, failed=%d", writesCompleted.Load(), writesFailed.Load())
	t.Logf("Reads:  completed=%d, failed=%d", readsCompleted.Load(), readsFailed.Load())
	t.Logf("Success rate: %.2f%% (min: %.2f%%)", successRate, minSuccessRate)
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	if successRate < minSuccessRate {
		t.Errorf("Success rate %.2f%% below minimum %.2f%%", successRate, minSuccessRate)
	}
}

// TestStabilityNodeFailures tests system stability when nodes fail
func TestStabilityNodeFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping node failures test in short mode")
	}
	testCases := []struct {
		name           string
		envConfig      *TestEnvironmentConfig
		failureCount   int
		failureDelay   time.Duration
		duration       time.Duration
		minSuccessRate float64
	}{
		{
			name: "SingleNodeFailure",
			envConfig: &TestEnvironmentConfig{
				NetworkProfile: network.ProfileLAN,
				NetworkType:    networkTypeFromEnv(gridkv.TCP),
				NodeCount:      5,
				ReplicaCount:   3,
				BasePort:       90000,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    2048,
				ShardCount:     128,
			},
			failureCount:   1,
			failureDelay:   10 * time.Second,
			duration:       60 * time.Second,
			minSuccessRate: 85,
		},
		{
			name: "MultipleNodeFailures",
			envConfig: &TestEnvironmentConfig{
				NetworkProfile: network.ProfileLAN,
				NetworkType:    networkTypeFromEnv(gridkv.TCP),
				NodeCount:      10,
				ReplicaCount:   3,
				BasePort:       91000,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    4096,
				ShardCount:     256,
			},
			failureCount:   3,
			failureDelay:   15 * time.Second,
			duration:       90 * time.Second,
			minSuccessRate: 75,
		},
		{
			name: "HalfNodesFailure",
			envConfig: &TestEnvironmentConfig{
				NetworkProfile: network.ProfileLAN,
				NetworkType:    networkTypeFromEnv(gridkv.TCP),
				NodeCount:      8,
				ReplicaCount:   3,
				BasePort:       92000,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    4096,
				ShardCount:     256,
			},
			failureCount:   4,
			failureDelay:   20 * time.Second,
			duration:       120 * time.Second,
			minSuccessRate: 60,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			testNodeFailureScenario(t, tc.envConfig, tc.failureCount, tc.failureDelay, tc.duration, tc.minSuccessRate)
		})
	}
}

func testNodeFailureScenario(t *testing.T, config *TestEnvironmentConfig, failureCount int, failureDelay time.Duration, testDuration time.Duration, minSuccessRate float64) {
	sim := NewTestEnvironmentSimulator(config)
	if err := sim.SetupCluster(t); err != nil {
		t.Fatalf("Failed to setup cluster: %v", err)
	}
	defer sim.Cleanup()

	nodes := sim.GetNodes()
	ctx := context.Background()
	stopCh := make(chan struct{})

	writtenKeys := make(map[string][]byte)
	writtenKeysMu := sync.RWMutex{}

	var (
		writesCompleted atomic.Int64
		writesFailed    atomic.Int64
		readsCompleted  atomic.Int64
		readsFailed     atomic.Int64
	)

	var wg sync.WaitGroup

	// Writers
	for w := 0; w < 20; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
					// Select random live node
					allNodes := sim.GetNodes()
					liveNodes := make([]*gridkv.GridKV, 0)
					for _, n := range allNodes {
						if n != nil {
							liveNodes = append(liveNodes, n)
						}
					}

					if len(liveNodes) == 0 {
						time.Sleep(100 * time.Millisecond)
						continue
					}

					nodeIdx := workerID % len(liveNodes)
					key := fmt.Sprintf("failure-w-%d-%d", workerID, time.Now().UnixNano())
					value := []byte(fmt.Sprintf("value-%d", time.Now().UnixNano()))
					err := liveNodes[nodeIdx].Set(ctx, key, value)
					if err != nil {
						writesFailed.Add(1)
					} else {
						writesCompleted.Add(1)
						writtenKeysMu.Lock()
						writtenKeys[key] = value
						writtenKeysMu.Unlock()
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
		}(w)
	}

	// Readers
	time.Sleep(2 * time.Second)
	for r := 0; r < 30; r++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
					sim.mu.RLock()
					liveNodes := make([]*gridkv.GridKV, 0)
					for _, n := range sim.nodes {
						if n != nil {
							liveNodes = append(liveNodes, n)
						}
					}
					sim.mu.RUnlock()

					if len(liveNodes) == 0 {
						time.Sleep(100 * time.Millisecond)
						continue
					}

					writtenKeysMu.RLock()
					if len(writtenKeys) == 0 {
						writtenKeysMu.RUnlock()
						time.Sleep(100 * time.Millisecond)
						continue
					}
					target := rand.Intn(len(writtenKeys))
					i := 0
					var key string
					for k := range writtenKeys {
						if i == target {
							key = k
							break
						}
						i++
					}
					writtenKeysMu.RUnlock()

					nodeIdx := workerID % len(liveNodes)
					_, err := liveNodes[nodeIdx].Get(ctx, key)
					if err != nil {
						readsFailed.Add(1)
					} else {
						readsCompleted.Add(1)
					}
					time.Sleep(50 * time.Millisecond)
				}
			}
		}(r)
	}

	// Trigger node failures
	time.Sleep(failureDelay)
	failureIndices := make([]int, 0, failureCount)
	for i := 0; i < failureCount && i < len(nodes)-1; i++ {
		idx := i + 1
		failureIndices = append(failureIndices, idx)
	}

	t.Logf("Shutting down %d nodes: %v", len(failureIndices), failureIndices)
	if err := sim.ShutdownNodes(failureIndices, 5*time.Second); err != nil {
		t.Logf("Warning: error shutting down nodes: %v", err)
	}

	time.Sleep(testDuration)
	close(stopCh)
	wg.Wait()

	// Wait for cluster to stabilize
	time.Sleep(5 * time.Second)

	totalOps := writesCompleted.Load() + readsCompleted.Load()
	totalFailed := writesFailed.Load() + readsFailed.Load()
	successRate := float64(0)
	if totalOps+totalFailed > 0 {
		successRate = float64(totalOps) / float64(totalOps+totalFailed) * 100
	}

	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("   Node Failure Test Results")
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("Failed nodes: %d", len(failureIndices))
	t.Logf("Writes: completed=%d, failed=%d", writesCompleted.Load(), writesFailed.Load())
	t.Logf("Reads:  completed=%d, failed=%d", readsCompleted.Load(), readsFailed.Load())
	t.Logf("Success rate: %.2f%% (min: %.2f%%)", successRate, minSuccessRate)
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	if successRate < minSuccessRate {
		t.Errorf("Success rate %.2f%% below minimum %.2f%%", successRate, minSuccessRate)
	}
}

// TestStabilitySuddenShutdown tests system when majority of nodes shut down suddenly
func TestStabilitySuddenShutdown(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping sudden shutdown test in short mode")
	}
	testCases := []struct {
		name           string
		envConfig      *TestEnvironmentConfig
		shutdownRatio  float64
		shutdownDelay  time.Duration
		duration       time.Duration
		minSuccessRate float64
	}{
		{
			name: "MajorityShutdown_60Percent",
			envConfig: &TestEnvironmentConfig{
				NetworkProfile: network.ProfileLAN,
				NetworkType:    networkTypeFromEnv(gridkv.TCP),
				NodeCount:      10,
				ReplicaCount:   3,
				BasePort:       93000,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    4096,
				ShardCount:     256,
			},
			shutdownRatio:  0.6,
			shutdownDelay:  15 * time.Second,
			duration:       90 * time.Second,
			minSuccessRate: 50,
		},
		{
			name: "MajorityShutdown_70Percent",
			envConfig: &TestEnvironmentConfig{
				NetworkProfile: network.ProfileLAN,
				NetworkType:    networkTypeFromEnv(gridkv.TCP),
				NodeCount:      10,
				ReplicaCount:   3,
				BasePort:       94000,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    4096,
				ShardCount:     256,
			},
			shutdownRatio:  0.7,
			shutdownDelay:  20 * time.Second,
			duration:       120 * time.Second,
			minSuccessRate: 40,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			testSuddenShutdownScenario(t, tc.envConfig, tc.shutdownRatio, tc.shutdownDelay, tc.duration, tc.minSuccessRate)
		})
	}
}

func testSuddenShutdownScenario(t *testing.T, config *TestEnvironmentConfig, shutdownRatio float64, shutdownDelay time.Duration, testDuration time.Duration, minSuccessRate float64) {
	sim := NewTestEnvironmentSimulator(config)
	if err := sim.SetupCluster(t); err != nil {
		t.Fatalf("Failed to setup cluster: %v", err)
	}
	defer sim.Cleanup()

	nodes := sim.GetNodes()
	ctx := context.Background()
	stopCh := make(chan struct{})

	var (
		writesCompleted atomic.Int64
		writesFailed    atomic.Int64
		readsCompleted  atomic.Int64
		readsFailed     atomic.Int64
	)

	var wg sync.WaitGroup

	// Writers
	for w := 0; w < 15; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
					sim.mu.RLock()
					liveNodes := make([]*gridkv.GridKV, 0)
					for _, n := range sim.nodes {
						if n != nil {
							liveNodes = append(liveNodes, n)
						}
					}
					sim.mu.RUnlock()

					if len(liveNodes) == 0 {
						time.Sleep(100 * time.Millisecond)
						continue
					}

					nodeIdx := workerID % len(liveNodes)
					key := fmt.Sprintf("shutdown-w-%d-%d", workerID, time.Now().UnixNano())
					value := []byte(fmt.Sprintf("value-%d", time.Now().UnixNano()))
					opCtx, opCancel := context.WithTimeout(ctx, 5*time.Second)
					err := liveNodes[nodeIdx].Set(opCtx, key, value)
					opCancel()
					if err != nil {
						writesFailed.Add(1)
					} else {
						writesCompleted.Add(1)
					}
					time.Sleep(50 * time.Millisecond)
				}
			}
		}(w)
	}

	// Readers
	time.Sleep(2 * time.Second)
	for r := 0; r < 20; r++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
					sim.mu.RLock()
					liveNodes := make([]*gridkv.GridKV, 0)
					for _, n := range sim.nodes {
						if n != nil {
							liveNodes = append(liveNodes, n)
						}
					}
					sim.mu.RUnlock()

					if len(liveNodes) == 0 {
						time.Sleep(100 * time.Millisecond)
						continue
					}

					key := fmt.Sprintf("shutdown-w-%d-%d", workerID%len(nodes), rand.Intn(1000))
					nodeIdx := workerID % len(liveNodes)
					opCtx, opCancel := context.WithTimeout(ctx, 5*time.Second)
					_, err := liveNodes[nodeIdx].Get(opCtx, key)
					opCancel()
					if err != nil {
						readsFailed.Add(1)
					} else {
						readsCompleted.Add(1)
					}
					time.Sleep(50 * time.Millisecond)
				}
			}
		}(r)
	}

	// Trigger sudden shutdown
	time.Sleep(shutdownDelay)
	shutdownCount := int(float64(len(nodes)) * shutdownRatio)
	shutdownIndices := make([]int, 0, shutdownCount)
	for i := 1; i < len(nodes) && len(shutdownIndices) < shutdownCount; i++ {
		shutdownIndices = append(shutdownIndices, i)
	}

	t.Logf("Sudden shutdown: %d nodes (%.1f%%)", len(shutdownIndices), shutdownRatio*100)
	if err := sim.ShutdownNodes(shutdownIndices, 3*time.Second); err != nil {
		t.Logf("Warning: error during shutdown: %v", err)
	}

	time.Sleep(testDuration)
	close(stopCh)
	wg.Wait()

	time.Sleep(5 * time.Second)

	totalOps := writesCompleted.Load() + readsCompleted.Load()
	totalFailed := writesFailed.Load() + readsFailed.Load()
	successRate := float64(0)
	if totalOps+totalFailed > 0 {
		successRate = float64(totalOps) / float64(totalOps+totalFailed) * 100
	}

	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("   Sudden Shutdown Test Results")
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("Shutdown: %d nodes (%.1f%%)", len(shutdownIndices), shutdownRatio*100)
	t.Logf("Writes: completed=%d, failed=%d", writesCompleted.Load(), writesFailed.Load())
	t.Logf("Reads:  completed=%d, failed=%d", readsCompleted.Load(), readsFailed.Load())
	t.Logf("Success rate: %.2f%% (min: %.2f%%)", successRate, minSuccessRate)
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	if successRate < minSuccessRate {
		t.Errorf("Success rate %.2f%% below minimum %.2f%%", successRate, minSuccessRate)
	}
}
