package tests

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gridkv "github.com/feellmoose/gridkv"
	"github.com/feellmoose/gridkv/internal/utils/network"
)

var prodLongRuntime = os.Getenv("GRIDKV_PROD_TEST_LONG") == "1"

func prodDuration(short, long time.Duration) time.Duration {
	if prodLongRuntime {
		return long
	}
	return short
}

const gibibyte int64 = 1 << 30

func prodDataSize(shortBytes, longBytes int64) int64 {
	if prodLongRuntime {
		return longBytes
	}
	return shortBytes
}

// TestProductionFunctionalityCorrectness tests correctness under various environments
func TestProductionFunctionalityCorrectness(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping functionality test in short mode")
	}
	testCases := []struct {
		name      string
		envConfig *TestEnvironmentConfig
		duration  time.Duration
	}{
		{
			name: "LAN_Environment",
			envConfig: &TestEnvironmentConfig{
				NetworkProfile: network.ProfileLAN,
				NetworkType:    networkTypeFromEnv(gridkv.TCP),
				NodeCount:      5,
				ReplicaCount:   3,
				BasePort:       60000,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    2048,
				ShardCount:     128,
			},
			duration: prodDuration(10*time.Second, 30*time.Second),
		},
		{
			name: "WAN_Environment",
			envConfig: &TestEnvironmentConfig{
				NetworkProfile: network.ProfileWAN,
				NetworkType:    networkTypeFromEnv(gridkv.TCP),
				NodeCount:      10,
				ReplicaCount:   3,
				BasePort:       61000,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    4096,
				ShardCount:     256,
			},
			duration: prodDuration(20*time.Second, 60*time.Second),
		},
		{
			name: "Global_Environment",
			envConfig: &TestEnvironmentConfig{
				NetworkProfile: network.ProfileGlobal,
				NetworkType:    networkTypeFromEnv(gridkv.TCP),
				NodeCount:      15,
				ReplicaCount:   3,
				BasePort:       62000,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    8192,
				ShardCount:     512,
			},
			duration: prodDuration(30*time.Second, 90*time.Second),
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
	sim.WaitForHealthyNodes(t, config.NodeCount, 10*time.Second)
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
	if testing.Short() {
		t.Skip("Skipping operation ratio test in short mode")
	}
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
				NetworkType:    networkTypeFromEnv(gridkv.TCP),
				NodeCount:      5,
				ReplicaCount:   3,
				BasePort:       63000,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    2048,
				ShardCount:     128,
			},
			duration:       prodDuration(10*time.Second, 30*time.Second),
			minSuccessRate: 85,
		},
		{
			name:        "ReadHeavy_10_85_5",
			writeRatio:  10,
			readRatio:   85,
			deleteRatio: 5,
			envConfig: &TestEnvironmentConfig{
				NetworkProfile: network.ProfileLAN,
				NetworkType:    networkTypeFromEnv(gridkv.TCP),
				NodeCount:      5,
				ReplicaCount:   3,
				BasePort:       63100,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    2048,
				ShardCount:     128,
			},
			duration:       prodDuration(10*time.Second, 30*time.Second),
			minSuccessRate: 90,
		},
		{
			name:        "Balanced_33_33_34",
			writeRatio:  33,
			readRatio:   33,
			deleteRatio: 34,
			envConfig: &TestEnvironmentConfig{
				NetworkProfile: network.ProfileLAN,
				NetworkType:    networkTypeFromEnv(gridkv.TCP),
				NodeCount:      5,
				ReplicaCount:   3,
				BasePort:       63200,
				StorageBackend: gridkv.BackendMemorySharded,
				MaxMemoryMB:    2048,
				ShardCount:     128,
			},
			duration:       prodDuration(10*time.Second, 30*time.Second),
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
	if testing.Short() {
		t.Skip("Skipping core operations test in short mode")
	}
	config := &TestEnvironmentConfig{
		NetworkProfile: network.ProfileLAN,
		NetworkType:    networkTypeFromEnv(gridkv.TCP),
		NodeCount:      1,
		ReplicaCount:   1,
		BasePort:       64000,
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
	if testing.Short() {
		t.Skip("Skipping data loss test in short mode")
	}
	config := &TestEnvironmentConfig{
		NetworkProfile: network.ProfileLAN,
		NetworkType:    networkTypeFromEnv(gridkv.TCP),
		NodeCount:      15,
		ReplicaCount:   3,
		BasePort:       64100,
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

	time.Sleep(prodDuration(8*time.Second, 20*time.Second))
	close(stopCh)
	wg.Wait()
	time.Sleep(prodDuration(1*time.Second, 2*time.Second))

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
		NetworkType:    networkTypeFromEnv(gridkv.TCP),
		NodeCount:      3,
		ReplicaCount:   3,
		BasePort:       64200,
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
		sim.WaitForReplicationSettle(nodes...)
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

// TestProductionDelayPhaseConvergence ensures replicated values become visible within bounded delays.
func TestProductionDelayPhaseConvergence(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping delay-phase convergence test in short mode")
	}

	const (
		totalKeys = 40
		valueSize = 512
	)

	delays := []time.Duration{
		0,
		500 * time.Millisecond,
		2 * time.Second,
		5 * time.Second,
	}

	config := &TestEnvironmentConfig{
		NetworkProfile: network.ProfileWAN,
		NetworkType:    networkTypeFromEnv(gridkv.TCP),
		NodeCount:      9,
		ReplicaCount:   3,
		BasePort:       64400,
		StorageBackend: gridkv.BackendMemorySharded,
		MaxMemoryMB:    4096,
		ShardCount:     256,
	}

	sim := NewTestEnvironmentSimulator(config)
	if err := sim.SetupCluster(t); err != nil {
		t.Fatalf("Failed to setup cluster: %v", err)
	}
	defer sim.Cleanup()

	nodes := sim.GetNodes()
	ctx := context.Background()

	written := make(map[string][]byte, totalKeys)
	writer := nodes[0]
	for i := 0; i < totalKeys; i++ {
		key := fmt.Sprintf("delay-phase-%d-%d", i, time.Now().UnixNano())
		value := randomValue(valueSize)
		target := writer
		var writeErr error
		for attempt := 0; attempt < 20; attempt++ {
			writeErr = target.Set(ctx, key, value)
			if writeErr == nil {
				break
			}
			errText := writeErr.Error()
			if strings.Contains(errText, "coordinator") || strings.Contains(errText, "cluster not ready") {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			break
		}
		if writeErr != nil {
			t.Fatalf("Set failed for %s: %v", key, writeErr)
		}
		valueCopy := make([]byte, len(value))
		copy(valueCopy, value)
		written[key] = valueCopy
	}

	// Prime pipelines before sampling.
	sim.WaitForReplicationSettle(nodes...)

	type phaseResult struct {
		delay   time.Duration
		found   int
		missing int
	}
	results := make([]phaseResult, 0, len(delays))

	for _, delay := range delays {
		if delay > 0 {
			time.Sleep(delay)
		}
		sim.WaitForReplicationSettle(nodes...)

		found := 0
		for key, expected := range written {
			if verifyKeyAcrossNodes(nodes, key, expected, 300*time.Millisecond) {
				found++
			}
		}
		result := phaseResult{
			delay:   delay,
			found:   found,
			missing: len(written) - found,
		}
		results = append(results, result)
		t.Logf("delay=%s reachable=%d missing=%d", delay, result.found, result.missing)
	}

	final := results[len(results)-1]
	convergence := float64(final.found) / float64(len(written)) * 100
	if convergence < 95 {
		t.Fatalf("Convergence %.2f%% below 95%% after %s", convergence, delays[len(delays)-1])
	}
}

// TestProductionCriticalMessageDelivery tests critical message delivery under load
func TestProductionCriticalMessageDelivery(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping critical message delivery test in short mode")
	}
	config := &TestEnvironmentConfig{
		NetworkProfile: network.ProfileLAN,
		NetworkType:    networkTypeFromEnv(gridkv.TCP),
		NodeCount:      10,
		ReplicaCount:   3,
		BasePort:       64300,
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

	time.Sleep(prodDuration(4*time.Second, 10*time.Second))
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

// TestProductionExtremeFailure ensures data survives when a majority of nodes fail under extreme latency.
func TestProductionExtremeFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping extreme failure test in short mode")
	}

	const (
		nodeCount        = 10
		replicas         = 4
		totalKeys        = 400
		failCount        = 6
		writeTimeout     = 15 * time.Second
		drainGracePeriod = 15 * time.Second
		consistencyWait  = 90 * time.Second
		minValueSize     = 4 * 1024  // 4 KiB
		maxValueSize     = 64 * 1024 // 64 KiB
	)

	config := &TestEnvironmentConfig{
		NetworkProfile: network.ProfileSatellite,
		NetworkType:    networkTypeFromEnv(gridkv.TCP),
		NodeCount:      nodeCount,
		ReplicaCount:   replicas,
		BasePort:       65000,
		StorageBackend: gridkv.BackendMemorySharded,
		MaxMemoryMB:    4096,
		ShardCount:     256,
	}

	sim := NewTestEnvironmentSimulator(config)
	if err := sim.SetupCluster(t); err != nil {
		t.Fatalf("Failed to setup cluster: %v", err)
	}
	defer sim.Cleanup()

	nodes := sim.GetNodes()
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()

	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	written := make(map[string][]byte, totalKeys)
	for i := 0; i < totalKeys; i++ {
		key := fmt.Sprintf("extreme-key-%d", i)
		valueSize := rnd.Intn(maxValueSize-minValueSize+1) + minValueSize
		value := make([]byte, valueSize)
		if _, err := rnd.Read(value); err != nil {
			t.Fatalf("failed to generate random value: %v", err)
		}
		if err := nodes[0].Set(ctx, key, value); err != nil {
			t.Fatalf("initial write failed: %v", err)
		}
		written[key] = append([]byte(nil), value...)
	}

	sim.WaitForReplicationSettle(nodes...)

	failIndices := make([]int, 0, failCount)
	for i := 1; i <= failCount; i++ {
		failIndices = append(failIndices, i)
	}
	t.Logf("Draining %d nodes (failover simulation)...", failCount)
	if err := sim.ShutdownNodes(failIndices, 60*time.Second); err != nil {
		if strings.Contains(err.Error(), "close timeout") {
			t.Logf("ShutdownNodes timed out: %v (continuing verification)", err)
		} else {
			t.Fatalf("failed to shutdown nodes: %v", err)
		}
	}

	time.Sleep(drainGracePeriod)
	sim.WaitForHealthyNodes(t, nodeCount-failCount, 30*time.Second)

	sim.WaitForReplicationSettle(nodes...)

	nodes = sim.GetNodes()
	survivors := make([]*gridkv.GridKV, 0, nodeCount-failCount)
	for idx, node := range nodes {
		if node != nil && idx < nodeCount {
			survivors = append(survivors, node)
		}
	}
	if len(survivors) == 0 {
		t.Fatalf("no surviving nodes available for verification")
	}

	deadline := time.Now().Add(consistencyWait)
	for key, want := range written {
		var recovered bool
		for time.Now().Before(deadline) && !recovered {
			for _, node := range survivors {
				sim.WaitForReplicationSettle(node)
				val, err := node.Get(context.Background(), key)
				if err == nil && bytes.Equal(val, want) {
					recovered = true
					break
				}
			}
			if !recovered {
				time.Sleep(100 * time.Millisecond)
			}
		}
		if !recovered {
			t.Fatalf("key %s missing after majority failure", key)
		}
	}
}

func verifyKeyAcrossNodes(nodes []*gridkv.GridKV, key string, expected []byte, timeout time.Duration) bool {
	if len(nodes) == 0 {
		return false
	}
	order := rand.Perm(len(nodes))
	for _, idx := range order {
		node := nodes[idx]
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		value, err := node.Get(ctx, key)
		cancel()
		if err == nil && value != nil && bytes.Equal(value, expected) {
			return true
		}
	}
	return false
}

func TestProductionExtremeEventualConsistency(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping extreme eventual consistency test in short mode")
	}

	const (
		nodeCount        = 12
		replicas         = 2
		failCount        = 4
		minValueSize     = 256 * 1024
		maxValueSize     = 1 * 1024 * 1024
		workerMultiplier = 3
		basePort         = 64000
	)

	targetDataBytes := prodDataSize(2*gibibyte, 10*gibibyte)
	workloadDuration := prodDuration(90*time.Second, 4*time.Minute)
	consistencyWait := prodDuration(120*time.Second, 5*time.Minute)
	drainGracePeriod := prodDuration(30*time.Second, 90*time.Second)
	writeTimeout := 20 * time.Second
	readTimeout := 15 * time.Second

	config := &TestEnvironmentConfig{
		NetworkProfile: network.ProfileSatellite,
		NetworkType:    networkTypeFromEnv(gridkv.TCP),
		NodeCount:      nodeCount,
		ReplicaCount:   replicas,
		BasePort:       basePort,
		StorageBackend: gridkv.BackendMemorySharded,
		MaxMemoryMB:    6144,
		ShardCount:     512,
	}

	sim := NewTestEnvironmentSimulator(config)
	if err := sim.SetupCluster(t); err != nil {
		t.Fatalf("failed to setup cluster: %v", err)
	}
	defer sim.Cleanup()

	nodes := sim.GetNodes()
	tracker := newExtremeKeyTracker()
	var keySeq atomic.Int64
	workerCount := nodeCount * workerMultiplier
	var writeOps, readOps, deleteOps atomic.Int64
	var writeErrs, readErrs, deleteErrs atomic.Int64

	errCh := make(chan error, 1)
	reportErr := func(err error) {
		select {
		case errCh <- err:
		default:
		}
	}

	start := time.Now()
	stopTime := start.Add(workloadDuration)

	var failOnce atomic.Bool
	var failWG sync.WaitGroup
	enableFailures := prodLongRuntime

	induceFailures := func() {
		defer failWG.Done()
		failIndices := make([]int, 0, failCount)
		for idx := nodeCount - 1; idx >= 1 && len(failIndices) < failCount; idx-- {
			failIndices = append(failIndices, idx)
		}
		t.Logf("Inducing shutdown of %d nodes under load...", len(failIndices))
		if err := sim.ShutdownNodes(failIndices, 90*time.Second); err != nil && !strings.Contains(err.Error(), "timeout") {
			reportErr(fmt.Errorf("shutdown failed: %w", err))
		}
	}

	var wg sync.WaitGroup
	for workerID := 0; workerID < workerCount; workerID++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(time.Now().UnixNano() + int64(id)*7919))
			for time.Now().Before(stopTime) {
				if enableFailures && !failOnce.Load() && time.Since(start) > workloadDuration/2 {
					if failOnce.CompareAndSwap(false, true) {
						failWG.Add(1)
						go induceFailures()
					}
				}

				currentBytes := tracker.currentBytes()
				op := pickExtremeOperation(r, currentBytes, targetDataBytes)

				switch op {
				case "write":
					writeOps.Add(1)
					node := pickAliveNode(nodes, r)
					if node == nil {
						time.Sleep(5 * time.Millisecond)
						continue
					}
					valueSize := r.Intn(maxValueSize-minValueSize+1) + minValueSize
					seed := r.Int63()
					payload := deterministicPayload(seed, valueSize)
					preferNew := currentBytes < targetDataBytes || tracker.size() == 0
					key := tracker.pickKeyForWrite(r, preferNew, fmt.Sprintf("evt-key-%d", keySeq.Add(1)))

					ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
					err := node.Set(ctx, key, payload)
					cancel()
					if err != nil {
						writeErrs.Add(1)
						continue
					}
					tracker.upsert(key, extremeValueInfo{size: valueSize, seed: seed})
				case "read":
					readOps.Add(1)
					key, info, ok := tracker.random(r)
					if !ok {
						time.Sleep(5 * time.Millisecond)
						continue
					}
					node := pickAliveNode(nodes, r)
					if node == nil {
						time.Sleep(5 * time.Millisecond)
						continue
					}
					ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
					value, err := node.Get(ctx, key)
					cancel()
					if err != nil {
						readErrs.Add(1)
						continue
					}
					expected := deterministicPayload(info.seed, info.size)
					if !bytes.Equal(value, expected) {
						readErrs.Add(1)
						continue
					}
				case "delete":
					deleteOps.Add(1)
					key, _, ok := tracker.random(r)
					if !ok {
						time.Sleep(5 * time.Millisecond)
						continue
					}
					node := pickAliveNode(nodes, r)
					if node == nil {
						time.Sleep(5 * time.Millisecond)
						continue
					}
					ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
					err := node.Delete(ctx, key)
					cancel()
					if err != nil {
						deleteErrs.Add(1)
						continue
					}
					tracker.remove(key)
				}
			}
		}(workerID)
	}

	wg.Wait()
	if failOnce.Load() {
		failWG.Wait()
	}

	select {
	case err := <-errCh:
		t.Fatalf("workload error: %v", err)
	default:
	}

	t.Logf("Extreme workload complete: target=%.2f GiB current=%.2f GiB writes=%d (fail=%d) reads=%d (fail=%d) deletes=%d (fail=%d)",
		float64(targetDataBytes)/float64(gibibyte),
		float64(tracker.currentBytes())/float64(gibibyte),
		writeOps.Load(), writeErrs.Load(),
		readOps.Load(), readErrs.Load(),
		deleteOps.Load(), deleteErrs.Load())

	sim.WaitForReplicationSettle(nodes...)

	sim.LogNodeDiagnostics(t, "post-workload")

	time.Sleep(drainGracePeriod)

	survivors := make([]*gridkv.GridKV, 0, len(nodes))
	for _, node := range nodes {
		if node != nil {
			survivors = append(survivors, node)
		}
	}
	if len(survivors) == 0 {
		t.Fatalf("no surviving nodes for verification")
	}

	snapshot := tracker.snapshot()
	if len(snapshot) == 0 {
		t.Fatalf("no keys were written during workload")
	}

	verifyKeys := make([]string, 0, len(snapshot))
	for key := range snapshot {
		verifyKeys = append(verifyKeys, key)
	}
	rand.Shuffle(len(verifyKeys), func(i, j int) {
		verifyKeys[i], verifyKeys[j] = verifyKeys[j], verifyKeys[i]
	})
	maxSamples := 1024
	if prodLongRuntime {
		maxSamples = 4096
	}
	if len(verifyKeys) > maxSamples {
		verifyKeys = verifyKeys[:maxSamples]
	}

	verifyCh := make(chan string, len(verifyKeys))
	for _, key := range verifyKeys {
		verifyCh <- key
	}
	close(verifyCh)

	verifyErr := make(chan error, 1)
	verifyWorkerCount := len(survivors)
	if verifyWorkerCount > 8 {
		verifyWorkerCount = 8
	}
	if verifyWorkerCount < 1 {
		verifyWorkerCount = 1
	}

	var verifyWG sync.WaitGroup
	for i := 0; i < verifyWorkerCount; i++ {
		verifyWG.Add(1)
		go func() {
			defer verifyWG.Done()
			for key := range verifyCh {
				info := snapshot[key]
				expected := deterministicPayload(info.seed, info.size)
				deadline := time.Now().Add(consistencyWait)
				var consistent bool
				for time.Now().Before(deadline) && !consistent {
					for _, node := range survivors {
						sim.WaitForReplicationSettle(node)
						ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
						value, err := node.Get(ctx, key)
						cancel()
						if err == nil && bytes.Equal(value, expected) {
							consistent = true
							break
						}
					}
					if !consistent {
						time.Sleep(200 * time.Millisecond)
					}
				}
				if !consistent {
					select {
					case verifyErr <- fmt.Errorf("key %s not consistent after %.0fs", key, consistencyWait.Seconds()):
					default:
					}
					sim.LogNodeDiagnostics(t, "verification failure")
					return
				}
			}
		}()
	}

	verifyWG.Wait()
	select {
	case err := <-verifyErr:
		t.Fatalf("%v", err)
	default:
	}
}

type extremeValueInfo struct {
	size int
	seed int64
}

type extremeKeyTracker struct {
	mu         sync.RWMutex
	keys       []string
	index      map[string]int
	meta       map[string]extremeValueInfo
	totalBytes int64
}

func newExtremeKeyTracker() *extremeKeyTracker {
	return &extremeKeyTracker{
		index: make(map[string]int),
		meta:  make(map[string]extremeValueInfo),
	}
}

func (t *extremeKeyTracker) upsert(key string, info extremeValueInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if idx, ok := t.index[key]; ok {
		prev := t.meta[key]
		t.meta[key] = info
		delta := info.size - prev.size
		atomic.AddInt64(&t.totalBytes, int64(delta))
		t.keys[idx] = key
	} else {
		t.index[key] = len(t.keys)
		t.keys = append(t.keys, key)
		t.meta[key] = info
		atomic.AddInt64(&t.totalBytes, int64(info.size))
	}
}

func (t *extremeKeyTracker) remove(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	info, ok := t.meta[key]
	if !ok {
		return
	}
	idx := t.index[key]
	lastIdx := len(t.keys) - 1
	lastKey := t.keys[lastIdx]
	t.keys[idx] = lastKey
	t.index[lastKey] = idx
	t.keys = t.keys[:lastIdx]
	delete(t.index, key)
	delete(t.meta, key)
	atomic.AddInt64(&t.totalBytes, -int64(info.size))
}

func (t *extremeKeyTracker) random(r *rand.Rand) (string, extremeValueInfo, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if len(t.keys) == 0 {
		return "", extremeValueInfo{}, false
	}
	key := t.keys[r.Intn(len(t.keys))]
	return key, t.meta[key], true
}

func (t *extremeKeyTracker) pickKeyForWrite(r *rand.Rand, preferNew bool, nextKey string) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if preferNew || len(t.keys) == 0 || r.Float64() < 0.5 {
		return nextKey
	}
	key := t.keys[r.Intn(len(t.keys))]
	return key
}

func (t *extremeKeyTracker) snapshot() map[string]extremeValueInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()
	copyMap := make(map[string]extremeValueInfo, len(t.meta))
	for k, v := range t.meta {
		copyMap[k] = v
	}
	return copyMap
}

func (t *extremeKeyTracker) currentBytes() int64 {
	return atomic.LoadInt64(&t.totalBytes)
}

func (t *extremeKeyTracker) size() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.keys)
}

func deterministicPayload(seed int64, size int) []byte {
	r := rand.New(rand.NewSource(seed))
	buf := make([]byte, size)
	_, _ = r.Read(buf)
	return buf
}

func pickExtremeOperation(r *rand.Rand, currentBytes, targetBytes int64) string {
	lowerBound := targetBytes - targetBytes/10
	upperBound := targetBytes + targetBytes/10
	if currentBytes < lowerBound {
		if r.Float64() < 0.7 {
			return "write"
		}
	}
	if currentBytes > upperBound {
		if r.Float64() < 0.6 {
			return "delete"
		}
	}
	p := r.Float64()
	switch {
	case p < 0.45:
		return "write"
	case p < 0.8:
		return "read"
	default:
		return "delete"
	}
}

func pickAliveNode(nodes []*gridkv.GridKV, r *rand.Rand) *gridkv.GridKV {
	if len(nodes) == 0 {
		return nil
	}
	for i := 0; i < len(nodes)*2; i++ {
		idx := r.Intn(len(nodes))
		if node := nodes[idx]; node != nil {
			return node
		}
	}
	return nil
}
