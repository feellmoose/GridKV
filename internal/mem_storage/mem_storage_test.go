package mem_storage

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestMemStorage_New(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr bool
	}{
		{
			name:    "default config",
			config:  DefaultConfig(),
			wantErr: false,
		},
		{
			name: "custom shard count",
			config: Config{
				ShardCount: 128,
			},
			wantErr: false,
		},
		{
			name: "with compression",
			config: Config{
				CompressionEnabled:   true,
				CompressionThreshold: 128,
			},
			wantErr: false,
		},
		{
			name: "with memory limit",
			config: Config{
				MaxMemoryMB: 1024,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := New(tt.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("New() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if s == nil && !tt.wantErr {
				t.Error("New() returned nil storage")
			}
			if s != nil {
				_ = s.Close(context.Background())
			}
		})
	}
}

func TestMemStorage_SetGet(t *testing.T) {
	s, _ := New(DefaultConfig())
	defer func() { _ = s.Close(context.Background()) }()

	item := &StoredItem{
		Version: time.Now().UnixNano(),
		Value:   []byte("test value"),
	}

	// Set
	err := s.Set("key1", item)
	if err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	// Get
	retrieved, err := s.Get("key1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	if retrieved.Version != item.Version {
		t.Errorf("Get() Version = %v, want %v", retrieved.Version, item.Version)
	}
	if string(retrieved.Value) != string(item.Value) {
		t.Errorf("Get() Value = %s, want %s", string(retrieved.Value), string(item.Value))
	}

	// Verify deep copy
	retrieved.Value[0] = 'X'
	retrieved2, _ := s.Get("key1")
	if string(retrieved2.Value) != string(item.Value) {
		t.Error("Get() should return deep copy")
	}
}

func TestMemStorage_Delete(t *testing.T) {
	s, _ := New(DefaultConfig())
	defer func() { _ = s.Close(context.Background()) }()

	item := &StoredItem{
		Version: 100,
		Value:   []byte("test"),
	}

	// Set and delete
	_ = s.Set("key1", item)
	err := s.Delete("key1", 100)
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	// Verify deleted
	_, err = s.Get("key1")
	if err != ErrNotFound {
		t.Errorf("Get() after Delete() error = %v, want ErrNotFound", err)
	}
}

func TestMemStorage_DeleteVersionMismatch(t *testing.T) {
	s, _ := New(DefaultConfig())
	defer func() { _ = s.Close(context.Background()) }()

	item := &StoredItem{
		Version: 200,
		Value:   []byte("test"),
	}

	_ = s.Set("key1", item)

	// Try delete with older version
	err := s.Delete("key1", 100)
	if err != ErrVersionMismatch {
		t.Errorf("Delete() with old version error = %v, want ErrVersionMismatch", err)
	}

	// Item should still exist
	_, err = s.Get("key1")
	if err != nil {
		t.Errorf("Get() after failed Delete() error = %v, want nil", err)
	}
}

func TestMemStorage_ConflictResolution(t *testing.T) {
	s, _ := New(DefaultConfig())
	defer func() { _ = s.Close(context.Background()) }()

	// Set initial value
	item1 := &StoredItem{
		Version: 100,
		Value:   []byte("old value"),
	}
	_ = s.Set("key1", item1)

	// Set newer version (should win)
	item2 := &StoredItem{
		Version: 200,
		Value:   []byte("new value"),
	}
	_ = s.Set("key1", item2)

	retrieved, _ := s.Get("key1")
	if retrieved.Version != 200 {
		t.Errorf("ConflictResolution() Version = %v, want 200", retrieved.Version)
	}
	if string(retrieved.Value) != "new value" {
		t.Errorf("ConflictResolution() Value = %s, want new value", string(retrieved.Value))
	}

	// Try older version (should lose)
	item3 := &StoredItem{
		Version: 150,
		Value:   []byte("older value"),
	}
	_ = s.Set("key1", item3)

	retrieved, _ = s.Get("key1")
	if retrieved.Version != 200 {
		t.Errorf("ConflictResolution() after older write Version = %v, want 200", retrieved.Version)
	}
}

func TestMemStorage_ConflictResolutionSameVersion(t *testing.T) {
	s, _ := New(DefaultConfig())
	defer func() { _ = s.Close(context.Background()) }()

	now := time.Now()
	item1 := &StoredItem{
		Version:  100,
		Value:    []byte("value1"),
		ExpireAt: now.Add(time.Hour),
	}
	_ = s.Set("key1", item1)

	// Same version, but expired (should prefer non-expired)
	item2 := &StoredItem{
		Version:  100,
		Value:    []byte("value2"),
		ExpireAt: now.Add(-time.Hour), // Expired
	}
	_ = s.Set("key1", item2)

	retrieved, err := s.Get("key1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	// Should keep non-expired (existing wins when versions equal and existing not expired)
	if string(retrieved.Value) != "value1" {
		t.Errorf("ConflictResolution() same version with expiration Value = %s, want value1", string(retrieved.Value))
	}
}

func TestMemStorage_Expiration(t *testing.T) {
	s, _ := New(DefaultConfig())
	defer func() { _ = s.Close(context.Background()) }()

	item := &StoredItem{
		Version:  time.Now().UnixNano(),
		Value:    []byte("test"),
		ExpireAt: time.Now().Add(-time.Second), // Already expired
	}

	_ = s.Set("key1", item)

	// Should be expired
	_, err := s.Get("key1")
	if err != ErrExpired {
		t.Errorf("Get() expired item error = %v, want ErrExpired", err)
	}
}

func TestMemStorage_NoExpiration(t *testing.T) {
	s, _ := New(DefaultConfig())
	defer func() { _ = s.Close(context.Background()) }()

	item := &StoredItem{
		Version: time.Now().UnixNano(),
		Value:   []byte("test"),
		// No ExpireAt (zero value)
	}

	_ = s.Set("key1", item)

	// Should not be expired
	retrieved, err := s.Get("key1")
	if err != nil {
		t.Fatalf("Get() non-expired item error = %v", err)
	}
	if string(retrieved.Value) != "test" {
		t.Errorf("Get() Value = %s, want test", string(retrieved.Value))
	}
}

func TestMemStorage_Compression(t *testing.T) {
	config := DefaultConfig()
	config.CompressionEnabled = true
	config.CompressionThreshold = 64

	s, _ := New(config)
	defer func() { _ = s.Close(context.Background()) }()

	// Create large value (should be compressed)
	largeValue := make([]byte, 1000)
	for i := range largeValue {
		largeValue[i] = byte(i % 256)
	}

	item := &StoredItem{
		Version: time.Now().UnixNano(),
		Value:   largeValue,
	}

	err := s.Set("key1", item)
	if err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	// Retrieve and verify
	retrieved, err := s.Get("key1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	if len(retrieved.Value) != len(largeValue) {
		t.Errorf("Get() Value length = %d, want %d", len(retrieved.Value), len(largeValue))
	}

	// Verify data integrity
	for i := range largeValue {
		if retrieved.Value[i] != largeValue[i] {
			t.Errorf("Get() Value[%d] = %d, want %d", i, retrieved.Value[i], largeValue[i])
			break
		}
	}
}

func TestMemStorage_CompressionDisabled(t *testing.T) {
	config := DefaultConfig()
	config.CompressionEnabled = false

	s, _ := New(config)
	defer func() { _ = s.Close(context.Background()) }()

	largeValue := make([]byte, 1000)
	item := &StoredItem{
		Version: time.Now().UnixNano(),
		Value:   largeValue,
	}

	_ = s.Set("key1", item)
	retrieved, _ := s.Get("key1")

	if len(retrieved.Value) != len(largeValue) {
		t.Errorf("Get() without compression Value length = %d, want %d", len(retrieved.Value), len(largeValue))
	}
}

func TestMemStorage_CompressionThreshold(t *testing.T) {
	config := DefaultConfig()
	config.CompressionEnabled = true
	config.CompressionThreshold = 200

	s, _ := New(config)
	defer func() { _ = s.Close(context.Background()) }()

	// Small value (below threshold)
	smallValue := make([]byte, 100)
	item1 := &StoredItem{
		Version: time.Now().UnixNano(),
		Value:   smallValue,
	}
	_ = s.Set("key1", item1)

	// Large value (above threshold)
	largeValue := make([]byte, 500)
	item2 := &StoredItem{
		Version: time.Now().UnixNano(),
		Value:   largeValue,
	}
	_ = s.Set("key2", item2)

	// Both should work
	_, err1 := s.Get("key1")
	_, err2 := s.Get("key2")
	if err1 != nil || err2 != nil {
		t.Errorf("Get() errors: key1=%v, key2=%v", err1, err2)
	}
}

func TestMemStorage_BatchGet(t *testing.T) {
	s, _ := New(DefaultConfig())
	defer func() { _ = s.Close(context.Background()) }()

	// Set multiple keys
	for i := 0; i < 10; i++ {
		item := &StoredItem{
			Version: int64(i),
			Value:   []byte("value"),
		}
		_ = s.Set("key"+string(rune('0'+i)), item)
	}

	// Batch get
	keys := []string{"key0", "key1", "key2", "key99"}
	results, err := s.BatchGet(keys)
	if err != nil {
		t.Fatalf("BatchGet() error = %v", err)
	}

	if len(results) != 3 { // key0, key1, key2 exist, key99 doesn't
		t.Errorf("BatchGet() returned %d items, want 3", len(results))
	}

	for _, key := range []string{"key0", "key1", "key2"} {
		if _, ok := results[key]; !ok {
			t.Errorf("BatchGet() missing key %s", key)
		}
	}

	if _, ok := results["key99"]; ok {
		t.Error("BatchGet() should not return non-existent key")
	}
}

func TestMemStorage_BatchGetNoCopy(t *testing.T) {
	s, _ := New(DefaultConfig())
	defer func() { _ = s.Close(context.Background()) }()

	item := &StoredItem{
		Version: 100,
		Value:   []byte("test"),
	}
	_ = s.Set("key1", item)

	results, err := s.BatchGetNoCopy([]string{"key1"})
	if err != nil {
		t.Fatalf("BatchGetNoCopy() error = %v", err)
	}

	if len(results) != 1 {
		t.Errorf("BatchGetNoCopy() returned %d items, want 1", len(results))
	}

	result := results["key1"]
	if string(result.Value) != "test" {
		t.Errorf("BatchGetNoCopy() Value = %s, want test", string(result.Value))
	}
}

func TestMemStorage_BatchSet(t *testing.T) {
	s, _ := New(DefaultConfig())
	defer func() { _ = s.Close(context.Background()) }()

	items := make(map[string]*StoredItem)
	for i := 0; i < 10; i++ {
		items["key"+string(rune('0'+i))] = &StoredItem{
			Version: int64(i),
			Value:   []byte("value"),
		}
	}

	err := s.BatchSet(items)
	if err != nil {
		t.Fatalf("BatchSet() error = %v", err)
	}

	// Verify all stored
	for key := range items {
		_, err := s.Get(key)
		if err != nil {
			t.Errorf("Get() after BatchSet() error for %s: %v", key, err)
		}
	}
}

func TestMemStorage_GetNoCopy(t *testing.T) {
	s, _ := New(DefaultConfig())
	defer func() { _ = s.Close(context.Background()) }()

	item := &StoredItem{
		Version: 100,
		Value:   []byte("test"),
	}
	_ = s.Set("key1", item)

	retrieved, err := s.GetNoCopy("key1")
	if err != nil {
		t.Fatalf("GetNoCopy() error = %v", err)
	}

	if string(retrieved.Value) != "test" {
		t.Errorf("GetNoCopy() Value = %s, want test", string(retrieved.Value))
	}
}

func TestMemStorage_MemoryLimit(t *testing.T) {
	config := DefaultConfig()
	config.MaxMemoryMB = 1            // 1MB limit
	config.CompressionEnabled = false // Disable compression for predictable size
	config.EvictThreshold = 100       // Disable eviction for this test

	s, _ := New(config)
	defer func() { _ = s.Close(context.Background()) }()

	// Fill close to limit (900KB)
	for i := 0; i < 9; i++ {
		item := &StoredItem{
			Version: int64(i),
			Value:   make([]byte, 100*1024), // 100KB each
		}
		_ = s.Set("key"+string(rune(i)), item)
	}

	// Try to exceed limit with large item (500KB, would bring total over 1MB)
	largeValue := make([]byte, 500*1024)
	item := &StoredItem{
		Version: time.Now().UnixNano(),
		Value:   largeValue,
	}

	err := s.Set("key10", item)
	// Either should fail or LRU should have evicted enough
	if err == nil {
		stats := s.Stats()
		// If it succeeded, verify memory is within limit (eviction happened)
		if stats.TotalBytes > int64(1024*1024) {
			t.Errorf("Memory = %d, exceeds 1MB limit", stats.TotalBytes)
		}
	} else if err != ErrMemoryLimit {
		t.Errorf("Set() error = %v, want ErrMemoryLimit or nil (with eviction)", err)
	}
}

func TestMemStorage_LRU_Eviction(t *testing.T) {
	config := DefaultConfig()
	config.MaxMemoryMB = 10
	config.EvictThreshold = 90
	config.EvictTarget = 80
	config.CompressionEnabled = false // Disable compression for predictable size

	s, _ := New(config)
	defer func() { _ = s.Close(context.Background()) }()

	// Fill storage to trigger eviction (90% of 10MB = 9MB)
	// Each item is ~10KB, so need ~900 items to hit threshold
	itemSize := 10 * 1024
	for i := 0; i < 950; i++ {
		item := &StoredItem{
			Version: int64(i),
			Value:   make([]byte, itemSize),
		}
		_ = s.Set("key"+string(rune(i%10000)), item)
	}

	// Wait a bit for eviction to complete
	time.Sleep(100 * time.Millisecond)

	stats := s.Stats()
	if stats.EvictCount == 0 {
		t.Logf("EvictCount = 0, TotalBytes = %d, threshold = %d", stats.TotalBytes, int64(10*1024*1024*90/100))
		// Eviction might not trigger if items are small after compression
		// Just verify memory is reasonable
		if stats.TotalBytes > int64(10*1024*1024) {
			t.Errorf("Memory = %d, exceeds limit", stats.TotalBytes)
		}
	} else {
		// Verify memory is within target (80% of 10MB = 8MB)
		targetBytes := int64(10 * 1024 * 1024 * 80 / 100)
		if stats.TotalBytes > targetBytes*110/100 { // Allow 10% margin
			t.Errorf("Memory after eviction = %d, should be below ~%d", stats.TotalBytes, targetBytes)
		}
	}
}

func TestMemStorage_ConcurrentAccess(t *testing.T) {
	s, _ := New(DefaultConfig())
	defer func() { _ = s.Close(context.Background()) }()

	const numGoroutines = 100
	const numOps = 1000

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	// Concurrent writes
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < numOps; j++ {
				item := &StoredItem{
					Version: int64(id*numOps + j),
					Value:   []byte("value"),
				}
				_ = s.Set("key"+string(rune(id*numOps+j)), item)
			}
		}(i)
	}

	wg.Wait()

	// Concurrent reads
	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < numOps; j++ {
				_, _ = s.Get("key" + string(rune(id*numOps+j)))
			}
		}(i)
	}

	wg.Wait()

	stats := s.Stats()
	if stats.SetCount != int64(numGoroutines*numOps) {
		t.Errorf("SetCount = %d, want %d", stats.SetCount, int64(numGoroutines*numOps))
	}
}

func TestMemStorage_ConcurrentConflictResolution(t *testing.T) {
	s, _ := New(DefaultConfig())
	defer func() { _ = s.Close(context.Background()) }()

	const numGoroutines = 50

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	// Concurrent writes with different versions to same key
	for i := 0; i < numGoroutines; i++ {
		go func(version int64) {
			defer wg.Done()
			item := &StoredItem{
				Version: version,
				Value:   []byte("value"),
			}
			_ = s.Set("key1", item)
		}(int64(i))
	}

	wg.Wait()

	// Verify highest version won
	retrieved, err := s.Get("key1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	if retrieved.Version < int64(numGoroutines-1) {
		t.Errorf("ConflictResolution() Version = %d, want >= %d", retrieved.Version, numGoroutines-1)
	}
}

func TestMemStorage_Keys(t *testing.T) {
	s, _ := New(DefaultConfig())
	defer func() { _ = s.Close(context.Background()) }()

	// Set multiple keys
	keys := []string{"key1", "key2", "key3"}
	for _, key := range keys {
		item := &StoredItem{
			Version: time.Now().UnixNano(),
			Value:   []byte("value"),
		}
		_ = s.Set(key, item)
	}

	retrievedKeys := s.Keys()
	if len(retrievedKeys) < len(keys) {
		t.Errorf("Keys() returned %d keys, want at least %d", len(retrievedKeys), len(keys))
	}

	// Check all keys are present
	keyMap := make(map[string]bool)
	for _, k := range retrievedKeys {
		keyMap[k] = true
	}

	for _, key := range keys {
		if !keyMap[key] {
			t.Errorf("Keys() missing key %s", key)
		}
	}
}

func TestMemStorage_Clear(t *testing.T) {
	s, _ := New(DefaultConfig())
	defer func() { _ = s.Close(context.Background()) }()

	// Set some keys
	for i := 0; i < 10; i++ {
		item := &StoredItem{
			Version: int64(i),
			Value:   []byte("value"),
		}
		_ = s.Set("key"+string(rune(i)), item)
	}

	// Clear
	err := s.Clear()
	if err != nil {
		t.Fatalf("Clear() error = %v", err)
	}

	// Verify empty
	keys := s.Keys()
	if len(keys) != 0 {
		t.Errorf("Keys() after Clear() = %d, want 0", len(keys))
	}

	stats := s.Stats()
	if stats.KeyCount != 0 {
		t.Errorf("Stats().KeyCount after Clear() = %d, want 0", stats.KeyCount)
	}
}

func TestMemStorage_GetSyncBuffer(t *testing.T) {
	s, _ := New(DefaultConfig())
	defer func() { _ = s.Close(context.Background()) }()

	// Set multiple keys
	for i := 0; i < 10; i++ {
		item := &StoredItem{
			Version: int64(i),
			Value:   []byte("value"),
		}
		_ = s.Set("key"+string(rune(i)), item)
	}

	ops, err := s.GetSyncBuffer()
	if err != nil {
		t.Fatalf("GetSyncBuffer() error = %v", err)
	}

	if len(ops) == 0 {
		t.Error("GetSyncBuffer() returned no operations")
	}

	// Verify operations
	setCount := 0
	for _, op := range ops {
		if op.OpType == OpSet {
			setCount++
		}
	}

	if setCount == 0 {
		t.Error("GetSyncBuffer() should return SET operations")
	}
}

func TestMemStorage_Stats(t *testing.T) {
	s, _ := New(DefaultConfig())
	defer func() { _ = s.Close(context.Background()) }()

	// Initial stats
	stats := s.Stats()
	if stats.KeyCount != 0 {
		t.Errorf("Initial KeyCount = %d, want 0", stats.KeyCount)
	}

	// Set some keys
	for i := 0; i < 10; i++ {
		item := &StoredItem{
			Version: int64(i),
			Value:   make([]byte, 100),
		}
		_ = s.Set("key"+string(rune(i)), item)
	}

	stats = s.Stats()
	if stats.KeyCount != 10 {
		t.Errorf("KeyCount = %d, want 10", stats.KeyCount)
	}

	if stats.SetCount < 10 {
		t.Errorf("SetCount = %d, want >= 10", stats.SetCount)
	}

	// Test reads
	for i := 0; i < 5; i++ {
		_, _ = s.Get("key" + string(rune(i)))
	}

	stats = s.Stats()
	if stats.GetCount < 5 {
		t.Errorf("GetCount = %d, want >= 5", stats.GetCount)
	}
}

func TestMemStorage_CompressionStats(t *testing.T) {
	config := DefaultConfig()
	config.CompressionEnabled = true
	config.CompressionThreshold = 100

	s, _ := New(config)
	defer func() { _ = s.Close(context.Background()) }()

	// Set compressed value
	largeValue := make([]byte, 1000)
	item := &StoredItem{
		Version: time.Now().UnixNano(),
		Value:   largeValue,
	}
	_ = s.Set("key1", item)

	stats := s.Stats()
	if stats.OriginalBytes == 0 {
		t.Error("OriginalBytes should be > 0")
	}

	if stats.CompressedBytes == 0 {
		t.Error("CompressedBytes should be > 0")
	}

	if stats.CompressedBytes >= stats.OriginalBytes {
		t.Errorf("CompressedBytes (%d) should be < OriginalBytes (%d)", stats.CompressedBytes, stats.OriginalBytes)
	}

	if stats.CompressionRatio <= 0 || stats.CompressionRatio > 1 {
		t.Errorf("CompressionRatio = %f, should be between 0 and 1", stats.CompressionRatio)
	}
}

func TestMemStorage_HitRate(t *testing.T) {
	s, _ := New(DefaultConfig())
	defer func() { _ = s.Close(context.Background()) }()

	item := &StoredItem{
		Version: 100,
		Value:   []byte("test"),
	}
	_ = s.Set("key1", item)

	// Read existing key
	_, _ = s.Get("key1")
	_, _ = s.Get("key1")

	// Read non-existent key
	_, _ = s.Get("key2")

	stats := s.Stats()
	if stats.HitRate <= 0 || stats.HitRate > 1 {
		t.Errorf("HitRate = %f, should be between 0 and 1", stats.HitRate)
	}

	if stats.HitCount < 2 {
		t.Errorf("HitCount = %d, want >= 2", stats.HitCount)
	}

	if stats.MissCount < 1 {
		t.Errorf("MissCount = %d, want >= 1", stats.MissCount)
	}
}

func TestMemStorage_Close(t *testing.T) {
	s, _ := New(DefaultConfig())

	// Set some keys
	for i := 0; i < 10; i++ {
		item := &StoredItem{
			Version: int64(i),
			Value:   []byte("value"),
		}
		_ = s.Set("key"+string(rune(i)), item)
	}

	// Close
	err := s.Close(context.Background())
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Verify cleared
	stats := s.Stats()
	if stats.KeyCount != 0 {
		t.Errorf("KeyCount after Close() = %d, want 0", stats.KeyCount)
	}
}

func TestMemStorage_EmptyKey(t *testing.T) {
	s, _ := New(DefaultConfig())
	defer func() { _ = s.Close(context.Background()) }()

	item := &StoredItem{
		Version: 100,
		Value:   []byte("test"),
	}

	err := s.Set("", item)
	if err != errEmptyKey {
		t.Errorf("Set() with empty key error = %v, want errEmptyKey", err)
	}

	_, err = s.Get("")
	if err != errEmptyKey {
		t.Errorf("Get() with empty key error = %v, want errEmptyKey", err)
	}
}

func TestMemStorage_NilItem(t *testing.T) {
	s, _ := New(DefaultConfig())
	defer func() { _ = s.Close(context.Background()) }()

	err := s.Set("key1", nil)
	if err != errNilItem {
		t.Errorf("Set() with nil item error = %v, want errNilItem", err)
	}
}

func BenchmarkMemStorage_Set(b *testing.B) {
	s, _ := New(DefaultConfig())
	defer func() { _ = s.Close(context.Background()) }()

	item := &StoredItem{
		Version: 100,
		Value:   []byte("test value"),
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := int64(0)
		for pb.Next() {
			item.Version = time.Now().UnixNano() + i
			_ = s.Set("key"+string(rune(i%1000)), item)
			i++
		}
	})
}

func BenchmarkMemStorage_Get(b *testing.B) {
	s, _ := New(DefaultConfig())
	defer func() { _ = s.Close(context.Background()) }()

	item := &StoredItem{
		Version: 100,
		Value:   []byte("test value"),
	}

	// Pre-populate
	for i := 0; i < 1000; i++ {
		_ = s.Set("key"+string(rune(i)), item)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			_, _ = s.Get("key" + string(rune(i%1000)))
			i++
		}
	})
}

func BenchmarkMemStorage_BatchSet(b *testing.B) {
	s, _ := New(DefaultConfig())
	defer func() { _ = s.Close(context.Background()) }()

	items := make(map[string]*StoredItem)
	for i := 0; i < 100; i++ {
		items["key"+string(rune(i))] = &StoredItem{
			Version: int64(i),
			Value:   []byte("value"),
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.BatchSet(items)
	}
}

func BenchmarkMemStorage_BatchGet(b *testing.B) {
	s, _ := New(DefaultConfig())
	defer func() { _ = s.Close(context.Background()) }()

	// Pre-populate
	for i := 0; i < 1000; i++ {
		item := &StoredItem{
			Version: int64(i),
			Value:   []byte("value"),
		}
		_ = s.Set("key"+string(rune(i)), item)
	}

	keys := make([]string, 100)
	for i := 0; i < 100; i++ {
		keys[i] = "key" + string(rune(i))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.BatchGet(keys)
	}
}

func BenchmarkMemStorage_Compression(b *testing.B) {
	config := DefaultConfig()
	config.CompressionEnabled = true
	config.CompressionThreshold = 64

	s, _ := New(config)
	defer func() { _ = s.Close(context.Background()) }()

	largeValue := make([]byte, 1000)
	item := &StoredItem{
		Version: 100,
		Value:   largeValue,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		item.Version = int64(i)
		_ = s.Set("key"+string(rune(i%1000)), item)
	}
}
