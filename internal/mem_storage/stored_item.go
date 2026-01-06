package mem_storage

import (
	"time"

	"github.com/feellmoose/gridkv/internal/utils/zerocopy"
)

// StoredItem represents a key-value entry with versioning and TTL.
// Layout:
// - Version (8 bytes): Hot path for conflict resolution
// - ExpireAt (16 bytes): Expiration check
// - Value (24 bytes slice header): Frequently accessed
// - Key (16 bytes string header): Accessed less frequently
//
// Memory layout: 64 bytes per item (cache line aligned)
type StoredItem struct {
	// Version is the HLC timestamp for conflict resolution.
	// Higher version wins in concurrent writes.
	Version int64

	// ExpireAt is the expiration timestamp (zero = no expiration).
	// Used for TTL management and tombstone cleanup.
	ExpireAt time.Time

	// Value is the stored data (immutable after creation).
	// Must be deep-copied when returned to callers.
	Value []byte

	// Key is the associated key (optional, for convenience).
	// May be empty if item is used without key context.
	Key string
}

// ResolveConflict resolves conflict between two items using last-write-wins.
// Returns true if 'other' should replace 'item'.
//
// Rules:
//   - Higher version wins
//   - If versions equal, prefer non-expired item
//   - If both expired/not-expired, keep existing (item)
func (item *StoredItem) ResolveConflict(other *StoredItem) bool {
	if other == nil {
		return false
	}

	// Version-based conflict resolution (last-write-wins)
	if other.Version > item.Version {
		return true
	}
	if other.Version < item.Version {
		return false
	}

	// Versions equal: prefer non-expired
	itemExpired := item.IsExpired()
	otherExpired := other.IsExpired()

	if otherExpired && !itemExpired {
		return false // Keep non-expired
	}
	if !otherExpired && itemExpired {
		return true // Prefer non-expired
	}

	// Both same expiration status: keep existing
	return false
}

// CompareVersion compares version with another item.
// Returns: -1 if item < other, 0 if equal, 1 if item > other
func (item *StoredItem) CompareVersion(other *StoredItem) int {
	if other == nil {
		return 1
	}

	if item.Version < other.Version {
		return -1
	}
	if item.Version > other.Version {
		return 1
	}
	return 0
}

// IsExpired checks if item has expired.
func (item *StoredItem) IsExpired() bool {
	if item.ExpireAt.IsZero() {
		return false
	}
	return time.Now().After(item.ExpireAt)
}

// IsTombstone checks if item is a tombstone (deleted marker).
// Tombstone has empty value but valid version.
func (item *StoredItem) IsTombstone() bool {
	return len(item.Value) == 0 && item.Version > 0
}

// Copy creates a shallow copy (shares Value slice).
// Use DeepCopy for independent copies.
func (item *StoredItem) Copy() *StoredItem {
	if item == nil {
		return nil
	}
	return &StoredItem{
		Version:  item.Version,
		ExpireAt: item.ExpireAt,
		Value:    item.Value, // Shallow: shares slice
		Key:      item.Key,
	}
}

// DeepCopy creates a full independent copy.
// Value slice is copied, safe for concurrent modification.
func (item *StoredItem) DeepCopy() *StoredItem {
	if item == nil {
		return nil
	}

	copied := &StoredItem{
		Version:  item.Version,
		ExpireAt: item.ExpireAt,
		Key:      item.Key,
	}

	if item.Value != nil {
		// Use zero-copy FastCloneBytes for better performance
		copied.Value = zerocopy.FastCloneBytes(item.Value)
	}

	return copied
}

// Clone is an alias for DeepCopy for convenience.
func (item *StoredItem) Clone() *StoredItem {
	return item.DeepCopy()
}

// Reset clears all fields for object pool reuse.
func (item *StoredItem) Reset() {
	if item == nil {
		return
	}
	item.Version = 0
	item.ExpireAt = time.Time{}
	item.Value = nil
	item.Key = ""
}

// WithVersion returns a copy with updated version.
func (item *StoredItem) WithVersion(version int64) *StoredItem {
	copy := item.DeepCopy()
	copy.Version = version
	return copy
}

// WithTTL returns a copy with TTL set.
// ttl <= 0 means no expiration.
func (item *StoredItem) WithTTL(ttl time.Duration) *StoredItem {
	copy := item.DeepCopy()
	if ttl > 0 {
		copy.ExpireAt = time.Now().Add(ttl)
	} else {
		copy.ExpireAt = time.Time{}
	}
	return copy
}

// Size returns approximate memory size in bytes.
// Includes struct overhead and value length.
func (item *StoredItem) Size() int64 {
	if item == nil {
		return 0
	}
	// Struct: 64 bytes (aligned)
	// Value slice: 24 (header) + len(Value)
	// Key string: 16 (header) + len(Key)
	return 64 + int64(24+len(item.Value)) + int64(16+len(item.Key))
}

// IsValid checks if item is valid (has value or is tombstone).
func (item *StoredItem) IsValid() bool {
	return item != nil && item.Version > 0
}
