package gossip

import (
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/feellmoose/gridkv/internal/storage"
	"google.golang.org/protobuf/proto"
)

// ============================================================================
// Object Pools
// ============================================================================

var (
	// GossipMessage pool - reuse message objects
	gossipMessagePool = sync.Pool{
		New: func() interface{} {
			return &GossipMessage{}
		},
	}

	// CacheSyncOperation pool
	cacheSyncOpPool = sync.Pool{
		New: func() interface{} {
			return &CacheSyncOperation{}
		},
	}

	// SyncMessage pool
	syncMessagePool = sync.Pool{
		New: func() interface{} {
			return &SyncMessage{}
		},
	}

	// IncrementalSyncPayload pool
	incrementalSyncPool = sync.Pool{
		New: func() interface{} {
			return &IncrementalSyncPayload{}
		},
	}

	// Proto clone buffer pool (larger for signature operations)
	protoCloneBufferPool = sync.Pool{
		New: func() interface{} {
			buf := make([]byte, 0, 16384) // 16KB for larger messages
			return &buf
		},
	}

	// ACK channel pool - reuse channels to reduce allocation
	ackChannelPool = sync.Pool{
		New: func() interface{} {
			return make(chan *CacheSyncAckPayload, 1)
		},
	}

	// GossipMessage for replication pool
	replicationMessagePool = sync.Pool{
		New: func() interface{} {
			return &GossipMessage{}
		},
	}
)

// ============================================================================
// Pool Access Functions
// ============================================================================

// getGossipMessage gets a GossipMessage from pool
//
//go:inline
func getGossipMessage() *GossipMessage {
	return gossipMessagePool.Get().(*GossipMessage)
}

// putGossipMessage returns a GossipMessage to pool after reset
//
//go:inline
func putGossipMessage(msg *GossipMessage) {
	if msg == nil {
		return
	}
	msg.Reset()
	gossipMessagePool.Put(msg)
}

// getCacheSyncOperation gets a CacheSyncOperation from pool
//
//go:inline
func getCacheSyncOperation() *CacheSyncOperation {
	return cacheSyncOpPool.Get().(*CacheSyncOperation)
}

// putCacheSyncOperation returns a CacheSyncOperation to pool
//
//go:inline
func putCacheSyncOperation(op *CacheSyncOperation) {
	if op == nil {
		return
	}
	op.Reset()
	cacheSyncOpPool.Put(op)
}

// getAckChannel gets an ACK channel from pool
//
//go:inline
func getAckChannel() chan *CacheSyncAckPayload {
	return ackChannelPool.Get().(chan *CacheSyncAckPayload)
}

// putAckChannel returns an ACK channel to pool after clearing
//
//go:inline
func putAckChannel(ch chan *CacheSyncAckPayload) {
	select {
	case <-ch:
	default:
	}
	ackChannelPool.Put(ch)
}

// getReplicationMessage gets a GossipMessage for replication from pool
//
//go:inline
func getReplicationMessage() *GossipMessage {
	return replicationMessagePool.Get().(*GossipMessage)
}

// putReplicationMessage returns a GossipMessage to pool
//
//go:inline
func putReplicationMessage(msg *GossipMessage) {
	if msg == nil {
		return
	}
	msg.Reset()
	replicationMessagePool.Put(msg)
}

// ============================================================================
// Zero-Copy Optimizations
// ============================================================================

// stringToBytes converts string to []byte without allocation
//
//go:inline
func stringToBytes(s string) []byte {
	if s == "" {
		return nil
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

// bytesToString converts []byte to string without allocation
//
//go:inline
func bytesToString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(b), len(b))
}

// copyBytesIfNeeded creates a copy only if the slice might be modified
//
//go:inline
func copyBytesIfNeeded(b []byte, needCopy bool) []byte {
	if !needCopy {
		return b
	}
	if len(b) == 0 {
		return nil
	}
	result := make([]byte, len(b))
	copy(result, b)
	return result
}

// ============================================================================
// Lock-Free Structures
// ============================================================================

// lockFreeCounter is a lock-free atomic counter
type lockFreeCounter struct {
	value atomic.Int64
}

// newLockFreeCounter creates a new lock-free counter
//
//go:inline
func newLockFreeCounter() *lockFreeCounter {
	return &lockFreeCounter{}
}

// Add adds delta to the counter and returns the new value
//
//go:inline
func (c *lockFreeCounter) Add(delta int64) int64 {
	return c.value.Add(delta)
}

// Load returns the current value
//
//go:inline
func (c *lockFreeCounter) Load() int64 {
	return c.value.Load()
}

// Store sets the value
//
//go:inline
func (c *lockFreeCounter) Store(val int64) {
	c.value.Store(val)
}

// lockFreeMap is a lock-free map using sync.Map (already lock-free)
type lockFreeMap[K comparable, V any] struct {
	m sync.Map
}

// newLockFreeMap creates a new lock-free map
//
//go:inline
func newLockFreeMap[K comparable, V any]() *lockFreeMap[K, V] {
	return &lockFreeMap[K, V]{}
}

// Load returns the value for a key
//
//go:inline
func (m *lockFreeMap[K, V]) Load(key K) (V, bool) {
	val, ok := m.m.Load(key)
	if !ok {
		var zero V
		return zero, false
	}
	return val.(V), true
}

// Store sets the value for a key
//
//go:inline
func (m *lockFreeMap[K, V]) Store(key K, value V) {
	m.m.Store(key, value)
}

// Delete removes a key
//
//go:inline
func (m *lockFreeMap[K, V]) Delete(key K) {
	m.m.Delete(key)
}

// ============================================================================
// Memory Allocation Optimizations
// ============================================================================

// preAllocatedSlicePool provides pre-allocated slices to reduce allocations
type preAllocatedSlicePool[T any] struct {
	pool sync.Pool
	size int
}

// newPreAllocatedSlicePool creates a pool of pre-allocated slices
func newPreAllocatedSlicePool[T any](size int) *preAllocatedSlicePool[T] {
	return &preAllocatedSlicePool[T]{
		pool: sync.Pool{
			New: func() interface{} {
				return make([]T, 0, size)
			},
		},
		size: size,
	}
}

// Get gets a slice from the pool
//
//go:inline
func (p *preAllocatedSlicePool[T]) Get() []T {
	return p.pool.Get().([]T)
}

// Put returns a slice to the pool after resetting
//
//go:inline
func (p *preAllocatedSlicePool[T]) Put(s []T) {
	if cap(s) >= p.size {
		s = s[:0]
		p.pool.Put(s)
	}
}

// ============================================================================
// Algorithm Optimizations
// ============================================================================

// fastBinarySearch performs binary search on a sorted slice
//
//go:inline
func fastBinarySearch(arr []uint32, target uint32) int {
	left, right := 0, len(arr)
	for left < right {
		mid := left + (right-left)/2
		if arr[mid] < target {
			left = mid + 1
		} else {
			right = mid
		}
	}
	return left
}

// fastKeyDedup performs fast deduplication of operations by key
//
//go:inline
func fastKeyDedup(ops []*CacheSyncOperation) []*CacheSyncOperation {
	if len(ops) == 0 {
		return nil
	}

	keyMap := make(map[string]*CacheSyncOperation, len(ops))
	result := make([]*CacheSyncOperation, 0, len(ops))

	for _, op := range ops {
		if op.Key != "" {
			if existing, exists := keyMap[op.Key]; !exists || op.ClientVersion > existing.ClientVersion {
				keyMap[op.Key] = op
			}
		} else {
			result = append(result, op)
		}
	}

	for _, op := range keyMap {
		result = append(result, op)
	}

	return result
}

// ============================================================================
// Proto Utilities
// ============================================================================

// fastProtoClone performs a fast proto clone using pooled buffer
func fastProtoClone(msg proto.Message) (proto.Message, []byte, error) {
	bufPtr := protoCloneBufferPool.Get().(*[]byte)
	defer func() {
		*bufPtr = (*bufPtr)[:0]
		protoCloneBufferPool.Put(bufPtr)
	}()

	data, err := proto.MarshalOptions{}.MarshalAppend(*bufPtr, msg)
	if err != nil {
		return nil, nil, err
	}

	newMsg := proto.Clone(msg)
	if newMsg == nil {
		return nil, nil, err
	}

	return newMsg, data, nil
}

// fastProtoCloneForSign performs optimized clone for signing
func fastProtoCloneForSign(msg *GossipMessage) (*GossipMessage, []byte, error) {
	bufPtr := protoCloneBufferPool.Get().(*[]byte)
	defer func() {
		*bufPtr = (*bufPtr)[:0]
		protoCloneBufferPool.Put(bufPtr)
	}()

	clone := proto.Clone(msg).(*GossipMessage)
	clone.Signature = nil

	data, err := proto.MarshalOptions{}.MarshalAppend(*bufPtr, clone)
	if err != nil {
		return nil, nil, err
	}

	dataCopy := make([]byte, len(data))
	copy(dataCopy, data)

	return clone, dataCopy, nil
}

// ============================================================================
// Inline Helper Functions
// ============================================================================

// fastMin returns the minimum of two integers
//
//go:inline
func fastMin(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// fastMax returns the maximum of two integers
//
//go:inline
func fastMax(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// fastAbs returns the absolute value
//
//go:inline
func fastAbs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// optimizedStorageItemToProto converts storage item to proto with minimal allocations
func optimizedStorageItemToProto(item *storage.StoredItem) *StoredItem {
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

// optimizedProtoItemToStorage converts proto item to storage with minimal allocations
func optimizedProtoItemToStorage(item *StoredItem, version int64) *storage.StoredItem {
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
