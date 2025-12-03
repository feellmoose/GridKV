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

// FastCloneBytes creates a new byte slice with the same content.
// Optimized to minimize allocations and improve copy performance.
//
//go:inline
func FastCloneBytes(src []byte) []byte {
	if src == nil {
		return nil
	}
	if len(src) == 0 {
		return []byte{}
	}
	// Pre-allocate with exact capacity to avoid reallocation
	dst := make([]byte, len(src))
	copy(dst, src)
	return dst
}

