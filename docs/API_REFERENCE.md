# GridKV API Reference

**Version**: v3.1  
**Type**: API Documentation

---

## 📚 Core API

GridKV provides a simple, focused API for distributed key-value operations.

---

## 🔧 Initialization

### NewGridKV

```go
func NewGridKV(opts *GridKVOptions) (*GridKV, error)
```

Creates a new GridKV instance embedded in your application.

**Parameters**:
- `opts`: Configuration options (see GridKVOptions below)

**Returns**:
- `*GridKV`: Initialized GridKV instance
- `error`: Configuration or initialization error

**Example**:
```go
kv, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
    LocalNodeID:  "app-1",
    LocalAddress: "localhost:8001",
    Network:      &gossip.NetworkOptions{Type: gossip.TCP, BindAddr: "localhost:8001"},
    Storage:      &storage.StorageOptions{Backend: storage.BackendMemorySharded},
})
if err != nil {
    log.Fatal(err)
}
defer kv.Close()
```

---

## 📝 Data Operations

### Set

```go
func (g *GridKV) Set(ctx context.Context, key string, value []byte) error
```

Stores a key-value pair with automatic replication.

**Parameters**:
- `ctx`: Context for timeout/cancellation (recommended: 5s timeout)
- `key`: Key to store (non-empty, max 256 bytes recommended)
- `value`: Data to store (max 1MB recommended)

**Returns**:
- `error`: nil on success, error on failure

**Behavior**:
- Replicates to N instances (default 3)
- Returns success when W instances confirm (default 2)
- Deep-copies value (safe to modify after call)

**Example**:
```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

err := kv.Set(ctx, "user:123", []byte("Alice"))
if err != nil {
    log.Printf("Set failed: %v", err)
}
```

**Performance**: ~100ns (local), ~2ms (LAN quorum)

### Get

```go
func (g *GridKV) Get(ctx context.Context, key string) ([]byte, error)
```

Retrieves a value by key from the distributed store.

**Parameters**:
- `ctx`: Context for timeout/cancellation (recommended: 3s timeout)
- `key`: Key to retrieve (non-empty)

**Returns**:
- `[]byte`: Value data (deep copy, safe to modify)
- `error`: nil on success, storage.ErrItemNotFound if key doesn't exist

**Behavior**:
- Reads from R instances in parallel (default 2)
- Returns value with highest version (most recent)
- Triggers read-repair if versions differ

**Example**:
```go
ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
defer cancel()

value, err := kv.Get(ctx, "user:123")
if err == storage.ErrItemNotFound {
    log.Println("Key not found")
} else if err != nil {
    log.Printf("Get failed: %v", err)
} else {
    log.Printf("Value: %s", value)
}
```

**Performance**: 43ns (local), <1ms (LAN quorum)

### Delete

```go
func (g *GridKV) Delete(ctx context.Context, key string) error
```

Removes a key-value pair from the distributed store.

**Parameters**:
- `ctx`: Context for timeout/cancellation (recommended: 5s timeout)
- `key`: Key to delete (non-empty)

**Returns**:
- `error`: nil on success or if key doesn't exist

**Behavior**:
- Sends delete to N instances
- Returns success when W instances confirm
- Deleting non-existent key is not an error (idempotent)

**Example**:
```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

err := kv.Delete(ctx, "user:123")
if err != nil {
    log.Printf("Delete failed: %v", err)
}
```

**Performance**: ~100ns (local), ~2ms (LAN quorum)

---

## 🔚 Lifecycle

### Close

```go
func (g *GridKV) Close() error
```

Gracefully shuts down GridKV and releases resources.

**Returns**:
- `error`: nil on clean shutdown, error if issues

**Behavior**:
- Stops Gossip protocol
- Closes network connections
- Flushes storage backend
- Subsequent operations will fail after Close

**Example**:
```go
kv, _ := gridkv.NewGridKV(opts)
defer kv.Close()  // Always use defer

// Use kv...
// Close() called automatically on exit
```

---

## ⚙️ Configuration Types

### GridKVOptions

```go
type GridKVOptions struct {
    // Identity
    LocalNodeID  string   // This instance ID (required)
    LocalAddress string   // This instance address (required)
    SeedAddrs    []string // Other instances to join (optional)
    
    // Network
    Network *gossip.NetworkOptions // Network config (required)
    
    // Storage
    Storage *storage.StorageOptions // Storage config (required)
    
    // Replication (optional, defaults shown)
    ReplicaCount int // N (default: 3)
    WriteQuorum  int // W (default: 2)
    ReadQuorum   int // R (default: 2)
    
    // Multi-DC (optional)
    DataCenter string // Datacenter identifier
    
    // Tuning (optional)
    GossipInterval     time.Duration // Gossip frequency (default: 1s)
    FailureTimeout     time.Duration // Mark suspect (default: 5s)
    ReplicationTimeout time.Duration // Replication timeout (default: 2s)
}
```

### NetworkOptions

```go
type NetworkOptions struct {
    Type     NetworkType // gossip.TCP (required)
    BindAddr string      // Address to bind (required)
    MaxConns int         // Max connections (optional, default: 1000)
}
```

### StorageOptions

```go
type StorageOptions struct {
    Backend      StorageBackendType // storage.BackendMemorySharded (required)
    Shards       int                // Shard count (optional, default: 32)
    MaxMemoryMB  int64              // Memory limit (optional, default: 1024)
}
```

---

## 🚨 Error Handling

### Common Errors

```go
// Set/Get/Delete errors
- "GridKV not initialized" - Instance not properly created
- "key cannot be empty" - Empty key string
- "quorum not met" - Failed to reach W (write) or R (read) instances
- context.DeadlineExceeded - Operation timed out

// Get-specific errors
- storage.ErrItemNotFound - Key does not exist
- storage.ErrItemExpired - Key expired (if TTL set)

// NewGridKV errors
- "GridKVOptions cannot be nil" - Missing options
- "LocalNodeID is required" - Missing node ID
- "LocalAddress is required" - Missing address
- "Network options are required" - Missing network config
- "Storage options are required" - Missing storage config
```

### Error Handling Pattern

```go
value, err := kv.Get(ctx, "user:123")
if err == storage.ErrItemNotFound {
    // Key doesn't exist - handle gracefully
    return nil
} else if err != nil {
    // Other error - may be temporary (network, timeout)
    log.Printf("Get error: %v", err)
    return err
}

// Use value
processUser(value)
```

---

## 📊 Monitoring

### GetStats (Not in current API)

GridKV v3.1 uses **external metrics export** instead of GetStats():

```go
import "github.com/feellmoose/gridkv/internal/metrics"

// Setup metrics export
exporter := metrics.PrometheusExporter(outputFunc)
gkMetrics := metrics.NewGridKVMetrics(exporter)

// Record operations
gkMetrics.IncrementSet()
gkMetrics.IncrementGet()

// Export metrics
gkMetrics.Export(ctx)
```

See: [METRICS_EXPORT.md](METRICS_EXPORT.md)

---

## 🎯 API Design Principles

### Simplicity

- Only 3 core operations: Set, Get, Delete
- No complex data structures
- Straightforward semantics

### Consistency

- All operations use Context for timeout/cancellation
- All operations return errors explicitly
- All operations are idempotent where possible

### Safety

- All public APIs are panic-safe
- Deep copies prevent data corruption
- Thread-safe for concurrent access

---

**GridKV API Reference** - Simple, Safe, Effective ✅

**Last Updated**: 2025-11-09  
**GridKV Version**: v3.1
