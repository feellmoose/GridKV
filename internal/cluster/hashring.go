package cluster

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/zeebo/xxh3"
)

// Object pools for hash ring operations
var (
	// String slice pool for GetN results
	getNResultPool = sync.Pool{
		New: func() interface{} {
			return make([]string, 0, 8)
		},
	}

	// Map pool for seen tracking in GetN
	getNSeenPool = sync.Pool{
		New: func() interface{} {
			return make(map[string]bool, 8)
		},
	}
)

// HashRing provides consistent hashing with virtual nodes
type HashRing interface {
	Get(key string) string
	GetN(key string, n int) []string
	Version() int64
	Update(version int64, nodes []string) bool
}

type ringNode struct {
	hash   uint64
	nodeID string
}

// ringData is immutable ring data (copy-on-write)
type ringData struct {
	nodes []ringNode
}

type hashRing struct {
	version      atomic.Int64
	virtualNodes int
	data         unsafe.Pointer // *ringData (atomic pointer for lock-free reads)
	mu           sync.Mutex     // Only for writes
}

func newHashRing(virtualNodes int) *hashRing {
	if virtualNodes <= 0 {
		virtualNodes = 128
	}

	r := &hashRing{
		virtualNodes: virtualNodes,
	}
	// Initialize with empty ring
	r.data = unsafe.Pointer(&ringData{nodes: make([]ringNode, 0)})
	return r
}

func (r *hashRing) Version() int64 {
	return r.version.Load()
}

func (r *hashRing) Update(version int64, nodeIDs []string) bool {
	// Fast path: check version without lock
	if version <= r.version.Load() {
		return false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Double check after acquiring lock
	if version <= r.version.Load() {
		return false
	}

	// Handle empty nodes gracefully
	if len(nodeIDs) == 0 {
		newData := &ringData{nodes: make([]ringNode, 0)}
		atomic.StorePointer(&r.data, unsafe.Pointer(newData))
		r.version.Store(version)
		return true
	}

	// Build new ring (copy-on-write)
	newNodes := make([]ringNode, 0, len(nodeIDs)*r.virtualNodes)

	for _, nodeID := range nodeIDs {
		for i := 0; i < r.virtualNodes; i++ {
			vnodeKey := nodeID + ":" + string(rune(i))
			hash := xxh3.HashString128(vnodeKey).Hi
			newNodes = append(newNodes, ringNode{
				hash:   hash,
				nodeID: nodeID,
			})
		}
	}

	// Sort by hash
	sort.Slice(newNodes, func(i, j int) bool {
		return newNodes[i].hash < newNodes[j].hash
	})

	// Atomically update pointer (readers will see new data)
	newData := &ringData{nodes: newNodes}
	atomic.StorePointer(&r.data, unsafe.Pointer(newData))
	r.version.Store(version)

	return true
}

func (r *hashRing) Get(key string) string {
	// Lock-free read: atomically load pointer
	data := (*ringData)(atomic.LoadPointer(&r.data))
	if data == nil || len(data.nodes) == 0 {
		return ""
	}

	hash := xxh3.HashString128(key).Hi
	idx := r.findNode(data.nodes, hash)
	if idx < 0 {
		return ""
	}

	return data.nodes[idx].nodeID
}

func (r *hashRing) GetN(key string, n int) []string {
	// Lock-free read: atomically load pointer
	data := (*ringData)(atomic.LoadPointer(&r.data))
	if data == nil || len(data.nodes) == 0 || n <= 0 {
		return nil
	}

	hash := xxh3.HashString128(key).Hi
	idx := r.findNode(data.nodes, hash)
	if idx < 0 {
		return nil
	}

	// Use pool to reduce allocations
	result := getNResultPool.Get().([]string)
	result = result[:0] // Reset length, keep capacity
	seen := getNSeenPool.Get().(map[string]bool)
	for k := range seen {
		delete(seen, k)
	}
	defer func() {
		if cap(result) <= 32 {
			getNResultPool.Put(result[:0])
		}
		if len(seen) <= 32 {
			getNSeenPool.Put(seen)
		}
	}()

	for i := 0; i < len(data.nodes) && len(result) < n; i++ {
		pos := (idx + i) % len(data.nodes)
		nodeID := data.nodes[pos].nodeID

		if !seen[nodeID] {
			seen[nodeID] = true
			result = append(result, nodeID)
		}
	}

	// Return copy to avoid pool reuse issues
	resultCopy := make([]string, len(result))
	copy(resultCopy, result)
	return resultCopy
}

func (r *hashRing) findNode(nodes []ringNode, hash uint64) int {
	if len(nodes) == 0 {
		return -1
	}

	// Binary search for first node with hash >= key hash
	idx := sort.Search(len(nodes), func(i int) bool {
		return nodes[i].hash >= hash
	})

	if idx >= len(nodes) {
		idx = 0 // Wrap around
	}

	return idx
}

// lifecycle.Component implementation
func (r *hashRing) Name() string                    { return "hash-ring" }
func (r *hashRing) Start(ctx context.Context) error { return nil }
func (r *hashRing) Close(ctx context.Context) error { return nil }
