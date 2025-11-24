package gossip

import (
	"sync"
)

var (
	operationPool = sync.Pool{
		New: func() interface{} {
			return &CacheSyncOperation{}
		},
	}

	operationSlicePool = sync.Pool{
		New: func() interface{} {
			return make([]*CacheSyncOperation, 0, 1000)
		},
	}
)

func GetOperation() *CacheSyncOperation {
	return operationPool.Get().(*CacheSyncOperation)
}

func PutOperation(op *CacheSyncOperation) {
	if op == nil {
		return
	}
	op.Key = ""
	op.ClientVersion = 0
	op.Type = OperationType_OP_UNSPECIFIED
	op.DataPayload = nil
	operationPool.Put(op)
}

func GetOperationSlice() []*CacheSyncOperation {
	return operationSlicePool.Get().([]*CacheSyncOperation)
}

func PutOperationSlice(ops []*CacheSyncOperation) {
	if cap(ops) > 10000 {
		return
	}
	for i := range ops {
		ops[i] = nil
	}
	ops = ops[:0]
	operationSlicePool.Put(ops)
}
