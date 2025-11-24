package gossip

import (
	"sync"
	"sync/atomic"
	"time"
)

type HashRingCache struct {
	cache   sync.Map
	ring    *ConsistentHash
	ttl     time.Duration
	maxSize int
	size    atomic.Int64
}

type CachedResult struct {
	replicas []string
	cachedAt time.Time
}

func NewHashRingCache(ring *ConsistentHash, ttl time.Duration, maxSize int) *HashRingCache {
	return &HashRingCache{
		ring:    ring,
		ttl:     ttl,
		maxSize: maxSize,
	}
}

func (hrc *HashRingCache) GetN(key string, n int) []string {
	if cached, ok := hrc.cache.Load(key); ok {
		result := cached.(*CachedResult)
		if time.Since(result.cachedAt) < hrc.ttl {
			return result.replicas
		}
		hrc.cache.Delete(key)
		hrc.size.Add(-1)
	}

	replicas := hrc.ring.GetN(key, n)
	if hrc.size.Load() < int64(hrc.maxSize) {
		hrc.cache.Store(key, &CachedResult{
			replicas: replicas,
			cachedAt: time.Now(),
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
