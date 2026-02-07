package mem_storage

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/feellmoose/gridkv/internal/utils/compress"
	"github.com/feellmoose/gridkv/internal/utils/lifecycle"
	"github.com/feellmoose/gridkv/internal/utils/zerocopy"
	"github.com/zeebo/xxh3"
)

// MemStorage provides distributed-ready in-memory storage.
type MemStorage struct {
	shards      []*storageShard
	shardCount  int
	shardMask   uint64
	maxMemoryMB int64

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
	evictBatchSize int           // Number of keys to evict per batch
	asyncEviction  bool          // Enable async background eviction

	// Lifecycle
	cleanerStop    chan struct{}
	cleanerRunning atomic.Bool
	evictStop      chan struct{}
	evictRunning   atomic.Bool
}

// Config configures MemStorage.
type Config struct {
	// MaxMemoryMB limits total memory.
	// Zero or negative values are treated as "use conservative default" for SDK safety.
	MaxMemoryMB int64

	// ShardCount is number of shards (0 = auto: 2-4x CPU cores).
	ShardCount int

	CompressionEnabled bool

	// CompressionThreshold is min size to compress (bytes, default: 64).
	CompressionThreshold int

	// EvictThreshold is memory percentage to start eviction (default: 90).
	EvictThreshold int

	// EvictTarget is target memory percentage after eviction (default: 80).
	EvictTarget int

	// EvictBatchSize is number of keys to evict per batch (default: 10).
	EvictBatchSize int

	// AsyncEviction enables background async eviction (default: true).
	AsyncEviction bool
}

// storageShard represents a single storage shard.
type storageShard struct {
	// Data storage (lock-free sync.Map)
	data sync.Map // map[string]*compressedItem

	// Per-shard LRU (locked)
	lru   *shardLRU
	lruMu sync.Mutex

	// Per-shard sync buffer (protected by mutex)
	syncBuffer   []atomicSyncOp
	syncHead     atomic.Uint64
	syncTail     atomic.Uint64
	syncCapacity uint64
	syncMask     uint64
	syncMu       sync.Mutex

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
// Uses LZ4 compression for optimal balance between speed and compression ratio.
// Includes a conservative in-memory size limit suitable for long-running SDK usage.
func DefaultConfig() Config {
	return Config{
		MaxMemoryMB:          512, // soft limit per process; eviction keeps heap bounded for SDK usage
		ShardCount:           0,   // auto
		CompressionEnabled:   true,
		CompressionThreshold: 64,
		EvictThreshold:       85,
		EvictTarget:          70,
		EvictBatchSize:       10, // Default batch size
		AsyncEviction:        true,
	}
}

// New creates a new MemStorage instance.
func New(config Config) (*MemStorage, error) {
	shardCount := config.ShardCount
	if shardCount == 0 {
		// Auto: 8x CPU cores for massive concurrency (10000+ workers)
		cpuCount := runtime.GOMAXPROCS(0)
		shardCount = cpuCount * 8
		if shardCount < 256 {
			shardCount = 256
		}
		if shardCount > 4096 {
			shardCount = 4096 // Cap to prevent excessive overhead
		}
	}

	shardCount = int(nextPowerOf2(uint64(shardCount)))

	compressionThresh := config.CompressionThreshold
	if compressionThresh == 0 {
		compressionThresh = 64
	}

	evictThreshold := int64(config.EvictThreshold)
	if evictThreshold == 0 {
		// Start eviction a bit earlier to avoid hitting the hard limit
		evictThreshold = 85
	}

	evictTarget := int64(config.EvictTarget)
	if evictTarget == 0 {
		// Evict down to a lower watermark to give GC headroom
		evictTarget = 70
	}

	evictBatchSize := config.EvictBatchSize
	if evictBatchSize == 0 {
		evictBatchSize = 10 // Default batch size
	}
	if evictBatchSize < 1 {
		evictBatchSize = 1
	}
	if evictBatchSize > 100 {
		evictBatchSize = 100 // Cap to prevent excessive lock hold time
	}

	asyncEviction := config.AsyncEviction
	if !config.AsyncEviction {
		asyncEviction = true // Default enabled
	}

	maxMemoryMB := config.MaxMemoryMB
	if maxMemoryMB <= 0 {
		// Enforce a soft upper bound even when caller does not provide one.
		// This prevents unbounded growth in long-running SDK scenarios.
		maxMemoryMB = 512
	}

	s := &MemStorage{
		shards:             make([]*storageShard, shardCount),
		shardCount:         shardCount,
		shardMask:          uint64(shardCount - 1),
		maxMemoryMB:        maxMemoryMB,
		compressionEnabled: config.CompressionEnabled,
		compressionThresh:  compressionThresh,
		evictThreshold:     evictThreshold,
		evictTarget:        evictTarget,
		evictBatchSize:     evictBatchSize,
		asyncEviction:      asyncEviction,
		cleanerStop:        make(chan struct{}),
		evictStop:          make(chan struct{}),
	}

	// Initialize shards
	syncCapacity := nextPowerOf2(8192) // Massive capacity for 10000+ concurrent operations
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

	// Start async eviction if enabled
	if s.asyncEviction && s.maxMemoryMB > 0 {
		s.startAsyncEviction()
	}

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
		return ErrEmptyKey
	}
	if item == nil {
		return errNilItem
	}

	shard := s.getShard(key)

	compItem, itemSize, err := s.prepareCompressedItem(key, item)
	if err != nil {
		return err
	}
	if s.maxMemoryMB > 0 {
		if err := s.checkAndEvict(itemSize); err != nil {
			return err
		}
	}

	// Atomic conflict resolution with retry loop
	return s.storeItemWithConflictResolution(shard, key, compItem, itemSize, item)
}

// prepareCompressedItem handles compression and size calculation for an item
func (s *MemStorage) prepareCompressedItem(key string, item *StoredItem) (*compressedItem, int64, error) {
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

	compItem := &compressedItem{
		Version:    item.Version,
		ExpireAt:   item.ExpireAt,
		Value:      compressedValue,
		Compressed: isCompressed,
		OrigSize:   origSize,
	}

	return compItem, itemSize, nil
}

// storeItemWithConflictResolution handles the atomic storage with conflict resolution
func (s *MemStorage) storeItemWithConflictResolution(shard *storageShard, key string, compItem *compressedItem, itemSize int64, originalItem *StoredItem) error {
	// Atomic conflict resolution with retry loop for CAS
	for {
		existing, loaded := shard.data.Load(key)
		if !loaded {
			// New key: try to store
			if err := s.tryStoreNewKey(shard, key, compItem, itemSize, originalItem); err == nil {
				return nil
			}
			// Race condition: key was inserted by another goroutine, retry
			continue
		}

		// Key exists: resolve conflict
		if err := s.tryUpdateExistingKey(shard, key, existing, compItem, itemSize, originalItem); err == nil {
			return nil
		}
		// Update failed, retry
	}
}

// tryStoreNewKey attempts to store a new key
func (s *MemStorage) tryStoreNewKey(shard *storageShard, key string, compItem *compressedItem, itemSize int64, originalItem *StoredItem) error {
	if _, exists := shard.data.LoadOrStore(key, compItem); !exists {
		// Successfully stored new key
		s.updateStatsForNewKey(shard, compItem, itemSize)
		s.touchLRU(shard, key)
		s.addToSyncBuffer(shard, key, OpSet, originalItem)
		return nil
	}
	return errRetry // Signal that storage failed due to race condition
}

// updateStatsForNewKey updates all statistics for a newly stored key
func (s *MemStorage) updateStatsForNewKey(shard *storageShard, compItem *compressedItem, itemSize int64) {
	shard.keyCount.Add(1)
	shard.byteCount.Add(itemSize)
	shard.compressedSize.Add(int64(len(compItem.Value)))
	shard.originalSize.Add(int64(compItem.OrigSize))
	s.totalKeys.Add(1)
	s.totalBytes.Add(itemSize)
	s.compressedBytes.Add(int64(len(compItem.Value)))
	s.originalBytes.Add(int64(compItem.OrigSize))
	s.setCount.Add(1)
}

// tryUpdateExistingKey attempts to update an existing key with conflict resolution
func (s *MemStorage) tryUpdateExistingKey(shard *storageShard, key string, existing interface{}, compItem *compressedItem, itemSize int64, originalItem *StoredItem) error {
	existingItem := existing.(*compressedItem)

	existingVersion := existingItem.Version
	if compItem.Version <= existingVersion {
		s.setCount.Add(1)
		return nil
	}

	// Verify existing item hasn't changed (atomic snapshot)
	current, currentLoaded := shard.data.Load(key)
	if !currentLoaded {
		// Item was deleted, retry from beginning
		return errRetry
	}
	currentItem := current.(*compressedItem)
	if currentItem.Version != existingVersion {
		// Item changed, retry
		return errRetry
	}

	// New version wins: atomic replace using CAS
	if shard.data.CompareAndSwap(key, existing, compItem) {
		s.updateStatsForExistingKey(shard, existingItem, compItem, itemSize)
		s.touchLRU(shard, key)
		s.addToSyncBuffer(shard, key, OpSet, originalItem)
		return nil
	}

	// CAS failed, retry
	return errRetry
}

// updateStatsForExistingKey updates statistics when replacing an existing key
func (s *MemStorage) updateStatsForExistingKey(shard *storageShard, oldItem, newItem *compressedItem, newItemSize int64) {
	oldSize := s.calculateItemSize("", oldItem.Value) // Key not needed for size calc
	shard.byteCount.Add(newItemSize - oldSize)
	shard.compressedSize.Add(int64(len(newItem.Value) - len(oldItem.Value)))
	shard.originalSize.Add(int64(newItem.OrigSize - oldItem.OrigSize))
	s.totalBytes.Add(newItemSize - oldSize)
	s.compressedBytes.Add(int64(len(newItem.Value) - len(oldItem.Value)))
	s.originalBytes.Add(int64(newItem.OrigSize - oldItem.OrigSize))
	s.setCount.Add(1)
}

// Get retrieves key with automatic decompression.
// Returns deep copy, safe for modification.
func (s *MemStorage) Get(key string) (*StoredItem, error) {
	if key == "" {
		return nil, ErrEmptyKey
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

	var valueBytes []byte
	if compItem.Compressed {
		var err error
		valueBytes, err = s.decompressValue(compItem)
		if err != nil {
			return nil, err
		}
	} else {
		valueBytes = fastCloneBytes(compItem.Value)
	}
	result := &StoredItem{
		Version:  compItem.Version,
		ExpireAt: compItem.ExpireAt,
		Value:    valueBytes,
	}

	return result, nil
}

func (s *MemStorage) Delete(key string, version int64) error {
	if key == "" {
		return ErrEmptyKey
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

func (s *MemStorage) compress(data []byte) ([]byte, bool) {
	return compress.Compress(data, s.compressionThresh)
}

func (s *MemStorage) decompressValue(compItem *compressedItem) ([]byte, error) {
	if !compItem.Compressed {
		return nil, nil
	}
	est := compItem.OrigSize
	if est <= 0 {
		est = len(compItem.Value) * 2
	}
	if est < len(compItem.Value)*2 {
		est = len(compItem.Value) * 2
	}
	out, _, err := compress.DecompressTo(compItem.Value, nil, est)
	if err != nil {
		return nil, fmt.Errorf("lz4 decompress failed: %w", err)
	}
	return out, nil
}

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
// With async eviction enabled, this only does a quick check and triggers async eviction.
func (s *MemStorage) checkAndEvict(itemSize int64) error {
	maxBytes := s.maxMemoryMB * 1024 * 1024
	current := s.totalBytes.Load()
	threshold := maxBytes * s.evictThreshold / 100

	if current+itemSize <= threshold {
		return nil
	}

	// The background goroutine will handle the rest
	if s.asyncEviction {
		// Quick synchronous eviction: only if we're way over limit
		if current+itemSize > maxBytes {
			// Over hard limit, must evict synchronously
			evicted := s.evictBatchLRU(5) // Quick batch
			if evicted == 0 {
				return ErrMemoryLimit
			}
		}
		// Otherwise, let async eviction handle it
		return nil
	}

	// Synchronous eviction (original behavior)
	target := maxBytes * s.evictTarget / 100
	evicted := 0
	maxEvictions := 200
	for current+itemSize > target && evicted < maxEvictions {
		batchEvicted := s.evictBatchLRU(s.evictBatchSize)
		if batchEvicted == 0 {
			break
		}
		evicted += batchEvicted
		current = s.totalBytes.Load()
	}

	// Check if still over limit
	if current+itemSize > maxBytes {
		return ErrMemoryLimit
	}

	return nil
}

// evictBatchLRU evicts multiple items from current shard in batch.
func (s *MemStorage) evictBatchLRU(batchSize int) int {
	if batchSize <= 0 {
		batchSize = 1
	}
	if batchSize > 100 {
		batchSize = 100
	}

	idx := s.evictShardIdx.Add(1) & s.shardMask
	shard := s.shards[idx]

	shard.lruMu.Lock()
	defer shard.lruMu.Unlock()

	evicted := 0
	for evicted < batchSize && shard.lru.list.Len() > 0 {
		oldest := shard.lru.list.Back()
		if oldest == nil {
			break
		}
		key := oldest.Value.(string)
		shard.lru.list.Remove(oldest)
		delete(shard.lru.keyMap, key)

		value, loaded := shard.data.LoadAndDelete(key)
		if !loaded {
			continue
		}

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
		evicted++
	}

	return evicted
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
// Designed to reduce memory allocations for sync operations.
func (s *MemStorage) addToSyncBuffer(shard *storageShard, key string, opType OpType, item *StoredItem) {
	// Create minimal sync item to reduce memory usage
	var syncItem *StoredItem
	if item != nil {
		// Shallow copy essential fields only - don't copy value for sync
		syncItem = &StoredItem{
			Version:  item.Version,
			ExpireAt: item.ExpireAt,
			Key:      key, // Use provided key instead of item.Key
			Value:    nil, // Don't copy value - not needed for sync
		}
	}

	op := &SyncOperation{
		Key:    key,
		OpType: opType,
		Item:   syncItem,
	}

	shard.syncMu.Lock()
	defer shard.syncMu.Unlock()

	head := shard.syncHead.Load()
	tail := shard.syncTail.Load()

	// Check if buffer is full
	if head-tail >= shard.syncCapacity {
		// Buffer is full, advance tail to make room (drop oldest operations)
		newTail := head - shard.syncCapacity + 1
		// Clear dropped operations to help GC
		for i := tail; i < newTail; i++ {
			shard.syncBuffer[i&shard.syncMask].op = nil
		}
		shard.syncTail.Store(newTail)
	}

	// Add operation to buffer
	shard.syncBuffer[head&shard.syncMask].op = op
	shard.syncHead.Store(head + 1)
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
	totalExpired := 0

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
				totalExpired++
			}
		}
	}

	// Periodic GC hint for long-running processes when many items expired
	if totalExpired > 1000 {
		runtime.GC()
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
		estimatedCap = 100
	}
	ops := make([]*SyncOperation, 0, estimatedCap)

	for _, shard := range s.shards {
		shard.syncMu.Lock()

		head := shard.syncHead.Load()
		tail := shard.syncTail.Load()

		if head == tail {
			shard.syncMu.Unlock()
			continue
		}

		// Collect all pending operations
		for i := tail; i < head; i++ {
			if op := shard.syncBuffer[i&shard.syncMask].op; op != nil {
				if op.Item != nil && op.Item.Value == nil && op.OpType == OpSet {
					if stored, err := s.GetNoCopy(op.Key); err == nil && stored != nil && len(stored.Value) > 0 {
						op.Item.Value = make([]byte, len(stored.Value))
						copy(op.Item.Value, stored.Value)
					}
				}
				ops = append(ops, op)
				// Clear the buffer slot
				shard.syncBuffer[i&shard.syncMask].op = nil
			}
		}

		// Advance tail to indicate all operations consumed
		shard.syncTail.Store(head)
		shard.syncMu.Unlock()
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

// startAsyncEviction starts background goroutine for async eviction.
func (s *MemStorage) startAsyncEviction() {
	if !s.evictRunning.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer s.evictRunning.Store(false)

		ticker := time.NewTicker(100 * time.Millisecond) // Check every 100ms
		defer ticker.Stop()

		for {
			select {
			case <-s.evictStop:
				return
			case <-ticker.C:
				s.runAsyncEviction()
			}
		}
	}()
}

func (s *MemStorage) runAsyncEviction() {
	if s.maxMemoryMB <= 0 {
		return
	}

	maxBytes := s.maxMemoryMB * 1024 * 1024
	current := s.totalBytes.Load()
	threshold := maxBytes * s.evictThreshold / 100

	if current <= threshold {
		return // Below threshold, no eviction needed
	}

	// Evict until below target
	target := maxBytes * s.evictTarget / 100
	evicted := 0
	maxEvictions := s.evictBatchSize * 10 // Evict more aggressively in background

	for current > target && evicted < maxEvictions {
		batchEvicted := s.evictBatchLRU(s.evictBatchSize)
		if batchEvicted == 0 {
			break
		}
		evicted += batchEvicted
		current = s.totalBytes.Load()
	}
}

// closeInternal releases resources (internal implementation).
func (s *MemStorage) closeInternal() error {
	// Stop async eviction
	if s.evictRunning.Swap(false) {
		select {
		case s.evictStop <- struct{}{}:
		default:
		}
		close(s.evictStop)
		time.Sleep(50 * time.Millisecond)
	}

	// Stop cleaner
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

// lifecycle.Component implementation
func (s *MemStorage) Name() string {
	return "storage"
}

func (s *MemStorage) Start(ctx context.Context) error {
	// Storage starts cleaner in New(), no additional start needed
	return nil
}

func (s *MemStorage) Close(ctx context.Context) error {
	return s.closeInternal()
}

// Ensure MemStorage implements lifecycle.Component
var _ lifecycle.Component = (*MemStorage)(nil)

// GetNoCopy retrieves key without copying value.
func (s *MemStorage) GetNoCopy(key string) (*StoredItem, error) {
	if key == "" {
		return nil, ErrEmptyKey
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

	var valueBytes []byte
	if compItem.Compressed {
		var err error
		valueBytes, err = s.decompressValue(compItem)
		if err != nil {
			return nil, err
		}
	} else {
		valueBytes = compItem.Value
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

			var valueBytes []byte
			if compItem.Compressed {
				valueBytes, _ = s.decompressValue(compItem)
				if valueBytes == nil {
					continue
				}
			} else {
				valueBytes = fastCloneBytes(compItem.Value)
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

// BatchGetNoCopy retrieves multiple keys without copying.
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

			var valueBytes []byte
			if compItem.Compressed {
				valueBytes, _ = s.decompressValue(compItem)
				if valueBytes == nil {
					continue
				}
			} else {
				valueBytes = compItem.Value
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

func fastCloneBytes(src []byte) []byte {
	return zerocopy.FastCloneBytes(src)
}

// Errors
var (
	ErrNotFound        = errors.New("item not found")
	ErrExpired         = errors.New("item expired")
	ErrVersionMismatch = errors.New("version mismatch")
	ErrMemoryLimit     = errors.New("memory limit exceeded")
	ErrEmptyKey        = errors.New("empty key")
	errNilItem         = errors.New("nil item")
	errRetry           = errors.New("retry operation")
)
