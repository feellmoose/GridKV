package compress

import (
	"bytes"
	"testing"
)

func TestCompress(t *testing.T) {
	small := make([]byte, 32)
	result, ok := Compress(small, 64)
	if ok {
		t.Error("small data should not be compressed")
	}
	if len(result) != len(small) {
		t.Errorf("result length = %d, want %d", len(result), len(small))
	}

	pattern := []byte("key123value456data789")
	large := make([]byte, 1024)
	for i := range large {
		large[i] = pattern[i%len(pattern)]
	}
	result, ok = Compress(large, 64)
	if ok {
		if len(result) >= len(large) {
			t.Errorf("compressed size = %d, should be < %d", len(result), len(large))
		}
	} else {
		if len(result) != len(large) {
			t.Errorf("uncompressed result length = %d, want %d", len(result), len(large))
		}
	}
}

func TestCompressTo(t *testing.T) {
	pattern := []byte("key123value456data789")
	data := make([]byte, 1024)
	for i := range data {
		data[i] = pattern[i%len(pattern)]
	}

	// Test with nil dst (should allocate)
	result, n, ok := CompressTo(data, nil, 64)
	if !ok {
		t.Fatal("compression should succeed")
	}
	if n <= 0 || n >= len(data) {
		t.Errorf("compressed size = %d, expected 0 < n < %d", n, len(data))
	}
	if len(result) != n {
		t.Errorf("result length = %d, want %d", len(result), n)
	}

	// Test with sufficient dst capacity
	dst := make([]byte, 0, CompressBound(len(data)))
	result2, n2, ok2 := CompressTo(data, dst, 64)
	if !ok2 {
		t.Fatal("compression should succeed")
	}
	if n2 != n {
		t.Errorf("compressed size = %d, want %d", n2, n)
	}
	if len(result2) != n2 {
		t.Errorf("result length = %d, want %d", len(result2), n2)
	}
}

func TestDecompress(t *testing.T) {
	pattern := []byte("key123value456data789")
	original := make([]byte, 1024)
	for i := range original {
		original[i] = pattern[i%len(pattern)]
	}

	compressed, _, ok := CompressTo(original, nil, 64)
	if !ok {
		t.Fatal("compression should succeed")
	}

	decompressed, err := Decompress(compressed, len(original)*2)
	if err != nil {
		t.Fatalf("Decompress() error = %v", err)
	}

	if len(decompressed) != len(original) {
		t.Errorf("decompressed length = %d, want %d", len(decompressed), len(original))
	}
	if !bytes.Equal(decompressed, original) {
		t.Error("decompressed data does not match original")
	}
}

func TestDecompressTo(t *testing.T) {
	pattern := []byte("key123value456data789")
	original := make([]byte, 1024)
	for i := range original {
		original[i] = pattern[i%len(pattern)]
	}

	compressed, _, ok := CompressTo(original, nil, 64)
	if !ok {
		t.Fatal("compression should succeed")
	}

	// Test with nil dst
	result, n, err := DecompressTo(compressed, nil, len(original)*2)
	if err != nil {
		t.Fatalf("DecompressTo() error = %v", err)
	}
	if n != len(original) {
		t.Errorf("decompressed size = %d, want %d", n, len(original))
	}
	if len(result) != n {
		t.Errorf("result length = %d, want %d", len(result), n)
	}
	if !bytes.Equal(result, original) {
		t.Error("decompressed data does not match original")
	}

	// Test with sufficient dst capacity
	dst := make([]byte, 0, len(original)*2)
	result2, n2, err2 := DecompressTo(compressed, dst, len(original)*2)
	if err2 != nil {
		t.Fatalf("DecompressTo() error = %v", err2)
	}
	if n2 != len(original) {
		t.Errorf("decompressed size = %d, want %d", n2, len(original))
	}
	if len(result2) != n2 {
		t.Errorf("result length = %d, want %d", len(result2), n2)
	}
	if !bytes.Equal(result2, original) {
		t.Error("decompressed data does not match original")
	}
}

func TestCompressBound(t *testing.T) {
	sizes := []int{64, 256, 1024, 4096, 65536}
	for _, size := range sizes {
		bound := CompressBound(size)
		if bound <= 0 {
			t.Errorf("CompressBound(%d) = %d, expected > 0", size, bound)
		}
		if bound < size {
			t.Errorf("CompressBound(%d) = %d, expected >= %d", size, bound, size)
		}
	}
}

func TestCompressThreshold(t *testing.T) {
	threshold := CompressThreshold()
	if threshold <= 0 {
		t.Error("threshold should be positive")
	}
	if threshold != 64 {
		t.Errorf("threshold = %d, want 64", threshold)
	}
}

func BenchmarkCompress(b *testing.B) {
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i % 256)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = Compress(data, 64)
	}
}

func BenchmarkDecompress(b *testing.B) {
	original := make([]byte, 4096)
	for i := range original {
		original[i] = byte(i % 256)
	}
	compressed, _, _ := CompressTo(original, nil, 64)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = Decompress(compressed, len(original)*2)
	}
}
