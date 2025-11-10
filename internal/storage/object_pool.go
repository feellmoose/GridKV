package storage

// File: object_pool.go
// Purpose: Object pooling for reducing allocations
//
// This file provides sync.Pool based object pools for frequently allocated types.
// Reduces GC pressure and improves performance by reusing objects.
//
// Pooled types:
//   - StoredItem
//   - CacheSyncOperation
//   - FullStateItem
//   - Byte buffers (128B, 1KB, 10KB)

import (
	"sync"
	"time"
)

// Object pools for reducing allocations (OPTIMIZATION)
// Using sync.Pool provides automatic scaling and GC-friendly object reuse

var (
	// StoredItem pool for reducing allocations in Set/Get operations
	storedItemPool = sync.Pool{
		New: func() interface{} {
			return &StoredItem{}
		},
	}

	// CacheSyncOperation pool for reducing allocations in sync operations
	syncOperationPool = sync.Pool{
		New: func() interface{} {
			return &CacheSyncOperation{}
		},
	}

	// FullStateItem pool for reducing allocations in full sync
	fullStateItemPool = sync.Pool{
		New: func() interface{} {
			return &FullStateItem{}
		},
	}

	// Byte slice pool for value buffers (small sizes: 128B, 1KB, 10KB)
	bytePool128 = sync.Pool{
		New: func() interface{} {
			buf := make([]byte, 128)
			return &buf
		},
	}

	bytePool1K = sync.Pool{
		New: func() interface{} {
			buf := make([]byte, 1024)
			return &buf
		},
	}

	bytePool10K = sync.Pool{
		New: func() interface{} {
			buf := make([]byte, 10240)
			return &buf
		},
	}
)

// GetStoredItem retrieves a StoredItem from the pool.
// IMPORTANT: Call PutStoredItem when done to return it to the pool.
//
// Returns:
//   - *StoredItem: A clean StoredItem ready for use
func GetStoredItem() *StoredItem {
	item := storedItemPool.Get().(*StoredItem)
	// Reset to zero values
	item.Value = nil
	item.ExpireAt = time.Time{} // Zero time
	item.Version = 0
	return item
}

// PutStoredItem returns a StoredItem to the pool.
// The item should not be used after calling this function.
//
// Parameters:
//   - item: The item to return to the pool
func PutStoredItem(item *StoredItem) {
	if item != nil {
		// Clear value to avoid holding references
		item.Value = nil
		storedItemPool.Put(item)
	}
}

// GetSyncOperation retrieves a CacheSyncOperation from the pool.
//
// Returns:
//   - *CacheSyncOperation: A clean sync operation
func GetSyncOperation() *CacheSyncOperation {
	op := syncOperationPool.Get().(*CacheSyncOperation)
	op.Key = ""
	op.Version = 0
	op.Type = ""
	op.Data = nil
	return op
}

// PutSyncOperation returns a CacheSyncOperation to the pool.
//
// Parameters:
//   - op: The operation to return to the pool
func PutSyncOperation(op *CacheSyncOperation) {
	if op != nil {
		op.Data = nil // Clear reference
		syncOperationPool.Put(op)
	}
}

// GetFullStateItem retrieves a FullStateItem from the pool.
//
// Returns:
//   - *FullStateItem: A clean full state item
func GetFullStateItem() *FullStateItem {
	item := fullStateItemPool.Get().(*FullStateItem)
	item.Key = ""
	item.Version = 0
	item.Item = nil
	return item
}

// PutFullStateItem returns a FullStateItem to the pool.
//
// Parameters:
//   - item: The item to return to the pool
func PutFullStateItem(item *FullStateItem) {
	if item != nil {
		item.Item = nil // Clear reference
		fullStateItemPool.Put(item)
	}
}

// GetByteBuffer retrieves an appropriately sized byte buffer from the pool.
// Choose the smallest buffer that fits the required size for best performance.
//
// Parameters:
//   - size: The minimum size needed
//
// Returns:
//   - *[]byte: A pointer to a byte slice (may be larger than requested)
//   - int: The pool index (0=128B, 1=1KB, 2=10KB, -1=no pool)
func GetByteBuffer(size int) (*[]byte, int) {
	if size <= 128 {
		return bytePool128.Get().(*[]byte), 0
	} else if size <= 1024 {
		return bytePool1K.Get().(*[]byte), 1
	} else if size <= 10240 {
		return bytePool10K.Get().(*[]byte), 2
	}
	// For larger sizes, allocate directly (no pool benefit)
	buf := make([]byte, size)
	return &buf, -1
}

// PutByteBuffer returns a byte buffer to the appropriate pool.
//
// Parameters:
//   - buf: The buffer to return
//   - poolIndex: The pool index returned by GetByteBuffer
func PutByteBuffer(buf *[]byte, poolIndex int) {
	if buf == nil || poolIndex < 0 {
		return
	}

	switch poolIndex {
	case 0:
		bytePool128.Put(buf)
	case 1:
		bytePool1K.Put(buf)
	case 2:
		bytePool10K.Put(buf)
	}
}

// CopyToPooledBuffer copies data to a pooled buffer.
// This is useful when you need to store a copy of the data.
//
// Parameters:
//   - src: Source data to copy
//
// Returns:
//   - []byte: A new slice containing a copy of the data
func CopyToPooledBuffer(src []byte) []byte {
	if len(src) == 0 {
		return nil
	}

	// Get buffer from pool
	buf, poolIdx := GetByteBuffer(len(src))

	// If we got a pooled buffer, copy data
	if poolIdx >= 0 {
		// Slice to exact size and copy
		dst := (*buf)[:len(src)]
		copy(dst, src)
		return dst
	}

	// For non-pooled buffers, just copy normally
	dst := make([]byte, len(src))
	copy(dst, src)
	return dst
}
