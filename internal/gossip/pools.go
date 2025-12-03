package gossip

import (
	"sync"
)

// Unified object pools for gossip package - simplified and optimized

var (
	// String slice pool for replica lists
	stringSlicePool = sync.Pool{
		New: func() interface{} {
			return make([]string, 0, 16) // Increased from 10 for better capacity
		},
	}

	// Byte slice pool for temporary buffers (tiered sizes)
	byteSlicePool = sync.Pool{
		New: func() interface{} {
			return make([]byte, 0, 1024) // Increased from 512 for better reuse
		},
	}

	// Operation pool for cache sync operations
	operationPool = sync.Pool{
		New: func() interface{} {
			return &CacheSyncOperation{}
		},
	}

	// Operation slice pool for batch operations - larger capacity
	operationSlicePool = sync.Pool{
		New: func() interface{} {
			return make([]*CacheSyncOperation, 0, 2000) // Increased from 1000
		},
	}

	// Note: gossipMessagePool is defined in manager.go to avoid circular dependencies
)

// GetStringSlice retrieves a string slice from the pool.
func GetStringSlice() []string {
	return stringSlicePool.Get().([]string)[:0]
}

// PutStringSlice returns a string slice to the pool.
func PutStringSlice(slice []string) {
	if slice == nil || cap(slice) > 1000 {
		return
	}
	for i := range slice {
		slice[i] = "" // Clear references
	}
	stringSlicePool.Put(slice[:0])
}

// GetByteSlice retrieves a byte slice from the pool.
func GetByteSlice() []byte {
	return byteSlicePool.Get().([]byte)[:0]
}

// PutByteSlice returns a byte slice to the pool.
func PutByteSlice(slice []byte) {
	if slice == nil || cap(slice) > 65536 {
		return
	}
	byteSlicePool.Put(slice[:0])
}

// GetOperation retrieves a CacheSyncOperation from the pool.
func GetOperation() *CacheSyncOperation {
	op := operationPool.Get().(*CacheSyncOperation)
	// Reset to zero values
	op.Key = ""
	op.ClientVersion = 0
	op.Type = OperationType_OP_UNSPECIFIED
	op.DataPayload = nil
	return op
}

// PutOperation returns a CacheSyncOperation to the pool.
func PutOperation(op *CacheSyncOperation) {
	if op == nil {
		return
	}
	op.Key = ""
	op.ClientVersion = 0
	op.Type = OperationType_OP_UNSPECIFIED
	op.DataPayload = nil
	op.SetData = nil
	operationPool.Put(op)
}

// GetOperationSlice retrieves an operation slice from the pool.
func GetOperationSlice() []*CacheSyncOperation {
	return operationSlicePool.Get().([]*CacheSyncOperation)[:0]
}

// PutOperationSlice returns an operation slice to the pool.
func PutOperationSlice(ops []*CacheSyncOperation) {
	if ops == nil || cap(ops) > 20000 { // Increased from 10000
		return
	}
	// Clear references to avoid memory leaks
	for i := range ops {
		ops[i] = nil
	}
	operationSlicePool.Put(ops[:0])
}

// Note: getGossipMessage and putGossipMessage are defined in manager.go

