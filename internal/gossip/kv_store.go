package gossip

import (
	"time"

	st "github.com/feellmoose/gridkv/internal/storage"
)

// This eliminates the adapter layer and provides direct access to optimized implementation
// NOTE: StoredItem is defined in proto, so we don't re-export it here

type (
	// Re-export storage configuration types
	StorageBackendType = st.StorageBackendType
	StorageOptions     = st.StorageOptions
	StorageStats       = st.StorageStats
)

// Re-export constants
const (
	Memory        = st.BackendMemory        // High-performance lock-free cache (~800K-1M+ ops/sec)
	MemorySharded = st.BackendMemorySharded // Ultra-high-performance sharded cache (~1M-1.5M+ ops/sec, 256 shards)
)

// Use storage.StoredItem directly (not proto StoredItem)
// The interface methods use st.StoredItem which is the storage version

// KVStore defines the unified interface for distributed key-value storage.
type KVStore interface {
	// Basic storage operations
	Set(key string, item *st.StoredItem) error
	Get(key string) (*st.StoredItem, error)
	Delete(key string, version int64) error
	Keys() []string
	Clear() error
	Close() error

	// Gossip synchronization methods (proto types)
	GetSyncBuffer() ([]*CacheSyncOperation, error)
	GetFullSyncSnapshot() ([]*FullStateItem, error)
	ApplyIncrementalSync(operations []*CacheSyncOperation) error
	ApplyFullSyncSnapshot(snapshot []*FullStateItem, snapshotTS time.Time) error

	Stats() st.StorageStats
}
