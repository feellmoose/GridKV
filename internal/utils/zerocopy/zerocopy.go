package zerocopy

// Package zerocopy provides zero-allocation utilities for performance-critical operations.
//
// WARNING: These functions use unsafe pointer operations.
// Use only in performance-critical code paths where allocations matter.
//
// Safety rules:
//   - StringToBytes: Returned []byte shares memory with string. DO NOT MODIFY.
//   - BytesToString: String shares memory with []byte. DO NOT MODIFY []byte after conversion.
//   - FastCloneBytes: Safe for all use cases.

import (
	"unsafe"
)

// StringToBytes converts string to []byte without allocation.
//
// WARNING: The returned []byte shares memory with the string.
// Do not modify the returned slice. Safe for read-only operations only.
//
// Performance: Zero allocation vs 1 allocation for []byte(str)
//
// Example:
//
//	s := "hello"
//	b := StringToBytes(s)
//	// b is read-only, do not modify
//
//go:inline
func StringToBytes(s string) []byte {
	if s == "" {
		return nil
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

// BytesToString converts []byte to string without allocation.
//
// WARNING: The string shares memory with the []byte.
// Do not modify the original []byte after conversion.
//
// Performance: Zero allocation vs 1 allocation for string(b)
//
// Example:
//
//	b := []byte("hello")
//	s := BytesToString(b)
//	// Do not modify b after this point
//
//go:inline
func BytesToString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(b), len(b))
}

// FastCloneBytes creates a new byte slice with the same content.
//
// This is safe for all use cases as it creates an independent copy.
//
// Example:
//
//	src := []byte("hello")
//	dst := FastCloneBytes(src)
//	// dst is independent, safe to modify
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
