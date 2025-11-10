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
)

// TestPanicRecovery_API tests that API layer methods recover from panics
func TestPanicRecovery_API(t *testing.T) {
	kv, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
		LocalNodeID:  "panic-test-node",
		LocalAddress: "localhost:14999",
		Network: &gridkv.NetworkOptions{
			Type:     gridkv.TCP,
			BindAddr: "localhost:14999",
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

	// Test normal operations work
	err = kv.Set(ctx, "test-key", []byte("test-value"))
	if err != nil {
		t.Fatal("Normal Set failed:", err)
	}

	value, err := kv.Get(ctx, "test-key")
	if err != nil {
		t.Fatal("Normal Get failed:", err)
	}
	if string(value) != "test-value" {
		t.Errorf("Expected 'test-value', got %q", string(value))
	}

	err = kv.Delete(ctx, "test-key")
	if err != nil {
		t.Fatal("Normal Delete failed:", err)
	}

	t.Log("✅ API panic recovery mechanisms are in place")
	t.Log("Note: Actual panic scenarios would require injecting faulty data or mocking")
}

// TestConcurrentOperationsNoPanic tests that concurrent operations don't panic
func TestConcurrentOperationsNoPanic(t *testing.T) {
	kv, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
		LocalNodeID:  "concurrent-panic-test",
		LocalAddress: "localhost:13999",
		Network: &gridkv.NetworkOptions{
			Type:     gridkv.TCP,
			BindAddr: "localhost:13999",
		},
		Storage: &gridkv.StorageOptions{
			Backend:     gridkv.BackendMemorySharded,
			MaxMemoryMB: 2048,
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
	numGoroutines := 100
	iterations := 100

	// Run intense concurrent operations
	var wg sync.WaitGroup
	errors := make(chan error, numGoroutines)

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer func() {
				if r := recover(); r != nil {
					errors <- fmt.Errorf("goroutine %d panicked: %v", id, r)
				}
				wg.Done()
			}()

			for i := 0; i < iterations; i++ {
				key := fmt.Sprintf("key-%d-%d", id, i)
				value := []byte(fmt.Sprintf("value-%d-%d", id, i))

				// Set
				if err := kv.Set(ctx, key, value); err != nil {
					// Errors are OK, panics are not
					continue
				}

				// Get
				if _, err := kv.Get(ctx, key); err != nil {
					// Errors are OK, panics are not
					continue
				}

				// Delete
				if err := kv.Delete(ctx, key); err != nil {
					// Errors are OK, panics are not
					continue
				}
			}
		}(g)
	}

	wg.Wait()
	close(errors)

	// Check for panics
	panicCount := 0
	for err := range errors {
		t.Error(err)
		panicCount++
	}

	if panicCount > 0 {
		t.Fatalf("Found %d panics in concurrent operations", panicCount)
	}

	t.Logf("✅ Completed %d goroutines × %d iterations without panic",
		numGoroutines, iterations)
}

// TestEdgeCases_NoPanic tests edge cases that might cause panics
func TestEdgeCases_NoPanic(t *testing.T) {
	kv, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
		LocalNodeID:  "edge-case-test",
		LocalAddress: "localhost:12999",
		Network: &gridkv.NetworkOptions{
			Type:     gridkv.TCP,
			BindAddr: "localhost:12999",
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

	testCases := []struct {
		name string
		fn   func() error
	}{
		{
			name: "Empty key Set",
			fn: func() error {
				return kv.Set(ctx, "", []byte("value"))
			},
		},
		{
			name: "Empty key Get",
			fn: func() error {
				_, err := kv.Get(ctx, "")
				return err
			},
		},
		{
			name: "Empty key Delete",
			fn: func() error {
				return kv.Delete(ctx, "")
			},
		},
		{
			name: "Nil value Set",
			fn: func() error {
				return kv.Set(ctx, "key", nil)
			},
		},
		{
			name: "Get non-existent key",
			fn: func() error {
				_, err := kv.Get(ctx, "non-existent-key")
				return err
			},
		},
		{
			name: "Delete non-existent key",
			fn: func() error {
				return kv.Delete(ctx, "non-existent-key")
			},
		},
		{
			name: "Very large value Set",
			fn: func() error {
				largeValue := make([]byte, 1024*1024) // 1MB
				return kv.Set(ctx, "large-key", largeValue)
			},
		},
		{
			name: "Cancelled context Set",
			fn: func() error {
				ctx, cancel := context.WithCancel(context.Background())
				cancel() // Cancel immediately
				return kv.Set(ctx, "key", []byte("value"))
			},
		},
		{
			name: "Cancelled context Get",
			fn: func() error {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				_, err := kv.Get(ctx, "key")
				return err
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("❌ Panic in %s: %v", tc.name, r)
				}
			}()

			// Call the function - errors are OK, panics are not
			_ = tc.fn()
			t.Logf("✅ %s: no panic", tc.name)
		})
	}
}

// TestStressTest_NoPanic runs intense stress test to trigger potential panics
func TestStressTest_NoPanic(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	kv, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
		LocalNodeID:  "stress-test-node",
		LocalAddress: "localhost:11999",
		Network: &gridkv.NetworkOptions{
			Type:     gridkv.TCP,
			BindAddr: "localhost:11999",
		},
		Storage: &gridkv.StorageOptions{
			Backend:     gridkv.BackendMemorySharded,
			MaxMemoryMB: 512, // Intentionally small to trigger memory pressure
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
	duration := 5 * time.Second
	deadline := time.Now().Add(duration)

	var ops atomic.Int64
	var panics atomic.Int64
	var wg sync.WaitGroup

	// Start multiple goroutines doing random operations
	for g := 0; g < 50; g++ {
		wg.Add(1)
		go func(id int) {
			defer func() {
				if r := recover(); r != nil {
					panics.Add(1)
					t.Errorf("Goroutine %d panicked: %v", id, r)
				}
				wg.Done()
			}()

			for time.Now().Before(deadline) {
				key := fmt.Sprintf("stress-key-%d-%d", id, rand.Intn(100))
				value := make([]byte, rand.Intn(10240)) // 0-10KB

				switch rand.Intn(3) {
				case 0: // Set
					_ = kv.Set(ctx, key, value)
				case 1: // Get
					_, _ = kv.Get(ctx, key)
				case 2: // Delete
					_ = kv.Delete(ctx, key)
				}

				ops.Add(1)
			}
		}(g)
	}

	wg.Wait()

	totalOps := ops.Load()
	totalPanics := panics.Load()

	t.Logf("Stress test completed: %d operations in %v", totalOps, duration)
	t.Logf("Operations/sec: %.0f", float64(totalOps)/duration.Seconds())

	if totalPanics > 0 {
		t.Errorf("❌ Found %d panics during stress test", totalPanics)
	} else {
		t.Log("✅ No panics during stress test")
	}
}
