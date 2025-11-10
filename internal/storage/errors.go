package storage

// File: errors.go
// Purpose: Pre-allocated error objects for zero-allocation error handling
//
// This file contains all common errors used in storage operations.
// Pre-allocating errors avoids 1-2 allocations per operation, improving performance.

import "errors"

// Pre-allocated error objects to avoid allocations in hot path.
// This optimization reduces allocation count by 1-2 per operation.
var (
	// Common errors
	ErrItemNotFound        = errors.New("item not found")
	ErrVersionMismatch     = errors.New("version mismatch")
	ErrItemExpired         = errors.New("item has expired")
	ErrMemoryLimitExceeded = errors.New("memory limit exceeded")

	// Validation errors
	errEmptyKey        = errors.New("empty key not allowed")
	errNilItem         = errors.New("nil item not allowed")
	errBackendNotFound = errors.New("storage backend not found")
)
