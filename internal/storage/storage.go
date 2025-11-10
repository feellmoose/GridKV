// Package storage provides pluggable storage backends for GridKV.
//
// This package defines the Storage interface and implements two in-memory backends:
//   - Memory: Simple in-memory map (development/testing)
//   - MemorySharded: Sharded in-memory for high concurrency (production, recommended)
//
// All storage backends must implement the Storage interface and provide:
//   - Thread-safe operations (safe for concurrent access)
//   - Deep copy semantics (returned data is safe to modify by caller)
//   - Atomic operations where possible
//   - Efficient concurrent access (minimal lock contention)
//
// Performance targets:
//   - Get: < 1µs (memory), < 100µs (persistent)
//   - Set: < 2µs (memory), < 200µs (persistent)
//   - Delete: < 2µs (memory), < 200µs (persistent)
//
// Backend selection guide:
//   - Memory: Simple, low throughput (600-700K ops/s), development/testing
//   - MemorySharded: High throughput (1-2M+ ops/s), recommended for production
package storage

import (
	"time"
)

// StoredItem represents a single key-value pair with metadata.
//
// This is the fundamental storage unit in GridKV, containing:
//   - Value: The actual data bytes
//   - Version: Hybrid Logical Clock timestamp for conflict resolution
//   - ExpireAt: TTL expiration time (zero value = no expiration)
//
// Memory layout: ~40 bytes overhead + len(Value)
//
// Thread-safety: StoredItem itself is not thread-safe; the Storage
// implementation is responsible for synchronization.
type StoredItem struct {
	// ExpireAt is the expiration timestamp for TTL and tombstones.
	// Zero value (time.Time{}) means no expiration.
	// Used for: TTL expiration check and tombstone garbage collection.
	ExpireAt time.Time

	// Version is the Hybrid Logical Clock timestamp.
	// Used for: Conflict resolution in quorum reads/writes.
	// Higher version wins in case of conflicts.
	Version int64

	// Value is the actual data stored.
	// Must be treated as immutable after storage (deep copy on return).
	Value []byte
}

// Storage is the interface that all storage backends must implement.
//
// Provides:
//   - Basic KV operations (Set, Get, Delete)
//   - TTL and versioning support
//   - Gossip synchronization (incremental and full sync)
//   - Monitoring and stats
//
// Thread-safety: All methods must be safe for concurrent access.
// Implementations must handle synchronization internally.
//
// Implementations:
//   - MemoryStorage: Simple sync.Map (600-700K ops/s)
//   - MemoryShardedStorage: Sharded maps for high concurrency (1-2M+ ops/s, recommended)
//
// Example:
//
//	// Create storage
//	store, err := NewStorage(&StorageOptions{
//	    Backend: BackendMemorySharded,
//	    ShardCount: 32,
//	})
//
//	// Use storage
//	item := &StoredItem{Value: []byte("data"), Version: 123}
//	store.Set("key", item)
//	retrieved, _ := store.Get("key")
type Storage interface {
	// Set stores or updates a key-value pair.
	//
	// Parameters:
	//   - key: The key to store (non-empty string)
	//   - item: The item to store (must not be nil)
	//     item.Value will be deep-copied by most implementations.
	//     item.Version should be from HLC for proper ordering.
	//
	// Returns:
	//   - error: nil on success, error on failure
	//
	// Thread-safety: Safe for concurrent calls.
	Set(key string, item *StoredItem) error

	// Get retrieves a value by key.
	//
	// Parameters:
	//   - key: The key to retrieve
	//
	// Returns:
	//   - *StoredItem: The stored item (deep copy, safe to modify)
	//   - error: ErrItemNotFound if key doesn't exist, other errors on failure
	//
	// The returned item is a deep copy and safe for caller to modify.
	//
	// Thread-safety: Safe for concurrent calls.
	Get(key string) (*StoredItem, error)

	// Delete removes a key-value pair with optimistic locking.
	//
	// Parameters:
	//   - key: The key to delete
	//   - version: Expected version for optimistic locking
	//     Delete succeeds only if current version matches or is older.
	//
	// Returns:
	//   - error: ErrItemNotFound if key doesn't exist (not an error condition)
	//
	// Thread-safety: Safe for concurrent calls.
	Delete(key string, version int64) error

	// Keys returns all keys currently in storage.
	//
	// Returns:
	//   - []string: List of all keys (unordered, snapshot at call time)
	//
	// Warning: Expensive operation for large datasets (O(n) time and memory).
	// Use for debugging or monitoring only, not in hot path.
	//
	// Thread-safety: Safe for concurrent calls.
	Keys() []string

	// Clear removes all entries from storage.
	//
	// Warning: Irreversible operation. Use with caution.
	//
	// Thread-safety: Safe for concurrent calls.
	Clear() error

	// Close releases all resources and shuts down the storage backend.
	//
	// After Close, all subsequent operations will fail.
	// Implementations should flush any buffered data before returning.
	//
	// Thread-safety: Safe for concurrent calls (idempotent).
	Close() error

	// GetSyncBuffer returns recent changes for incremental synchronization.
	//
	// Returns:
	//   - []*CacheSyncOperation: List of recent SET/DELETE operations
	//   - error: nil on success
	//
	// Used by Gossip protocol for efficient delta synchronization.
	GetSyncBuffer() ([]*CacheSyncOperation, error)

	// GetFullSyncSnapshot returns complete storage state.
	//
	// Returns:
	//   - []*FullStateItem: All key-value pairs in storage
	//   - error: nil on success
	//
	// Used for initial sync or when incremental sync is not feasible.
	// Warning: Expensive for large datasets.
	GetFullSyncSnapshot() ([]*FullStateItem, error)

	// ApplyIncrementalSync applies incremental changes from another node.
	//
	// Parameters:
	//   - operations: List of SET/DELETE operations to apply
	//
	// Returns:
	//   - error: nil on success
	//
	// Implements last-write-wins using Version (HLC timestamp).
	ApplyIncrementalSync(operations []*CacheSyncOperation) error

	// ApplyFullSyncSnapshot applies a complete snapshot from another node.
	//
	// Parameters:
	//   - snapshot: Complete key-value state
	//   - snapshotTS: Timestamp of the snapshot
	//
	// Returns:
	//   - error: nil on success
	//
	// Merges snapshot with local state using version comparison.
	ApplyFullSyncSnapshot(snapshot []*FullStateItem, snapshotTS time.Time) error

	// Stats returns current storage statistics for monitoring.
	//
	// Returns:
	//   - StorageStats: Current stats (key count, size, etc.)
	Stats() StorageStats
}

// HighPerformanceStorage is an optional interface for storage backends that support
// high-performance operations like zero-copy reads and batch operations.
//
// Backends implementing this interface can provide significant performance improvements:
//   - GetNoCopy: 40-50% faster than Get() for read-only scenarios
//   - BatchGet: 2-3× faster than individual Gets for bulk reads
//   - BatchSet: 2× faster than individual Sets for bulk writes
//
// Usage: Check if your storage backend implements this interface using type assertion.
type HighPerformanceStorage interface {
	Storage

	// GetNoCopy retrieves a value without copying it.
	// ⚠️ WARNING: The returned *StoredItem shares memory with the storage.
	// Callers MUST NOT modify item.Value. Use this only for read-only scenarios.
	// Performance: ~40-50% faster than Get() for avoiding value copy overhead.
	GetNoCopy(key string) (*StoredItem, error)

	// BatchGet retrieves multiple keys efficiently in a single operation.
	// Returns a map of found keys to their values. Missing keys are not included.
	// Performance: ~2-3× faster than individual Get calls for 10+ keys.
	BatchGet(keys []string) (map[string]*StoredItem, error)

	// BatchGetNoCopy retrieves multiple keys without copying values.
	// ⚠️ WARNING: Returned items share memory with storage. Do not modify.
	// Performance: ~3-4× faster than individual Get calls for 10+ keys.
	BatchGetNoCopy(keys []string) (map[string]*StoredItem, error)

	// BatchSet stores multiple key-value pairs efficiently in a single operation.
	// Performance: ~2× faster than individual Set calls for 10+ keys.
	BatchSet(items map[string]*StoredItem) error
}

// CacheSyncOperation represents a single change for incremental synchronization.
//
// Used by the Gossip protocol to efficiently sync recent changes between nodes
// without transferring the entire dataset.
//
// Fields:
//   - Key: The affected key
//   - Version: HLC timestamp of the operation
//   - Type: Operation type ("SET" or "DELETE")
//   - Data: The stored item (nil for DELETE operations)
type CacheSyncOperation struct {
	Key     string
	Version int64
	Type    string      // "SET" or "DELETE"
	Data    *StoredItem // only present for SET operations
}

// FullStateItem represents a complete key-value entry for full synchronization.
//
// Used when a node joins the cluster or falls too far behind for incremental sync.
//
// Fields:
//   - Key: The key
//   - Version: HLC timestamp
//   - Item: The complete stored item
type FullStateItem struct {
	Key     string
	Version int64
	Item    *StoredItem
}

// StorageBackendType identifies the storage backend implementation.
//
// Available backends:
//   - BackendMemory: Simple, low concurrency (< 1M ops/s)
//   - BackendMemorySharded: High concurrency (1-2M+ ops/s, recommended)
type StorageBackendType string

const (
	// BackendMemory is a simple in-memory backend using sync.Map.
	// Performance: 600K-700K ops/s
	// Use case: Development, testing, low-traffic scenarios
	BackendMemory StorageBackendType = "Memory"

	// BackendMemorySharded is a high-performance sharded in-memory backend.
	// Performance: 1M-1.5M+ ops/s with 256 shards
	// Use case: Production, high-concurrency scenarios
	// Recommended shard count: 2-4x number of CPU cores
	BackendMemorySharded StorageBackendType = "MemorySharded"
)

// StorageOptions configures the storage backend.
//
// Example:
//
//	// High-performance production config
//	opts := &StorageOptions{
//	    Backend: BackendMemorySharded,
//	    ShardCount: 64,  // 64 shards for high concurrency
//	}
//
//	// Simple development config
//	opts := &StorageOptions{
//	    Backend: BackendMemory,
//	}
type StorageOptions struct {
	// Backend selects the storage implementation
	Backend StorageBackendType

	// MaxMemoryMB limits memory usage for in-memory backends (MB).
	// Default: 1024MB
	// Set to 0 for no limit (not recommended in production).
	MaxMemoryMB int64

	// ShardCount sets the number of shards for MemorySharded backend.
	// Higher values reduce lock contention but increase memory overhead.
	// Default: 256 (auto-calculated based on CPU count)
	// Recommended: 2-4x number of CPU cores
	// Range: 1-1024
	ShardCount int
}

// StorageStats provides runtime statistics for monitoring and diagnostics.
//
// All fields are snapshots at the time of the Stats() call and may be
// slightly outdated in high-concurrency scenarios.
type StorageStats struct {
	// KeyCount is the total number of keys in storage
	KeyCount int64

	// SyncBufferLen is the number of pending sync operations
	// High values may indicate sync lag
	SyncBufferLen int

	// CacheHitRate is the cache hit ratio (0.0 to 1.0)
	// Only applicable for backends with caching layers
	CacheHitRate float64

	// DBSize is the approximate storage size in bytes
	// May be estimated for some backends
	DBSize int64
}

// AtomicSyncOp wraps CacheSyncOperation for atomic storage in lock-free ring buffers.
//
// This is an internal type used by storage backends to implement
// thread-safe sync operation buffering without locks.
//
// Exported for use by storage backend implementations.
type AtomicSyncOp struct {
	Op *CacheSyncOperation
}

// NextPowerOf2 rounds n up to the next power of 2.
//
// Used for sizing ring buffers and hash tables to enable fast modulo
// operations using bitwise AND instead of expensive division.
//
// Parameters:
//   - n: Input value (uint64)
//
// Returns:
//   - uint64: Smallest power of 2 >= n
//
// Algorithm:
//   - Bit manipulation to set all bits right of the highest set bit
//   - Add 1 to get next power of 2
//   - Time: O(1) - constant time with 6 operations
//
// Example:
//
//	NextPowerOf2(0)   // 1
//	NextPowerOf2(1)   // 1
//	NextPowerOf2(7)   // 8
//	NextPowerOf2(8)   // 8
//	NextPowerOf2(100) // 128
//
// Use case: ring_buffer_size = NextPowerOf2(desired_size)
// Then: index = hash & (ring_buffer_size - 1)  // Fast modulo
//
// Exported for use by storage backend implementations.
func NextPowerOf2(n uint64) uint64 {
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
