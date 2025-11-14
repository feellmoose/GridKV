package gossip

import (
	"time"

	"github.com/feellmoose/gridkv/internal/storage"
)

// StorageBridge wraps any storage.Storage implementation and provides proto type conversion
type StorageBridge struct {
	store storage.Storage
}

// NewStorageBridge creates an optimized bridge for any storage type
func NewStorageBridge(store storage.Storage) *StorageBridge {
	return &StorageBridge{store: store}
}

// Delegate basic operations to underlying storage
func (b *StorageBridge) Set(key string, item *storage.StoredItem) error {
	return b.store.Set(key, item)
}

func (b *StorageBridge) Get(key string) (*storage.StoredItem, error) {
	return b.store.Get(key)
}

func (b *StorageBridge) Delete(key string, version int64) error {
	return b.store.Delete(key, version)
}

func (b *StorageBridge) Keys() []string {
	return b.store.Keys()
}

func (b *StorageBridge) Clear() error {
	return b.store.Clear()
}

func (b *StorageBridge) Close() error {
	return b.store.Close()
}

// GetSyncBuffer returns proto CacheSyncOperation types
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

// GetFullSyncSnapshot returns proto FullStateItem types
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

// ApplyIncrementalSync accepts proto CacheSyncOperation types
func (b *StorageBridge) ApplyIncrementalSync(operations []*CacheSyncOperation) error {
	for _, op := range operations {
		switch op.Type {
		case OperationType_OP_SET:
			item := protoItemToStorage(op.GetSetData(), op.ClientVersion)
			if err := b.store.Set(op.Key, item); err != nil {
				return err
			}
		case OperationType_OP_DELETE:
			if err := b.store.Delete(op.Key, op.ClientVersion); err != nil {
				if err != storage.ErrItemNotFound {
					return err
				}
			}
		}
	}
	return nil
}

// ApplyFullSyncSnapshot accepts proto FullStateItem types
func (b *StorageBridge) ApplyFullSyncSnapshot(snapshot []*FullStateItem, snapshotTS time.Time) error {
	storageItems := make([]*storage.FullStateItem, len(snapshot))
	for i, protoItem := range snapshot {
		storageItems[i] = protoFullItemToStorage(protoItem)
	}
	return b.store.ApplyFullSyncSnapshot(storageItems, snapshotTS)
}

// Stats returns storage statistics
func (b *StorageBridge) Stats() storage.StorageStats {
	return b.store.Stats()
}

// ============================================================================
// Type Converters
// ============================================================================

// storageItemToProto converts a storage.StoredItem to proto StoredItem
//
//go:inline
func storageItemToProto(item *storage.StoredItem) *StoredItem {
	if item == nil {
		return nil
	}
	var expire uint64
	if !item.ExpireAt.IsZero() {
		expire = uint64(item.ExpireAt.Unix())
	}

	valueCopy := make([]byte, len(item.Value))
	copy(valueCopy, item.Value)

	return &StoredItem{
		ExpireAt: expire,
		Value:    valueCopy,
	}
}

// protoItemToStorage converts a proto StoredItem to storage.StoredItem
//
//go:inline
func protoItemToStorage(item *StoredItem, version int64) *storage.StoredItem {
	if item == nil {
		return nil
	}
	var expire time.Time
	if item.ExpireAt != 0 {
		expire = time.Unix(int64(item.ExpireAt), 0)
	}
	valueCopy := make([]byte, len(item.Value))
	copy(valueCopy, item.Value)

	return &storage.StoredItem{
		ExpireAt: expire,
		Version:  version,
		Value:    valueCopy,
	}
}

// storageSyncOpToProto converts storage.CacheSyncOperation to proto CacheSyncOperation
func storageSyncOpToProto(op *storage.CacheSyncOperation) *CacheSyncOperation {
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

// storageFullItemToProto converts storage.FullStateItem to proto FullStateItem
func storageFullItemToProto(item *storage.FullStateItem) *FullStateItem {
	if item == nil {
		return nil
	}
	return &FullStateItem{
		Key:      item.Key,
		Version:  item.Version,
		ItemData: storageItemToProto(item.Item),
	}
}

// protoFullItemToStorage converts proto FullStateItem to storage.FullStateItem
func protoFullItemToStorage(item *FullStateItem) *storage.FullStateItem {
	if item == nil {
		return nil
	}
	return &storage.FullStateItem{
		Key:     item.Key,
		Version: item.Version,
		Item:    protoItemToStorage(item.ItemData, item.Version),
	}
}
