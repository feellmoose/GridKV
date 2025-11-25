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
 - **High Performance**: up to 95K ops/s (3-node Memory peak) / 66K ops/s (3-node MemorySharded peak) on a single host, plus 14.6M ops/s local microbenchmarks
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

All published numbers come from a single Linux 6.14 host (Go 1.23, Intel i7-12700H, 20 vCPUs, QUIC transport). We publish two workload modes:

- **Peak** – `GRIDKV_NO_THROTTLE=1 go test ./tests/... -run TestMemory*` removes the 10 ms / 5 ms / 20 ms sleeps from the mixed workload to show the maximum sustainable throughput of this single-host setup.
- **Validation** – default workload (60 % write / 30 % read / 10 % delete with sleeps) used for regression testing memory safety, eviction, and replication.

**Cluster Throughput (single-host LAN, QUIC)**:

- Peak / MemorySharded: 66 K ops/s (3 nodes), 17 K (5 nodes), 10 K (10 nodes), 9 K (15 nodes)
- Peak / Memory: 95 K ops/s (3 nodes), 38 K (5 nodes), 9 K (10 nodes)
- Validation / MemorySharded: 5.6 K ops/s (3 nodes), 3.4 K (5 nodes), 1.5 K (10 nodes), 2.0 K (15 nodes)
- Validation / Memory: 4.4 K ops/s (3 nodes), 2.7 K (5 nodes), 1.6 K (10 nodes)

**Latency** (MemorySharded / QUIC):
- Local (same instance): 43 ns
- LAN (cached, single replica): <1 ms
- LAN (forwarded): ~2 ms
- WAN (cached): <50 ms

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

| Backend | Peak Cluster (10-node, QUIC, **no throttle**) | Validation Cluster (10-node, QUIC, throttled) | Local Microbench (ops/s) | Use Case | Notes |
|---------|---------------------------------------------|---------------------------------------------|--------------------------|----------|-------|
| **MemorySharded** | **10.1K ops/s** total (writes 9.4K / reads 0.2K / deletes 0.5K) | **1.48K ops/s** total (writes 0.76K / reads 0.27K / deletes 0.45K) | 6.28M (100B small)<br>14.6M (read-heavy) | Production, high-throughput | Peak mode uses `GRIDKV_NO_THROTTLE=1`. Validation mode keeps mixed workload sleeps (10ms/5ms/20ms). |
| **Memory** | **9.0K ops/s** total (writes 8.5K / reads 0.1K / deletes 0.5K) | **1.62K ops/s** total (writes 0.80K / reads 0.21K / deletes 0.61K) | 3.35M (100B small)<br>8.91M (read-heavy) | Development, testing | Compression-friendly values reach 0.77M (4KB) / 0.32M (16KB). Set `GRIDKV_NO_THROTTLE=1` for peak cluster tests. |

### MemorySharded (Recommended)

**Peak Cluster Throughput** (QUIC, `GRIDKV_NO_THROTTLE=1`, single i7-12700H host):
- **3-node**: 66.2K ops/s (writes 63.9K / reads 0.7K / deletes 1.6K)
- **5-node**: 17.2K ops/s (writes 15.4K / reads 1.3K / deletes 0.5K)
- **10-node**: 10.1K ops/s (writes 9.4K / reads 0.2K / deletes 0.5K)
- **15-node**: 9.0K ops/s (writes 8.5K / reads 0.2K / deletes 0.3K)

**Validation Cluster Throughput** (default throttled workload: writers 10 ms / readers 5 ms / deleters 20 ms):
- **3-node**: 5.63K ops/s (writes 1.92K / reads 3.25K / deletes 0.46K)
- **5-node**: 3.43K ops/s (writes 1.96K / reads 0.79K / deletes 0.68K)
- **10-node**: 1.48K ops/s (writes 0.76K / reads 0.27K / deletes 0.45K)
- **15-node**: 1.99K ops/s (writes 0.93K / reads 0.73K / deletes 0.33K)

_Transport note_: QUIC peak runs currently hit a 416 KiB UDP receive buffer ceiling on this Linux host, so read-heavy traffic flattens earlier than writes. Raising `/proc/sys/net/core/rmem_max` or running on multiple hosts yields higher read QPS.

**Local Microbenchmarks** (`go test ./internal/storage -run ^$ -bench BenchmarkMemorySharded -benchtime=2s`):
- Small (100B): 6.28M ops/s (159 ns/op)
- Write-only (100B): 6.63M ops/s (151 ns/op)
- Read-heavy: 14.6M ops/s (68.6 ns/op, 100% cache hit window)

**Configuration**:
```go
Storage: &gridkv.StorageOptions{
    Backend:     gridkv.BackendMemorySharded,
    ShardCount:  256,     // 2-4x CPU cores
    MaxMemoryMB: 4096,
}
```

### Memory (Simple)

**Peak Cluster Throughput** (QUIC, `GRIDKV_NO_THROTTLE=1`, single i7-12700H host):
- **3-node**: 94.6K ops/s (writes 61.7K / reads 30.6K / deletes 2.4K)
- **5-node**: 38.4K ops/s (writes 37.3K / reads 0.24K / deletes 0.86K)
- **10-node**: 9.0K ops/s (writes 8.45K / reads 0.12K / deletes 0.47K)

**Validation Cluster Throughput** (default throttled workload):
- **3-node**: 4.45K ops/s (writes 1.63K / reads 2.43K / deletes 0.38K)
- **5-node**: 2.73K ops/s (writes 1.63K / reads 0.53K / deletes 0.57K)
- **10-node**: 1.62K ops/s (writes 0.80K / reads 0.21K / deletes 0.61K)

_Transport note_: Removing throttles shifts the bottleneck from application logic to transport; TCP peak numbers on the same host land within ±30% of the QUIC peak results depending on node count.

**Local Microbenchmarks** (`go test ./internal/storage -run ^$ -bench BenchmarkMemory -benchtime=2s`):
- Small (100B): 3.35M ops/s (306 ns/op)
- Medium (4 KB, compressible): 0.77M ops/s (1.32 µs/op, 0.8 % compressed size)
- Large (16 KB, compressible): 0.32M ops/s (3.23 µs/op, 0.2 % compressed size)
- Write-only (100B): 3.30M ops/s (308 ns/op)
- Read-heavy: 8.91M ops/s (115 ns/op)

_Benchmark environment: Linux 6.14, Go 1.23, Intel i7-12700H (20 vCPUs), GOMAXPROCS=20._

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

All benchmarks were executed on a single Linux 6.14 host (Go 1.23, Intel i7-12700H, 20 vCPUs, GOMAXPROCS=20) with QUIC transport. Two workload modes are published:

- **Peak** – run `GRIDKV_NO_THROTTLE=1 go test ./tests/... -run TestMemory*` to disable the 10 ms / 5 ms / 20 ms sleeps in `runMixedClusterWorkload`. This shows the maximum sustainable throughput in a single-host setup.
- **Validation** – default workload (sleeps enabled) that we use for regression testing of memory safety, compression, eviction, and replication.

> **Important**: These numbers come from a LAN-style simulation on a *single* machine, and we currently only validate cluster sizes up to 15 nodes in this environment. Multi-host scaling characteristics may differ until we run distributed benchmarks.

### Peak Cluster Throughput (QUIC, `GRIDKV_NO_THROTTLE=1`)

| MemorySharded Cluster | Total QPS | Write QPS | Read QPS | Delete QPS |
|----------------------|-----------|-----------|----------|------------|
| 3 nodes | 66,184 | 63,866 | 688 | 1,630 |
| 5 nodes | 17,154 | 15,395 | 1,264 | 495 |
| 10 nodes | 10,059 | 9,390 | 216 | 454 |
| 15 nodes | 8,979 | 8,542 | 172 | 266 |

| Memory Cluster | Total QPS | Write QPS | Read QPS | Delete QPS |
|----------------|-----------|-----------|----------|------------|
| 3 nodes | 94,625 | 61,709 | 30,564 | 2,352 |
| 5 nodes | 38,396 | 37,300 | 241 | 856 |
| 10 nodes | 9,039 | 8,452 | 120 | 467 |

_Note_: On this host, QUIC hits the default 416 KiB UDP receive buffer which limits small-cluster read throughput. Raising `rmem_max` or using multiple machines increases the read numbers substantially.

### Validation Cluster Throughput (Mixed 60/30/10 with throttles)

| MemorySharded Cluster | Total QPS | Write QPS | Read QPS | Delete QPS |
|----------------------|-----------|-----------|----------|------------|
| 3 nodes | 5,627 | 1,921 | 3,246 | 459 |
| 5 nodes | 3,430 | 1,959 | 794 | 677 |
| 10 nodes | 1,476 | 758 | 268 | 450 |
| 15 nodes | 1,990 | 928 | 728 | 334 |

| Memory Cluster | Total QPS | Write QPS | Read QPS | Delete QPS |
|----------------|-----------|-----------|----------|------------|
| 3 nodes | 4,452 | 1,634 | 2,434 | 383 |
| 5 nodes | 2,725 | 1,630 | 529 | 566 |
| 10 nodes | 1,621 | 797 | 211 | 613 |

### Local Microbenchmarks (`go test ./internal/storage -run ^$ -bench BenchmarkMemory -benchtime=2s`)

| Backend | Scenario | Ops/sec | Notes |
|---------|----------|--------:|-------|
| Memory | Small values (100 B) | 3.35M | 306 ns/op |
| Memory | Medium values (4 KB, compressible) | 0.77M | 1.32 µs/op, 0.8 % compressed size |
| Memory | Large values (16 KB, compressible) | 0.32M | 3.23 µs/op, 0.2 % compressed size |
| Memory | Write-only (100 B) | 3.30M | 308 ns/op |
| Memory | Read-heavy hot set | 8.91M | 115 ns/op |
| MemorySharded | Small values (100 B) | 6.28M | 159 ns/op |
| MemorySharded | Write-only (100 B) | 6.63M | 151 ns/op |
| MemorySharded | Read-heavy hot set | 14.6M | 68.6 ns/op |

**Latency (MemorySharded / QUIC)**:
- Local (same instance): 43 ns
- Distributed Get (LAN, cached): <1 ms
- Distributed Get (LAN, forwarded): ~2 ms
- Distributed Get (WAN, cached): <50 ms
- Distributed Set (async): ~2 ms

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
