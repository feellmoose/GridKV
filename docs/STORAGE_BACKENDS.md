# Storage Backends

**Version**: v3.1  
**Available Backends**: Memory, MemorySharded (default)

---

## Overview

GridKV provides two in-memory storage backends optimized for different concurrency levels. Both backends implement the `storage.Storage` interface and are thread-safe.

---

## Backend Comparison

| Backend | Throughput | Latency | Concurrency | Memory Overhead | Use Case |
|---------|-----------|---------|-------------|-----------------|----------|
| **MemorySharded** | 1-2M+ ops/s | <1 µs | Excellent (sharded locks) | Medium | Production (recommended) |
| **Memory** | 600-700K ops/s | ~1.5 µs | Good (sync.Map) | Low | Development/Testing |

---

## 1. MemorySharded (Recommended)

**Implementation**: `internal/storage/memory_sharded.go`

### Architecture

```
ShardedMemoryStorage
├── Shard 0 (RWMutex + map)
├── Shard 1 (RWMutex + map)
├── ...
└── Shard N (RWMutex + map)

Key → xxHash(key) % N → Shard
```

**Sharding Strategy**:
- xxHash (XXH64) for fast, uniform key distribution (10 GB/s throughput)
- Each shard has independent RWMutex lock
- Reduces lock contention by sharding factor
- Power-of-2 shard count for fast modulo (bitwise AND)

### Characteristics

- ✅ **Highest throughput**: 1-2M+ ops/s
- ✅ **Excellent concurrency**: Lock contention reduced by N (shard count)
- ✅ **Zero dependencies**: Pure Go implementation
- ✅ **Production-ready**: Recommended for all production deployments
- ✅ **Configurable sharding**: 32-256 shards (default 256)

### Concurrency Model

```go
Lock Strategy:
  Read operations:  RLock on target shard (multiple concurrent readers)
  Write operations: Lock on target shard (exclusive per shard)
  
Concurrency Improvement:
  256 shards: ~256x less contention vs single lock
  64 shards:  ~64x less contention vs single lock
  32 shards:  ~32x less contention vs single lock
  
Throughput Scaling:
  1 thread:   700K ops/s
  8 threads:  5M ops/s    (7.1x scaling)
  16 threads: 8M ops/s    (11.4x scaling)
  32 threads: 10M+ ops/s  (14x+ scaling)
```

### Performance

```
Benchmark (Intel i7-12700H, 20 cores):
  Get:    <1 µs      1-2M ops/s    minimal allocations
  Set:    <2 µs      800K-1.5M/s   minimal allocations
  Delete: <2 µs      800K-1.5M/s   minimal allocations
  
Concurrency:
  Single-threaded: 700K ops/s
  Multi-threaded:  10M+ ops/s (scales linearly)
```

### Configuration

```go
Storage: &storage.StorageOptions{
    Backend:     storage.BackendMemorySharded,
    ShardCount:  256,     // Default: 256 (excellent for high concurrency)
    MaxMemoryMB: 2048,    // Soft limit (not enforced)
}
```

**Tuning Guidelines**:
```
Shard Count Selection:
  Development:  32 shards   (simple, good enough)
  Production:   64-256      (optimal for most workloads)
  High-traffic: 256         (maximum concurrency)
  
Memory Overhead per Shard: ~200 bytes
Total Overhead: ShardCount × 200 bytes (~50 KB for 256 shards)

Recommendation: Use 256 shards (default) for production
```

### Use Cases

- ✅ Production deployments (recommended)
- ✅ High-concurrency workloads (>100K ops/s)
- ✅ Multi-core servers (4+ cores)
- ✅ Cache layer for web applications
- ✅ Session storage
- ✅ Distributed caching

---

## 2. Memory (Simple)

**Implementation**: `internal/storage/memory.go`

### Architecture

```
MemoryStorage
└── Single sync.Map
    ├── Lock-free reads (common case)
    └── Synchronized writes
```

### Characteristics

- ✅ **Simple**: Single `sync.Map`, minimal code
- ✅ **Good performance**: 600-700K ops/s
- ✅ **Read-optimized**: Lock-free reads in common case
- ✅ **Zero dependencies**: Pure Go
- ⚠️ **Lower concurrency**: Single lock domain limits scaling

### Concurrency Model

```go
sync.Map Features:
  - Lock-free reads for keys written before read
  - Optimized for read-heavy workloads
  - Amortized O(1) operations
  - Two-map strategy (read map + dirty map)
  
Limitations:
  - Write contention higher than sharded
  - Scales poorly beyond 8-16 threads
  - Not optimized for write-heavy workloads
```

### Performance

```
Benchmark (Intel i7-12700H, 20 cores):
  Get:    ~1.5 µs    600-700K ops/s
  Set:    ~2 µs      500-600K ops/s
  Delete: ~2 µs      500-600K ops/s
  
Concurrency Scaling:
  1 thread:   600K ops/s
  8 threads:  2M ops/s    (3.3x scaling - sublinear)
  16 threads: 3M ops/s    (5x scaling - diminishing)
  32 threads: 3.5M ops/s  (5.8x scaling - saturated)
```

### Configuration

```go
Storage: &storage.StorageOptions{
    Backend:     storage.BackendMemory,
    MaxMemoryMB: 1024,
}
```

### Use Cases

- Development and testing
- Simple deployments (<4 CPU cores)
- Read-heavy workloads (90%+ reads)
- Low to medium traffic (<100K ops/s)
- Single-node applications

---

## Selection Guide

### Quick Decision Matrix

| Your Scenario | Recommended Backend |
|---------------|---------------------|
| Production deployment | **MemorySharded** (256 shards) |
| High concurrency (>100K ops/s) | **MemorySharded** |
| Multi-core server (4+ cores) | **MemorySharded** |
| Development/Testing | **Memory** |
| Single-core or low traffic | **Memory** |

### Detailed Comparison

**Choose MemorySharded** if:
- ✅ Production deployment (recommended)
- ✅ High throughput required (>100K ops/s)
- ✅ Multi-core server (4+ cores)
- ✅ Write-heavy or mixed workload
- ✅ Want maximum performance

**Choose Memory** if:
- Simple deployment
- Development/testing
- Low traffic (<100K ops/s)
- Single-core or embedded systems
- Read-heavy workload (>90% reads)

---

## Storage Interface

Both backends implement the same interface:

```go
type Storage interface {
    // Basic CRUD operations
    Set(key string, item *StoredItem) error
    Get(key string) (*StoredItem, error)
    Delete(key string, version int64) error
    
    // Management
    Keys() []string
    Clear() error
    Close() error
    
    // Distributed synchronization
    GetSyncBuffer() ([]*CacheSyncOperation, error)
    GetFullSyncSnapshot() ([]*FullStateItem, error)
    ApplyIncrementalSync([]*CacheSyncOperation) error
    ApplyFullSyncSnapshot([]*FullStateItem, time.Time) error
    
    // Monitoring
    Stats() StorageStats
}
```

**Thread-safety**: All methods are safe for concurrent access from multiple goroutines.

**Deep Copy**: All methods return deep copies of data, safe for caller to modify.

---

## Synchronization Mechanisms

### Incremental Sync

**Purpose**: Replicate recent changes to peers via Gossip

**Implementation**:
```go
Lock-Free Ring Buffer (both backends):
  - Atomic operations (no locks)
  - Power-of-2 sizing for fast modulo (bitwise AND)
  - Bounded memory (8192 operations default)
  - Circular overwrite on overflow
  
Process:
  1. Each write atomically adds operation to ring buffer
  2. Periodic sync reads buffer (non-blocking, lock-free)
  3. Batch send operations to peers
  4. Peers apply with HLC version checking
  
Performance:
  Write overhead: ~2 atomic operations (~10ns)
  Sync overhead:  O(buffer_size) amortized
  Network:        Batched for efficiency
```

### Full Sync

**Purpose**: New node bootstrap or recovery from network partition

**Implementation**:
```go
Process:
  1. New node requests full snapshot from peer
  2. Peer serializes all keys (point-in-time consistent)
  3. Receiver clears local state
  4. Apply complete snapshot atomically
  5. Resume incremental sync
  
Optimization:
  - Snapshot is versioned (HLC timestamp)
  - Applied atomically (clear + bulk load)
  - Minimal lock contention during application
```

---

## Performance Tuning

### MemorySharded Tuning

```go
ShardCount: 256  // Default (recommended)

Guidelines:
  32 shards:  Good for development, <4 cores
  64 shards:  Good for production, 4-8 cores
  128 shards: Good for high-traffic, 8-16 cores
  256 shards: Optimal for maximum concurrency (16+ cores)
  
Memory Overhead: ShardCount × 200 bytes
  32 shards:  ~6 KB
  64 shards:  ~13 KB
  128 shards: ~26 KB
  256 shards: ~50 KB (negligible)
  
Recommendation: Always use 256 (overhead is tiny)
```

### Memory Tuning

```go
MaxMemoryMB: 1024  // Soft limit (not enforced)

Note:
  - No automatic eviction
  - No hard memory limit
  - Suitable for bounded datasets
  - Monitor actual memory usage with Stats()
```

### General Tips

```go
// Reduce allocations with sync.Pool
var itemPool = sync.Pool{
    New: func() interface{} {
        return &StoredItem{}
    },
}

// Pre-allocate slices for batch operations
keys := make([]string, 0, expectedCount)

// Use xxHash for custom key hashing (if needed)
import "github.com/cespare/xxhash/v2"
hash := xxhash.Sum64String(key)
```

---

## Memory Allocations

### Per-Operation Allocations

| Backend | Operation | Bytes/op | Allocs/op |
|---------|-----------|----------|-----------|
| MemorySharded | Get | ~800 | 18-20 |
| MemorySharded | Set | ~900 | 20-22 |
| Memory | Get | ~1000 | 20-23 |
| Memory | Set | ~1100 | 22-25 |

### Allocation Breakdown

```
Main Sources (both backends):
  1. Deep copy of value       (~30%)
  2. Sync buffer operations   (~25%)
  3. Key hashing             (~15%)
  4. HLC timestamp           (~10%)
  5. Map operations          (~20%)
```

---

## Monitoring

### Storage Statistics

```go
type StorageStats struct {
    KeyCount      int64   // Current number of keys
    SyncBufferLen int     // Pending sync operations
    DBSize        int64   // Estimated memory usage (bytes)
}
```

**Access**:
```go
stats := storage.Stats()
log.Printf("Keys: %d, Buffer: %d, Size: %d MB", 
    stats.KeyCount, 
    stats.SyncBufferLen, 
    stats.DBSize/(1024*1024))
```

**Recommended Monitoring**:
- **KeyCount**: Track growth rate, set alerts on anomalies
- **SyncBufferLen**: Should be <1000 typically, >5000 indicates sync lag
- **DBSize**: Estimate memory usage (may be approximate)

---

## Implementation Details

### Backend Registration

Both backends register automatically via `init()`:

```go
func init() {
    RegisterBackend(BackendMemory, NewMemoryStorage)
    RegisterBackend(BackendMemorySharded, NewShardedMemoryStorage)
}
```

**No Import Required**: All backends are built-in.

### Object Pooling

Both backends use `sync.Pool` for:
- ✅ Reduced GC pressure (~40% fewer collections)
- ✅ Memory reuse (avoids repeated allocations)
- ✅ Better cache locality

---

## Best Practices

### Production Deployment

```go
// Recommended production configuration
&storage.StorageOptions{
    Backend:     storage.BackendMemorySharded,
    ShardCount:  256,     // Maximum concurrency
    MaxMemoryMB: 2048,    // 2GB soft limit
}
```

### Development

```go
// Simple development configuration
&storage.StorageOptions{
    Backend:     storage.BackendMemory,
    MaxMemoryMB: 512,
}
```

### Monitoring

```go
// Check stats periodically
ticker := time.NewTicker(10 * time.Second)
go func() {
    for range ticker.C {
        stats := storage.Stats()
        if stats.SyncBufferLen > 5000 {
            log.Warn("Sync buffer is high: %d", stats.SyncBufferLen)
        }
    }
}()
```

---

## References

- Go sync.Map: https://pkg.go.dev/sync#Map
- xxHash: https://cyan4973.github.io/xxHash/
- Lock-Free Programming: Art of Multiprocessor Programming (Herlihy & Shavit)

---

**Last Updated**: 2025-11-09  
**GridKV Version**: v3.1
