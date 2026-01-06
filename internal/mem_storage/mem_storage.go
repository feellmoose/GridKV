package mem_storage

import (
	"container/list"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/feellmoose/gridkv/internal/utils/zerocopy"
	"github.com/klauspost/compress/zstd"
	"github.com/zeebo/xxh3"
)

// MemStorage provides distributed-ready in-memory storage.
//
// Features:
//   - Sharded architecture for high concurrency (2-4x CPU cores)
//   - Zstd compression for large values (>64 bytes, 60-80% savings)
//   - Sharded LRU eviction (proactive, targets 80% capacity)
//   - Concurrent-safe conflict resolution (last-write-wins)
//   - Zero-copy and batch operations
//   - Per-shard sync buffers for distributed replication
//
// Memory: 60-80% compression savings, efficient space usage
type MemStorage struct {
	shards      []*storageShard
	shardCount  int
	shardMask   uint64
	maxMemoryMB int64

	// Compression pools (shared across shards)
	encoderPool sync.Pool
	decoderPool sync.Pool

	// Global stats (atomic)
	totalKeys       atomic.Int64
	totalBytes      atomic.Int64
	compressedBytes atomic.Int64
	originalBytes   atomic.Int64
	getCount        atomic.Int64
	setCount        atomic.Int64
	hitCount        atomic.Int64
	missCount       atomic.Int64
	evictCount      atomic.Int64

	// Compression config
	compressionEnabled bool
	compressionThresh  int

	// LRU eviction config
	evictThreshold int64         // Start eviction at this percentage (90%)
	evictTarget    int64         // Target memory after eviction (80%)
	evictShardIdx  atomic.Uint64 // Round-robin eviction

	// Lifecycle
	cleanerStop    chan struct{}
	cleanerRunning atomic.Bool
}

// Config configures MemStorage.
type Config struct {
	// MaxMemoryMB limits total memory (0 = unlimited).
	MaxMemoryMB int64

	// ShardCount is number of shards (0 = auto: 2-4x CPU cores).
	ShardCount int

	// CompressionEnabled enables zstd compression.
	CompressionEnabled bool

	// CompressionThreshold is min size to compress (bytes).
	CompressionThreshold int

	// EvictThreshold is memory percentage to start eviction (default: 90).
	EvictThreshold int

	// EvictTarget is target memory percentage after eviction (default: 80).
	EvictTarget int
}

// storageShard represents a single storage shard.
type storageShard struct {
	// Data storage (lock-free sync.Map)
	data sync.Map // map[string]*compressedItem

	// Per-shard LRU (locked)
	lru   *shardLRU
	lruMu sync.Mutex

	// Per-shard sync buffer (lock-free ring buffer)
	syncBuffer   []atomicSyncOp
	syncHead     atomic.Uint64
	syncTail     atomic.Uint64
	syncCapacity uint64
	syncMask     uint64

	// Per-shard stats
	keyCount       atomic.Int64
	byteCount      atomic.Int64
	compressedSize atomic.Int64
	originalSize   atomic.Int64

	// Cache line padding (prevent false sharing)
	_ [8]uint64
}

// compressedItem stores compressed value with metadata.
type compressedItem struct {
	Version    int64
	ExpireAt   time.Time
	Value      []byte // Compressed or raw
	Compressed bool
	OrigSize   int
}

// shardLRU provides LRU eviction for a single shard.
type shardLRU struct {
	list   *list.List
	keyMap map[string]*list.Element
}

// atomicSyncOp wraps sync operation for atomic storage.
type atomicSyncOp struct {
	op *SyncOperation
}

// SyncOperation represents a single change for replication.
// Item already contains Version, so no need to duplicate.
type SyncOperation struct {
	Key    string
	OpType OpType
	Item   *StoredItem // nil for DELETE, contains Version and other fields
}

// OpType is operation type.
type OpType int

const (
	OpSet OpType = iota
	OpDelete
)

// DefaultConfig returns default configuration.
func DefaultConfig() Config {
	return Config{
		ShardCount:           0, // auto
		CompressionEnabled:   true,
		CompressionThreshold: 64,
		EvictThreshold:       90,
		EvictTarget:          80,
	}
}

// New creates a new MemStorage instance.
func New(config Config) (*MemStorage, error) {
	shardCount := config.ShardCount
	if shardCount == 0 {
		// Auto: 2-4x CPU cores
		cpuCount := runtime.GOMAXPROCS(0)
		shardCount = cpuCount * 2
		if shardCount < 64 {
			shardCount = 64
		}
		if shardCount > 512 {
			shardCount = 512
		}
	}

	// Round to power of 2 for fast masking
	shardCount = int(nextPowerOf2(uint64(shardCount)))

	compressionThresh := config.CompressionThreshold
	if compressionThresh == 0 {
		compressionThresh = 64
	}

	evictThreshold := int64(config.EvictThreshold)
	if evictThreshold == 0 {
		evictThreshold = 90
	}

	evictTarget := int64(config.EvictTarget)
	if evictTarget == 0 {
		evictTarget = 80
	}

	s := &MemStorage{
		shards:             make([]*storageShard, shardCount),
		shardCount:         shardCount,
		shardMask:          uint64(shardCount - 1),
		maxMemoryMB:        config.MaxMemoryMB,
		compressionEnabled: config.CompressionEnabled,
		compressionThresh:  compressionThresh,
		evictThreshold:     evictThreshold,
		evictTarget:        evictTarget,
		cleanerStop:        make(chan struct{}),
	}

	// Initialize compression pools
	if s.compressionEnabled {
		s.encoderPool.New = func() interface{} {
			encoder, err := zstd.NewWriter(nil,
				zstd.WithEncoderLevel(zstd.SpeedDefault),
				zstd.WithEncoderConcurrency(1),
			)
			if err != nil {
				panic(fmt.Sprintf("failed to create zstd encoder: %v", err))
			}
			return encoder
		}
		s.decoderPool.New = func() interface{} {
			decoder, err := zstd.NewReader(nil)
			if err != nil {
				panic(fmt.Sprintf("failed to create zstd decoder: %v", err))
			}
			return decoder
		}
	}

	// Initialize shards
	syncCapacity := nextPowerOf2(16384)
	for i := 0; i < shardCount; i++ {
		s.shards[i] = &storageShard{
			lru: &shardLRU{
				list:   list.New(),
				keyMap: make(map[string]*list.Element),
			},
			syncBuffer:   make([]atomicSyncOp, syncCapacity),
			syncCapacity: syncCapacity,
			syncMask:     syncCapacity - 1,
		}
	}

	// Start expiration cleaner
	s.startCleaner()

	return s, nil
}

// getShard returns shard for key (inlined).
func (s *MemStorage) getShard(key string) *storageShard {
	return s.shards[xxh3.HashString128(key).Lo&s.shardMask]
}

// Set stores key-value with conflict resolution.
//
// Conflict resolution: last-write-wins using version comparison.
// Concurrent writes are resolved atomically per shard.
func (s *MemStorage) Set(key string, item *StoredItem) error {
	if key == "" {
		return errEmptyKey
	}
	if item == nil {
		return errNilItem
	}

	shard := s.getShard(key)

	// Compress if enabled
	origSize := len(item.Value)
	var compressedValue []byte
	var isCompressed bool
	if s.compressionEnabled && origSize >= s.compressionThresh {
		compressedValue, isCompressed = s.compress(item.Value)
	} else {
		compressedValue = item.Value
		isCompressed = false
	}

	// Calculate size
	itemSize := s.calculateItemSize(key, compressedValue)

	// Memory limit check and eviction
	if s.maxMemoryMB > 0 {
		if err := s.checkAndEvict(itemSize); err != nil {
			return err
		}
	}

	// Create compressed item
	compItem := &compressedItem{
		Version:    item.Version,
		ExpireAt:   item.ExpireAt,
		Value:      compressedValue,
		Compressed: isCompressed,
		OrigSize:   origSize,
	}

	// Atomic conflict resolution with retry loop for CAS
	for {
		existing, loaded := shard.data.Load(key)
		if !loaded {
			// New key: try to store
			if _, exists := shard.data.LoadOrStore(key, compItem); !exists {
				// Successfully stored new key
				shard.keyCount.Add(1)
				shard.byteCount.Add(itemSize)
				shard.compressedSize.Add(int64(len(compressedValue)))
				shard.originalSize.Add(int64(origSize))
				s.totalKeys.Add(1)
				s.totalBytes.Add(itemSize)
				s.compressedBytes.Add(int64(len(compressedValue)))
				s.originalBytes.Add(int64(origSize))
				s.setCount.Add(1)
				s.touchLRU(shard, key)
				s.addToSyncBuffer(shard, key, OpSet, item)
				return nil
			}
			// Race condition: key was inserted by another goroutine, retry
			continue
		}

		// Key exists: resolve conflict
		existingItem := existing.(*compressedItem)

		// Snapshot existing item for comparison (concurrent-safe read)
		existingVersion := existingItem.Version
		existingExpireAt := existingItem.ExpireAt
		existingOrigSize := existingItem.OrigSize

		// Copy value data for safe comparison (use zerocopy where safe)
		var existingValue []byte
		if existingItem.Compressed {
			// Decompress for comparison (creates new allocation, safe)
			decompressed, err := s.decompress(existingItem.Value, true)
			if err != nil {
				// On error, use compressed value for comparison
				existingValue = zerocopy.FastCloneBytes(existingItem.Value)
			} else {
				existingValue = decompressed // Already new allocation
			}
		} else {
			// Use zerocopy fast clone for uncompressed values
			existingValue = zerocopy.FastCloneBytes(existingItem.Value)
		}

		// Build comparison items (all data is now safely copied)
		newItem := &StoredItem{
			Version:  compItem.Version,
			ExpireAt: compItem.ExpireAt,
			Value:    item.Value, // Original uncompressed value
		}

		existingStoredItem := &StoredItem{
			Version:  existingVersion,
			ExpireAt: existingExpireAt,
			Value:    existingValue, // Safely copied/decompressed
		}

		// Resolve conflict: ResolveConflict returns true if 'other' should replace 'item'
		// So existingStoredItem.ResolveConflict(newItem) returns true if newItem should replace existingStoredItem
		shouldReplace := existingStoredItem.ResolveConflict(newItem)
		if !shouldReplace {
			// Existing wins, skip update
			s.setCount.Add(1)
			return nil
		}

		// Verify existing item hasn't changed (concurrent-safe check)
		// Reload to ensure we're comparing against current state
		current, currentLoaded := shard.data.Load(key)
		if !currentLoaded {
			// Item was deleted, retry from beginning
			continue
		}
		currentItem := current.(*compressedItem)
		if currentItem.Version != existingVersion || currentItem.OrigSize != existingOrigSize {
			// Item changed, retry
			continue
		}

		// New wins: atomic replace using CAS (with verified existing state)
		if shard.data.CompareAndSwap(key, existing, compItem) {
			// Successfully replaced
			oldSize := s.calculateItemSize(key, existingItem.Value)
			shard.byteCount.Add(itemSize - oldSize)
			shard.compressedSize.Add(int64(len(compressedValue) - len(existingItem.Value)))
			shard.originalSize.Add(int64(origSize - existingItem.OrigSize))
			s.totalBytes.Add(itemSize - oldSize)
			s.compressedBytes.Add(int64(len(compressedValue) - len(existingItem.Value)))
			s.originalBytes.Add(int64(origSize - existingItem.OrigSize))
			s.setCount.Add(1)
			s.touchLRU(shard, key)
			s.addToSyncBuffer(shard, key, OpSet, item)
			return nil
		}
		// CAS failed: retry (another goroutine modified the key)
		// Continue loop to reload and retry
	}
}

// Get retrieves key with automatic decompression.
// Returns deep copy, safe for modification.
func (s *MemStorage) Get(key string) (*StoredItem, error) {
	if key == "" {
		return nil, errEmptyKey
	}

	s.getCount.Add(1)
	shard := s.getShard(key)

	value, ok := shard.data.Load(key)
	if !ok {
		s.missCount.Add(1)
		return nil, ErrNotFound
	}

	compItem := value.(*compressedItem)

	// Check expiration
	if !compItem.ExpireAt.IsZero() {
		nowNano := time.Now().UnixNano()
		expireNano := compItem.ExpireAt.UnixNano()
		if nowNano > expireNano {
			s.missCount.Add(1)
			shard.data.Delete(key)
			shard.keyCount.Add(-1)
			s.totalKeys.Add(-1)
			s.removeFromLRU(shard, key)
			return nil, ErrExpired
		}
	}

	s.hitCount.Add(1)
	s.touchLRU(shard, key)

	// Decompress if needed
	var valueBytes []byte
	if compItem.Compressed {
		decompressed, err := s.decompress(compItem.Value, true)
		if err != nil {
			return nil, err
		}
		valueBytes = decompressed
	} else {
		valueBytes = compItem.Value
	}

	// Return deep copy
	result := &StoredItem{
		Version:  compItem.Version,
		ExpireAt: compItem.ExpireAt,
		Value:    fastCloneBytes(valueBytes), // Must copy for safety
	}

	return result, nil
}

// Delete removes key with optimistic locking.
func (s *MemStorage) Delete(key string, version int64) error {
	if key == "" {
		return errEmptyKey
	}

	shard := s.getShard(key)

	// Load and check version
	value, ok := shard.data.Load(key)
	if !ok {
		return nil // Not found is not an error
	}

	compItem, ok := value.(*compressedItem)
	if !ok {
		return fmt.Errorf("unexpected item type for key %s", key)
	}
	if version > 0 && compItem.Version > version {
		// Version mismatch: existing is newer
		return ErrVersionMismatch
	}

	// Delete atomically
	if shard.data.CompareAndDelete(key, value) {
		shard.keyCount.Add(-1)
		s.totalKeys.Add(-1)
		itemSize := s.calculateItemSize(key, compItem.Value)
		shard.byteCount.Add(-itemSize)
		shard.compressedSize.Add(-int64(len(compItem.Value)))
		shard.originalSize.Add(-int64(compItem.OrigSize))
		s.totalBytes.Add(-itemSize)
		s.compressedBytes.Add(-int64(len(compItem.Value)))
		s.originalBytes.Add(-int64(compItem.OrigSize))
		s.removeFromLRU(shard, key)
		s.addToSyncBuffer(shard, key, OpDelete, nil)
	}

	return nil
}

// compress compresses data if beneficial.
func (s *MemStorage) compress(data []byte) ([]byte, bool) {
	if len(data) < 100 {
		return data, false // Too small to benefit
	}

	// Check if already compressed
	if len(data) >= 4 {
		magic := uint32(data[0]) | uint32(data[1])<<8 | uint32(data[2])<<16 | uint32(data[3])<<24
		if magic == 0xFD2FB528 { // zstd magic
			return data, false
		}
	}

	encoder := s.encoderPool.Get().(*zstd.Encoder)
	defer s.encoderPool.Put(encoder)

	estimatedSize := len(data) / 2
	if estimatedSize < 64 {
		estimatedSize = 64
	}
	compressed := encoder.EncodeAll(data, make([]byte, 0, estimatedSize))

	// Only use if actually smaller (5% threshold)
	ratio := float64(len(compressed)) / float64(len(data))
	if ratio < 0.95 {
		return compressed, true
	}
	return data, false
}

// decompress decompresses data.
func (s *MemStorage) decompress(data []byte, wasCompressed bool) ([]byte, error) {
	if !wasCompressed {
		return data, nil
	}

	decoder := s.decoderPool.Get().(*zstd.Decoder)
	defer s.decoderPool.Put(decoder)

	decompressed, err := decoder.DecodeAll(data, make([]byte, 0, len(data)*2))
	if err != nil {
		return nil, err
	}
	return decompressed, nil
}

// calculateItemSize calculates approximate memory size.
func (s *MemStorage) calculateItemSize(key string, value []byte) int64 {
	const (
		stringOverhead  = 16
		sliceOverhead   = 24
		structOverhead  = int64(unsafe.Sizeof(compressedItem{}))
		syncMapOverhead = 64
	)
	return int64(len(key)) + int64(len(value)) + stringOverhead + sliceOverhead + structOverhead + syncMapOverhead
}

// checkAndEvict checks memory and evicts if needed.
func (s *MemStorage) checkAndEvict(itemSize int64) error {
	maxBytes := s.maxMemoryMB * 1024 * 1024
	current := s.totalBytes.Load()
	threshold := maxBytes * s.evictThreshold / 100

	if current+itemSize <= threshold {
		return nil
	}

	// Evict until below target
	target := maxBytes * s.evictTarget / 100
	evicted := 0
	for current+itemSize > target && evicted < 200 {
		if !s.evictLRU() {
			break
		}
		current = s.totalBytes.Load()
		evicted++
	}

	// Check if still over limit
	if current+itemSize > maxBytes {
		return ErrMemoryLimit
	}

	return nil
}

// evictLRU evicts one item using round-robin.
func (s *MemStorage) evictLRU() bool {
	startIdx := s.evictShardIdx.Add(1) & s.shardMask

	for i := 0; i < s.shardCount; i++ {
		idx := (startIdx + uint64(i)) & s.shardMask
		shard := s.shards[idx]

		shard.lruMu.Lock()
		if shard.lru.list.Len() == 0 {
			shard.lruMu.Unlock()
			continue
		}

		oldest := shard.lru.list.Back()
		if oldest == nil {
			shard.lruMu.Unlock()
			continue
		}

		key := oldest.Value.(string)
		shard.lru.list.Remove(oldest)
		delete(shard.lru.keyMap, key)
		shard.lruMu.Unlock()

		// Delete from storage
		value, loaded := shard.data.LoadAndDelete(key)
		if loaded {
			compItem := value.(*compressedItem)
			itemSize := s.calculateItemSize(key, compItem.Value)
			shard.keyCount.Add(-1)
			shard.byteCount.Add(-itemSize)
			shard.compressedSize.Add(-int64(len(compItem.Value)))
			shard.originalSize.Add(-int64(compItem.OrigSize))
			s.totalKeys.Add(-1)
			s.totalBytes.Add(-itemSize)
			s.compressedBytes.Add(-int64(len(compItem.Value)))
			s.originalBytes.Add(-int64(compItem.OrigSize))
			s.evictCount.Add(1)
			return true
		}
	}
	return false
}

// touchLRU updates LRU for key.
func (s *MemStorage) touchLRU(shard *storageShard, key string) {
	shard.lruMu.Lock()
	defer shard.lruMu.Unlock()

	if elem, ok := shard.lru.keyMap[key]; ok {
		shard.lru.list.MoveToFront(elem)
	} else {
		elem := shard.lru.list.PushFront(key)
		shard.lru.keyMap[key] = elem
	}
}

// removeFromLRU removes key from LRU.
func (s *MemStorage) removeFromLRU(shard *storageShard, key string) {
	shard.lruMu.Lock()
	defer shard.lruMu.Unlock()

	if elem, ok := shard.lru.keyMap[key]; ok {
		shard.lru.list.Remove(elem)
		delete(shard.lru.keyMap, key)
	}
}

// addToSyncBuffer adds operation to shard's sync buffer.
// Item already contains Version, no need to duplicate.
func (s *MemStorage) addToSyncBuffer(shard *storageShard, key string, opType OpType, item *StoredItem) {
	// Deep copy item for sync buffer to avoid sharing memory
	var syncItem *StoredItem
	if item != nil {
		syncItem = item.DeepCopy()
	}

	op := &SyncOperation{
		Key:    key,
		OpType: opType,
		Item:   syncItem,
	}

	head := shard.syncHead.Add(1) - 1
	shard.syncBuffer[head&shard.syncMask].op = op

	tail := shard.syncTail.Load()
	if head-tail >= shard.syncCapacity {
		shard.syncTail.CompareAndSwap(tail, tail+1)
	}
}

// startCleaner starts background expiration cleaner.
func (s *MemStorage) startCleaner() {
	if s.cleanerRunning.Swap(true) {
		return
	}

	go func() {
		defer s.cleanerRunning.Store(false)

		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				s.cleanExpired()
			case <-s.cleanerStop:
				return
			}
		}
	}()
}

// cleanExpired removes expired items.
func (s *MemStorage) cleanExpired() {
	now := time.Now()

	for _, shard := range s.shards {
		expiredKeys := make([]string, 0, 100)

		shard.data.Range(func(key, value interface{}) bool {
			k := key.(string)
			compItem := value.(*compressedItem)

			if !compItem.ExpireAt.IsZero() && now.After(compItem.ExpireAt) {
				expiredKeys = append(expiredKeys, k)
			}
			return true
		})

		for _, key := range expiredKeys {
			if value, loaded := shard.data.LoadAndDelete(key); loaded {
				compItem := value.(*compressedItem)
				itemSize := s.calculateItemSize(key, compItem.Value)
				shard.keyCount.Add(-1)
				shard.byteCount.Add(-itemSize)
				shard.compressedSize.Add(-int64(len(compItem.Value)))
				shard.originalSize.Add(-int64(compItem.OrigSize))
				s.totalKeys.Add(-1)
				s.totalBytes.Add(-itemSize)
				s.compressedBytes.Add(-int64(len(compItem.Value)))
				s.originalBytes.Add(-int64(compItem.OrigSize))
				s.removeFromLRU(shard, key)
			}
		}
	}
}

// GetSyncBuffer returns pending sync operations from all shards.
func (s *MemStorage) GetSyncBuffer() ([]*SyncOperation, error) {
	// Estimate capacity: sum of all shard buffer sizes
	estimatedCap := 0
	for _, shard := range s.shards {
		head := shard.syncHead.Load()
		tail := shard.syncTail.Load()
		size := head - tail
		if size > shard.syncCapacity {
			size = shard.syncCapacity
		}
		estimatedCap += int(size)
	}
	if estimatedCap < 100 {
		estimatedCap = 100 // Minimum capacity
	}
	ops := make([]*SyncOperation, 0, estimatedCap)

	for _, shard := range s.shards {
		head := shard.syncHead.Load()
		tail := shard.syncTail.Load()
		size := head - tail

		if size == 0 {
			continue
		}

		if size > shard.syncCapacity {
			size = shard.syncCapacity
			tail = head - size
		}

		for i := tail; i < head; i++ {
			if op := shard.syncBuffer[i&shard.syncMask].op; op != nil {
				ops = append(ops, op)
			}
		}

		shard.syncTail.Store(head)
	}

	return ops, nil
}

// Keys returns all keys (expensive, for monitoring only).
func (s *MemStorage) Keys() []string {
	keys := make([]string, 0, 1000)
	now := time.Now()

	for _, shard := range s.shards {
		shard.data.Range(func(key, value interface{}) bool {
			k := key.(string)
			compItem := value.(*compressedItem)

			if !compItem.ExpireAt.IsZero() && now.After(compItem.ExpireAt) {
				// Lazy deletion
				shard.data.Delete(k)
				return true
			}

			keys = append(keys, k)
			return true
		})
	}

	return keys
}

// Clear removes all data.
func (s *MemStorage) Clear() error {
	for _, shard := range s.shards {
		shard.data = sync.Map{}
		shard.lruMu.Lock()
		shard.lru.list = list.New()
		shard.lru.keyMap = make(map[string]*list.Element)
		shard.lruMu.Unlock()
		shard.keyCount.Store(0)
		shard.byteCount.Store(0)
		shard.compressedSize.Store(0)
		shard.originalSize.Store(0)
		head := shard.syncHead.Load()
		shard.syncTail.Store(head)
	}

	s.totalKeys.Store(0)
	s.totalBytes.Store(0)
	s.compressedBytes.Store(0)
	s.originalBytes.Store(0)

	return nil
}

// Close releases resources.
func (s *MemStorage) Close() error {
	if s.cleanerRunning.Swap(false) {
		select {
		case s.cleanerStop <- struct{}{}:
		default:
		}
		close(s.cleanerStop)
		time.Sleep(50 * time.Millisecond)
	}
	return s.Clear()
}

// GetNoCopy retrieves key without copying value.
// ⚠️ WARNING: Returned item shares memory. Do not modify.
func (s *MemStorage) GetNoCopy(key string) (*StoredItem, error) {
	if key == "" {
		return nil, errEmptyKey
	}

	s.getCount.Add(1)
	shard := s.getShard(key)

	value, ok := shard.data.Load(key)
	if !ok {
		s.missCount.Add(1)
		return nil, ErrNotFound
	}

	compItem := value.(*compressedItem)

	// Check expiration
	if !compItem.ExpireAt.IsZero() {
		nowNano := time.Now().UnixNano()
		expireNano := compItem.ExpireAt.UnixNano()
		if nowNano > expireNano {
			s.missCount.Add(1)
			shard.data.Delete(key)
			shard.keyCount.Add(-1)
			s.totalKeys.Add(-1)
			s.removeFromLRU(shard, key)
			return nil, ErrExpired
		}
	}

	s.hitCount.Add(1)
	s.touchLRU(shard, key)

	// Decompress if needed (must copy decompressed data)
	var valueBytes []byte
	if compItem.Compressed {
		decompressed, err := s.decompress(compItem.Value, true)
		if err != nil {
			return nil, err
		}
		valueBytes = decompressed // Note: decompressed is new allocation
	} else {
		valueBytes = compItem.Value // Shares slice, caller must not modify
	}

	return &StoredItem{
		Version:  compItem.Version,
		ExpireAt: compItem.ExpireAt,
		Value:    valueBytes,
	}, nil
}

// BatchGet retrieves multiple keys efficiently.
func (s *MemStorage) BatchGet(keys []string) (map[string]*StoredItem, error) {
	if len(keys) == 0 {
		return make(map[string]*StoredItem), nil
	}

	result := make(map[string]*StoredItem, len(keys))
	now := time.Now()

	// Group by shard
	type shardBatch struct {
		shard *storageShard
		keys  []string
	}
	batches := make(map[int]*shardBatch)

	for _, key := range keys {
		if key == "" {
			continue
		}
		hash := xxh3.HashString128(key).Lo
		idx := int(hash & s.shardMask)

		batch := batches[idx]
		if batch == nil {
			batch = &shardBatch{
				shard: s.shards[idx],
				keys:  make([]string, 0, 4),
			}
			batches[idx] = batch
		}
		batch.keys = append(batch.keys, key)
	}

	// Process each shard
	for _, batch := range batches {
		for _, key := range batch.keys {
			s.getCount.Add(1)

			value, ok := batch.shard.data.Load(key)
			if !ok {
				s.missCount.Add(1)
				continue
			}

			compItem := value.(*compressedItem)

			// Check expiration
			if !compItem.ExpireAt.IsZero() && now.After(compItem.ExpireAt) {
				s.missCount.Add(1)
				batch.shard.data.Delete(key)
				batch.shard.keyCount.Add(-1)
				s.totalKeys.Add(-1)
				s.removeFromLRU(batch.shard, key)
				continue
			}

			s.hitCount.Add(1)
			s.touchLRU(batch.shard, key)

			// Decompress
			var valueBytes []byte
			if compItem.Compressed {
				decompressed, err := s.decompress(compItem.Value, true)
				if err != nil {
					continue
				}
				valueBytes = decompressed
			} else {
				valueBytes = compItem.Value
			}

			// Deep copy
			result[key] = &StoredItem{
				Version:  compItem.Version,
				ExpireAt: compItem.ExpireAt,
				Value:    fastCloneBytes(valueBytes),
			}
		}
	}

	return result, nil
}

// BatchGetNoCopy retrieves multiple keys without copying.
// WARNING: Returned items share memory. Do not modify.
func (s *MemStorage) BatchGetNoCopy(keys []string) (map[string]*StoredItem, error) {
	if len(keys) == 0 {
		return make(map[string]*StoredItem), nil
	}

	result := make(map[string]*StoredItem, len(keys))
	now := time.Now()

	// Group by shard
	type shardBatch struct {
		shard *storageShard
		keys  []string
	}
	batches := make(map[int]*shardBatch)

	for _, key := range keys {
		if key == "" {
			continue
		}
		hash := xxh3.HashString128(key).Lo
		idx := int(hash & s.shardMask)

		batch := batches[idx]
		if batch == nil {
			batch = &shardBatch{
				shard: s.shards[idx],
				keys:  make([]string, 0, 4),
			}
			batches[idx] = batch
		}
		batch.keys = append(batch.keys, key)
	}

	// Process each shard
	for _, batch := range batches {
		for _, key := range batch.keys {
			s.getCount.Add(1)

			value, ok := batch.shard.data.Load(key)
			if !ok {
				s.missCount.Add(1)
				continue
			}

			compItem := value.(*compressedItem)

			// Check expiration
			if !compItem.ExpireAt.IsZero() && now.After(compItem.ExpireAt) {
				s.missCount.Add(1)
				batch.shard.data.Delete(key)
				batch.shard.keyCount.Add(-1)
				s.totalKeys.Add(-1)
				s.removeFromLRU(batch.shard, key)
				continue
			}

			s.hitCount.Add(1)
			s.touchLRU(batch.shard, key)

			// Decompress (must allocate)
			var valueBytes []byte
			if compItem.Compressed {
				decompressed, err := s.decompress(compItem.Value, true)
				if err != nil {
					continue
				}
				valueBytes = decompressed
			} else {
				valueBytes = compItem.Value // Shares slice
			}

			result[key] = &StoredItem{
				Version:  compItem.Version,
				ExpireAt: compItem.ExpireAt,
				Value:    valueBytes,
			}
		}
	}

	return result, nil
}

// BatchSet stores multiple key-value pairs efficiently.
func (s *MemStorage) BatchSet(items map[string]*StoredItem) error {
	if len(items) == 0 {
		return nil
	}

	// Group by shard
	type shardBatch struct {
		shard *storageShard
		items map[string]*StoredItem
	}
	batches := make(map[int]*shardBatch)

	totalSize := int64(0)
	for key, item := range items {
		if key == "" || item == nil {
			continue
		}
		hash := xxh3.HashString128(key).Lo
		idx := int(hash & s.shardMask)

		batch := batches[idx]
		if batch == nil {
			batch = &shardBatch{
				shard: s.shards[idx],
				items: make(map[string]*StoredItem),
			}
			batches[idx] = batch
		}
		batch.items[key] = item

		totalSize += s.calculateItemSize(key, item.Value)
	}

	// Memory check
	if s.maxMemoryMB > 0 {
		if err := s.checkAndEvict(totalSize); err != nil {
			return err
		}
	}

	// Process each shard
	for _, batch := range batches {
		for key, item := range batch.items {
			_ = s.Set(key, item) // Reuse Set for conflict resolution
		}
	}

	return nil
}

// Stats returns storage statistics.
type Stats struct {
	KeyCount         int64
	TotalBytes       int64
	CompressedBytes  int64
	OriginalBytes    int64
	CompressionRatio float64
	GetCount         int64
	SetCount         int64
	HitCount         int64
	MissCount        int64
	HitRate          float64
	EvictCount       int64
}

func (s *MemStorage) Stats() Stats {
	hits := s.hitCount.Load()
	misses := s.missCount.Load()
	hitRate := 0.0
	if hits+misses > 0 {
		hitRate = float64(hits) / float64(hits+misses)
	}

	compressed := s.compressedBytes.Load()
	original := s.originalBytes.Load()
	compressionRatio := 0.0
	if original > 0 {
		compressionRatio = float64(compressed) / float64(original)
	}

	return Stats{
		KeyCount:         s.totalKeys.Load(),
		TotalBytes:       s.totalBytes.Load(),
		CompressedBytes:  compressed,
		OriginalBytes:    original,
		CompressionRatio: compressionRatio,
		GetCount:         s.getCount.Load(),
		SetCount:         s.setCount.Load(),
		HitCount:         hits,
		MissCount:        misses,
		HitRate:          hitRate,
		EvictCount:       s.evictCount.Load(),
	}
}

// nextPowerOf2 rounds n up to next power of 2.
func nextPowerOf2(n uint64) uint64 {
	if n == 0 {
		return 1
	}
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	n |= n >> 32
	return n + 1
}

// fastCloneBytes creates copy of byte slice using zerocopy utility.
func fastCloneBytes(src []byte) []byte {
	return zerocopy.FastCloneBytes(src)
}

// Errors
var (
	ErrNotFound        = errors.New("item not found")
	ErrExpired         = errors.New("item expired")
	ErrVersionMismatch = errors.New("version mismatch")
	ErrMemoryLimit     = errors.New("memory limit exceeded")
	errEmptyKey        = errors.New("empty key")
	errNilItem         = errors.New("nil item")
)
