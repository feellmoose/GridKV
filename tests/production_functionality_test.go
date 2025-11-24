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

// TestProductionFunctionalityCorrectness tests correctness under various environments
func TestProductionFunctionalityCorrectness(t *testing.T) {
	testCases := []struct {
		name      string
		envConfig *TestEnvironmentConfig
		duration  time.Duration
	}{
		{
			name: "LAN_Environment",
			envConfig: &TestEnvironmentConfig{
				NetworkProfile: network.ProfileLAN,
				NetworkType:    gridkv.TCP,
				NodeCount:      5,
				ReplicaCount:   3,
				BasePort:       60000,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    2048,
				ShardCount:     128,
			},
			duration: 30 * time.Second,
		},
		{
			name: "WAN_Environment",
			envConfig: &TestEnvironmentConfig{
				NetworkProfile: network.ProfileWAN,
				NetworkType:    gridkv.TCP,
				NodeCount:      10,
				ReplicaCount:   3,
				BasePort:       61000,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    4096,
				ShardCount:     256,
			},
			duration: 60 * time.Second,
		},
		{
			name: "Global_Environment",
			envConfig: &TestEnvironmentConfig{
				NetworkProfile: network.ProfileGlobal,
				NetworkType:    gridkv.TCP,
				NodeCount:      15,
				ReplicaCount:   3,
				BasePort:       62000,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    8192,
				ShardCount:     512,
			},
			duration: 90 * time.Second,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			testCorrectnessScenario(t, tc.envConfig, tc.duration)
		})
	}
}

func testCorrectnessScenario(t *testing.T, config *TestEnvironmentConfig, testDuration time.Duration) {
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
		writesSubmitted  atomic.Int64
		writesCompleted  atomic.Int64
		writesFailed     atomic.Int64
		readsSubmitted   atomic.Int64
		readsCompleted   atomic.Int64
		readsFailed      atomic.Int64
		readsMismatch    atomic.Int64
		deletesSubmitted atomic.Int64
		deletesCompleted atomic.Int64
		deletesFailed    atomic.Int64
	)

	var wg sync.WaitGroup

	// Writers
	numWriters := 20
	for w := 0; w < numWriters; w++ {
		wg.Add(1)
		go func(writerID int) {
			defer wg.Done()
			nodeIdx := writerID % len(nodes)
			for {
				select {
				case <-stopCh:
					return
				default:
					key := fmt.Sprintf("key-%d-%d", writerID, writesSubmitted.Load())
					value := []byte(fmt.Sprintf("value-%d-%d", writerID, time.Now().UnixNano()))
					writesSubmitted.Add(1)
					err := nodes[nodeIdx].Set(ctx, key, value)
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
	numReaders := 30
	for r := 0; r < numReaders; r++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			nodeIdx := readerID % len(nodes)
			for {
				select {
				case <-stopCh:
					return
				default:
					writtenKeysMu.RLock()
					if len(writtenKeys) == 0 {
						writtenKeysMu.RUnlock()
						time.Sleep(100 * time.Millisecond)
						continue
					}
					target := rand.Intn(len(writtenKeys))
					i := 0
					var key string
					var expectedValue []byte
					for k, v := range writtenKeys {
						if i == target {
							key = k
							expectedValue = make([]byte, len(v))
							copy(expectedValue, v)
							break
						}
						i++
					}
					writtenKeysMu.RUnlock()

					readsSubmitted.Add(1)
					value, err := nodes[nodeIdx].Get(ctx, key)
					if err != nil {
						readsFailed.Add(1)
					} else {
						readsCompleted.Add(1)
						if string(value) != string(expectedValue) {
							readsMismatch.Add(1)
						}
					}
					time.Sleep(50 * time.Millisecond)
				}
			}
		}(r)
	}

	// Deleters
	numDeleters := 5
	for d := 0; d < numDeleters; d++ {
		wg.Add(1)
		go func(deleterID int) {
			defer wg.Done()
			nodeIdx := deleterID % len(nodes)
			for {
				select {
				case <-stopCh:
					return
				default:
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

					deletesSubmitted.Add(1)
					err := nodes[nodeIdx].Delete(ctx, key)
					if err != nil {
						deletesFailed.Add(1)
					} else {
						deletesCompleted.Add(1)
						writtenKeysMu.Lock()
						delete(writtenKeys, key)
						writtenKeysMu.Unlock()
					}
					time.Sleep(50 * time.Millisecond)
				}
			}
		}(d)
	}

	time.Sleep(testDuration)
	close(stopCh)
	wg.Wait()

	// Final convergence verification
	finalSnapshot := make(map[string][]byte)
	writtenKeysMu.RLock()
	for k, v := range writtenKeys {
		valueCopy := make([]byte, len(v))
		copy(valueCopy, v)
		finalSnapshot[k] = valueCopy
	}
	writtenKeysMu.RUnlock()

	finalKeys := make([]string, 0, len(finalSnapshot))
	for k := range finalSnapshot {
		finalKeys = append(finalKeys, k)
	}

	verificationReads := 0
	verificationSuccess := 0
	verificationMismatch := 0
	maxVerifyWait := 5 * time.Second
	verifyInterval := 50 * time.Millisecond

	for _, key := range finalKeys {
		expectedValue := finalSnapshot[key]
		deadline := time.Now().Add(maxVerifyWait)
		ok := false
		for time.Now().Before(deadline) {
			verificationReads++
			idx := rand.Intn(len(nodes))
			value, err := nodes[idx].Get(ctx, key)
			if err == nil && string(value) == string(expectedValue) {
				verificationSuccess++
				ok = true
				break
			}
			time.Sleep(verifyInterval)
		}
		if !ok {
			verificationMismatch++
		}
	}

	// Calculate metrics
	writeSuccessRate := float64(writesCompleted.Load()) / float64(writesSubmitted.Load()) * 100
	readSuccessRate := float64(readsCompleted.Load()) / float64(readsSubmitted.Load()) * 100
	deleteSuccessRate := float64(deletesCompleted.Load()) / float64(deletesSubmitted.Load()) * 100
	convergenceRate := float64(0)
	if len(finalKeys) > 0 {
		convergenceRate = float64(verificationSuccess) / float64(len(finalKeys)) * 100
	}

	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("   Functionality Test Results")
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("Writes:  submitted=%d, completed=%d, failed=%d (%.2f%%)", writesSubmitted.Load(), writesCompleted.Load(), writesFailed.Load(), writeSuccessRate)
	t.Logf("Reads:   submitted=%d, completed=%d, failed=%d, mismatch=%d (%.2f%%)", readsSubmitted.Load(), readsCompleted.Load(), readsFailed.Load(), readsMismatch.Load(), readSuccessRate)
	t.Logf("Deletes: submitted=%d, completed=%d, failed=%d (%.2f%%)", deletesSubmitted.Load(), deletesCompleted.Load(), deletesFailed.Load(), deleteSuccessRate)
	t.Logf("Convergence: %d keys, %d successful, %d mismatches (%.2f%%)", len(finalKeys), verificationSuccess, verificationMismatch, convergenceRate)
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// Assertions
	if writeSuccessRate < 90 {
		t.Errorf("Write success rate %.2f%% below 90%%", writeSuccessRate)
	}
	if readSuccessRate < 80 {
		t.Errorf("Read success rate %.2f%% below 80%%", readSuccessRate)
	}
	if len(finalKeys) > 0 && convergenceRate < 95 {
		t.Errorf("Convergence rate %.2f%% below 95%%", convergenceRate)
	}
}

// TestProductionOperationRatio tests different operation ratios
func TestProductionOperationRatio(t *testing.T) {
	testCases := []struct {
		name           string
		writeRatio     int
		readRatio      int
		deleteRatio    int
		envConfig      *TestEnvironmentConfig
		duration       time.Duration
		minSuccessRate float64
	}{
		{
			name:        "WriteHeavy_80_15_5",
			writeRatio:  80,
			readRatio:   15,
			deleteRatio: 5,
			envConfig: &TestEnvironmentConfig{
				NetworkProfile: network.ProfileLAN,
				NetworkType:    gridkv.TCP,
				NodeCount:      5,
				ReplicaCount:   3,
				BasePort:       70000,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    2048,
				ShardCount:     128,
			},
			duration:       30 * time.Second,
			minSuccessRate: 85,
		},
		{
			name:        "ReadHeavy_10_85_5",
			writeRatio:  10,
			readRatio:   85,
			deleteRatio: 5,
			envConfig: &TestEnvironmentConfig{
				NetworkProfile: network.ProfileLAN,
				NetworkType:    gridkv.TCP,
				NodeCount:      5,
				ReplicaCount:   3,
				BasePort:       71000,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    2048,
				ShardCount:     128,
			},
			duration:       30 * time.Second,
			minSuccessRate: 90,
		},
		{
			name:        "Balanced_33_33_34",
			writeRatio:  33,
			readRatio:   33,
			deleteRatio: 34,
			envConfig: &TestEnvironmentConfig{
				NetworkProfile: network.ProfileLAN,
				NetworkType:    gridkv.TCP,
				NodeCount:      5,
				ReplicaCount:   3,
				BasePort:       72000,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    2048,
				ShardCount:     128,
			},
			duration:       30 * time.Second,
			minSuccessRate: 85,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			testOperationRatioScenario(t, tc.envConfig, tc.writeRatio, tc.readRatio, tc.deleteRatio, tc.duration, tc.minSuccessRate)
		})
	}
}

func testOperationRatioScenario(t *testing.T, config *TestEnvironmentConfig, writeRatio, readRatio, deleteRatio int, testDuration time.Duration, minSuccessRate float64) {
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
		writesCompleted  atomic.Int64
		writesFailed     atomic.Int64
		readsCompleted   atomic.Int64
		readsFailed      atomic.Int64
		deletesCompleted atomic.Int64
		deletesFailed    atomic.Int64
	)

	var wg sync.WaitGroup
	totalWorkers := 50

	// Calculate worker distribution
	writeWorkers := totalWorkers * writeRatio / 100
	readWorkers := totalWorkers * readRatio / 100
	deleteWorkers := totalWorkers * deleteRatio / 100

	// Writers
	for w := 0; w < writeWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			nodeIdx := workerID % len(nodes)
			for {
				select {
				case <-stopCh:
					return
				default:
					key := fmt.Sprintf("ratio-w-%d-%d", workerID, time.Now().UnixNano())
					value := []byte(fmt.Sprintf("value-%d", time.Now().UnixNano()))
					err := nodes[nodeIdx].Set(ctx, key, value)
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
	for r := 0; r < readWorkers; r++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			nodeIdx := workerID % len(nodes)
			for {
				select {
				case <-stopCh:
					return
				default:
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

					_, err := nodes[nodeIdx].Get(ctx, key)
					if err != nil {
						readsFailed.Add(1)
					} else {
						readsCompleted.Add(1)
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
		}(r)
	}

	// Deleters
	for d := 0; d < deleteWorkers; d++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			nodeIdx := workerID % len(nodes)
			for {
				select {
				case <-stopCh:
					return
				default:
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

					err := nodes[nodeIdx].Delete(ctx, key)
					if err != nil {
						deletesFailed.Add(1)
					} else {
						deletesCompleted.Add(1)
						writtenKeysMu.Lock()
						delete(writtenKeys, key)
						writtenKeysMu.Unlock()
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
		}(d)
	}

	time.Sleep(testDuration)
	close(stopCh)
	wg.Wait()

	totalOps := writesCompleted.Load() + readsCompleted.Load() + deletesCompleted.Load()
	totalFailed := writesFailed.Load() + readsFailed.Load() + deletesFailed.Load()
	successRate := float64(totalOps) / float64(totalOps+totalFailed) * 100

	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("   Operation Ratio Test Results")
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("Ratio: Write=%d%%, Read=%d%%, Delete=%d%%", writeRatio, readRatio, deleteRatio)
	t.Logf("Writes:  completed=%d, failed=%d", writesCompleted.Load(), writesFailed.Load())
	t.Logf("Reads:   completed=%d, failed=%d", readsCompleted.Load(), readsFailed.Load())
	t.Logf("Deletes: completed=%d, failed=%d", deletesCompleted.Load(), deletesFailed.Load())
	t.Logf("Overall success rate: %.2f%% (min: %.2f%%)", successRate, minSuccessRate)
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	if successRate < minSuccessRate {
		t.Errorf("Success rate %.2f%% below minimum %.2f%%", successRate, minSuccessRate)
	}
}

// TestProductionCoreOperations tests basic KV operations
func TestProductionCoreOperations(t *testing.T) {
	config := &TestEnvironmentConfig{
		NetworkProfile: network.ProfileLAN,
		NetworkType:    gridkv.TCP,
		NodeCount:      1,
		ReplicaCount:   1,
		BasePort:       110000,
		StorageBackend: gridkv.BackendMemory,
		MaxMemoryMB:    512,
		ShardCount:     0,
	}

	sim := NewTestEnvironmentSimulator(config)
	if err := sim.SetupCluster(t); err != nil {
		t.Fatalf("Failed to setup cluster: %v", err)
	}
	defer sim.Cleanup()

	nodes := sim.GetNodes()
	ctx := context.Background()
	kv := nodes[0]

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

	t.Run("MultipleOperations", func(t *testing.T) {
		for i := 0; i < 10; i++ {
			key := fmt.Sprintf("multi-%d", i)
			value := []byte(fmt.Sprintf("value-%d", i))
			err := kv.Set(ctx, key, value)
			if err != nil {
				t.Errorf("Set %s failed: %v", key, err)
			}
		}

		for i := 0; i < 10; i++ {
			key := fmt.Sprintf("multi-%d", i)
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

// TestProductionDataLossProbability tests data loss under various failure scenarios
func TestProductionDataLossProbability(t *testing.T) {
	config := &TestEnvironmentConfig{
		NetworkProfile: network.ProfileLAN,
		NetworkType:    gridkv.TCP,
		NodeCount:      15,
		ReplicaCount:   3,
		BasePort:       111000,
		StorageBackend: gridkv.BackendMemorySharded,
		MaxMemoryMB:    2048,
		ShardCount:     256,
	}

	sim := NewTestEnvironmentSimulator(config)
	if err := sim.SetupCluster(t); err != nil {
		t.Fatalf("Failed to setup cluster: %v", err)
	}
	defer sim.Cleanup()

	nodes := sim.GetNodes()
	ctx := context.Background()
	stopCh := make(chan struct{})

	type KeyValue struct {
		key     string
		value   []byte
		written time.Time
	}
	allKeys := make(map[string]*KeyValue)
	allKeysMu := sync.RWMutex{}

	var (
		writesCompleted atomic.Int64
		readsCompleted  atomic.Int64
		readsLost       atomic.Int64
		readsMismatch   atomic.Int64
	)

	var wg sync.WaitGroup

	// Writers
	for w := 0; w < 30; w++ {
		wg.Add(1)
		go func(writerID int) {
			defer wg.Done()
			nodeIdx := writerID % len(nodes)
			for {
				select {
				case <-stopCh:
					return
				default:
					key := fmt.Sprintf("loss-key-%d-%d", writerID, time.Now().UnixNano())
					value := []byte(fmt.Sprintf("loss-value-%d-%d", writerID, time.Now().UnixNano()))
					err := nodes[nodeIdx].Set(ctx, key, value)
					if err == nil {
						writesCompleted.Add(1)
						allKeysMu.Lock()
						allKeys[key] = &KeyValue{
							key:     key,
							value:   value,
							written: time.Now(),
						}
						allKeysMu.Unlock()
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
		}(w)
	}

	// Readers
	for r := 0; r < 20; r++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			nodeIdx := readerID % len(nodes)
			for {
				select {
				case <-stopCh:
					return
				default:
					allKeysMu.RLock()
					keys := make([]string, 0, len(allKeys))
					for k := range allKeys {
						keys = append(keys, k)
					}
					allKeysMu.RUnlock()

					if len(keys) == 0 {
						time.Sleep(50 * time.Millisecond)
						continue
					}

					key := keys[readerID%len(keys)]
					allKeysMu.RLock()
					expectedKV := allKeys[key]
					allKeysMu.RUnlock()

					if expectedKV == nil {
						continue
					}

					value, err := nodes[nodeIdx].Get(ctx, key)
					if err != nil {
						readsLost.Add(1)
					} else {
						readsCompleted.Add(1)
						if string(value) != string(expectedKV.value) {
							readsMismatch.Add(1)
						}
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
		}(r)
	}

	time.Sleep(20 * time.Second)
	close(stopCh)
	wg.Wait()
	time.Sleep(2 * time.Second)

	// Final verification
	allKeysMu.RLock()
	totalKeys := len(allKeys)
	keysToVerify := make([]string, 0, totalKeys)
	for k := range allKeys {
		keysToVerify = append(keysToVerify, k)
	}
	allKeysMu.RUnlock()

	verificationSuccess := 0
	verificationLost := 0
	verificationMismatch := 0

	for _, key := range keysToVerify {
		allKeysMu.RLock()
		expectedKV := allKeys[key]
		allKeysMu.RUnlock()

		if expectedKV == nil {
			continue
		}

		found := false
		for i := 0; i < len(nodes) && !found; i++ {
			value, err := nodes[i].Get(ctx, key)
			if err == nil {
				found = true
				if string(value) == string(expectedKV.value) {
					verificationSuccess++
				} else {
					verificationMismatch++
				}
				break
			}
		}
		if !found {
			verificationLost++
		}
	}

	lossProbability := float64(verificationLost) / float64(totalKeys) * 100
	mismatchProbability := float64(verificationMismatch) / float64(totalKeys) * 100

	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("   Data Loss Analysis")
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("Total keys:            %d", totalKeys)
	t.Logf("Verification success:    %d", verificationSuccess)
	t.Logf("Verification lost:       %d", verificationLost)
	t.Logf("Verification mismatch:   %d", verificationMismatch)
	t.Logf("Data loss probability:   %.4f%%", lossProbability)
	t.Logf("Data mismatch probability: %.4f%%", mismatchProbability)
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	if lossProbability > 1.0 {
		t.Errorf("Data loss probability too high: %.4f%%", lossProbability)
	}
	if mismatchProbability > 0.1 {
		t.Errorf("Data mismatch probability too high: %.4f%%", mismatchProbability)
	}
}

// TestProductionConsistencyConvergence tests eventual consistency convergence
func TestProductionConsistencyConvergence(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping convergence test in short mode")
	}

	config := &TestEnvironmentConfig{
		NetworkProfile: network.ProfileLAN,
		NetworkType:    gridkv.TCP,
		NodeCount:      3,
		ReplicaCount:   3,
		BasePort:       112000,
		StorageBackend: gridkv.BackendMemorySharded,
		MaxMemoryMB:    1024,
		ShardCount:     128,
	}

	sim := NewTestEnvironmentSimulator(config)
	if err := sim.SetupCluster(t); err != nil {
		t.Fatalf("Failed to setup cluster: %v", err)
	}
	defer sim.Cleanup()

	nodes := sim.GetNodes()
	sim.WaitForHealthyNodes(t, 3, 5*time.Second)

	const totalWrites = 60
	entries := make([]struct {
		key string
		val []byte
	}, totalWrites)
	for i := 0; i < totalWrites; i++ {
		entries[i].key = fmt.Sprintf("consistency-%d-%d", i, time.Now().UnixNano())
		entries[i].val = []byte(fmt.Sprintf("value-%d", i))
	}

	ctxTimeout := 4 * time.Second
	writeStart := time.Now()

	var wg sync.WaitGroup
	var writeErr error
	var writeErrMu sync.Mutex

	wg.Add(totalWrites)
	for i := range entries {
		entry := entries[i]
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), ctxTimeout)
			defer cancel()
			if err := nodes[0].Set(ctx, entry.key, entry.val); err != nil {
				writeErrMu.Lock()
				if writeErr == nil {
					writeErr = err
				}
				writeErrMu.Unlock()
			}
		}()
	}
	wg.Wait()

	if writeErr != nil {
		t.Fatalf("writes failed: %v", writeErr)
	}

	elapsedWrites := time.Since(writeStart)
	totalQPS := float64(totalWrites) / elapsedWrites.Seconds()
	t.Logf("write burst: %d ops in %s (%.0f ops/sec)", totalWrites, elapsedWrites, totalQPS)
	if totalQPS < 200 {
		t.Fatalf("write throughput too low: %.0f ops/sec", totalQPS)
	}

	// Wait for replication
	for flushRound := 0; flushRound < 3; flushRound++ {
		for _, node := range nodes {
			node.FlushAllPipelines()
		}
		time.Sleep(150 * time.Millisecond)
	}

	// Verify convergence
	consistencyDeadline := time.Now().Add(3 * time.Second)
	consistent := false
	for !consistent && time.Now().Before(consistencyDeadline) {
		consistent = true
		for _, entry := range entries {
			for _, node := range nodes {
				ctx, cancel := context.WithTimeout(context.Background(), ctxTimeout)
				val, err := node.Get(ctx, entry.key)
				cancel()
				if err != nil || string(val) != string(entry.val) {
					consistent = false
					break
				}
			}
			if !consistent {
				break
			}
		}
		if !consistent {
			time.Sleep(50 * time.Millisecond)
		}
	}

	if !consistent {
		t.Fatalf("replicas did not converge within 3s")
	}

	t.Logf("consistency convergence in %s", time.Since(writeStart))
}

// TestProductionCriticalMessageDelivery tests critical message delivery under load
func TestProductionCriticalMessageDelivery(t *testing.T) {
	config := &TestEnvironmentConfig{
		NetworkProfile: network.ProfileLAN,
		NetworkType:    gridkv.TCP,
		NodeCount:      10,
		ReplicaCount:   3,
		BasePort:       113000,
		StorageBackend: gridkv.BackendMemorySharded,
		MaxMemoryMB:    1024,
		ShardCount:     128,
	}

	sim := NewTestEnvironmentSimulator(config)
	if err := sim.SetupCluster(t); err != nil {
		t.Fatalf("Failed to setup cluster: %v", err)
	}
	defer sim.Cleanup()

	nodes := sim.GetNodes()
	ctx := context.Background()
	stopCh := make(chan struct{})

	var (
		opsSubmitted   atomic.Int64
		opsCompleted   atomic.Int64
		opsFailed      atomic.Int64
		readsCompleted atomic.Int64
		readsFailed    atomic.Int64
	)

	var wg sync.WaitGroup
	concurrentOps := 500

	// Writers
	for w := 0; w < concurrentOps; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			nodeIdx := workerID % len(nodes)
			for {
				select {
				case <-stopCh:
					return
				default:
					key := fmt.Sprintf("critical-key-%d-%d", workerID, opsSubmitted.Load())
					value := []byte(fmt.Sprintf("critical-value-%d", workerID))
					opsSubmitted.Add(1)
					err := nodes[nodeIdx].Set(ctx, key, value)
					if err != nil {
						opsFailed.Add(1)
					} else {
						opsCompleted.Add(1)
					}
					time.Sleep(1 * time.Millisecond)
				}
			}
		}(w)
	}

	// Readers
	for w := 0; w < concurrentOps/2; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			nodeIdx := workerID % len(nodes)
			for {
				select {
				case <-stopCh:
					return
				default:
					key := fmt.Sprintf("critical-key-%d", workerID%100)
					_, err := nodes[nodeIdx].Get(ctx, key)
					if err != nil {
						readsFailed.Add(1)
					} else {
						readsCompleted.Add(1)
					}
					time.Sleep(2 * time.Millisecond)
				}
			}
		}(w)
	}

	time.Sleep(10 * time.Second)
	close(stopCh)
	wg.Wait()

	writeSuccessRate := float64(opsCompleted.Load()) / float64(opsSubmitted.Load()) * 100
	readSuccessRate := float64(0)
	if readsCompleted.Load()+readsFailed.Load() > 0 {
		readSuccessRate = float64(readsCompleted.Load()) / float64(readsCompleted.Load()+readsFailed.Load()) * 100
	}

	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("   Critical Message Delivery Test")
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	t.Logf("Write success rate: %.2f%%", writeSuccessRate)
	t.Logf("Read success rate:  %.2f%%", readSuccessRate)
	t.Logf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	if writeSuccessRate < 50 {
		t.Errorf("Write success rate too low: %.2f%%", writeSuccessRate)
	}
	if readsCompleted.Load() > 0 && readSuccessRate < 30 {
		t.Errorf("Read success rate too low: %.2f%%", readSuccessRate)
	}
}
