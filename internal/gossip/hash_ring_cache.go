package gossip

import (
	"sync"
	"sync/atomic"
	"time"
)

// HashRingCache caches hash ring lookups without TTL overhead
// Uses simple size-based eviction for high performance
type HashRingCache struct {
	cache   sync.Map
	ring    *ConsistentHash
	maxSize int
	size    atomic.Int64
}

type CachedResult struct {
	replicas []string
	// Removed cachedAt - no TTL needed for stable hash ring results
}

func NewHashRingCache(ring *ConsistentHash, ttl time.Duration, maxSize int) *HashRingCache {
	// ttl parameter kept for backward compatibility but not used
	return &HashRingCache{
		ring:    ring,
		maxSize: maxSize,
	}
}

func (hrc *HashRingCache) GetN(key string, n int) []string {
	// Fast path: direct cache lookup (no TTL check)
	if cached, ok := hrc.cache.Load(key); ok {
		result := cached.(*CachedResult)
		return result.replicas
	}

	// Cache miss: compute and store
	replicas := hrc.ring.GetN(key, n)
	currentSize := hrc.size.Load()
	
	if currentSize < int64(hrc.maxSize) {
		// Room available: store directly
		hrc.cache.Store(key, &CachedResult{
			replicas: replicas,
		})
		hrc.size.Add(1)
	} else if currentSize >= int64(hrc.maxSize) {
		// Cache full: random eviction (simple and fast)
		// Delete first entry found in map iteration (random)
		hrc.cache.Range(func(k, v interface{}) bool {
			hrc.cache.Delete(k)
			hrc.size.Add(-1)
			return false // Stop after first deletion
		})
		// Store new entry
		hrc.cache.Store(key, &CachedResult{
			replicas: replicas,
		})
		hrc.size.Add(1)
	}
	
	return replicas
}

func (hrc *HashRingCache) Invalidate() {
	hrc.cache.Range(func(key, value interface{}) bool {
		hrc.cache.Delete(key)
		return true
	})
	hrc.size.Store(0)
}

func (hrc *HashRingCache) InvalidateKey(key string) {
	hrc.cache.Delete(key)
	hrc.size.Add(-1)
}
