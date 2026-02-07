package bufferpool

import (
	"sync"
	"sync/atomic"
)

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
				buf := make([]byte, 0, 256)
				return &buf
			},
		},
		tier2: &sync.Pool{
			New: func() interface{} {
				buf := make([]byte, 0, 1024)
				return &buf
			},
		},
		tier3: &sync.Pool{
			New: func() interface{} {
				buf := make([]byte, 0, 4096)
				return &buf
			},
		},
		tier4: &sync.Pool{
			New: func() interface{} {
				buf := make([]byte, 0, 16384)
				return &buf
			},
		},
		tier5: &sync.Pool{
			New: func() interface{} {
				buf := make([]byte, 0, 65536) // 64KB
				return &buf
			},
		},
	}
}

// Get returns a buffer from the appropriate tier
func (p *TieredBufferPool) Get(size int) []byte {
	atomic.AddUint64(&p.stats.total, 1)

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

	poolBufPtr := fromPool.Get().(*[]byte)
	poolBuf := *poolBufPtr
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
		// Cap allocation to prevent excessive memory usage
		const maxAlloc = 2 * 1024 * 1024 // 2MB max
		allocSize := size
		if allocSize > maxAlloc {
			allocSize = maxAlloc
		}
		buf = make([]byte, 0, allocSize)
		fromPool.Put(poolBufPtr)
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

	// Don't pool very large buffers to prevent memory bloat
	const maxPoolSize = 65536 // 64KB max for pooling
	if cap > maxPoolSize {
		return
	}

	// Return to appropriate tier based on capacity
	// Reset the slice and store pointer to avoid SA6002
	resetBuf := buf[:0]
	bufPtr := &resetBuf

	// Return to appropriate tier based on capacity
	switch {
	case cap <= 256:
		return
	case cap <= 1024:
		p.tier1.Put(bufPtr)
	case cap <= 4096:
		p.tier2.Put(bufPtr)
	case cap <= 16384:
		p.tier3.Put(bufPtr)
	case cap <= 65536:
		p.tier4.Put(bufPtr)
	default:
		// Should not reach here due to maxPoolSize check above
		return
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
