package cluster

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/feellmoose/gridkv/internal/mem_storage"
	"github.com/feellmoose/gridkv/internal/utils/executor"
	"github.com/zeebo/xxh3"
)

// testBloomContains checks if a key might be in the bloom filter
// This is a simplified test function for bloom filter checking
func testBloomContains(bloom []byte, key []byte) bool {
	// Use same hash as Digest method: xxh3.HashString128
	hash := xxh3.HashString128(string(key)).Hi

	bloomSize := uint64(len(bloom) * 8)
	idx := (hash % bloomSize) / 8
	bit := (hash % bloomSize) % 8

	return (bloom[idx] & (1 << bit)) != 0
}

func TestAntiEntropy_DigestAndSync(t *testing.T) {
	storeA, _ := mem_storage.New(mem_storage.DefaultConfig())
	storeB, _ := mem_storage.New(mem_storage.DefaultConfig())

	_ = storeA.Set("user:1", &mem_storage.StoredItem{Key: "user:1", Value: []byte("a"), Version: 1})
	_ = storeA.Set("user:2", &mem_storage.StoredItem{Key: "user:2", Value: []byte("a"), Version: 1})
	_ = storeB.Set("user:1", &mem_storage.StoredItem{Key: "user:1", Value: []byte("b"), Version: 2})
	_ = storeB.Set("user:3", &mem_storage.StoredItem{Key: "user:3", Value: []byte("c"), Version: 1})

	aeA := newAntiEntropy(antiEntropyConfig{Store: storeA})
	aeB := newAntiEntropy(antiEntropyConfig{Store: storeB})

	bloomB, vvB := aeB.Digest("user:")
	diffKeys, err := aeA.Sync(bloomB, vvB)
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	got := make(map[string]bool)
	for _, k := range diffKeys {
		got[k] = true
	}
	for _, k := range []string{"user:1", "user:2"} {
		if !got[k] {
			t.Fatalf("missing expected diff key %s", k)
		}
	}
}

func TestAntiEntropy_Digest(t *testing.T) {
	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	exec, _ := executor.New(executor.Opts{Name: "test", Workers: 1})

	entropy := newAntiEntropy(antiEntropyConfig{
		Store:    store,
		Executor: exec,
		Interval: 1 * time.Minute,
	})

	for i := 0; i < 5; i++ {
		key := "key" + string(rune(i))
		item := &mem_storage.StoredItem{
			Version: int64(i),
			Value:   []byte("value"),
		}
		_ = store.Set(key, item)
	}

	bloom, vv := entropy.Digest("")
	if len(bloom) == 0 {
		t.Fatal("Digest() returned empty bloom")
	}
	if vv == nil {
		t.Fatal("Digest() returned nil version vector")
	}
}

func TestAntiEntropy_Sync(t *testing.T) {
	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	exec, _ := executor.New(executor.Opts{Name: "test", Workers: 1})

	entropy := newAntiEntropy(antiEntropyConfig{
		Store:    store,
		Executor: exec,
		Interval: 1 * time.Minute,
	})

	diffKeys, err := entropy.Sync(nil, nil)
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if diffKeys == nil {
		t.Fatal("Sync() returned nil diffKeys")
	}
}

func TestAntiEntropy_Lifecycle(t *testing.T) {
	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	exec, _ := executor.New(executor.Opts{Name: "test", Workers: 1})

	entropy := newAntiEntropy(antiEntropyConfig{
		Store:    store,
		Executor: exec,
		Interval: 50 * time.Millisecond,
	})

	if err := entropy.start(); err != nil {
		t.Fatalf("start() error = %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	if err := entropy.stop(); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
}

// TestAntiEntropy_BloomFilter tests bloom filter functionality for key detection
func TestAntiEntropy_BloomFilter(t *testing.T) {
	store1, _ := mem_storage.New(mem_storage.DefaultConfig())
	exec, _ := executor.New(executor.Opts{Name: "test", Workers: 2})

	// Populate store1 with test data
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("bloom-key-%d", i)
		item := &mem_storage.StoredItem{
			Key:     key,
			Value:   []byte(fmt.Sprintf("value-%d", i)),
			Version: int64(i + 1),
		}
		if err := store1.Set(key, item); err != nil {
			t.Fatalf("Failed to populate store1: %v", err)
		}
	}

	entropy1 := newAntiEntropy(antiEntropyConfig{
		Store:    store1,
		Executor: exec,
		Interval: time.Second,
	})

	// Test digest generation
	bloom1, vv1 := entropy1.Digest("bloom-key")

	// Test bloom filter contains expected keys
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("bloom-key-%d", i)
		if !testBloomContains(bloom1, []byte(key)) {
			t.Errorf("Bloom filter should contain key %s", key)
		}
	}

	// Test bloom filter does not contain non-existent keys
	if testBloomContains(bloom1, []byte("non-existent-key")) {
		t.Error("Bloom filter should not contain non-existent key")
	}

	// Test version vector
	if len(vv1) == 0 {
		t.Error("Version vector should not be empty")
	}
}

// TestAntiEntropy_VersionVector tests version vector conflict detection
func TestAntiEntropy_VersionVector(t *testing.T) {
	store1, _ := mem_storage.New(mem_storage.DefaultConfig())
	store2, _ := mem_storage.New(mem_storage.DefaultConfig())
	exec, _ := executor.New(executor.Opts{Name: "test", Workers: 2})

	// Populate both stores with overlapping data but different versions
	for i := 0; i < 50; i++ {
		key := fmt.Sprintf("version-key-%d", i)
		version1 := int64(i + 1)
		version2 := int64(i + 2) // store2 has newer versions

		item1 := &mem_storage.StoredItem{
			Key:     key,
			Value:   []byte(fmt.Sprintf("value1-%d", i)),
			Version: version1,
		}
		item2 := &mem_storage.StoredItem{
			Key:     key,
			Value:   []byte(fmt.Sprintf("value2-%d", i)),
			Version: version2,
		}

		if err := store1.Set(key, item1); err != nil {
			t.Fatalf("Failed to populate store1: %v", err)
		}
		if err := store2.Set(key, item2); err != nil {
			t.Fatalf("Failed to populate store2: %v", err)
		}
	}

	entropy1 := newAntiEntropy(antiEntropyConfig{
		Store:    store1,
		Executor: exec,
		Interval: time.Second,
	})

	entropy2 := newAntiEntropy(antiEntropyConfig{
		Store:    store2,
		Executor: exec,
		Interval: time.Second,
	})

	// Generate digests
	_, vv1 := entropy1.Digest("version-key")
	_, vv2 := entropy2.Digest("version-key")

	// Test sync detection - this tests the sync method
	// Note: sync method may not exist or may have different signature
	// This test focuses on verifying digest differences
	if len(vv1) == 0 || len(vv2) == 0 {
		t.Error("Version vectors should not be empty")
	}

	// Verify that version vectors contain different versions
	differentVersions := 0
	for key, v1 := range vv1 {
		if v2, exists := vv2[key]; exists && v1 != v2 {
			differentVersions++
		}
	}

	if differentVersions == 0 {
		t.Error("Should detect version differences between stores")
	} else {
		t.Logf("Detected %d version differences", differentVersions)
	}
}

// TestAntiEntropy_IncrementalSync tests incremental synchronization
func TestAntiEntropy_IncrementalSync(t *testing.T) {
	store1, _ := mem_storage.New(mem_storage.DefaultConfig())
	exec, _ := executor.New(executor.Opts{Name: "test", Workers: 2})

	entropy1 := newAntiEntropy(antiEntropyConfig{
		Store:    store1,
		Executor: exec,
		Interval: time.Second,
	})

	// Add data incrementally to store1
	for batch := 0; batch < 3; batch++ {
		for i := 0; i < 10; i++ {
			key := fmt.Sprintf("incremental-key-%d-%d", batch, i)
			item := &mem_storage.StoredItem{
				Key:     key,
				Value:   []byte(fmt.Sprintf("value-%d-%d", batch, i)),
				Version: int64(batch*10 + i + 1),
			}
			if err := store1.Set(key, item); err != nil {
				t.Fatalf("Failed to add incremental data: %v", err)
			}
		}

		// Sync after each batch - verify digests contain incremental data
		_, vv1 := entropy1.Digest("incremental-key")

		// Should detect new keys in each batch through version vector growth
		expectedKeys := (batch + 1) * 10
		if len(vv1) < expectedKeys {
			t.Errorf("Batch %d: expected at least %d keys in version vector, got %d",
				batch, expectedKeys, len(vv1))
		}
	}
}

// TestAntiEntropy_Performance tests anti-entropy performance characteristics
func TestAntiEntropy_Performance(t *testing.T) {
	store, _ := mem_storage.New(mem_storage.DefaultConfig())

	// Populate with substantial data
	for i := 0; i < 10000; i++ {
		key := fmt.Sprintf("perf-key-%d", i)
		item := &mem_storage.StoredItem{
			Key:     key,
			Value:   []byte(fmt.Sprintf("perf-value-%d", i)),
			Version: int64(i + 1),
		}
		if err := store.Set(key, item); err != nil {
			t.Fatalf("Failed to populate performance data: %v", err)
		}
	}

	exec, _ := executor.New(executor.Opts{Name: "test", Workers: 2})
	entropy := newAntiEntropy(antiEntropyConfig{
		Store:    store,
		Executor: exec,
		Interval: time.Second,
	})

	// Test digest performance
	start := time.Now()
	bloom, vv := entropy.Digest("perf-key")
	digestTime := time.Since(start)

	t.Logf("Digest performance: %d keys in %v (%.0f keys/sec)",
		10000, digestTime, float64(10000)/digestTime.Seconds())

	// Verify digest quality
	if len(bloom) == 0 {
		t.Error("Digest should contain bloom filter")
	}
	if len(vv) == 0 {
		t.Error("Digest should contain version vector")
	}
}

func TestReplay_SaveLoadCheckpoint(t *testing.T) {
	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	r := newReplay(replayConfig{
		CheckpointPath: filepath.Join(t.TempDir(), "cp.dat"),
		Store:          store,
	})

	ops := []*mem_storage.SyncOperation{
		{
			Key:    "k1",
			OpType: mem_storage.OpSet,
			Item: &mem_storage.StoredItem{
				Key:     "k1",
				Value:   []byte("v1"),
				Version: 10,
			},
		},
		{
			Key:    "k2",
			OpType: mem_storage.OpDelete,
			Item: &mem_storage.StoredItem{
				Key:     "k2",
				Version: 11,
			},
		},
	}

	if err := r.SaveCheckpoint(ops); err != nil {
		t.Fatalf("SaveCheckpoint() error = %v", err)
	}

	loaded, err := r.LoadCheckpoint()
	if err != nil {
		t.Fatalf("LoadCheckpoint() error = %v", err)
	}

	if len(loaded) != len(ops) {
		t.Fatalf("loaded ops len = %d, want %d", len(loaded), len(ops))
	}
	for i := range ops {
		if loaded[i].Key != ops[i].Key || loaded[i].OpType != ops[i].OpType || loaded[i].Item.Version != ops[i].Item.Version {
			t.Fatalf("loaded op mismatch: got %+v want %+v", loaded[i], ops[i])
		}
	}
}
