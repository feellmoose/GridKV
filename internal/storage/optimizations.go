package storage

// File: optimizations.go
// Purpose: Performance optimization utilities
//
// This file provides various optimization tools:
//   - ValueBufferPool: Size-specific buffer pooling
//   - HotKeyCache: Local hot key caching with TTL
//   - GC optimization utilities
//   - System configuration recommendations
//
// These tools help reduce GC pressure and improve overall performance.

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
	"time"
)

// ValueBufferPool provides pooled buffers for value storage to reduce GC pressure.
// This implements the optimization recommendation from the performance roadmap:
// "Use sync.Pool to cache value buffers and reduce GC frequency"
type ValueBufferPool struct {
	pools map[int]*sync.Pool // Size-specific pools
	sizes []int              // Common buffer sizes
}

// NewValueBufferPool creates a new value buffer pool with predefined size categories.
// Common sizes are: 256B, 1KB, 4KB, 16KB, 64KB for optimal memory reuse.
func NewValueBufferPool() *ValueBufferPool {
	sizes := []int{256, 1024, 4096, 16384, 65536}
	pools := make(map[int]*sync.Pool)

	for _, size := range sizes {
		s := size // Capture for closure
		pools[size] = &sync.Pool{
			New: func() interface{} {
				buf := make([]byte, s)
				return &buf
			},
		}
	}

	return &ValueBufferPool{
		pools: pools,
		sizes: sizes,
	}
}

// Get retrieves a buffer of at least the requested size.
// Returns nil if size exceeds the largest pool size.
func (p *ValueBufferPool) Get(size int) *[]byte {
	for _, poolSize := range p.sizes {
		if size <= poolSize {
			return p.pools[poolSize].Get().(*[]byte)
		}
	}
	return nil // Size too large, caller should allocate directly
}

// Put returns a buffer to the appropriate pool.
func (p *ValueBufferPool) Put(buf *[]byte) {
	if buf == nil {
		return
	}

	size := cap(*buf)
	for _, poolSize := range p.sizes {
		if size == poolSize {
			// Reset buffer before returning to pool
			*buf = (*buf)[:0]
			p.pools[poolSize].Put(buf)
			return
		}
	}
	// Buffer doesn't match any pool size, let GC handle it
}

// Global value buffer pool instance
var globalValueBufferPool = NewValueBufferPool()

// GetValueBuffer retrieves a buffer from the global pool.
func GetValueBuffer(size int) *[]byte {
	return globalValueBufferPool.Get(size)
}

// PutValueBuffer returns a buffer to the global pool.
func PutValueBuffer(buf *[]byte) {
	globalValueBufferPool.Put(buf)
}

// OptimizationConfig provides recommended configuration based on system resources.
// This implements the optimization roadmap recommendations.
type OptimizationConfig struct {
	// Memory settings (70% of available RAM)
	MaxMemoryMB int64

	// Shard count (CPU cores * 2-4x)
	ShardCount int

	// Connection pool settings
	MaxConnections int
	IdleTimeout    int // seconds

	// GC tuning
	GOGCPercent int
}

// GetRecommendedConfig returns optimized configuration based on system resources.
// Implements the performance roadmap:
// - MaxMemoryMB: 70% of physical memory
// - ShardCount: CPU cores * 2-4x (default 4x)
// - MaxConnections: CPU cores * 2
func GetRecommendedConfig() *OptimizationConfig {
	numCPU := runtime.NumCPU()

	// Get system memory
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	// Calculate 70% of available memory (in MB)
	// Note: m.Sys is an approximation, in production use syscall or gopsutil
	maxMemoryMB := int64(float64(m.Sys) * 0.7 / 1024 / 1024)
	if maxMemoryMB < 256 {
		maxMemoryMB = 256 // Minimum 256MB
	}

	return &OptimizationConfig{
		MaxMemoryMB:    maxMemoryMB,
		ShardCount:     numCPU * 4, // 4x multiplier for high concurrency
		MaxConnections: numCPU * 2,
		IdleTimeout:    30,  // 30 seconds
		GOGCPercent:    200, // Reduce GC frequency
	}
}

// ApplyGCOptimizations applies recommended GC settings for production use.
// From roadmap: "Set GOGC=200 to reduce GC frequency"
func ApplyGCOptimizations() int {
	previousGOGC := debug.SetGCPercent(200)
	return previousGOGC
}

// HotKeyStrategy defines strategies for handling hot keys.
type HotKeyStrategy int

const (
	// HotKeyStrategyCache caches hot keys locally with TTL
	HotKeyStrategyCache HotKeyStrategy = iota

	// HotKeyStrategyShard splits hot keys into multiple sub-keys
	HotKeyStrategyShard
)

// HotKeyCache provides local caching for frequently accessed keys.
// From roadmap: "Cache hot keys locally with TTL 100ms"
type HotKeyCache struct {
	cache   *sync.Map
	ttl     int64 // nanoseconds
	enabled bool
}

// CacheEntry represents a cached key-value pair with expiration.
type CacheEntry struct {
	Value    []byte
	ExpireAt int64 // Unix nanoseconds
}

// NewHotKeyCache creates a new hot key cache with the specified TTL (in milliseconds).
// Recommended TTL: 100ms for high-frequency keys.
func NewHotKeyCache(ttlMs int) *HotKeyCache {
	return &HotKeyCache{
		cache:   &sync.Map{},
		ttl:     int64(ttlMs) * 1000000, // Convert ms to ns
		enabled: true,
	}
}

// Get retrieves a value from the cache if it exists and hasn't expired.
func (c *HotKeyCache) Get(key string) ([]byte, bool) {
	if !c.enabled {
		return nil, false
	}

	val, ok := c.cache.Load(key)
	if !ok {
		return nil, false
	}

	entry := val.(*CacheEntry)
	now := time.Now().UnixNano()

	if now > entry.ExpireAt {
		c.cache.Delete(key)
		return nil, false
	}

	return entry.Value, true
}

// Set stores a value in the cache with the configured TTL.
func (c *HotKeyCache) Set(key string, value []byte) {
	if !c.enabled {
		return
	}

	entry := &CacheEntry{
		Value:    append([]byte(nil), value...), // Deep copy
		ExpireAt: time.Now().UnixNano() + c.ttl,
	}

	c.cache.Store(key, entry)
}

// Clear removes all entries from the cache.
func (c *HotKeyCache) Clear() {
	c.cache = &sync.Map{}
}

// Enable enables the cache.
func (c *HotKeyCache) Enable() {
	c.enabled = true
}

// Disable disables the cache.
func (c *HotKeyCache) Disable() {
	c.enabled = false
}

// GetShardedKey generates a sharded key name for hot key distribution.
// From roadmap: "Split hot keys into multiple sub-keys with hash suffix"
// Example: "user:1000" becomes "user:1000:shard:3"
func GetShardedKey(originalKey string, shardID, shardCount int) string {
	if shardCount <= 1 {
		return originalKey
	}
	shardID = shardID % shardCount
	return fmt.Sprintf("%s:shard:%d", originalKey, shardID)
}
