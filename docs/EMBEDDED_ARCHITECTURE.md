# GridKV Embedded Architecture

**Version**: v3.1  
**Type**: Core Concept

---

## 🎯 What is Embedded Architecture?

GridKV is a **Go-native library** that embeds directly into your application process. Unlike traditional distributed caches (Redis, Memcached) that run as separate servers, GridKV runs **inside your application**.

---

## 🚀 Traditional vs Embedded

### Traditional Architecture (Redis/Memcached)

```
┌─────────────────────────────────────────────────────────┐
│                   Your Infrastructure                    │
├─────────────────────────────────────────────────────────┤
│                                                           │
│  Application Tier:                                        │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐              │
│  │  App 1   │  │  App 2   │  │  App 3   │              │
│  │ (Go)     │  │ (Go)     │  │ (Go)     │              │
│  └────┬─────┘  └────┬─────┘  └────┬─────┘              │
│       │             │             │                      │
│       └─────────────┼─────────────┘                      │
│                     │ Network calls                      │
│                     ▼                                     │
│  Cache Tier (Separate System):                           │
│  ┌─────────────────────────────────────┐                │
│  │        Redis Cluster                 │                │
│  │  ┌────────┐ ┌────────┐ ┌────────┐  │                │
│  │  │ Redis1 │ │ Redis2 │ │ Redis3 │  │                │
│  │  │ Master │ │ Replica│ │ Replica│  │                │
│  │  └────────┘ └────────┘ └────────┘  │                │
│  └─────────────────────────────────────┘                │
│                                                           │
│  Issues:                                                  │
│  • 2 separate systems to deploy                          │
│  • 2 separate systems to monitor                         │
│  • Network latency on EVERY cache call (1-5ms)          │
│  • Complex Redis cluster configuration                   │
│  • Separate Redis operations team needed                 │
│                                                           │
└─────────────────────────────────────────────────────────┘
```

### Embedded Architecture (GridKV)

```
┌─────────────────────────────────────────────────────────┐
│                   Your Infrastructure                    │
├─────────────────────────────────────────────────────────┤
│                                                           │
│  Application Tier (GridKV Embedded):                     │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────┐│
│  │  App 1          │  │  App 2          │  │  App 3  ││
│  │ ┌────────────┐  │  │ ┌────────────┐  │  │ ┌──────┐││
│  │ │ Business   │  │  │ │ Business   │  │  │ │Biznes│││
│  │ │ Logic      │  │  │ │ Logic      │  │  │ │Logic │││
│  │ └────────────┘  │  │ └────────────┘  │  │ └──────┘││
│  │ ┌────────────┐  │◄─┼─►┌────────────┐  │◄─┼─►┌─────┐││
│  │ │  GridKV    │  │  │  │  GridKV    │  │  │ │Grid││││
│  │ │ (Embedded) │  │  │  │ (Embedded) │  │  │ │KV  │││
│  │ └────────────┘  │  │  └────────────┘  │  │ └─────┘││
│  └─────────────────┘  └─────────────────┘  └─────────┘│
│         │                    │                   │       │
│         └────────────────────┴───────────────────┘       │
│              Gossip Protocol (Auto-Clustering)           │
│                                                           │
│  Benefits:                                                │
│  ✅ 1 system to deploy (just your app)                   │
│  ✅ 1 system to monitor                                  │
│  ✅ In-process reads (43ns, no network!)                │
│  ✅ Auto-clustering (Gossip protocol)                    │
│  ✅ Simpler operations                                   │
│                                                           │
└─────────────────────────────────────────────────────────┘
```

---

## ✅ Advantages of Embedded Architecture

### 1. Zero External Dependencies

**Traditional**:
```bash
# Deploy Redis cluster
$ helm install redis bitnami/redis-cluster
$ kubectl apply -f redis-cluster.yaml
# Configure app to connect
$ kubectl apply -f app-with-redis-config.yaml
```

**GridKV**:
```bash
# Just deploy your app (GridKV inside)
$ kubectl apply -f app.yaml
# Done! ✅
```

### 2. In-Process Performance

**Redis (Even on Same Host)**:
```
App → Socket → Network stack → Redis process → Network stack → Socket → App
└─────────────── 1-5ms latency ────────────────┘
```

**GridKV (Local Data)**:
```
App → GridKV function call (in same process) → Return
└──────── 43ns latency (100x faster!) ────────┘
```

### 3. Automatic Clustering

**Redis Cluster**:
```bash
# Manual cluster setup
redis-cli --cluster create \
    host1:6379 host2:6379 host3:6379 \
    --cluster-replicas 1

# Manual failover configuration
# Sentinel setup required
```

**GridKV**:
```go
// Instance auto-joins via Gossip
kv, _ := gridkv.NewGridKV(&gridkv.GridKVOptions{
    LocalNodeID:  "instance-2",
    LocalAddress: "host2:8001",
    SeedAddrs:    []string{"host1:8001"}, // Just point to any existing instance
    // ... other config
})
// Cluster formed automatically! No manual setup needed.
```

### 4. Simplified Operations

| Aspect | Traditional (Redis) | GridKV (Embedded) |
|--------|--------------------|--------------------|
| **Deployment** | App + Redis cluster | App only |
| **Monitoring** | App metrics + Redis metrics | App metrics (GridKV included) |
| **Logging** | App logs + Redis logs | App logs only |
| **Failure Domains** | 2 (app & Redis) | 1 (app) |
| **On-Call Alerts** | App issues + Redis issues | App issues only |
| **Upgrade Process** | App + Redis separately | App only |

### 5. Resource Efficiency

**Redis Setup**:
- 3 Redis nodes (each 2GB RAM) = 6GB total
- 3 App instances (each 1GB RAM) = 3GB total
- **Total**: 9GB RAM for app + cache

**GridKV Setup**:
- 3 App instances with GridKV embedded (each 2GB RAM) = 6GB total
- **Total**: 6GB RAM for app + cache
- **Savings**: 33% fewer resources! ✅

---

## 🎯 When to Use Embedded Architecture

### ✅ Perfect For

1. **Go Microservices**
   - Native language integration
   - No FFI/RPC overhead
   - Type-safe APIs

2. **Cloud-Native Applications**
   - Kubernetes friendly (no external dependencies)
   - Docker single-container deployment
   - Serverless/edge compatible

3. **Stateful Services**
   - Session storage with business logic
   - Cache co-located with compute
   - Configuration distribution

4. **Simplified Operations**
   - Small teams (no dedicated cache ops)
   - Startup/MVP (reduce moving parts)
   - Edge deployments (minimal infrastructure)

### ⚠️ Consider External Cache When

1. **Multi-Language**
   - Need access from Python, Java, etc.
   - Shared cache across polyglot services

2. **Rich Data Structures**
   - Need Redis Lists, Sets, Sorted Sets
   - Complex operations (SCAN, ZRANGE, etc.)

3. **Existing Redis Investment**
   - Already have Redis cluster
   - Team trained on Redis operations

---

## 📊 Performance Characteristics

### Latency Comparison

| Operation | Redis (LAN) | GridKV (Local) | GridKV (Remote) |
|-----------|-------------|----------------|-----------------|
| Get | 1-5ms | **43ns** (100x faster) | <1ms |
| Set | 1-5ms | **100ns** (50x faster) | <2ms |
| Delete | 1-5ms | **100ns** (50x faster) | <2ms |

### When GridKV Reads are Fast

```go
// Scenario 1: Key is on local instance (common with consistent hashing)
data, _ := kv.Get(ctx, "session:user-123")
// ↑ 43ns - In-process function call, no network

// Scenario 2: Key is on remote instance (same DC)
data, _ := kv.Get(ctx, "session:user-456")
// ↑ <1ms - Network call to peer instance

// With proper load distribution, ~70% of reads are local!
```

---

## 🔧 Deployment Patterns

### Pattern 1: Web Application Cluster

```go
// Each web server instance embeds GridKV
func main() {
    kv, _ := gridkv.NewGridKV(&gridkv.GridKVOptions{
        LocalNodeID:  os.Getenv("HOSTNAME"),
        LocalAddress: os.Getenv("POD_IP") + ":8001",
        SeedAddrs:    getOtherInstances(), // K8s service discovery
        // ... config
    })
    defer kv.Close()
    
    // Web server with embedded session store
    http.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
        sessionID := generateSessionID()
        kv.Set(ctx, "session:"+sessionID, userData)
        // Session stored across all web server instances
    })
}
```

### Pattern 2: Microservice Mesh

```go
// Each microservice embeds GridKV for shared config/cache
type UserService struct {
    cache *gridkv.GridKV  // Embedded cache
}

func NewUserService() *UserService {
    cache, _ := gridkv.NewGridKV(&gridkv.GridKVOptions{
        LocalNodeID:  "user-service-" + instanceID,
        LocalAddress: serviceIP + ":8001",
        SeedAddrs:    discoverPeers("user-service"),
        // ... config
    })
    
    return &UserService{cache: cache}
}

// All instances of user-service share cache
```

### Pattern 3: Edge Deployment

```go
// Edge nodes (CDN, IoT gateways) with embedded cache
func main() {
    kv, _ := gridkv.NewGridKV(&gridkv.GridKVOptions{
        LocalNodeID:  "edge-" + location,
        LocalAddress: publicIP + ":8001",
        SeedAddrs:    regionalHub, // Connect to regional cluster
        DataCenter:   location,     // "cdn-us-west", "cdn-eu-central"
        // ... config
    })
    
    // Edge nodes with embedded cache, auto-sync with hub
}
```

---

## ⚙️ How It Works

### 1. Application Startup

```go
// When your app starts
kv, _ := gridkv.NewGridKV(options)

// GridKV:
// 1. Starts Gossip protocol (SWIM)
// 2. Connects to seed nodes
// 3. Discovers cluster members
// 4. Joins the cluster automatically
// 5. Starts syncing data
```

### 2. Data Operations

```go
// When you write data
kv.Set(ctx, "key", value)

// GridKV:
// 1. Computes consistent hash → finds N nodes
// 2. Writes to W nodes in parallel (quorum)
// 3. Returns success after W confirmations
// 4. Continues async replication to remaining nodes
```

### 3. Failure Handling

```
Instance crashes:
  ↓
Other instances detect (SWIM, <1s)
  ↓
Mark instance as dead
  ↓
Re-route traffic automatically
  ↓
Re-replicate data to healthy instances
  ↓
No manual intervention needed ✅
```

---

## 🎯 Best Practices

### 1. Instance Sizing

```go
// Reserve memory for GridKV (rule of thumb: 20-30% of instance memory)
// Example: 4GB instance → 1GB for GridKV

Storage: &storage.StorageOptions{
    Backend: storage.BackendMemorySharded,
    MaxMemoryMB: 1024,  // 1GB limit
}
```

### 2. Seed Nodes

```go
// Provide multiple seed nodes for redundancy
SeedAddrs: []string{
    "instance-1:8001",
    "instance-2:8001",
    "instance-3:8001", // At least 2-3 seeds recommended
}
```

### 3. Graceful Shutdown

```go
func main() {
    kv, _ := gridkv.NewGridKV(options)
    defer kv.Close()  // Always use defer for cleanup
    
    // Setup signal handler
    sigChan := make(chan os.Signal, 1)
    signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
    
    <-sigChan
    // defer kv.Close() will run here
}
```

---

## 📊 Resource Usage

### Memory

```
Per GridKV instance:
- Base overhead: ~50MB (Gossip, hash ring, etc.)
- Per key: ~100 bytes overhead + value size
- Configurable limit: MaxMemoryMB setting

Example:
- 10,000 keys × 1KB average value = ~10MB
- Base overhead = ~50MB
- Total = ~60MB per instance
```

### CPU

```
- Idle: < 1% CPU (periodic Gossip heartbeats)
- Under load: Scales with request rate
- 1M ops/s ≈ 1-2 CPU cores
```

### Network

```
- Gossip overhead: ~10KB/s per instance (heartbeats)
- Replication: Proportional to write rate
- Cross-DC: Async, minimal bandwidth
```

---

## 🔒 Security Considerations

### Network Security

```go
// Enable message signing for untrusted networks
GridKVOptions{
    EnableCrypto: true,  // Ed25519 signatures
    // Adds ~15µs overhead per Gossip message
}
```

### Data Isolation

- Each GridKV instance has isolated storage
- No shared memory between instances
- Data transfer only via network (encrypted if enabled)

---

## 🎉 Summary

**Embedded Architecture Benefits**:

| Benefit | Impact |
|---------|--------|
| **Zero external servers** | Simpler deployment |
| **In-process reads** | 100x faster (43ns vs 1-5ms) |
| **Auto-clustering** | No manual configuration |
| **One system** | Easier operations |
| **Go-native** | Type-safe, no FFI overhead |
| **Resource efficient** | 33% less memory |

**Trade-offs**:

| Limitation | Workaround |
|------------|------------|
| Go-only | Use Redis for multi-language |
| KV-only | Use Redis for List/Set/ZSet |
| Memory-only | External persistence for durability |

---

**GridKV Embedded Architecture** - Simplicity meets Distribution ✅

**Last Updated**: 2025-11-09  
**GridKV Version**: v3.1

