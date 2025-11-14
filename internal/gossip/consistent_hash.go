// Package gossip implements the Gossip protocol for distributed cluster management.
//
// This package provides the core distributed systems primitives including:
//   - Consistent hashing for data distribution (Dynamo-style)
//   - SWIM-based failure detection
//   - Quorum-based replication (N/W/R)
//   - Epidemic broadcast for state synchronization
//
// The consistent hashing implementation is based on the algorithm described in:
// "Dynamo: Amazon's Highly Available Key-value Store" (SOSP 2007)
// https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf
package gossip

import (
	"sort"
	"strconv"
	"sync"

	"github.com/cespare/xxhash/v2"
)

// Hash is a function type that computes a 32-bit hash from a byte slice.
// Custom hash functions can be provided for testing or specific requirements.
//
// Default implementation uses xxHash (XXH64) which provides:
//   - Speed: ~10 GB/s on modern CPUs
//   - Quality: Good distribution properties
//   - Simplicity: No cryptographic overhead needed for load balancing
type Hash func(data []byte) uint32

// ConsistentHash implements Dynamo-style consistent hashing with virtual nodes.
//
// Consistent hashing distributes keys across nodes such that adding or removing
// a node only affects 1/N of the keys (where N is the number of nodes).
//
// Algorithm:
//  1. Hash each physical node to create R virtual nodes (replicas)
//  2. Place virtual nodes on a hash ring (0 to 2^32-1)
//  3. To locate a key: hash(key) -> find next node clockwise on ring
//
// Properties:
//   - Load balance: Virtual nodes improve distribution uniformity
//   - Minimal disruption: Only ~1/N keys move when nodes change
//   - Deterministic: Same key always maps to same nodes
//
// Performance:
//   - Add/Remove: O(R log(N*R)) where R=replicas, N=nodes
//   - Get: O(log(N*R)) binary search
//   - GetN: O(N*R) worst case (typically O(replicas))
//
// Thread-safety: All methods are thread-safe using RWMutex.
//
// References:
//   - Karger et al. "Consistent Hashing and Random Trees" (1997)
//   - DeCandia et al. "Dynamo: Amazon's Highly Available Key-value Store" (2007)
//
// Example:
//
//	ring := NewConsistentHash(150, nil) // 150 virtual nodes per physical node
//	ring.Add("node1")
//	ring.Add("node2")
//	ring.Add("node3")
//
//	// Find primary node for a key
//	node := ring.Get("user:12345") // e.g., "node2"
//
//	// Find N replicas for quorum
//	replicas := ring.GetN("user:12345", 3) // e.g., ["node2", "node1", "node3"]
type ConsistentHash struct {
	// mu protects all fields from concurrent access
	mu sync.RWMutex

	// hashFunc computes the hash for keys and virtual nodes
	// Default: xxHash (fast, non-cryptographic)
	hashFunc Hash

	// replicas is the number of virtual nodes per physical node
	// Higher values improve load distribution but increase memory
	// Typical range: 50-500, recommended: 150
	replicas int

	// keys is the sorted list of all virtual node hashes
	// Used for binary search in O(log n) time
	// Invariant: always sorted in ascending order
	keys []uint32

	// ring maps virtual node hash -> physical node name
	// Size: replicas * number_of_physical_nodes
	ring map[uint32]string

	// nodes tracks which physical nodes exist
	// Used for O(1) duplicate detection and GetN deduplication
	nodes map[string]bool
}

// NewConsistentHash creates a new consistent hash ring.
//
// Parameters:
//   - replicas: Number of virtual nodes per physical node (typically 50-500)
//     Higher values improve load distribution but increase memory usage.
//     Recommended: 150 (good balance of distribution and memory)
//     Memory per node: ~replicas * (8 bytes hash + 16 bytes string pointer)
//   - fn: Hash function for keys and nodes (nil = use default xxHash)
//     Custom functions useful for testing or specific requirements.
//
// Returns:
//   - *ConsistentHash: Initialized hash ring (empty, call Add to populate)
//
// Performance:
//   - Time: O(1)
//   - Memory: O(replicas * nodes) after adding nodes
//
// Example:
//
//	// Default configuration (xxHash, 150 replicas)
//	ring := NewConsistentHash(150, nil)
//
//	// Custom hash function (for testing)
//	customHash := func(data []byte) uint32 {
//	    return crc32.ChecksumIEEE(data)
//	}
//	ring := NewConsistentHash(100, customHash)
//
// Thread-safety: Safe to call concurrently.
func NewConsistentHash(replicas int, fn Hash) *ConsistentHash {
	if fn == nil {
		// Default: xxHash (XXH64) truncated to 32 bits
		// xxHash provides excellent speed (~10 GB/s) and distribution
		// without cryptographic overhead (vs SHA256: ~500 MB/s)
		fn = func(data []byte) uint32 {
			return uint32(xxhash.Sum64(data))
		}
	}

	return &ConsistentHash{
		hashFunc: fn,
		replicas: replicas,
		ring:     make(map[uint32]string),
		nodes:    make(map[string]bool),
	}
}

// Add registers a physical node and creates its virtual nodes on the ring.
//
// The node is hashed R times (where R = replicas) to create virtual nodes
// distributed around the ring. This improves load distribution uniformity.
//
// Parameters:
//   - name: Unique identifier for the physical node (e.g., "node1", "192.168.1.10:8001")
//     Must be unique across all nodes in the ring.
//     Calling Add with an existing name is a no-op (idempotent).
//
// Algorithm:
//  1. Check if node already exists (idempotent)
//  2. Generate R virtual node hashes: hash(strconv.Itoa(i) + name)
//  3. Merge new hashes into sorted key list in O(R + N) time
//
// Performance:
//   - Time: O(R + N) where R=replicas, N=existing virtual nodes
//     Previously O(R log(R+N)) with sort, now optimized with merge
//   - Memory: +R entries in ring map, +R entries in keys array
//   - Amortized: O(R) when N >> R (common case)
//
// Example:
//
//	ring := NewConsistentHash(150, nil)
//	ring.Add("node1")        // Creates 150 virtual nodes
//	ring.Add("node2")        // Creates 150 more virtual nodes
//	ring.Add("node1")        // No-op (already exists)
//	fmt.Println(ring.Members()) // ["node1", "node2"]
//
// Thread-safety: Safe to call concurrently (uses write lock).
//
// Optimization notes:
//   - Sorted merge (O(R+N)) faster than insert-sort (O(R*log(R+N)))
//   - Pre-allocation reduces slice growth allocations
//   - Hash computation dominates for small clusters (< 100 nodes)
func (c *ConsistentHash) Add(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Check if the node already exists to prevent duplicate addition.
	if c.nodes[name] {
		return
	}
	c.nodes[name] = true

	newKeys := make([]uint32, 0, len(c.keys)+c.replicas)

	// Collect all hashes for this node first
	hashes := make([]uint32, c.replicas)
	for i := 0; i < c.replicas; i++ {
		// Generate a unique key for each virtual node: ReplicaIndex + NodeName.
		key := strconv.Itoa(i) + name
		hash := c.hashFunc([]byte(key))
		hashes[i] = hash

		// Store the virtual node hash and its corresponding real node name.
		c.ring[hash] = name
	}

	// Sort the new hashes to minimize insertions
	sort.Slice(hashes, func(i, j int) bool {
		return hashes[i] < hashes[j]
	})

	i, j := 0, 0
	for i < len(c.keys) && j < len(hashes) {
		if c.keys[i] < hashes[j] {
			newKeys = append(newKeys, c.keys[i])
			i++
		} else {
			newKeys = append(newKeys, hashes[j])
			j++
		}
	}

	// Append remaining elements
	if i < len(c.keys) {
		newKeys = append(newKeys, c.keys[i:]...)
	}
	if j < len(hashes) {
		newKeys = append(newKeys, hashes[j:]...)
	}

	c.keys = newKeys
}

// Remove deletes a physical node and all its virtual nodes from the ring.
//
// After removal, keys previously owned by this node will be redistributed
// to the next node clockwise on the ring (minimal data movement).
//
// Parameters:
//   - name: Identifier of the node to remove
//     If the node doesn't exist, this is a no-op (idempotent).
//
// Algorithm:
//  1. Check if node exists (idempotent)
//  2. Compute R virtual node hashes to remove
//  3. Filter these hashes from sorted key list in O(N) time
//
// Performance:
//   - Time: O(R + N) where R=replicas, N=total virtual nodes
//   - Memory: -R entries from ring map, -R entries from keys array
//   - In-place filtering avoids allocation
//
// Data movement:
//   - On average, 1/M of keys move (where M = number of remaining nodes)
//   - Keys owned by removed node move to next clockwise node
//   - Other keys unaffected (consistent hashing property)
//
// Example:
//
//	ring := NewConsistentHash(150, nil)
//	ring.Add("node1")
//	ring.Add("node2")
//	ring.Add("node3")
//	ring.Remove("node2")     // Keys from node2 -> node1 or node3
//	fmt.Println(ring.Members()) // ["node1", "node3"]
//
// Thread-safety: Safe to call concurrently (uses write lock).
func (c *ConsistentHash) Remove(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Check if the node does not exist.
	if !c.nodes[name] {
		return
	}
	delete(c.nodes, name)

	toRemove := make(map[uint32]bool, c.replicas)
	for i := 0; i < c.replicas; i++ {
		key := strconv.Itoa(i) + name
		hash := c.hashFunc([]byte(key))

		// Delete the virtual node from the ring map.
		delete(c.ring, hash)
		toRemove[hash] = true
	}

	// This is O(n) instead of O(n log n) rebuild+sort
	newLen := 0
	for i := 0; i < len(c.keys); i++ {
		if !toRemove[c.keys[i]] {
			c.keys[newLen] = c.keys[i]
			newLen++
		}
	}
	c.keys = c.keys[:newLen]
	// Keys remain sorted since we only removed elements
}

// Get returns the primary node responsible for a key.
//
// The key is hashed and mapped to the first virtual node encountered
// clockwise on the ring. This is the "coordinator" node in Dynamo terminology.
//
// Parameters:
//   - key: The key to locate (e.g., "user:12345", "session:abc")
//     Can be any string; different keys distribute across nodes.
//
// Returns:
//   - string: Physical node name (e.g., "node2")
//     Empty string if ring is empty (no nodes added yet).
//
// Algorithm:
//  1. Compute hash(key) -> uint32
//  2. Binary search for first virtual node >= hash
//  3. Wrap around if hash > all virtual nodes
//  4. Return physical node owning that virtual node
//
// Performance:
//   - Time: O(log N) where N = total virtual nodes (replicas * physical nodes)
//   - Typical: log(150 * 10) = ~11 comparisons for 10-node cluster
//   - Cache-friendly: binary search on contiguous array
//
// Example:
//
//	ring := NewConsistentHash(150, nil)
//	ring.Add("node1")
//	ring.Add("node2")
//	ring.Add("node3")
//
//	node := ring.Get("user:12345")     // e.g., "node2"
//	same := ring.Get("user:12345")     // Deterministic: same "node2"
//	different := ring.Get("user:67890") // Different key -> possibly different node
//
//	// Empty ring
//	emptyRing := NewConsistentHash(150, nil)
//	node := emptyRing.Get("key")       // Returns ""
//
// Thread-safety: Safe to call concurrently (uses read lock).
func (c *ConsistentHash) Get(key string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(c.keys) == 0 {
		return "" // Ring is empty.
	}

	// Calculate the hash value of the key.
	hash := c.hashFunc([]byte(key))

	// This avoids function call overhead in the hot path
	i := fastBinarySearch(c.keys, hash)

	// Wrap-around logic: If i equals the length of the keys list, the key's hash is greater than all virtual node hashes,
	// so it should wrap back to the start of the ring, keys[0].
	index := i % len(c.keys)

	// Return the real node name corresponding to that virtual node.
	return c.ring[c.keys[index]]
}

// GetN returns N replica nodes for a key in preference order.
//
// Returns the top N physical nodes responsible for storing replicas of this key.
// The first node is the primary (coordinator), subsequent nodes are replicas.
// Nodes are returned in clockwise order around the ring (preference list).
//
// Parameters:
//   - key: The key to locate (same as Get)
//   - replicationFactor: Number of distinct physical nodes to return (N in N/W/R)
//     Capped at the number of available physical nodes.
//     replicationFactor=0 returns nil.
//
// Returns:
//   - []string: List of physical node names in preference order
//     Length: min(replicationFactor, number of physical nodes)
//     Empty/nil if ring is empty or replicationFactor=0
//
// Algorithm:
//  1. Hash key and find position on ring (same as Get)
//  2. Walk clockwise, collecting unique physical nodes
//  3. Skip virtual nodes that map to already-seen physical nodes
//  4. Stop when N unique physical nodes found or ring fully traversed
//
// Performance:
//   - Time: O(V) where V = virtual nodes to traverse
//     Average: O(N * replicas) to find N unique physical nodes
//     Worst: O(total virtual nodes) if ring has few physical nodes
//   - Typical: O(N * 150) for 150 replicas per node
//   - Memory: O(N) for result slice + seen map
//
// Example:
//
//	ring := NewConsistentHash(150, nil)
//	ring.Add("node1")
//	ring.Add("node2")
//	ring.Add("node3")
//
//	// Quorum replication (N=3, W=2, R=2)
//	replicas := ring.GetN("user:12345", 3)
//	// Returns: ["node2", "node1", "node3"] (preference order)
//	// Write to first 2 nodes (W=2), read from any 2 (R=2)
//
//	// Request more nodes than available
//	replicas := ring.GetN("key", 10)
//	// Returns: ["node2", "node1", "node3"] (only 3 nodes exist)
//
//	// Single node (no replication)
//	replicas := ring.GetN("key", 1)
//	// Returns: ["node2"] (same as Get)
//
// Use cases:
//   - Quorum writes: Write to first W nodes from GetN(key, N)
//   - Quorum reads: Read from R nodes, return most recent
//   - Hinted handoff: Use GetN(key, N+1) for extra fallback node
//
// Thread-safety: Safe to call concurrently (uses read lock).
func (c *ConsistentHash) GetN(key string, replicationFactor int) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(c.keys) == 0 || replicationFactor == 0 {
		return nil
	}

	if replicationFactor > len(c.nodes) {
		replicationFactor = len(c.nodes)
	}

	hash := c.hashFunc([]byte(key))

	i := sort.Search(len(c.keys), func(j int) bool {
		return c.keys[j] >= hash
	})

	var nodes []string
	seen := make(map[string]bool)

	startIndex := i

	for count := 0; len(nodes) < replicationFactor && count < len(c.keys); count++ {
		currentIdx := (startIndex + count) % len(c.keys)

		virtualNodeHash := c.keys[currentIdx]
		nodeID := c.ring[virtualNodeHash]

		if !seen[nodeID] {
			nodes = append(nodes, nodeID)
			seen[nodeID] = true
		}
	}

	return nodes
}

// Members returns all physical nodes currently in the ring.
//
// Returns:
//   - []string: List of physical node names (unordered)
//     Empty slice if no nodes have been added.
//     Order is not guaranteed (iteration over map).
//
// Performance:
//   - Time: O(M) where M = number of physical nodes
//   - Memory: O(M) for result slice
//
// Example:
//
//	ring := NewConsistentHash(150, nil)
//	ring.Add("node1")
//	ring.Add("node2")
//	members := ring.Members()
//	// Returns: ["node1", "node2"] or ["node2", "node1"] (unordered)
//
// Thread-safety: Safe to call concurrently (uses read lock).
func (c *ConsistentHash) Members() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	names := make([]string, 0, len(c.nodes))
	for name := range c.nodes {
		names = append(names, name)
	}
	return names
}
