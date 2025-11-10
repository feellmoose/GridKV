# GridKV Quick Start Guide

**Version**: v3.1  
**Audience**: Developers new to GridKV

---

## 🎯 What is GridKV?

GridKV is a **Go-native embedded distributed cache** that you import into your application like any other Go library.

**Key Concept**: No external cache servers needed - GridKV runs inside your app!

---

## 🚀 5-Minute Tutorial

### Step 1: Install

```bash
go get github.com/feellmoose/gridkv
```

### Step 2: Import and Initialize

```go
package main

import (
    "context"
    "log"
    "github.com/feellmoose/gridkv"
    "github.com/feellmoose/gridkv/internal/gossip"
    "github.com/feellmoose/gridkv/internal/storage"
)

func main() {
    // Create GridKV instance (embedded in your app)
    kv, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
        // Required: Identify this instance
        LocalNodeID:  "app-instance-1",
        LocalAddress: "localhost:8001",
        
        // Optional: Join existing cluster
        SeedAddrs: []string{"localhost:8002"}, // Other instances
        
        // Required: Network configuration
        Network: &gossip.NetworkOptions{
            Type:     gossip.TCP,
            BindAddr: "localhost:8001",
        },
        
        // Required: Storage configuration
        Storage: &storage.StorageOptions{
            Backend: storage.BackendMemorySharded, // Recommended
            Shards:  32,
        },
    })
    if err != nil {
        log.Fatal(err)
    }
    defer kv.Close()
    
    // Use GridKV!
    ctx := context.Background()
    
    // Store data
    kv.Set(ctx, "user:123", []byte("Alice"))
    
    // Retrieve data
    value, _ := kv.Get(ctx, "user:123")
    log.Printf("User: %s", value) // "Alice"
    
    // Delete data
    kv.Delete(ctx, "user:123")
}
```

### Step 3: Run Multiple Instances

```bash
# Terminal 1: Start instance 1
$ go run main.go --node-id=app-1 --addr=localhost:8001

# Terminal 2: Start instance 2 (auto-joins instance 1)
$ go run main.go --node-id=app-2 --addr=localhost:8002 --seeds=localhost:8001

# Terminal 3: Start instance 3
$ go run main.go --node-id=app-3 --addr=localhost:8003 --seeds=localhost:8001
```

**Cluster formed automatically!** Instances discover each other via Gossip.

---

## 💡 Basic Concepts

### Embedded Architecture

GridKV runs **inside your application process**, not as a separate server.

```
Traditional:           GridKV:
App → Redis server     App (GridKV embedded)
  ↑                      ↑
Network overhead       In-process (43ns)
```

### Auto-Clustering

Instances automatically form a cluster using Gossip protocol (SWIM).

```
Instance 1 starts → Creates cluster
Instance 2 starts → Joins via SeedAddrs → Cluster has 2 members
Instance 3 starts → Joins via SeedAddrs → Cluster has 3 members

All automatic - no manual cluster configuration!
```

### Data Replication

Data automatically replicates to N instances (default 3).

```
kv.Set(ctx, "session:user-123", sessionData)
  ↓
GridKV automatically:
  1. Hashes key → finds 3 instances to store data
  2. Writes to those 3 instances (replication)
  3. Returns success when 2 instances confirm (quorum)
```

---

## 📖 Common Patterns

### Pattern 1: Single Instance (Development)

```go
kv, _ := gridkv.NewGridKV(&gridkv.GridKVOptions{
    LocalNodeID:  "dev",
    LocalAddress: "localhost:8001",
    SeedAddrs:    nil, // No seeds = first/only instance
    Network:      &gossip.NetworkOptions{Type: gossip.TCP, BindAddr: "localhost:8001"},
    Storage:      &storage.StorageOptions{Backend: storage.BackendMemorySharded},
})
```

### Pattern 2: Multi-Instance Cluster

```go
// Instance 1 (seed)
kv1, _ := gridkv.NewGridKV(&gridkv.GridKVOptions{
    LocalNodeID:  "instance-1",
    LocalAddress: "10.0.1.1:8001",
    SeedAddrs:    nil,
    Network:      &gossip.NetworkOptions{Type: gossip.TCP, BindAddr: "10.0.1.1:8001"},
    Storage:      &storage.StorageOptions{Backend: storage.BackendMemorySharded},
})

// Instance 2 (joins instance 1)
kv2, _ := gridkv.NewGridKV(&gridkv.GridKVOptions{
    LocalNodeID:  "instance-2",
    LocalAddress: "10.0.1.2:8001",
    SeedAddrs:    []string{"10.0.1.1:8001"}, // Point to instance 1
    Network:      &gossip.NetworkOptions{Type: gossip.TCP, BindAddr: "10.0.1.2:8001"},
    Storage:      &storage.StorageOptions{Backend: storage.BackendMemorySharded},
})

// Instances 3, 4, 5... same pattern
```

### Pattern 3: Session Storage

```go
type WebServer struct {
    kv *gridkv.GridKV  // Embedded session store
}

func NewWebServer() *WebServer {
    kv, _ := gridkv.NewGridKV(options)
    return &WebServer{kv: kv}
}

func (s *WebServer) HandleLogin(w http.ResponseWriter, r *http.Request) {
    // Generate session
    sessionID := generateSessionID()
    sessionData := []byte(`{"user_id": 123, "role": "admin"}`)
    
    // Store session (auto-replicated across web servers)
    s.kv.Set(r.Context(), "session:"+sessionID, sessionData)
    
    // Set cookie
    http.SetCookie(w, &http.Cookie{Name: "session_id", Value: sessionID})
}

func (s *WebServer) HandleRequest(w http.ResponseWriter, r *http.Request) {
    // Get session (from any web server instance)
    sessionID := getSessionCookie(r)
    sessionData, err := s.kv.Get(r.Context(), "session:"+sessionID)
    if err != nil {
        http.Error(w, "Not logged in", 401)
        return
    }
    
    // Use session data
    // ...
}
```

---

## ⚙️ Configuration Essentials

### Required Parameters

1. **LocalNodeID**: Unique identifier for this instance
   ```go
   LocalNodeID: "app-instance-1"  // Must be unique
   ```

2. **LocalAddress**: Network address for this instance
   ```go
   LocalAddress: "10.0.1.10:8001"  // host:port
   ```

3. **Network**: Network configuration
   ```go
   Network: &gossip.NetworkOptions{
       Type:     gossip.TCP,
       BindAddr: "10.0.1.10:8001",
   }
   ```

4. **Storage**: Storage backend configuration
   ```go
   Storage: &storage.StorageOptions{
       Backend: storage.BackendMemorySharded, // Recommended
       Shards:  32,                           // More shards = better concurrency
   }
   ```

### Optional Parameters

```go
SeedAddrs:    []string{"host1:8001", "host2:8001"}, // For joining cluster
ReplicaCount: 3,  // Number of replicas (default: 3)
WriteQuorum:  2,  // Write quorum (default: 2)
ReadQuorum:   2,  // Read quorum (default: 2)
DataCenter:   "us-east", // For multi-DC deployments
```

---

## 🔍 Troubleshooting

### Instance won't start?

**Check**:
1. Port not in use: `lsof -i :8001`
2. LocalAddress reachable by other instances
3. Network and Storage options provided

### Instance won't join cluster?

**Check**:
1. SeedAddrs points to running instances
2. Network connectivity (firewall, security groups)
3. Seed instances are healthy

### Data not replicating?

**Check**:
1. Cluster has N instances (N = ReplicaCount)
2. Network connectivity between instances
3. Check logs for replication errors

---

## 📚 Next Steps

### Learn More
- [Architecture](ARCHITECTURE.md) - How GridKV works internally
- [Consistency Model](CONSISTENCY_MODEL.md) - Quorum replication explained

### Deploy to Production
- [examples/05_production_ready](../examples/05_production_ready/) - Best practices
- [Metrics Export](METRICS_EXPORT.md) - Monitoring setup

### Optimize Performance
- [Performance Guide](PERFORMANCE.md) - Tuning tips
- [Storage Backends](STORAGE_BACKENDS.md) - Backend selection

---

**GridKV Quick Start** - From zero to distributed cache in 5 minutes! 🚀

**Last Updated**: 2025-11-09  
**GridKV Version**: v3.1

