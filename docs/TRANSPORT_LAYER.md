# Transport Layer

**Version**: v3.1  
**Status**: TCP (default), gnet (high-performance)

---

## Overview

GridKV provides a pluggable transport layer with two network protocol implementations. The transport layer abstracts network communication, allowing the gossip protocol to operate seamlessly.

---

## Transport Interface

### Core Abstractions

```go
type Transport interface {
    Dial(address string) (TransportConn, error)
    Listen(address string) (TransportListener, error)
}

type TransportConn interface {
    WriteDataWithContext(ctx context.Context, data []byte) error
    ReadDataWithContext(ctx context.Context) ([]byte, error)
    Close() error
    LocalAddr() net.Addr
    RemoteAddr() net.Addr
}

type TransportListener interface {
    Start() error
    HandleMessage(handler func([]byte) error) TransportListener
    Stop() error
    Addr() net.Addr
}
```

---

## Available Transports

### 1. TCP (Default, Recommended)

**Implementation**: `internal/transport/tcp.go`

**Characteristics**:
- Reliable, ordered delivery
- Connection-oriented with flow control
- Zero external dependencies
- Production-ready

**TCP Optimizations**:
```go
conn.SetNoDelay(true)          // Disable Nagle's algorithm (reduce latency)
conn.SetKeepAlive(true)        // Enable TCP keep-alive
conn.SetKeepAlivePeriod(30s)   // Keep-alive interval
conn.SetReadBuffer(32KB)       // Read buffer size
conn.SetWriteBuffer(32KB)      // Write buffer size
```

**Performance**:
- Latency: ~1-2ms (LAN)
- Throughput: ~500MB/s
- Concurrent connections: >1000

**Message Framing**:
```
Wire Format:
  [4-byte length prefix (big-endian)][message data]
  
Write:
  1. Write 4-byte length
  2. Write message data
  
Read:
  1. Read 4-byte length
  2. Validate length (<10MB max)
  3. Read exact message data
```

**Use Cases**:
- ✅ Production deployments (recommended)
- ✅ Reliable communication required
- ✅ LAN/WAN scenarios
- ✅ Zero external dependencies

**Configuration**:
```go
Network: &gossip.NetworkOptions{
    Type:     gossip.TCP,
    BindAddr: "0.0.0.0:8001",
}
```

### 2. gnet (High-Performance)

**Implementation**: `internal/transport/gnet.go`

**Characteristics**:
- Event-driven architecture (Reactor pattern)
- Zero-copy I/O
- Very high throughput
- Linux/macOS only

**Dependencies**: `github.com/panjf2000/gnet/v2`

**Event-Driven Architecture**:
```go
Reactor Pattern:
  - Non-blocking I/O
  - Event loop per CPU core
  - Minimal context switching
  
Zero-Copy:
  - Direct buffer access
  - No intermediate copies
  - Reduced memory allocations
```

**Performance**:
- Latency: ~0.5-1ms
- Throughput: ~800MB/s (1.6x faster than TCP)
- Connections: >100K concurrent

**Use Cases**:
- Maximum throughput required
- High connection count (>10K)
- Linux/macOS deployments
- Accept external dependency

**Configuration**:
```go
Network: &gossip.NetworkOptions{
    Type:     gossip.GNET,
    BindAddr: "0.0.0.0:8001",
}
```

---

## Connection Management

### Connection Pooling

**Architecture**:
```go
type ConnPool struct {
    transport   Transport
    address     string
    maxIdle     int           // Max idle connections
    maxConns    int           // Max total connections
    idleTimeout time.Duration
    
    mu        sync.Mutex
    idleConns []TransportConn
    total     int
}
```

**Benefits**:
- Reduced connection overhead
- Amortized handshake costs
- Better resource utilization
- Configurable limits

**Default Configuration**:
```
MaxConns:    1000
MaxIdle:     100
IdleTimeout: 90s
```

**Tuning Guidelines**:
- High concurrency → Increase MaxConns
- Burst traffic → Increase MaxIdle
- Save resources → Decrease IdleTimeout

### Connection Lifecycle

```
States:
  1. Created (Dial)
  2. Active (in-use)
  3. Idle (returned to pool)
  4. Closed (timeout or pool full)

Pool Management:
  - Get(): Reuse idle or create new
  - Put(): Return to pool or close
  - Background cleanup: Close expired connections
```

---

## Transport Selection

### Decision Matrix

| Requirement | Recommended Transport |
|-------------|----------------------|
| Production, reliable | TCP |
| Maximum throughput | gnet |
| Development/testing | TCP |
| Linux/macOS only | gnet |
| Zero dependencies | TCP |

### Performance Comparison

| Transport | Latency | Throughput | Reliability | Dependencies |
|-----------|---------|------------|-------------|--------------|
| TCP | 1-2ms | 500MB/s | High | None |
| gnet | 0.5-1ms | 800MB/s | High | gnet/v2 |

---

## Message Protocol

### Framing

All transports use length-prefixed framing:

```
Wire Format:
  [4 bytes: message length (big-endian)][message data]

Benefits:
  - Simple parsing
  - No delimiter ambiguity
  - Efficient buffering
  - Max message size: 10MB (configurable)
```

### Flow Control

- **TCP**: Built-in TCP flow control
- **gnet**: Event-driven backpressure

---

## Network Configuration

### Timeouts

```go
NetworkOptions:
  ReadTimeout:  5 * time.Second   // Read operation timeout
  WriteTimeout: 5 * time.Second   // Write operation timeout
  DialTimeout:  3 * time.Second   // Connection establishment
```

**Tuning by Network Type**:
- LAN: 1-5s timeouts
- WAN: 10-30s timeouts
- Satellite: 60s+ timeouts

### Buffer Sizes

```go
Recommended:
  ReadBuffer:  32 KB   // Small messages
  WriteBuffer: 32 KB   // Small messages
  
For large values:
  ReadBuffer:  256 KB
  WriteBuffer: 256 KB
```

---

## Error Handling

### Retry Logic

```go
Strategy: Exponential backoff with jitter

Attempt 1: Immediate
Attempt 2: 100ms + random(0-50ms)
Attempt 3: 200ms + random(0-100ms)
...
Max attempts: 3 (default)
```

### Connection Failures

```go
Handling:
  1. Detect connection failure
  2. Remove from pool
  3. Retry with new connection
  4. If all retries fail, mark node as suspect (SWIM)
```

---

## Security

### Transport-Level Security

**TCP**:
- Application-level encryption (Ed25519 signing)
- TLS wrapper (future enhancement)

**gnet**:
- No built-in security
- Application-level encryption needed

### Message Authentication

All transports benefit from GridKV's message signing:

```go
Process:
  1. Serialize message
  2. Sign with Ed25519
  3. Attach signature
  4. Send via transport
  
Receiver:
  1. Receive via transport
  2. Verify signature
  3. Process if valid
```

---

## Monitoring

### Metrics

Per-transport metrics available via GridKVMetrics:
- Active connections
- Bytes sent/received
- Error rate
- Latency percentiles

### Health Checks

```go
TCP health check:
  conn.SetReadDeadline(1ms)
  conn.Read(1 byte)
  
  Timeout = healthy
  Data/Error = investigate
```

---

## Transport Registry

### Registration

```go
func init() {
    transport.RegisterTransport("tcp", func() (Transport, error) {
        return NewTCPTransport(), nil
    })
}
```

### Dynamic Selection

```go
transport, err := transport.NewTransport("tcp")
if err != nil {
    // Transport not available (not imported)
}
```

---

## Implementation Files

- `internal/transport/transport.go` - Interface definitions
- `internal/transport/registry.go` - Transport registry
- `internal/transport/tcp.go` - TCP implementation
- `internal/transport/tcp_register.go` - TCP registration
- `internal/transport/gnet.go` - gnet implementation
- `internal/transport/gnet_metrics.go` - gnet metrics

---

## References

- TCP: RFC 793, RFC 7323 (TCP Extensions)
- gnet: Event-driven networking in Go
- Reactor Pattern: Douglas C. Schmidt

---

**Last Updated**: 2025-11-09  
**GridKV Version**: v3.1
