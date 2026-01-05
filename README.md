# GridKV

[![Go Version](https://img.shields.io/badge/go-%3E%3D1.21-blue.svg)](https://golang.org/)
[![License](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

A high-performance distributed key-value cache SDK using memory with eventual consistency guarantees.

English/[中文](./README_CN.md)

## Features

- **High Performance & Thread Safety**: Sub-millisecond local operations optimized for concurrent workloads with thread-safe public methods
- **Eventual Consistency & Replication**: Epidemic gossip protocol with batch replication ensuring data convergence across nodes
- **Distributed & Fault Tolerant**: Consistent hashing with virtual nodes and SWIM protocol for load balancing and failure detection
- **Adaptive Networking**: LAN/WAN optimization with TCP/QUIC support for inter-node communication
- **Memory Storage**: In-memory backend with TTL support and automatic compression

## Installation

```bash
go get github.com/feellmoose/gridkv
```

## Quick Start

```go
package main

import (
    "log"
    "time"
    "github.com/feellmoose/gridkv"
)

func main() {
    opts := &gridkv.GridKVOptions{
        LocalNodeID:  "node1",
        LocalAddress: "localhost:8080",
        SeedAddrs:    []string{"localhost:8081"},
    }

    kv, err := gridkv.NewGridKV(opts)
    if err != nil {
        log.Fatal(err)
    }
    defer kv.Close()

    // Set key with TTL
    kv.Set("key", []byte("value"), time.Hour)

    // Get value
    value, _ := kv.Get("key")
    println(string(value))
}
```

## Performance

Run benchmarks:

```bash
go test -bench=. ./tests/
```

### Integration Tests Configuration

Integration tests and benchmarks can be configured via environment variables for different machine specifications:

#### Memory Configuration
- `TEST_MAX_MEMORY_MB`: Default memory per node (default: 1024MB)
- `BENCH_MAX_MEMORY_MB`: Benchmark memory per node (default: 256MB)
- `INTEGRATION_MAX_MEMORY_MB`: Integration test memory per node (default: 512MB)

#### Cluster Configuration
- `TEST_NODE_COUNT`: Number of test nodes (default: 100)
- `BENCH_NODE_COUNT`: Number of benchmark nodes (default: 50)
- `INTEGRATION_LARGE_CLUSTER_THRESHOLD`: Large cluster threshold (default: 100)

#### Performance Tuning
- `INTEGRATION_CONCURRENCY`: Test concurrency level (default: varies by test)
- `INTEGRATION_TEST_DURATION`: Test duration (default: 5s)
- `BENCH_REPLICA_COUNT`: Benchmark replication factor (default: 3)

Example usage:
```bash
# Low-memory machine (8GB RAM)
export TEST_MAX_MEMORY_MB=256
export BENCH_MAX_MEMORY_MB=128
export TEST_NODE_COUNT=20

# High-performance machine (64GB RAM)
export TEST_MAX_MEMORY_MB=4096
export BENCH_MAX_MEMORY_MB=2048
export TEST_NODE_COUNT=200
export INTEGRATION_CONCURRENCY=100

go test -v -short ./...
```

## License

MIT License - see [LICENSE](LICENSE) for details.