package gossip

import (
	"time"

	st "github.com/feellmoose/gridkv/internal/storage"
)

type (
	StorageBackendType = st.StorageBackendType
	StorageOptions     = st.StorageOptions
	StorageStats       = st.StorageStats
)

const (
	Memory        = st.BackendMemory
	MemorySharded = st.BackendMemorySharded
)

type KVStore interface {
	Set(key string, item *st.StoredItem) error
	Get(key string) (*st.StoredItem, error)
	Delete(key string, version int64) error
	Keys() []string
	Clear() error
	Close() error
	GetSyncBuffer() ([]*CacheSyncOperation, error)
	GetFullSyncSnapshot() ([]*FullStateItem, error)
	ApplyIncrementalSync(operations []*CacheSyncOperation) error
	ApplyFullSyncSnapshot(snapshot []*FullStateItem, snapshotTS time.Time) error
	Stats() st.StorageStats
}

type StorageBridge struct {
	store st.Storage
}

func NewStorageBridge(store st.Storage) *StorageBridge {
	return &StorageBridge{store: store}
}

//go:inline
func (b *StorageBridge) Set(key string, item *st.StoredItem) error {
	return b.store.Set(key, item)
}

//go:inline
func (b *StorageBridge) Get(key string) (*st.StoredItem, error) {
	// Use GetNoCopy for better performance when available
	if highPerfStore, ok := b.store.(interface {
		GetNoCopy(key string) (*st.StoredItem, error)
	}); ok {
		// Use GetNoCopy for zero-copy read, then copy
		item, err := highPerfStore.GetNoCopy(key)
		if err != nil {
			return nil, err
		}
		// Copy (caller may modify)
		return copyStorageItem(item), nil
	}
	// Fallback to regular Get
	return b.store.Get(key)
}

//go:inline
func (b *StorageBridge) Delete(key string, version int64) error {
	return b.store.Delete(key, version)
}

//go:inline
func (b *StorageBridge) Keys() []string {
	return b.store.Keys()
}

//go:inline
func (b *StorageBridge) Clear() error {
	return b.store.Clear()
}

//go:inline
func (b *StorageBridge) Close() error {
	return b.store.Close()
}

func (b *StorageBridge) GetSyncBuffer() ([]*CacheSyncOperation, error) {
	ops, err := b.store.GetSyncBuffer()
	if err != nil || len(ops) == 0 {
		return nil, err
	}

	protoOps := make([]*CacheSyncOperation, len(ops))
	for i, op := range ops {
		protoOps[i] = storageSyncOpToProto(op)
	}
	return protoOps, nil
}

func (b *StorageBridge) GetFullSyncSnapshot() ([]*FullStateItem, error) {
	items, err := b.store.GetFullSyncSnapshot()
	if err != nil || len(items) == 0 {
		return nil, err
	}

	protoItems := make([]*FullStateItem, len(items))
	for i, item := range items {
		protoItems[i] = storageFullItemToProto(item)
	}
	return protoItems, nil
}

func (b *StorageBridge) ApplyIncrementalSync(operations []*CacheSyncOperation) error {
	if len(operations) == 0 {
		return nil
	}

	// Group operations by type for batch processing
	setItems := make(map[string]*st.StoredItem, len(operations))
	deleteKeys := make([]struct {
		key     string
		version int64
	}, 0, len(operations))

	// Pre-process operations: validate and group
	for _, op := range operations {
		switch op.Type {
		case OperationType_OP_SET:
			item := protoItemToStorage(op.GetSetData(), op.ClientVersion)
			if item == nil {
				// Skip malformed operation
				continue
			}
			// Use latest version if key appears multiple times
			if existing, ok := setItems[op.Key]; !ok || item.Version > existing.Version {
				setItems[op.Key] = item
			}
		case OperationType_OP_DELETE:
			deleteKeys = append(deleteKeys, struct {
				key     string
				version int64
			}{key: op.Key, version: op.ClientVersion})
		}
	}

	// Batch apply SET operations using BatchSet if available
	if len(setItems) > 0 {
		if batchStore, ok := b.store.(interface {
			BatchSet(items map[string]*st.StoredItem) error
		}); ok {
			// Use batch API for better performance
			if err := batchStore.BatchSet(setItems); err != nil {
				// Fallback to individual operations
				for key, item := range setItems {
					if err := b.store.Set(key, item); err != nil {
						return err
					}
				}
			}
		} else {
			// Fallback to individual operations
			for key, item := range setItems {
				if err := b.store.Set(key, item); err != nil {
					return err
				}
			}
		}
	}

	for _, delOp := range deleteKeys {
		if err := b.store.Delete(delOp.key, delOp.version); err != nil {
			if err != st.ErrItemNotFound {
				return err
			}
		}
	}

	return nil
}

func (b *StorageBridge) ApplyFullSyncSnapshot(snapshot []*FullStateItem, snapshotTS time.Time) error {
	// Convert defensively and drop nil/malformed entries
	storageItems := make([]*st.FullStateItem, 0, len(snapshot))
	for _, protoItem := range snapshot {
		if protoItem == nil {
			continue
		}
		converted := protoFullItemToStorage(protoItem)
		if converted == nil {
			continue
		}
		// If item payload is missing, treat as tombstone-only; allow backend to decide
		storageItems = append(storageItems, converted)
	}
	if len(storageItems) == 0 {
		return nil
	}
	return b.store.ApplyFullSyncSnapshot(storageItems, snapshotTS)
}

func (b *StorageBridge) Stats() st.StorageStats {
	return b.store.Stats()
}

// storageItemToProto and protoItemToStorage are now defined in replication.go
// They are package-level functions accessible from here

//go:inline
func storageSyncOpToProto(op *st.CacheSyncOperation) *CacheSyncOperation {
	if op == nil {
		return nil
	}

	protoOp := &CacheSyncOperation{
		Key:           op.Key,
		ClientVersion: op.Version,
	}

	switch op.Type {
	case "SET":
		protoOp.Type = OperationType_OP_SET
		if op.Data != nil {
			protoOp.DataPayload = &CacheSyncOperation_SetData{
				SetData: storageItemToProto(op.Data),
			}
		}
	case "DELETE":
		protoOp.Type = OperationType_OP_DELETE
	default:
		protoOp.Type = OperationType_OP_UNSPECIFIED
	}

	return protoOp
}

//go:inline
func storageFullItemToProto(item *st.FullStateItem) *FullStateItem {
	if item == nil {
		return nil
	}
	return &FullStateItem{
		Key:      item.Key,
		Version:  item.Version,
		ItemData: storageItemToProto(item.Item),
	}
}

//go:inline
func protoFullItemToStorage(item *FullStateItem) *st.FullStateItem {
	if item == nil {
		return nil
	}
	return &st.FullStateItem{
		Key:     item.Key,
		Version: item.Version,
		Item:    protoItemToStorage(item.ItemData, item.Version),
	}
}
