package compress

import (
	"sync"

	"github.com/pierrec/lz4/v4"
)

// Pool provides buffer pooling for LZ4 compression operations.
type Pool struct {
	compressPools   [3]*sync.Pool
	decompressPools [3]*sync.Pool
}

// NewPool creates a new compression buffer pool.
func NewPool() *Pool {
	return &Pool{
		compressPools: [3]*sync.Pool{
			{New: func() interface{} { buf := make([]byte, 0, 256*1024); return &buf }},
			{New: func() interface{} { buf := make([]byte, 0, 1024*1024); return &buf }},
			{New: func() interface{} { buf := make([]byte, 0, 4*1024*1024); return &buf }},
		},
		decompressPools: [3]*sync.Pool{
			{New: func() interface{} { buf := make([]byte, 0, 256*1024); return &buf }},
			{New: func() interface{} { buf := make([]byte, 0, 512*1024); return &buf }},
			{New: func() interface{} { buf := make([]byte, 0, 2*1024*1024); return &buf }},
		},
	}
}

// GetCompress returns a buffer for compression.
func (p *Pool) GetCompress(size int) []byte {
	boundSize := lz4.CompressBlockBound(size)
	return p.getFromTier(p.compressPools, boundSize)
}

// PutCompress returns a buffer to compression pool.
func (p *Pool) PutCompress(buf []byte) {
	p.putToTier(p.compressPools, buf)
}

// GetDecompress returns a buffer for decompression.
func (p *Pool) GetDecompress(size int) []byte {
	return p.getFromTier(p.decompressPools, size+64) // 64 byte margin
}

// PutDecompress returns a buffer to decompression pool.
func (p *Pool) PutDecompress(buf []byte) {
	p.putToTier(p.decompressPools, buf)
}

func (p *Pool) getFromTier(pools [3]*sync.Pool, size int) []byte {
	idx := 0
	if size > 256*1024 {
		if size > 512*1024 {
			idx = 2
		} else {
			idx = 1
		}
	}
	bufPtr := pools[idx].Get().(*[]byte)
	buf := (*bufPtr)[:0]
	if cap(buf) < size {
		pools[idx].Put(bufPtr)
		// Cap allocation to prevent excessive memory usage
		const maxAlloc = 8 * 1024 * 1024 // 8MB max
		if size > maxAlloc {
			size = maxAlloc
		}
		return make([]byte, size)
	}
	return buf[:size]
}

func (p *Pool) putToTier(pools [3]*sync.Pool, buf []byte) {
	if buf == nil {
		return
	}
	capSize := cap(buf)
	// Don't pool very large buffers to prevent memory bloat
	const maxPoolSize = 2 * 1024 * 1024 // 2MB max for pooling
	if capSize > maxPoolSize {
		return
	}
	idx := 0
	if capSize > 256*1024 {
		if capSize > 512*1024 {
			idx = 2
		} else {
			idx = 1
		}
	}
	// Reset the slice and store pointer to avoid SA6002
	resetBuf := buf[:0]
	bufPtr := &resetBuf
	pools[idx].Put(bufPtr)
}

// Global pool for shared use
var globalPool = NewPool()
