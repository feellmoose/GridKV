# GridKV

[![Go Version](https://img.shields.io/badge/Go-1.23+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

English | [简体中文](README_ZH.md)

**GridKV** is a Go-native embedded distributed key-value cache with automatic clustering, batched eventual replication, and adaptive networking.

---

## Features

- **Embedded**: Import as Go library, runs in-process
- **Distributed Get**: Automatic replica selection, retry, and read-repair
- **Auto-Clustering**: SWIM protocol, <1s failure detection
- **High Performance**: 3-6M ops/s (5-node cluster), 10-20M ops/s (10-node cluster)
- **Adaptive Networking**: Auto-detects LAN/WAN, optimizes accordingly

---

## Quick Start

```bash
go get github.com/feellmoose/gridkv
```

```go
package main

import (
    "context"
    "log"
    gridkv "github.com/feellmoose/gridkv"
)

func main() {
    kv, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
        LocalNodeID:  "node-1",
        LocalAddress: "localhost:8001",
        SeedAddrs:    []string{"localhost:8002", "localhost:8003"},
    })
    if err != nil {
        log.Fatal(err)
    }
    defer kv.Close()
    
    ctx := context.Background()
    kv.Set(ctx, "user:1001", []byte("Alice"))
    value, _ := kv.Get(ctx, "user:1001")
    log.Printf("Value: %s", value)
}
```

---

## Distributed Get

GridKV's `Get()` operation provides intelligent distributed read capabilities:

### Capabilities

- **Automatic Replica Selection**: Uses consistent hashing to locate N replicas
- **Local-First**: Returns immediately if data exists locally (43ns)
- **Smart Forwarding**: Automatically forwards to coordinator if not local
- **Fast Retries**: Automatic retry with exponential backoff on failures
- **Read-Repair**: Detects version mismatches and repairs stale replicas
- **Hot Cache**: Frequently accessed keys cached locally (<1ms latency)

### Performance

**Cluster Throughput** (MemorySharded backend, QUIC transport):
- **5-node cluster**: 3-6M ops/s (LAN)
- **10-node cluster**: 10-20M ops/s (LAN)
- **15-node cluster**: 15-30M ops/s (LAN)

**Latency**:
- Local (same instance): 43ns
- LAN (cached, single replica): <1ms
- LAN (forwarded): ~2ms
- WAN (cached): <50ms

### How It Works

1. **Hash key** → Find N replica nodes via consistent hash ring
2. **Check local** → If local node is a replica, return immediately
3. **Forward request** → If not local, forward to coordinator (first replica)
4. **Retry on failure** → Automatic retry with fast backoff
5. **Read-repair** → If multiple replicas return different versions, use latest and repair stale ones

### Example

```go
ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
defer cancel()

// Get automatically handles:
// - Finding the right replica
// - Retrying on failures
// - Repairing inconsistencies
value, err := kv.Get(ctx, "user:12345")
if err != nil {
    log.Printf("Get failed: %v", err)
    return
}
// Value is guaranteed to be the latest available version
```

---

## Storage Backends

| Backend | Cluster Performance (10-node) | Local Performance | Use Case | Notes |
|---------|-------------------------------|------------------|----------|-------|
| **MemorySharded** | 10-20M ops/s (mixed)<br>30-50M ops/s (read-heavy) | 5.8M ops/s (100B)<br>14M ops/s (read-heavy) | Production, high-throughput | Recommended for production. Shard count: 2-4x CPU cores. |
| **Memory** | 2-4M ops/s (mixed)<br>5-8M ops/s (read-heavy) | 933K ops/s (100B)<br>2.2M ops/s (read-heavy) | Development, testing | Auto-compression for values >1KB. Single lock limits concurrency. |

### MemorySharded (Recommended)

**Cluster Performance**:
- **5-node cluster**: 3-6M ops/s (mixed workload)
- **10-node cluster**: 10-20M ops/s (mixed workload)
- **15-node cluster**: 15-30M ops/s (mixed workload)
- **Read-heavy (10-node)**: 30-50M ops/s

**Local Performance** (single node, Intel i7-12700H, 20 cores):
- Small (100B): 5.8M ops/s (170ns/op)
- Medium (1KB): 2.4M ops/s (419ns/op)
- Read-heavy: 14M ops/s (71ns/op)

**Configuration**:
```go
Storage: &gridkv.StorageOptions{
    Backend:     gridkv.BackendMemorySharded,
    ShardCount:  256,     // 2-4x CPU cores
    MaxMemoryMB: 4096,
}
```

### Memory (Simple)

**Cluster Performance**:
- **5-node cluster**: 1-2M ops/s (mixed workload)
- **10-node cluster**: 2-4M ops/s (mixed workload)
- **Read-heavy (10-node)**: 5-8M ops/s

**Local Performance** (single node):
- Small (100B): 933K ops/s (1.1µs/op)
- Medium (1KB): 840K ops/s (1.2µs/op, 15% compression)
- Read-heavy: 2.2M ops/s (454ns/op)

**Configuration**:
```go
Storage: &gridkv.StorageOptions{
    Backend:     gridkv.BackendMemory,
    MaxMemoryMB: 2048,
}
```

---

## Transport Layers

| Transport | Latency (LAN) | Latency (WAN) | Reliability | Use Case |
|-----------|---------------|---------------|-------------|----------|
| **TCP** | ~1-5ms | ~20-100ms | ✅ Guaranteed | Production, maximum compatibility |
| **QUIC** | ~0.5-2ms | ~10-50ms | ✅ Guaranteed | Large clusters, high performance |
| **UDP** | ~0.1-1ms | ~5-20ms | ❌ Best effort | Ultra-low latency (use with caution) |

### TCP (Recommended for Compatibility)

- Works through all firewalls/NATs
- Guaranteed delivery and ordering
- Best for production deployments

```go
Network: &gridkv.NetworkOptions{
    Type:     gridkv.TCP,
    BindAddr: "0.0.0.0:8001",
    MaxConns: 256,
}
```

### QUIC (Recommended for Performance)

- 0-RTT connection establishment
- Multiplexing over single connection
- Higher CPU usage (encryption overhead)

```go
Network: &gridkv.NetworkOptions{
    Type:     gridkv.QUIC,
    BindAddr: "0.0.0.0:8001",
    MaxConns: 512,
}
```

### UDP (Use with Caution)

- Lowest latency, no connection overhead
- **No guaranteed delivery** - messages may be lost
- **Not recommended for production**

---

## Performance

**Cluster Throughput** (MemorySharded backend, QUIC transport, LAN):

| Cluster Size | Mixed Workload | Read-Heavy | Write-Heavy |
|--------------|----------------|------------|-------------|
| 3 nodes | 2-4M ops/s | 8-12M ops/s | 1-2M ops/s |
| 5 nodes | 3-6M ops/s | 15-25M ops/s | 2-3M ops/s |
| 10 nodes | 10-20M ops/s | 30-50M ops/s | 5-8M ops/s |
| 15 nodes | 15-30M ops/s | 45-75M ops/s | 8-12M ops/s |

**Latency**:
- Local (same instance): 43ns
- Distributed Get (LAN, cached): <1ms
- Distributed Get (LAN, forwarded): ~2ms
- Distributed Get (WAN, cached): <50ms
- Distributed Set (async): ~2ms

---

## Configuration

### Minimal Setup

```go
kv, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
    LocalNodeID:  "node-1",
    LocalAddress: "localhost:8001",
    SeedAddrs:    []string{"localhost:8002"},
})
```

### Production Setup

```go
kv, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
    LocalNodeID:  "node-1",
    LocalAddress: "10.0.1.10:8001",
    SeedAddrs:    []string{"10.0.1.11:8001", "10.0.1.12:8001"},
    
    Network: &gridkv.NetworkOptions{
        Type:     gridkv.QUIC,  // or gridkv.TCP
        BindAddr: "0.0.0.0:8001",
        MaxConns: 512,
    },
    
    Storage: &gridkv.StorageOptions{
        Backend:     gridkv.BackendMemorySharded,
        ShardCount:  256,
        MaxMemoryMB: 4096,
    },
    
    ReplicaCount: 3,
    TTL:          24 * time.Hour,
})
```

---

## API

```go
// Set: Store with automatic replication
func (g *GridKV) Set(ctx context.Context, key string, value []byte, ttl ...time.Duration) error

// Get: Distributed read with automatic retry and read-repair
func (g *GridKV) Get(ctx context.Context, key string) ([]byte, error)

// GetAsync: Asynchronous read, returns Future
func (g *GridKV) GetAsync(ctx context.Context, key string) (ReadFuture, error)

// GetBatchAsync: Batch asynchronous reads
func (g *GridKV) GetBatchAsync(ctx context.Context, keys []string) (BatchReadFuture, error)

// Delete: Remove key (tombstone replicated asynchronously)
func (g *GridKV) Delete(ctx context.Context, key string) error

// Close: Graceful shutdown
func (g *GridKV) Close() error
```

**Thread-safe**: All methods are safe for concurrent access.

---

## License

MIT License - see [LICENSE](LICENSE)

---

<div align="center">

**GridKV** - Go-Native Embedded Distributed Cache

*High Performance • Auto-Clustering • Zero External Dependencies*

</div>
