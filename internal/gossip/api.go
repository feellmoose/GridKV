package gossip

import (
	"context"
	"time"

	"github.com/feellmoose/gridkv/internal/storage"
)

// Simplified API methods for easier usage.
// These wrap the proto-based API with simpler byte-based operations.

// SetBytes stores a key-value pair with automatic versioning.
// This is a convenience method that automatically generates a version.
//
// Parameters:
//   - ctx: Context for timeout and cancellation
//   - key: The key to store
//   - value: The value bytes to store
//
// Returns:
//   - error: Any storage or replication error
func (gm *GossipManager) SetBytes(ctx context.Context, key string, value []byte) error {
	// Generate version from current time
	version := time.Now().UnixNano()

	item := &storage.StoredItem{
		Value:    value,
		ExpireAt: time.Time{}, // No expiration
		Version:  version,
	}

	return gm.Set(ctx, key, item)
}

// GetBytes retrieves a value by key, returning just the bytes.
// This is a convenience method that extracts just the value.
//
// Parameters:
//   - ctx: Context for timeout and cancellation
//   - key: The key to retrieve
//
// Returns:
//   - []byte: The value bytes
//   - error: Not found or any read error
func (gm *GossipManager) GetBytes(ctx context.Context, key string) ([]byte, error) {
	item, err := gm.Get(ctx, key)
	if err != nil {
		return nil, err
	}

	return item.Value, nil
}

// DeleteKey deletes a key with automatic versioning.
// This is a convenience method that automatically generates a version.
//
// Parameters:
//   - ctx: Context for timeout and cancellation
//   - key: The key to delete
//
// Returns:
//   - error: Any deletion error
func (gm *GossipManager) DeleteKey(ctx context.Context, key string) error {
	version := time.Now().UnixNano()
	return gm.Delete(ctx, key, version)
}
