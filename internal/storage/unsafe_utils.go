package storage

// File: unsafe_utils.go
// Purpose: Unsafe utilities for performance-critical operations
//
// This file provides unsafe utility functions that eliminate allocations
// in hot paths. Use with caution.
//
// Performance benefits:
//   - StringToBytes: Zero allocation conversion
//   - BytesToString: Zero allocation conversion
//   - FastCloneBytes: Fast copy

import (
	"unsafe"
)

// Unsafe utilities for hot paths using pointer operations to eliminate allocations.
// Use with caution and only in performance-critical code.
//
// Performance gains:
// - StringToBytes: Zero allocation (vs 1 allocation)
// - BytesToString: Zero allocation (vs 1 allocation)

// StringToBytes converts string to []byte without allocation.
// ⚠️ WARNING: The returned []byte shares memory with the string.
// Do not modify the returned slice. This is safe for read-only operations.
//
// Performance: Zero allocation vs 1 allocation for []byte(str)
//
//go:inline
func StringToBytes(s string) []byte {
	if s == "" {
		return nil
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

// BytesToString converts []byte to string without allocation.
// ⚠️ WARNING: The string shares memory with the []byte.
// Do not modify the original []byte after conversion.
//
// Performance: Zero allocation vs 1 allocation for string(b)
//
//go:inline
func BytesToString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(b), len(b))
}

// CopyBytes performs a fast byte copy using unsafe pointers.
// This is equivalent to copy() but may be faster for small sizes.
//
//go:inline
func CopyBytes(dst, src []byte) int {
	n := len(src)
	if len(dst) < n {
		n = len(dst)
	}
	if n == 0 {
		return 0
	}

	// For small copies, this can be faster than copy()
	if n <= 32 {
		for i := 0; i < n; i++ {
			dst[i] = src[i]
		}
		return n
	}

	// For larger copies, use built-in copy
	return copy(dst, src)
}

// FastCloneBytes creates a new byte slice with the same content.
//
//go:inline
func FastCloneBytes(src []byte) []byte {
	if src == nil {
		return nil
	}
	if len(src) == 0 {
		return []byte{}
	}

	dst := make([]byte, len(src))
	copy(dst, src)
	return dst
}

// GetStringHeader returns the underlying string data pointer for advanced operations.
// This should only be used by experts who understand the implications.
// Deprecated: Use unsafe.StringData() directly instead.
func GetStringHeader(s string) unsafe.Pointer {
	return unsafe.Pointer(unsafe.StringData(s))
}

// GetSliceHeader returns the underlying slice data pointer for advanced operations.
// This should only be used by experts who understand the implications.
// Deprecated: Use unsafe.SliceData() directly instead.
func GetSliceHeader(s []byte) unsafe.Pointer {
	return unsafe.Pointer(unsafe.SliceData(s))
}
