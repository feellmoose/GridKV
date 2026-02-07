package compress

import (
	"github.com/pierrec/lz4/v4"
)

// CompressBound returns the maximum size of compressed data.
func CompressBound(size int) int {
	return lz4.CompressBlockBound(size)
}

// Compress compresses data using LZ4 if beneficial. Returns (compressed, true) or (original, false).
func Compress(data []byte, threshold int) ([]byte, bool) {
	if len(data) < threshold {
		return data, false
	}
	buf := globalPool.GetCompress(len(data))
	defer globalPool.PutCompress(buf)
	n, err := lz4.CompressBlock(data, buf, nil)
	if err != nil || n == 0 {
		return data, false
	}
	if n < len(data)*95/100 {
		out := make([]byte, n)
		copy(out, buf[:n])
		return out, true
	}
	return data, false
}

// CompressTo compresses into dst if cap(dst) >= CompressBound; else uses pool. Returns (data, n, true) or (original, 0, false).
func CompressTo(data, dst []byte, threshold int) ([]byte, int, bool) {
	if len(data) < threshold {
		return data, 0, false
	}
	bound := lz4.CompressBlockBound(len(data))
	useDst := cap(dst) >= bound
	var buf []byte
	if useDst {
		buf = dst[:bound]
	} else {
		buf = globalPool.GetCompress(len(data))
		defer globalPool.PutCompress(buf)
	}
	n, err := lz4.CompressBlock(data, buf, nil)
	if err != nil || n == 0 {
		return data, 0, false
	}
	if n < len(data)*95/100 {
		if useDst {
			return buf[:n], n, true
		}
		out := make([]byte, n)
		copy(out, buf[:n])
		return out, n, true
	}
	return data, 0, false
}

func normEstimate(data []byte, estimatedSize int) int {
	if estimatedSize <= 0 {
		estimatedSize = len(data) * 2
	}
	if estimatedSize < len(data)*2 {
		estimatedSize = len(data) * 2
	}
	// Cap at reasonable maximum to avoid excessive allocations
	const maxEstimate = 64 * 1024 * 1024 // 64MB
	if estimatedSize > maxEstimate {
		estimatedSize = maxEstimate
	}
	return estimatedSize
}

// Decompress decompresses using LZ4. Allocates result directly.
func Decompress(data []byte, estimatedSize int) ([]byte, error) {
	est := normEstimate(data, estimatedSize)
	result := make([]byte, est)
	n, err := lz4.UncompressBlock(data, result)
	if err != nil {
		// Retry with larger buffer. Use DecompressTo to avoid double allocation.
		// First buffer (result) will be GC'd automatically.
		out, _, err := DecompressTo(data, nil, estimatedSize*4)
		return out, err
	}
	// Success: copy to new buffer to avoid returning slice of local variable
	out := make([]byte, n)
	copy(out, result[:n])
	return out, nil
}

// DecompressTo decompresses into dst when cap(dst) >= estimatedSize; else uses pool. Never returns pool memory.
func DecompressTo(data, dst []byte, estimatedSize int) ([]byte, int, error) {
	est := normEstimate(data, estimatedSize)
	useDst := cap(dst) >= est
	var buf []byte
	if useDst {
		buf = dst[:est]
	} else {
		buf = globalPool.GetDecompress(est)
	}

	n, err := lz4.UncompressBlock(data, buf)
	if err != nil {
		if !useDst {
			globalPool.PutDecompress(buf)
		}
		buf = globalPool.GetDecompress(est * 4)
		n, err = lz4.UncompressBlock(data, buf)
		if err != nil {
			globalPool.PutDecompress(buf)
			return nil, 0, err
		}
		useDst = false
	}

	out := make([]byte, n)
	copy(out, buf[:n])
	if !useDst {
		globalPool.PutDecompress(buf)
	}
	return out, n, nil
}

// CompressThreshold returns recommended compression threshold.
func CompressThreshold() int { return 64 }
