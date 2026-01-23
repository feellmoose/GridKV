package cache

import (
	"hash/fnv"
	"sync"
	"sync/atomic"
)

// CountMinSketch implements Count-Min Sketch for frequency estimation.
// Used for hotkey cache admission policy to solve high eviction latency bottleneck.
type CountMinSketch struct {
	width  uint32
	depth  uint32
	counts [][]uint64
	seed   uint32
	mu     sync.RWMutex
}

// NewCountMinSketch creates a new Count-Min Sketch.
func NewCountMinSketch(width, depth uint32) *CountMinSketch {
	cms := &CountMinSketch{
		width: width,
		depth: depth,
		seed:  0x12345678,
	}
	cms.counts = make([][]uint64, depth)
	for i := uint32(0); i < depth; i++ {
		cms.counts[i] = make([]uint64, width)
	}
	return cms
}

// Estimate returns estimated frequency count.
func (cms *CountMinSketch) Estimate(key string) uint64 {
	cms.mu.RLock()
	defer cms.mu.RUnlock()

	hash := fnv.New64a()
	hash.Write([]byte(key))
	h1 := hash.Sum64()
	h2 := hash64(h1)

	minCount := uint64(0)
	for i := uint32(0); i < cms.depth; i++ {
		h := h1 + uint64(i)*h2 + uint64(cms.seed)*uint64(i)
		idx := h % uint64(cms.width)
		count := cms.counts[i][idx]
		if i == 0 || count < minCount {
			minCount = count
		}
	}
	return minCount
}

// Increment increments frequency count.
func (cms *CountMinSketch) Increment(key string) {
	cms.mu.Lock()
	defer cms.mu.Unlock()

	hash := fnv.New64a()
	hash.Write([]byte(key))
	h1 := hash.Sum64()
	h2 := hash64(h1)

	for i := uint32(0); i < cms.depth; i++ {
		h := h1 + uint64(i)*h2 + uint64(cms.seed)*uint64(i)
		idx := h % uint64(cms.width)
		if cms.counts[i][idx] < 0xFFFFFFFFFFFFFFFF {
			cms.counts[i][idx]++
		}
	}
}

// Reset halves all counters to prevent overflow and age old entries.
func (cms *CountMinSketch) Reset() {
	cms.mu.Lock()
	defer cms.mu.Unlock()

	for i := uint32(0); i < cms.depth; i++ {
		for j := uint32(0); j < cms.width; j++ {
			cms.counts[i][j] >>= 1
		}
	}
}

func hash64(h uint64) uint64 {
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}

// TinyLFU provides lightweight frequency tracking for hotkey cache.
// Solves high eviction latency bottleneck using Count-Min Sketch.
type TinyLFU struct {
	filter *CountMinSketch
	hits   atomic.Uint64
	misses atomic.Uint64
}

// NewTinyLFU creates a new TinyLFU for hotkey cache.
// Lightweight: 512 width, 4 depth - minimal memory overhead (~16KB)
func NewTinyLFU() *TinyLFU {
	return &TinyLFU{
		filter: NewCountMinSketch(512, 4),
	}
}

// RecordAccess records an access for frequency tracking.
func (tf *TinyLFU) RecordAccess(key string, hit bool) {
	if hit {
		tf.filter.Increment(key)
		tf.hits.Add(1)
	} else {
		tf.misses.Add(1)
	}
}

// Reset halves all counters periodically to age old entries.
func (tf *TinyLFU) Reset() {
	tf.filter.Reset()
}

// HitRate returns current hit rate.
func (tf *TinyLFU) HitRate() float64 {
	hits := tf.hits.Load()
	misses := tf.misses.Load()
	total := hits + misses
	if total == 0 {
		return 0
	}
	return float64(hits) / float64(total)
}

// Estimate returns estimated frequency for a key.
func (tf *TinyLFU) Estimate(key string) uint64 {
	return tf.filter.Estimate(key)
}
