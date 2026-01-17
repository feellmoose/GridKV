package bufferpool

import (
	"sync"
	"sync/atomic"
)

// TieredBufferPool implements a tiered buffer pool strategy based on industry best practices
// Uses multiple pools for different size ranges to optimize memory allocation and reuse
type TieredBufferPool struct {
	// Tier pools: small, medium, large, extra-large
	tier1 *sync.Pool // 64-256 bytes
	tier2 *sync.Pool // 256-1KB
	tier3 *sync.Pool // 1KB-4KB
	tier4 *sync.Pool // 4KB-16KB
	tier5 *sync.Pool // 16KB+

	// Statistics for adaptive sizing
	stats struct {
		tier1Hits uint64
		tier2Hits uint64
		tier3Hits uint64
		tier4Hits uint64
		tier5Hits uint64
		misses    uint64 // Direct allocations when pool doesn't fit
		total     uint64
	}
}

// NewTieredBufferPool creates a new tiered buffer pool
func NewTieredBufferPool() *TieredBufferPool {
	return &TieredBufferPool{
		tier1: &sync.Pool{
			New: func() interface{} {
				return make([]byte, 0, 256)
			},
		},
		tier2: &sync.Pool{
			New: func() interface{} {
				return make([]byte, 0, 1024)
			},
		},
		tier3: &sync.Pool{
			New: func() interface{} {
				return make([]byte, 0, 4096)
			},
		},
		tier4: &sync.Pool{
			New: func() interface{} {
				return make([]byte, 0, 16384)
			},
		},
		tier5: &sync.Pool{
			New: func() interface{} {
				return make([]byte, 0, 65536) // 64KB
			},
		},
	}
}

// Get returns a buffer from the appropriate tier
// Strategy: Use pool for sizes > 256 bytes (Go's allocator is optimized for small objects < 256B)
// For very small buffers, direct allocation is often faster due to Go's size class optimization
func (p *TieredBufferPool) Get(size int) []byte {
	atomic.AddUint64(&p.stats.total, 1)

	// Very small buffers (< 256B): direct allocation is faster
	// Go's allocator has optimized size classes for small objects
	if size <= 256 {
		atomic.AddUint64(&p.stats.misses, 1)
		return make([]byte, 0, size)
	}

	// Select appropriate tier based on size
	var buf []byte
	var fromPool *sync.Pool
	var tierNum uint64

	switch {
	case size <= 1024:
		// Tier 1: 256B - 1KB
		fromPool = p.tier1
		tierNum = 1
	case size <= 4096:
		// Tier 2: 1KB - 4KB
		fromPool = p.tier2
		tierNum = 2
	case size <= 16384:
		// Tier 3: 4KB - 16KB
		fromPool = p.tier3
		tierNum = 3
	case size <= 65536:
		// Tier 4: 16KB - 64KB
		fromPool = p.tier4
		tierNum = 4
	default:
		// Tier 5: > 64KB
		fromPool = p.tier5
		tierNum = 5
	}

	poolBuf := fromPool.Get().([]byte)
	if cap(poolBuf) >= size {
		// Pool buffer is large enough
		buf = poolBuf[:size]
		switch tierNum {
		case 1:
			atomic.AddUint64(&p.stats.tier1Hits, 1)
		case 2:
			atomic.AddUint64(&p.stats.tier2Hits, 1)
		case 3:
			atomic.AddUint64(&p.stats.tier3Hits, 1)
		case 4:
			atomic.AddUint64(&p.stats.tier4Hits, 1)
		case 5:
			atomic.AddUint64(&p.stats.tier5Hits, 1)
		}
	} else {
		// Pool buffer too small, allocate directly and return pool buffer
		buf = make([]byte, 0, size)
		fromPool.Put(poolBuf[:0])
		atomic.AddUint64(&p.stats.misses, 1)
	}

	return buf
}

// Put returns a buffer to the appropriate tier pool
// Only buffers from pools should be returned here
func (p *TieredBufferPool) Put(buf []byte) {
	if buf == nil {
		return
	}

	cap := cap(buf)
	if cap == 0 {
		return
	}

	// Reset length but keep capacity
	reset := buf[:0]

	// Return to appropriate tier based on capacity
	switch {
	case cap <= 256:
		// Skip very small buffers - they're allocated directly
		return
	case cap <= 1024:
		p.tier1.Put(reset)
	case cap <= 4096:
		p.tier2.Put(reset)
	case cap <= 16384:
		p.tier3.Put(reset)
	case cap <= 65536:
		p.tier4.Put(reset)
	default:
		p.tier5.Put(reset)
	}
}

// GetAndPut is a convenience method that gets a buffer, executes a function,
// and returns the buffer to the pool automatically
// This pattern ensures buffers are always returned to the pool
func (p *TieredBufferPool) GetAndPut(size int, fn func([]byte) error) error {
	buf := p.Get(size)
	defer p.Put(buf)
	return fn(buf)
}


// Stats returns pool statistics
type PoolStats struct {
	Tier1Hits uint64
	Tier2Hits uint64
	Tier3Hits uint64
	Tier4Hits uint64
	Tier5Hits uint64
	Misses    uint64
	Total     uint64
}

// Stats returns current pool statistics
func (p *TieredBufferPool) Stats() PoolStats {
	return PoolStats{
		Tier1Hits: atomic.LoadUint64(&p.stats.tier1Hits),
		Tier2Hits: atomic.LoadUint64(&p.stats.tier2Hits),
		Tier3Hits: atomic.LoadUint64(&p.stats.tier3Hits),
		Tier4Hits: atomic.LoadUint64(&p.stats.tier4Hits),
		Tier5Hits: atomic.LoadUint64(&p.stats.tier5Hits),
		Misses:    atomic.LoadUint64(&p.stats.misses),
		Total:     atomic.LoadUint64(&p.stats.total),
	}
}

// HitRate returns the pool hit rate (0.0 to 1.0)
func (p *TieredBufferPool) HitRate() float64 {
	stats := p.Stats()
	if stats.Total == 0 {
		return 0.0
	}
	hits := stats.Tier1Hits + stats.Tier2Hits + stats.Tier3Hits + stats.Tier4Hits + stats.Tier5Hits
	return float64(hits) / float64(stats.Total)
}
