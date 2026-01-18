# GridKV

[![Go Version](https://img.shields.io/badge/go-%3E%3D1.24-blue.svg)](https://golang.org/)
[![GitHub License](https://img.shields.io/github/license/feellmoose/GridKV)](LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/feellmoose/gridkv)](https://goreportcard.com/report/github.com/feellmoose/gridkv)
[![GitHub Tag](https://img.shields.io/github/v/tag/feellmoose/GridKV)]()

A high-performance distributed key-value cache with eventual consistency guarantees, using in-memory storage.

[English](./README.md) / [中文](./README_CN.md)

## Features

- **High Performance & Thread-Safe**: Sub-millisecond local operations, all public methods support concurrent access
- **Eventual Consistency Replication**: Gossip protocol with batched replication ensures data convergence across nodes
- **Distributed Fault-Tolerant Architecture**: Consistent hashing with virtual nodes and SWIM protocol for load balancing and failure detection
- **In-Memory Storage**: Memory backend supports TTL and automatic compression

## Installation

```bash
go get github.com/feellmoose/gridkv
```

## Quick Start

```go
package main

import (
    "context"
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

    // Set key-value pair
    kv.Set(context.Background(), "key", []byte("value"), time.Hour)

    // Get value
    // Returns (nil, nil) if key not found, error only for real failures
    value, err := kv.Get(context.Background(), "key")
    if err != nil {
        log.Fatal(err)
    }
    if value != nil {
        println(string(value))
    }
}
```

## Testing

```bash
# Run all tests
go test ./...

# Run short tests only
go test -short ./...

# Run benchmarks
go test -bench=. ./tests/
```

## Architecture

- **SWIM Protocol**: Membership management and failure detection
- **Consistent Hashing**: Key distribution across nodes
- **Gossip Protocol**: Efficient data replication
- **HLC (Hybrid Logical Clock)**: Causality tracking and conflict resolution

See [internal/cluster/README.md](internal/cluster/README.md) for detailed architecture documentation.

## License

MIT License - see [LICENSE](LICENSE) for details.
