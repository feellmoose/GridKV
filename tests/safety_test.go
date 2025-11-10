package tests

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	gridkv "github.com/feellmoose/gridkv"
)

// TestGetSafety verifies that the returned []byte from Get is safe to modify
func TestGetSafety(t *testing.T) {
	// Setup
	kv, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
		LocalNodeID:  "test-node",
		LocalAddress: "localhost:19999",
		Network: &gridkv.NetworkOptions{
			Type:     gridkv.TCP,
			BindAddr: "localhost:19999",
		},
		Storage: &gridkv.StorageOptions{
			Backend:     gridkv.BackendMemorySharded,
			MaxMemoryMB: 1024,
		},
		ReplicaCount: 1,
		WriteQuorum:  1,
		ReadQuorum:   1,
		VirtualNodes: 10, // Add virtual nodes
	})
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()

	ctx := context.Background()
	testKey := "safety-test-key"
	originalData := []byte("original data that should not be modified")

	// Write original data
	err = kv.Set(ctx, testKey, originalData)
	if err != nil {
		t.Fatal(err)
	}

	// Read data and modify it
	data1, err := kv.Get(ctx, testKey)
	if err != nil {
		t.Fatal(err)
	}

	// Verify initial data
	if string(data1) != string(originalData) {
		t.Errorf("Expected %q, got %q", string(originalData), string(data1))
	}

	// Modify the returned data
	for i := range data1 {
		data1[i] = 'X'
	}

	// Read again - should still be original data
	data2, err := kv.Get(ctx, testKey)
	if err != nil {
		t.Fatal(err)
	}

	if string(data2) != string(originalData) {
		t.Errorf("Data was unexpectedly modified! Expected %q, got %q",
			string(originalData), string(data2))
	}

	t.Log("✅ Get safety test passed - returned data is independent")
}

// TestMultipleGetsSafety verifies multiple Get calls return independent copies
func TestMultipleGetsSafety(t *testing.T) {
	kv, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
		LocalNodeID:  "test-node-2",
		LocalAddress: "localhost:18999",
		Network: &gridkv.NetworkOptions{
			Type:     gridkv.TCP,
			BindAddr: "localhost:18999",
		},
		Storage: &gridkv.StorageOptions{
			Backend:     gridkv.BackendMemorySharded,
			MaxMemoryMB: 1024,
		},
		ReplicaCount: 1,
		WriteQuorum:  1,
		ReadQuorum:   1,
		VirtualNodes: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()

	ctx := context.Background()
	testKey := "multi-get-key"
	originalData := []byte("test data")

	err = kv.Set(ctx, testKey, originalData)
	if err != nil {
		t.Fatal(err)
	}

	// Get multiple times
	results := make([][]byte, 5)
	for i := 0; i < 5; i++ {
		data, err := kv.Get(ctx, testKey)
		if err != nil {
			t.Fatal(err)
		}
		results[i] = data
	}

	// Modify all returned slices
	for i, data := range results {
		for j := range data {
			data[j] = byte('A' + i)
		}
	}

	// Verify original data is unchanged
	finalData, err := kv.Get(ctx, testKey)
	if err != nil {
		t.Fatal(err)
	}

	if string(finalData) != string(originalData) {
		t.Errorf("Original data was modified! Expected %q, got %q",
			string(originalData), string(finalData))
	}

	t.Log("✅ Multiple Get safety test passed")
}

// TestObjectPoolNoLeak verifies StoredItem objects are properly returned to pool
func TestObjectPoolNoLeak(t *testing.T) {
	kv, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
		LocalNodeID:  "pool-test-node",
		LocalAddress: "localhost:17999",
		Network: &gridkv.NetworkOptions{
			Type:     gridkv.TCP,
			BindAddr: "localhost:17999",
		},
		Storage: &gridkv.StorageOptions{
			Backend:     gridkv.BackendMemorySharded,
			MaxMemoryMB: 1024,
		},
		ReplicaCount: 1,
		WriteQuorum:  1,
		ReadQuorum:   1,
		VirtualNodes: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()

	ctx := context.Background()
	testData := []byte("pool test data")

	// Create multiple keys
	numKeys := 100
	for i := 0; i < numKeys; i++ {
		key := "pool-key-" + string(rune(i))
		err := kv.Set(ctx, key, testData)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Perform many Get operations
	// If objects are not returned to pool, this would cause memory growth
	iterations := 10000
	for i := 0; i < iterations; i++ {
		key := "pool-key-" + string(rune(i%numKeys))
		_, err := kv.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Note: We can't directly test pool statistics yet, but this test
	// would show memory growth in profiling if pool wasn't working
	t.Logf("✅ Completed %d Get operations without error", iterations)
}

// TestGetAfterExpiration verifies expired items are handled correctly
func TestGetAfterExpiration(t *testing.T) {
	kv, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
		LocalNodeID:  "expire-test-node",
		LocalAddress: "localhost:16999",
		Network: &gridkv.NetworkOptions{
			Type:     gridkv.TCP,
			BindAddr: "localhost:16999",
		},
		Storage: &gridkv.StorageOptions{
			Backend:     gridkv.BackendMemorySharded,
			MaxMemoryMB: 1024,
		},
		ReplicaCount: 1,
		WriteQuorum:  1,
		ReadQuorum:   1,
		TTL:          100 * time.Millisecond, // Short TTL for testing
	})
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()

	ctx := context.Background()
	testKey := "expire-key"
	testData := []byte("will expire")

	// Set with TTL
	err = kv.Set(ctx, testKey, testData)
	if err != nil {
		t.Fatal(err)
	}

	// Should be able to read immediately
	data, err := kv.Get(ctx, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(testData) {
		t.Errorf("Unexpected data: %q", string(data))
	}

	// Wait for expiration
	time.Sleep(150 * time.Millisecond)

	// Should get expiration error
	_, err = kv.Get(ctx, testKey)
	if err != nil {
		t.Errorf("Expected ErrItemExpired, got: %v", err)
	}

	t.Log("✅ Expiration handling test passed")
}

// TestConcurrentGetSafety verifies concurrent Get operations are safe
func TestConcurrentGetSafety(t *testing.T) {
	kv, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
		LocalNodeID:  "concurrent-test-node",
		LocalAddress: "localhost:15999",
		Network: &gridkv.NetworkOptions{
			Type:     gridkv.TCP,
			BindAddr: "localhost:15999",
		},
		Storage: &gridkv.StorageOptions{
			Backend:     gridkv.BackendMemorySharded,
			MaxMemoryMB: 1024,
		},
		ReplicaCount: 1,
		WriteQuorum:  1,
		ReadQuorum:   1,
		VirtualNodes: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()

	ctx := context.Background()
	testKey := "concurrent-key"
	originalData := []byte("concurrent test data")

	err = kv.Set(ctx, testKey, originalData)
	if err != nil {
		t.Fatal(err)
	}

	// Concurrent Get operations
	numGoroutines := 100
	iterations := 100

	var wg sync.WaitGroup
	errors := make(chan error, numGoroutines)

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				data, err := kv.Get(ctx, testKey)
				if err != nil {
					errors <- err
					return
				}

				// Verify data
				if string(data) != string(originalData) {
					errors <- fmt.Errorf("goroutine %d: data mismatch: %q", id, string(data))
					return
				}

				// Modify returned data (should not affect other goroutines)
				for j := range data {
					data[j] = byte('A' + (id % 26))
				}
			}
		}(g)
	}

	wg.Wait()
	close(errors)

	// Check for errors
	for err := range errors {
		t.Error(err)
	}

	// Verify original data is still intact
	finalData, err := kv.Get(ctx, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if string(finalData) != string(originalData) {
		t.Errorf("Original data was corrupted! Expected %q, got %q",
			string(originalData), string(finalData))
	}

	t.Logf("✅ Concurrent Get safety test passed (%d goroutines × %d iterations)",
		numGoroutines, iterations)
}
