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

## License

MIT License - see [LICENSE](LICENSE) for details.