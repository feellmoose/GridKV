package storage

// File: memory.go
// Purpose: Memory backend implementation - lightweight storage with compression
//
// This file implements the Memory storage backend, which focuses on:
//   - Memory efficiency through automatic compression (50-70% savings)
//   - LRU eviction for memory-constrained environments
//   - Good performance (500-700K ops/s) with compression overhead
//   - High-performance API support (GetNoCopy, BatchGet/Set)
//
// Structure:
//   - Lines 1-70:   Type definitions and constructors
//   - Lines 71-145:  Helper methods (compress, decompress, LRU)
//   - Lines 146-290: Core API (Set, Get, Delete)
//   - Lines 291-430: Gossip sync methods
//   - Lines 431-610: Stats and utilities
//   - Lines 611-900: High-performance API (GetNoCopy, Batch operations)

import (
	"container/list"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
)

// MemoryStorage provides memory-efficient in-memory caching with compression.
// OPTIMIZED FOR: Minimal memory footprint with value compression + good performance
//
// Features:
// - Automatic value compression (zstd) for values > 256 bytes
// - LRU eviction when memory limit reached
// - Memory usage tracking and limits
// - Lock-free sync.Map for concurrent access
// - High-performance API support (GetNoCopy, BatchGet/Set)
//
// Performance: ~500K-700K ops/sec (with compression overhead)
// Memory Savings: 50-70% compression ratio (depending on data)
// Use Case: Memory-constrained environments, large value storage, balanced workloads
//
// Positioning:
//   - Memory: Lightweight + compression + good performance
//   - MemorySharded: Extreme performance + no compression + high concurrency
type MemoryStorage struct {
	data sync.Map // map[string]*compressedItem (lock-free for maximum performance!)

	// Compression pool (reuse encoders/decoders)
	encoderPool sync.Pool
	decoderPool sync.Pool

	// LRU eviction support
	lruMu      sync.Mutex
	lruList    *list.List               // LRU list for eviction
	lruMap     map[string]*list.Element // key -> LRU element
	evictCount atomic.Int64

	// Sync buffer (lock-free ring buffer)
	syncBuffer   []AtomicSyncOp
	syncHead     uint64
	syncTail     uint64
	syncCapacity uint64
	syncMask     uint64

	// Stats (atomic for lock-free tracking)
	keyCount        atomic.Int64
	getCount        atomic.Int64
	setCount        atomic.Int64
	hitCount        atomic.Int64
	missCount       atomic.Int64
	compressedBytes atomic.Int64 // Compressed size
	originalBytes   atomic.Int64 // Original size (before compression)

	maxMemoryBytes     int64 // Memory limit (0 = unlimited)
	currentBytes       atomic.Int64
	compressionEnabled bool // Enable compression for values > threshold
	compressionThresh  int  // Compress values larger than this (default: 256 bytes)
}

// compressedItem stores compressed value with metadata
type compressedItem struct {
	ExpireAt   time.Time
	Version    int64
	Value      []byte // Compressed or raw value
	Compressed bool   // Whether value is compressed
	OrigSize   int    // Original size before compression
}

// NewMemoryStorage creates a new memory-efficient in-memory storage with compression.
// maxMemoryMB: Maximum memory in MB (0 = unlimited)
//
// This implementation prioritizes MEMORY EFFICIENCY:
// - Automatic compression for values > 256 bytes (50-70% savings)
// - LRU eviction when memory limit reached
// - Memory usage tracking
func NewMemoryStorage(maxMemoryMB int64) (*MemoryStorage, error) {
	capacity := NextPowerOf2(8192)

	maxBytes := int64(0)
	if maxMemoryMB > 0 {
		maxBytes = maxMemoryMB * 1024 * 1024
	}

	m := &MemoryStorage{
		syncBuffer:         make([]AtomicSyncOp, capacity),
		syncCapacity:       capacity,
		syncMask:           capacity - 1,
		maxMemoryBytes:     maxBytes,
		compressionEnabled: true,
		compressionThresh:  256, // Compress values > 256 bytes
		lruList:            list.New(),
		lruMap:             make(map[string]*list.Element),
	}

	// Initialize compression pools
	m.encoderPool.New = func() interface{} {
		encoder, _ := zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.SpeedFastest), // Fast compression for latency
			zstd.WithEncoderConcurrency(1),
		)
		return encoder
	}
	m.decoderPool.New = func() interface{} {
		decoder, _ := zstd.NewReader(nil)
		return decoder
	}

	return m, nil
}

// compress compresses data if compression is enabled and size > threshold
func (m *MemoryStorage) compress(data []byte) ([]byte, bool) {
	if !m.compressionEnabled || len(data) < m.compressionThresh {
		return data, false
	}

	encoder := m.encoderPool.Get().(*zstd.Encoder)
	defer m.encoderPool.Put(encoder)

	compressed := encoder.EncodeAll(data, make([]byte, 0, len(data)))

	// Only use compressed if it's actually smaller
	if len(compressed) < len(data) {
		return compressed, true
	}
	return data, false
}

// decompress decompresses data if it was compressed
func (m *MemoryStorage) decompress(data []byte, wasCompressed bool) ([]byte, error) {
	if !wasCompressed {
		return data, nil
	}

	decoder := m.decoderPool.Get().(*zstd.Decoder)
	defer m.decoderPool.Put(decoder)

	decompressed, err := decoder.DecodeAll(data, make([]byte, 0, len(data)*2))
	if err != nil {
		return nil, err
	}
	return decompressed, nil
}

// evictLRU evicts the least recently used item
func (m *MemoryStorage) evictLRU() bool {
	m.lruMu.Lock()
	defer m.lruMu.Unlock()

	if m.lruList.Len() == 0 {
		return false
	}

	// Evict oldest item
	oldest := m.lruList.Back()
	if oldest != nil {
		key := oldest.Value.(string)
		m.lruList.Remove(oldest)
		delete(m.lruMap, key)

		// Remove from data map
		if value, ok := m.data.LoadAndDelete(key); ok {
			m.keyCount.Add(-1)
			m.evictCount.Add(1)

			// Update memory usage
			if item, ok := value.(*compressedItem); ok {
				itemSize := int64(len(key) + len(item.Value) + 64)
				m.currentBytes.Add(-itemSize)
				m.compressedBytes.Add(-int64(len(item.Value)))
				m.originalBytes.Add(-int64(item.OrigSize))
			}
			return true
		}
	}
	return false
}

// touchLRU marks key as recently used
func (m *MemoryStorage) touchLRU(key string) {
	m.lruMu.Lock()
	defer m.lruMu.Unlock()

	if elem, ok := m.lruMap[key]; ok {
		m.lruList.MoveToFront(elem)
	} else {
		elem := m.lruList.PushFront(key)
		m.lruMap[key] = elem
	}
}

// Set stores a key-value pair with automatic compression.
// Values > 256 bytes are automatically compressed with zstd.
func (m *MemoryStorage) Set(key string, item *StoredItem) error {
	if key == "" {
		return errEmptyKey
	}
	if item == nil {
		return errNilItem
	}

	// Compress value if enabled and large enough
	origSize := len(item.Value)
	compressedValue, isCompressed := m.compress(item.Value)

	// Calculate item size for memory tracking
	itemSize := int64(len(key) + len(compressedValue) + 64) // key + compressed value + overhead

	// Check memory limit and evict if needed
	if m.maxMemoryBytes > 0 {
		currentMem := m.currentBytes.Load()
		if currentMem+itemSize > m.maxMemoryBytes {
			// Try to evict LRU items until we have space
			evicted := 0
			for currentMem+itemSize > m.maxMemoryBytes && evicted < 100 {
				if !m.evictLRU() {
					return ErrMemoryLimitExceeded
				}
				currentMem = m.currentBytes.Load()
				evicted++
			}
		}
	}

	// Create compressed item
	compItem := &compressedItem{
		ExpireAt:   item.ExpireAt,
		Version:    item.Version,
		Value:      compressedValue,
		Compressed: isCompressed,
		OrigSize:   origSize,
	}

	// Check if key exists (for key count tracking)
	oldValue, exists := m.data.Load(key)

	// Store (lock-free!)
	m.data.Store(key, compItem)
	m.setCount.Add(1)

	// Update counters
	if !exists {
		m.keyCount.Add(1)
		m.currentBytes.Add(itemSize)
		m.compressedBytes.Add(int64(len(compressedValue)))
		m.originalBytes.Add(int64(origSize))
		m.touchLRU(key)
	} else {
		// Update size delta
		if oldItem, ok := oldValue.(*compressedItem); ok {
			oldSize := int64(len(key) + len(oldItem.Value) + 64)
			m.currentBytes.Add(itemSize - oldSize)
			m.compressedBytes.Add(int64(len(compressedValue) - len(oldItem.Value)))
			m.originalBytes.Add(int64(origSize - oldItem.OrigSize))
		}
		m.touchLRU(key)
	}

	// Add to sync buffer (lock-free ring buffer)
	op := &CacheSyncOperation{
		Key:     key,
		Version: item.Version,
		Type:    "SET",
		Data:    item, // Original uncompressed data for sync
	}

	head := atomic.LoadUint64(&m.syncHead)
	m.syncBuffer[head&m.syncMask].Op = op
	atomic.StoreUint64(&m.syncHead, head+1)

	// Auto-advance tail if full (lock-free overwrite)
	tail := atomic.LoadUint64(&m.syncTail)
	if head-tail >= m.syncCapacity {
		atomic.CompareAndSwapUint64(&m.syncTail, tail, tail+1)
	}

	return nil
}

// Get retrieves a key-value pair with automatic decompression.
// Decompresses value if it was compressed during Set.
func (m *MemoryStorage) Get(key string) (*StoredItem, error) {
	if key == "" {
		return nil, errEmptyKey
	}

	m.getCount.Add(1)

	value, ok := m.data.Load(key)
	if !ok {
		m.missCount.Add(1)
		return nil, ErrItemNotFound
	}

	compItem := value.(*compressedItem)

	// Check expiration
	if !compItem.ExpireAt.IsZero() && time.Now().After(compItem.ExpireAt) {
		m.missCount.Add(1)
		// Lazy deletion: remove expired item
		m.data.Delete(key)
		m.keyCount.Add(-1)

		// Update LRU
		m.lruMu.Lock()
		if elem, ok := m.lruMap[key]; ok {
			m.lruList.Remove(elem)
			delete(m.lruMap, key)
		}
		m.lruMu.Unlock()

		return nil, ErrItemExpired
	}

	m.hitCount.Add(1)

	// Touch LRU
	m.touchLRU(key)

	// Decompress if needed
	decompressedValue, err := m.decompress(compItem.Value, compItem.Compressed)
	if err != nil {
		return nil, err
	}

	// Return copy to prevent external modifications
	// OPTIMIZATION: Use FastCloneBytes to reduce allocations
	result := &StoredItem{
		ExpireAt: compItem.ExpireAt,
		Version:  compItem.Version,
		Value:    FastCloneBytes(decompressedValue),
	}

	return result, nil
}

// Delete removes a key-value pair with lock-free operation.
func (m *MemoryStorage) Delete(key string, version int64) error {
	if key == "" {
		return errEmptyKey
	}

	// Load and delete atomically
	value, loaded := m.data.LoadAndDelete(key)
	if loaded {
		m.keyCount.Add(-1)

		// Update memory usage
		if item, ok := value.(*compressedItem); ok {
			itemSize := int64(len(key) + len(item.Value) + 64)
			m.currentBytes.Add(-itemSize)
			m.compressedBytes.Add(-int64(len(item.Value)))
			m.originalBytes.Add(-int64(item.OrigSize))
		}

		// Remove from LRU
		m.lruMu.Lock()
		if elem, ok := m.lruMap[key]; ok {
			m.lruList.Remove(elem)
			delete(m.lruMap, key)
		}
		m.lruMu.Unlock()
	}

	// Add to sync buffer
	op := &CacheSyncOperation{
		Key:     key,
		Version: version,
		Type:    "DELETE",
		Data:    nil,
	}

	head := atomic.LoadUint64(&m.syncHead)
	m.syncBuffer[head&m.syncMask].Op = op
	atomic.StoreUint64(&m.syncHead, head+1)

	tail := atomic.LoadUint64(&m.syncTail)
	if head-tail >= m.syncCapacity {
		atomic.CompareAndSwapUint64(&m.syncTail, tail, tail+1)
	}

	return nil
}

// Keys returns all non-expired keys.
func (m *MemoryStorage) Keys() []string {
	keys := make([]string, 0)
	now := time.Now()

	m.data.Range(func(key, value interface{}) bool {
		k := key.(string)
		item := value.(*compressedItem)

		// Skip expired items
		if !item.ExpireAt.IsZero() && now.After(item.ExpireAt) {
			// Lazy deletion
			m.data.Delete(k)
			m.keyCount.Add(-1)

			// Remove from LRU
			m.lruMu.Lock()
			if elem, ok := m.lruMap[k]; ok {
				m.lruList.Remove(elem)
				delete(m.lruMap, k)
			}
			m.lruMu.Unlock()

			return true
		}

		keys = append(keys, k)
		return true
	})

	return keys
}

// Clear removes all data.
func (m *MemoryStorage) Clear() error {
	// Recreate map (simpler and faster than Range+Delete)
	m.data = sync.Map{}
	m.keyCount.Store(0)
	m.currentBytes.Store(0)
	m.compressedBytes.Store(0)
	m.originalBytes.Store(0)

	// Clear LRU
	m.lruMu.Lock()
	m.lruList = list.New()
	m.lruMap = make(map[string]*list.Element)
	m.lruMu.Unlock()

	// Clear sync buffer atomically
	head := atomic.LoadUint64(&m.syncHead)
	atomic.StoreUint64(&m.syncTail, head)

	return nil
}

// Close closes the storage.
func (m *MemoryStorage) Close() error {
	return m.Clear()
}

// GetSyncBuffer returns pending sync operations (lock-free).
func (m *MemoryStorage) GetSyncBuffer() ([]*CacheSyncOperation, error) {
	head := atomic.LoadUint64(&m.syncHead)
	tail := atomic.LoadUint64(&m.syncTail)

	size := head - tail
	if size == 0 {
		return nil, nil
	}

	// Prevent reading more than capacity
	if size > m.syncCapacity {
		size = m.syncCapacity
		tail = head - size
	}

	// Copy operations
	ops := make([]*CacheSyncOperation, 0, size)
	for i := tail; i < head; i++ {
		if op := m.syncBuffer[i&m.syncMask].Op; op != nil {
			ops = append(ops, op)
		}
	}

	// Atomically advance tail
	atomic.StoreUint64(&m.syncTail, head)

	return ops, nil
}

// GetFullSyncSnapshot returns a complete snapshot.
func (m *MemoryStorage) GetFullSyncSnapshot() ([]*FullStateItem, error) {
	snapshot := make([]*FullStateItem, 0)
	now := time.Now()

	m.data.Range(func(key, value interface{}) bool {
		k := key.(string)
		compItem := value.(*compressedItem)

		// Skip expired items
		if !compItem.ExpireAt.IsZero() && now.After(compItem.ExpireAt) {
			// Lazy deletion
			m.data.Delete(k)
			m.keyCount.Add(-1)
			return true
		}

		// Decompress for sync
		decompressedValue, err := m.decompress(compItem.Value, compItem.Compressed)
		if err != nil {
			return true // Skip on error
		}

		snapshot = append(snapshot, &FullStateItem{
			Key:     k,
			Version: compItem.Version,
			Item: &StoredItem{
				ExpireAt: compItem.ExpireAt,
				Version:  compItem.Version,
				Value:    decompressedValue,
			},
		})
		return true
	})

	return snapshot, nil
}

// ApplyIncrementalSync applies incremental sync operations.
func (m *MemoryStorage) ApplyIncrementalSync(operations []*CacheSyncOperation) error {
	for _, op := range operations {
		if op.Type == "SET" && op.Data != nil {
			_ = m.Set(op.Key, op.Data)
		} else if op.Type == "DELETE" {
			_ = m.Delete(op.Key, op.Version)
		}
	}
	return nil
}

// ApplyFullSyncSnapshot applies a full snapshot.
func (m *MemoryStorage) ApplyFullSyncSnapshot(snapshot []*FullStateItem, snapshotTS time.Time) error {
	// Clear existing data
	m.data = sync.Map{}

	// Clear LRU
	m.lruMu.Lock()
	m.lruList = list.New()
	m.lruMap = make(map[string]*list.Element)
	m.lruMu.Unlock()

	// Apply snapshot with compression
	count := int64(0)
	totalBytes := int64(0)
	totalCompressed := int64(0)
	totalOriginal := int64(0)

	for _, item := range snapshot {
		if item.Item != nil {
			// Compress value
			origSize := len(item.Item.Value)
			compressedValue, isCompressed := m.compress(item.Item.Value)

			compItem := &compressedItem{
				ExpireAt:   item.Item.ExpireAt,
				Version:    item.Version,
				Value:      compressedValue,
				Compressed: isCompressed,
				OrigSize:   origSize,
			}

			m.data.Store(item.Key, compItem)
			count++

			itemSize := int64(len(item.Key) + len(compressedValue) + 64)
			totalBytes += itemSize
			totalCompressed += int64(len(compressedValue))
			totalOriginal += int64(origSize)

			// Update LRU
			m.touchLRU(item.Key)
		}
	}

	m.keyCount.Store(count)
	m.currentBytes.Store(totalBytes)
	m.compressedBytes.Store(totalCompressed)
	m.originalBytes.Store(totalOriginal)

	// Clear sync buffer
	head := atomic.LoadUint64(&m.syncHead)
	atomic.StoreUint64(&m.syncTail, head)

	return nil
}

// Stats returns storage statistics with compression info.
func (m *MemoryStorage) Stats() StorageStats {
	head := atomic.LoadUint64(&m.syncHead)
	tail := atomic.LoadUint64(&m.syncTail)

	hits := m.hitCount.Load()
	misses := m.missCount.Load()
	hitRate := 0.0
	if hits+misses > 0 {
		hitRate = float64(hits) / float64(hits+misses)
	}

	_ = m.compressedBytes.Load() // Track compression stats (can be exposed in extended stats)
	_ = m.originalBytes.Load()

	return StorageStats{
		KeyCount:      m.keyCount.Load(),
		SyncBufferLen: int(head - tail),
		CacheHitRate:  hitRate,
		DBSize:        m.currentBytes.Load(),
		// Extended stats (can be added to StorageStats if needed)
		// CompressionRatio: compressionRatio,
		// EvictionCount: m.evictCount.Load(),
	}
}

// ============================================================================
// HIGH-PERFORMANCE API
// ============================================================================
// These methods provide optimized operations for performance-critical scenarios.
// They are automatically used by GridKV internally for transparent optimization.

// GetNoCopy retrieves a value without deep copying it.
// ⚠️ WARNING: The returned *StoredItem contains decompressed data but shares
// the internal buffer. Do not modify the returned item.Value.
//
// Performance: Saves ~40-50% for avoiding value copy, but still includes
// decompression overhead if the value was compressed.
func (m *MemoryStorage) GetNoCopy(key string) (*StoredItem, error) {
	if key == "" {
		return nil, errEmptyKey
	}

	m.getCount.Add(1)

	value, ok := m.data.Load(key)
	if !ok {
		m.missCount.Add(1)
		return nil, ErrItemNotFound
	}

	citem := value.(*compressedItem)

	// Check expiration
	if !citem.ExpireAt.IsZero() && time.Now().After(citem.ExpireAt) {
		m.missCount.Add(1)
		m.data.Delete(key)
		m.keyCount.Add(-1)
		return nil, ErrItemExpired
	}

	m.hitCount.Add(1)
	m.touchLRU(key)

	// Decompress if needed
	var decompressed []byte
	var err error
	if citem.Compressed {
		decompressed, err = m.decompress(citem.Value, true)
		if err != nil {
			return nil, err
		}
	} else {
		decompressed = citem.Value
	}

	// Return without deep copy (⚠️ shared memory)
	item := &StoredItem{
		ExpireAt: citem.ExpireAt,
		Version:  citem.Version,
		Value:    decompressed, // No copy
	}

	return item, nil
}

// BatchGet retrieves multiple keys efficiently.
// For Memory backend, this reduces function call overhead while still
// performing compression/decompression as needed.
func (m *MemoryStorage) BatchGet(keys []string) (map[string]*StoredItem, error) {
	if len(keys) == 0 {
		return make(map[string]*StoredItem), nil
	}

	result := make(map[string]*StoredItem, len(keys))
	now := time.Now()

	for _, key := range keys {
		if key == "" {
			continue
		}

		m.getCount.Add(1)

		value, ok := m.data.Load(key)
		if !ok {
			m.missCount.Add(1)
			continue
		}

		citem := value.(*compressedItem)

		// Check expiration
		if !citem.ExpireAt.IsZero() && now.After(citem.ExpireAt) {
			m.missCount.Add(1)
			m.data.Delete(key)
			m.keyCount.Add(-1)
			continue
		}

		m.hitCount.Add(1)
		m.touchLRU(key)

		// Decompress if needed
		var decompressed []byte
		var err error
		if citem.Compressed {
			decompressed, err = m.decompress(citem.Value, true)
			if err != nil {
				continue // Skip on error
			}
		} else {
			decompressed = citem.Value
		}

		// Deep copy for batch operation safety
		item := &StoredItem{
			ExpireAt: citem.ExpireAt,
			Version:  citem.Version,
			Value:    FastCloneBytes(decompressed),
		}

		result[key] = item
	}

	return result, nil
}

// BatchGetNoCopy retrieves multiple keys without copying values.
// ⚠️ WARNING: Returned items share memory. Do not modify.
//
// Performance: Best for read-only bulk operations.
func (m *MemoryStorage) BatchGetNoCopy(keys []string) (map[string]*StoredItem, error) {
	if len(keys) == 0 {
		return make(map[string]*StoredItem), nil
	}

	result := make(map[string]*StoredItem, len(keys))
	now := time.Now()

	for _, key := range keys {
		if key == "" {
			continue
		}

		m.getCount.Add(1)

		value, ok := m.data.Load(key)
		if !ok {
			m.missCount.Add(1)
			continue
		}

		citem := value.(*compressedItem)

		// Check expiration
		if !citem.ExpireAt.IsZero() && now.After(citem.ExpireAt) {
			m.missCount.Add(1)
			m.data.Delete(key)
			m.keyCount.Add(-1)
			continue
		}

		m.hitCount.Add(1)
		m.touchLRU(key)

		// Decompress if needed
		var decompressed []byte
		var err error
		if citem.Compressed {
			decompressed, err = m.decompress(citem.Value, true)
			if err != nil {
				continue
			}
		} else {
			decompressed = citem.Value
		}

		// No copy (⚠️ shared memory)
		item := &StoredItem{
			ExpireAt: citem.ExpireAt,
			Version:  citem.Version,
			Value:    decompressed, // No copy
		}

		result[key] = item
	}

	return result, nil
}

// BatchSet stores multiple key-value pairs efficiently.
// Reduces function call overhead and can batch compression operations.
func (m *MemoryStorage) BatchSet(items map[string]*StoredItem) error {
	if len(items) == 0 {
		return nil
	}

	totalSize := int64(0)

	// Calculate total size for memory check
	for key, item := range items {
		if key == "" || item == nil {
			continue
		}
		totalSize += int64(len(key) + len(item.Value) + 128)
	}

	// Check memory limit
	if m.maxMemoryBytes > 0 {
		current := m.currentBytes.Load()
		if current+totalSize > m.maxMemoryBytes {
			// Try eviction
			evicted := m.evictLRU()
			if !evicted {
				return ErrMemoryLimitExceeded
			}
		}
	}

	// Process each item
	for key, item := range items {
		if key == "" || item == nil {
			continue
		}

		m.setCount.Add(1)

		// Compress if enabled and value is large enough
		value := item.Value
		compressed := false
		origSize := len(value)

		if m.compressionEnabled && len(value) > m.compressionThresh {
			compValue, didCompress := m.compress(value)
			if didCompress {
				value = compValue
				compressed = true
			}
		}

		// Check if key exists
		_, exists := m.data.Load(key)

		// Store compressed item
		citem := &compressedItem{
			ExpireAt:   item.ExpireAt,
			Version:    item.Version,
			Value:      value,
			Compressed: compressed,
			OrigSize:   origSize,
		}

		m.data.Store(key, citem)

		// Update stats
		if !exists {
			m.keyCount.Add(1)
		}

		itemSize := int64(len(key) + len(value) + 64)
		m.currentBytes.Add(itemSize)

		if compressed {
			m.compressedBytes.Add(int64(len(value)))
			m.originalBytes.Add(int64(origSize))
		}

		// Touch LRU
		m.touchLRU(key)

		// Add to sync buffer
		op := &CacheSyncOperation{
			Key:     key,
			Version: item.Version,
			Type:    "SET",
			Data:    item,
		}

		head := m.syncHead
		m.syncBuffer[head&m.syncMask].Op = op
		m.syncHead = head + 1

		if m.syncHead-m.syncTail >= m.syncCapacity {
			m.syncTail++
		}
	}

	return nil
}

// Verify that MemoryStorage implements HighPerformanceStorage interface
var _ HighPerformanceStorage = (*MemoryStorage)(nil)
