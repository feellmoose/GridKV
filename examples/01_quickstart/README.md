# Example 01: Quick Start

**Difficulty**: Beginner  
**Time**: 5 minutes  
**Use Case**: Single-node setup for development/testing

---

## What You'll Learn

- How to create a GridKV instance
- Basic CRUD operations (Set, Get, Delete)
- Proper resource cleanup
- Error handling

---

## Quick Start

```bash
cd examples/01_quickstart
go run main.go
```

---

## Code Walkthrough

### 1. Import GridKV

```go
import gridkv "github.com/feellmoose/gridkv"
```

**That's it!** Only one import needed.

### 2. Create Instance

```go
kv, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
    LocalNodeID:  "quickstart-node",
    LocalAddress: "localhost:8001",
    
    Network: &gridkv.NetworkOptions{
        Type:     gridkv.TCP,
        BindAddr: "localhost:8001",
    },
    
    Storage: &gridkv.StorageOptions{
        Backend: gridkv.BackendMemorySharded,  // High-performance
    },
})
if err != nil {
    log.Fatal(err)
}
defer kv.Close()  // Always cleanup!
```

### 3. Set Value

```go
ctx := context.Background()
err = kv.Set(ctx, "user:1001", []byte("Alice"))
if err != nil {
    log.Printf("Set failed: %v", err)
}
```

### 4. Get Value

```go
value, err := kv.Get(ctx, "user:1001")
if err != nil {
    log.Printf("Get failed: %v", err)
}
fmt.Printf("Value: %s\n", value)  // Output: Alice
```

### 5. Delete Value

```go
err = kv.Delete(ctx, "user:1001")
if err != nil {
    log.Printf("Delete failed: %v", err)
}
```

---

## Configuration Explained

| Parameter | Value | Why |
|-----------|-------|-----|
| `LocalNodeID` | "quickstart-node" | Unique node identifier |
| `LocalAddress` | "localhost:8001" | Network address for this node |
| `Network.Type` | `gridkv.TCP` | Reliable transport (recommended) |
| `Storage.Backend` | `BackendMemorySharded` | High-performance storage |

---

## Real-World Use Case

### Session Storage Example

```go
// Store user session
sessionData := []byte(`{"userID": 123, "role": "admin", "loginTime": "2025-01-01T10:00:00Z"}`)
err := kv.Set(ctx, "session:abc123", sessionData)

// Retrieve session
session, err := kv.Get(ctx, "session:abc123")
if err != nil {
    // Session expired or doesn't exist
    return ErrUnauthorized
}

// Parse and validate session
// ...

// Delete session on logout
kv.Delete(ctx, "session:abc123")
```

---

## Performance

**Single-node performance**:
- Set: ~100-200ns (in-process)
- Get: ~50-100ns (in-process)
- Throughput: 1-2M ops/s

---

## Next Steps

- [Example 02: Distributed Cluster](../02_distributed_cluster/) - Multi-node setup
- [Example 04: High Performance](../04_high_performance/) - Performance tuning
- [API Reference](../../docs/API_REFERENCE.md) - Complete API documentation

---

**GridKV v3.1** - Quick Start Example
