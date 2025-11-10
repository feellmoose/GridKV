# GridKV Examples

Complete examples demonstrating GridKV usage in real-world scenarios.

---

## 🚀 Quick Start

### Minimal Example

```go
import gridkv "github.com/feellmoose/gridkv"

// Create a GridKV instance
kv, _ := gridkv.NewGridKV(&gridkv.GridKVOptions{
    LocalNodeID:  "node-1",
    LocalAddress: "localhost:8001",
    Network:      &gridkv.NetworkOptions{Type: gridkv.TCP, BindAddr: "localhost:8001"},
    Storage:      &gridkv.StorageOptions{Backend: gridkv.BackendMemorySharded},
})
defer kv.Close()

// Use it
kv.Set(context.Background(), "key", []byte("value"))
data, _ := kv.Get(context.Background(), "key")
```

---

## 📚 Example List

| Example | Description | Key Features |
|---------|-------------|--------------|
| [01_quickstart](01_quickstart/) | Single-node setup | Basic CRUD, QuickStart |
| [02_distributed_cluster](02_distributed_cluster/) | 3-node cluster | Multi-node, Replication |
| [03_multi_dc](03_multi_dc/) | Multi-datacenter | Geo-distribution, WAN |
| [04_high_performance](04_high_performance/) | Performance tuning | High throughput, Low latency |
| [05_production_ready](05_production_ready/) | Production config | Best practices, Monitoring |
| [08_production_pitfalls](08_production_pitfalls/) | Common mistakes | Troubleshooting, Optimization |
| [10_adaptive_network](10_adaptive_network/) | Adaptive networking | LAN/WAN detection, Auto-tuning |
| [11_metrics_export](11_metrics_export/) | Metrics & monitoring | Prometheus, OTLP |

---

## 🎯 Examples by Use Case

### Session Management
→ [01_quickstart](01_quickstart/) - Basic session storage
→ [02_distributed_cluster](02_distributed_cluster/) - Distributed sessions across web servers

### API Response Caching
→ [04_high_performance](04_high_performance/) - High-throughput API cache
→ [05_production_ready](05_production_ready/) - Production-grade API cache

### Multi-Region Deployment
→ [03_multi_dc](03_multi_dc/) - Cross-region data distribution
→ [10_adaptive_network](10_adaptive_network/) - Adaptive multi-DC setup

### Configuration Distribution
→ [02_distributed_cluster](02_distributed_cluster/) - Distributed config management

### Rate Limiting
→ [04_high_performance](04_high_performance/) - High-performance counter storage

---

## 🏃 Running Examples

### Run Single Example

```bash
cd examples/01_quickstart
go run main.go
```

### Run All Examples (Sequential)

```bash
for dir in examples/*/; do
    echo "Running: $dir"
    (cd "$dir" && go run main.go)
done
```

---

## 📖 Example Structure

Each example includes:
- `main.go` - Complete working code
- `README.md` - Detailed explanation
- Comments explaining key concepts
- Real-world use case context

---

## 🔧 Configuration Examples

### Development (Simple)

```go
&gridkv.GridKVOptions{
    LocalNodeID:  "dev-node",
    LocalAddress: "localhost:8001",
    Network:      &gridkv.NetworkOptions{Type: gridkv.TCP, BindAddr: ":8001"},
    Storage:      &gridkv.StorageOptions{Backend: gridkv.BackendMemory},
}
```

### Production (Recommended)

```go
&gridkv.GridKVOptions{
    LocalNodeID:  os.Getenv("NODE_ID"),
    LocalAddress: os.Getenv("NODE_ADDR") + ":8001",
    SeedAddrs:    strings.Split(os.Getenv("SEED_ADDRS"), ","),
    
    Network: &gridkv.NetworkOptions{
        Type:     gridkv.TCP,
        BindAddr: ":8001",
        MaxConns: 2000,
        MaxIdle:  200,
    },
    
    Storage: &gridkv.StorageOptions{
        Backend:     gridkv.BackendMemorySharded,
        ShardCount:  256,
        MaxMemoryMB: 2048,
    },
    
    ReplicaCount: 3,
    WriteQuorum:  2,
    ReadQuorum:   2,
}
```

### High-Performance (Maximum Throughput)

```go
&gridkv.GridKVOptions{
    LocalNodeID:  "perf-node",
    LocalAddress: "localhost:8001",
    
    Network: &gridkv.NetworkOptions{
        Type:     gridkv.GNET,  // High-performance (Linux/macOS)
        BindAddr: ":8001",
        MaxConns: 10000,
    },
    
    Storage: &gridkv.StorageOptions{
        Backend:     gridkv.BackendMemorySharded,
        ShardCount:  256,  // Maximum concurrency
        MaxMemoryMB: 8192,
    },
    
    ReadQuorum: 1,  // Fast reads
}
```

---

## 💡 Best Practices

### 1. Always Use Defer for Cleanup

```go
kv, err := gridkv.NewGridKV(opts)
if err != nil {
    log.Fatal(err)
}
defer kv.Close()  // ✅ Always cleanup
```

### 2. Use Context with Timeout

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

err := kv.Set(ctx, key, value)
```

### 3. Handle Errors Properly

```go
value, err := kv.Get(ctx, key)
if err != nil {
    log.Printf("Get failed: %v", err)
    return
}
```

### 4. Configure for Your Use Case

- **Session storage**: N=3, W=2, R=1 (fast reads)
- **Critical data**: N=5, W=3, R=3 (strong consistency)
- **Cache layer**: N=3, W=1, R=1 (low latency)

---

## 🔍 Troubleshooting

### Example Won't Start

Check:
1. Port not in use: `lsof -i :8001`
2. Firewall allows port
3. Correct Go version: `go version` (need 1.23+)

### Can't Connect to Seed Nodes

Check:
1. Seed nodes are running
2. Network connectivity: `ping <seed_addr>`
3. SeedAddrs format: `"host:port"` not `"http://host:port"`

### High Memory Usage

Adjust:
```go
Storage: &gridkv.StorageOptions{
    MaxMemoryMB: 1024,  // Reduce limit
}
```

---

## 📚 Learn More

- [Main README](../README.md) - Project overview
- [API Reference](../docs/API_REFERENCE.md) - Complete API docs
- [Deployment Guide](../docs/DEPLOYMENT_GUIDE.md) - Production deployment

---

**GridKV** - Go-Native Distributed Cache  
*Examples updated for v3.1*
