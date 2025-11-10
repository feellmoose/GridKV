# GridKV Deployment Guide

**Version**: v3.1  
**Audience**: DevOps, Platform Engineers

---

## 🎯 Deployment Models

GridKV supports multiple deployment models, all using the **embedded architecture** (no external servers).

---

## 🐳 Docker Deployment

### Single Container

```dockerfile
# Your application Dockerfile
FROM golang:1.23 AS builder
WORKDIR /app

# Copy dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build
COPY . .
RUN go build -o myapp .  # GridKV compiled into binary

FROM alpine:latest
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/myapp /app/myapp

# Set environment variables for GridKV
ENV GRIDKV_NODE_ID=""
ENV GRIDKV_ADDR=""
ENV GRIDKV_SEEDS=""

CMD ["/app/myapp"]
```

**Key Point**: GridKV is compiled into your application binary - no separate cache container needed!

### Docker Compose (3-Instance Cluster)

```yaml
version: '3.8'
services:
  app-1:
    build: .
    environment:
      - GRIDKV_NODE_ID=app-1
      - GRIDKV_ADDR=app-1:8001
      - GRIDKV_SEEDS=app-2:8001,app-3:8001
    networks:
      - gridkv-net
    
  app-2:
    build: .
    environment:
      - GRIDKV_NODE_ID=app-2
      - GRIDKV_ADDR=app-2:8001
      - GRIDKV_SEEDS=app-1:8001,app-3:8001
    networks:
      - gridkv-net
    
  app-3:
    build: .
    environment:
      - GRIDKV_NODE_ID=app-3
      - GRIDKV_ADDR=app-3:8001
      - GRIDKV_SEEDS=app-1:8001,app-2:8001
    networks:
      - gridkv-net

networks:
  gridkv-net:
    driver: bridge
```

**Notice**: No separate Redis service! GridKV embedded in app containers.

---

## ☸️ Kubernetes Deployment

### StatefulSet

```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: myapp
spec:
  serviceName: myapp
  replicas: 3
  selector:
    matchLabels:
      app: myapp
  template:
    metadata:
      labels:
        app: myapp
    spec:
      containers:
      - name: myapp
        image: mycompany/myapp:latest  # Your app with GridKV embedded
        env:
        - name: GRIDKV_NODE_ID
          valueFrom:
            fieldRef:
              fieldPath: metadata.name  # myapp-0, myapp-1, myapp-2
        - name: GRIDKV_ADDR
          valueFrom:
            fieldRef:
              fieldPath: status.podIP
        - name: GRIDKV_SEEDS
          value: "myapp-0.myapp:8001,myapp-1.myapp:8001,myapp-2.myapp:8001"
        ports:
        - containerPort: 8001
          name: gridkv
        - containerPort: 8080
          name: http
---
apiVersion: v1
kind: Service
metadata:
  name: myapp
spec:
  clusterIP: None  # Headless service for StatefulSet
  selector:
    app: myapp
  ports:
  - port: 8001
    name: gridkv
  - port: 8080
    name: http
```

**Key Points**:
- StatefulSet provides stable network identities
- Headless service enables instance discovery
- No separate cache deployment needed
- Each pod embeds GridKV

### Deployment (Alternative)

If you don't need stable identities, use Deployment:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: myapp
spec:
  replicas: 3
  template:
    spec:
      containers:
      - name: myapp
        image: mycompany/myapp:latest
        env:
        - name: GRIDKV_NODE_ID
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        - name: GRIDKV_ADDR
          valueFrom:
            fieldRef:
              fieldPath: status.podIP
        - name: GRIDKV_SEEDS
          value: "myapp.default.svc.cluster.local:8001"
        # Use service DNS for seed discovery
```

---

## 🌍 Multi-Region Deployment

### Cross-Region Setup

```yaml
# us-east-1 cluster
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: myapp-us-east
spec:
  replicas: 3
  template:
    spec:
      containers:
      - name: myapp
        env:
        - name: GRIDKV_DATA_CENTER
          value: "us-east-1"
        - name: GRIDKV_SEEDS
          value: "myapp-us-east-0:8001,myapp-eu-west-0:8001"  # Cross-region seeds

---
# eu-west-1 cluster  
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: myapp-eu-west
spec:
  replicas: 3
  template:
    spec:
      containers:
      - name: myapp
        env:
        - name: GRIDKV_DATA_CENTER
          value: "eu-west-1"
        - name: GRIDKV_SEEDS
          value: "myapp-eu-west-0:8001,myapp-us-east-0:8001"  # Cross-region seeds
```

**GridKV automatically**:
- Detects US ←→ EU as WAN (high latency)
- Slows Gossip interval for WAN
- Prefers local region for reads
- Async-replicates across regions

---

## 🔧 Production Configuration

### Recommended Settings

```go
// Production-ready configuration
&gridkv.GridKVOptions{
    // Identity
    LocalNodeID:  os.Getenv("HOSTNAME"),
    LocalAddress: os.Getenv("POD_IP") + ":8001",
    SeedAddrs:    strings.Split(os.Getenv("GRID KV_SEEDS"), ","),
    
    // Replication (N=3, W=2, R=2 for balance)
    ReplicaCount: 3,
    WriteQuorum:  2,
    ReadQuorum:   2,
    
    // Storage (high-performance sharded)
    Storage: &storage.StorageOptions{
        Backend:     storage.BackendMemorySharded,
        Shards:      64,    // 2-4x CPU cores
        MaxMemoryMB: 2048,  // Limit memory usage
    },
    
    // Network
    Network: &gossip.NetworkOptions{
        Type:     gossip.TCP,
        BindAddr: os.Getenv("POD_IP") + ":8001",
        MaxConns: 1000,
    },
    
    // Multi-DC (if applicable)
    DataCenter: os.Getenv("REGION"), // "us-east", "eu-west", etc.
    
    // Timeouts (tune based on network)
    GossipInterval:     1 * time.Second,  // Fast for LAN
    FailureTimeout:     5 * time.Second,  // Mark suspect
    ReplicationTimeout: 2 * time.Second,  // Write timeout
}
```

---

## 📊 Resource Planning

### Memory

```
Per instance memory calculation:
= Base overhead (~50MB)
+ (Number of keys × Average value size × Replication factor / Cluster size)
+ 20% buffer

Example:
- 1M keys total
- 1KB average value
- 3 replicas
- 10 instances
= 50MB + (1M × 1KB × 3 / 10) + 20%
= 50MB + 300MB + 60MB
= ~410MB per instance

Set: MaxMemoryMB = 512  # With buffer
```

### CPU

```
Light load (<100K ops/s):  0.1-0.5 CPU cores
Medium load (1M ops/s):    1-2 CPU cores
Heavy load (5M+ ops/s):    4-8 CPU cores

Recommendation: 2-4 CPU cores per instance
```

### Network

```
Gossip overhead:  ~10-50 KB/s (heartbeats)
Replication:      Depends on write rate
  1K writes/s × 1KB avg × 3 replicas = ~3 MB/s

Recommendation: 100 Mbps+ for production
```

---

## 🔒 Security

### Network Security

```go
// Enable in untrusted networks
GridKVOptions{
    EnableCrypto: true,  // Ed25519 message signing
}
```

### Firewall Rules

```bash
# Allow GridKV port (8001 by default)
# Inbound from other instances
iptables -A INPUT -p tcp --dport 8001 -s <instance-ip> -j ACCEPT

# Or in Kubernetes NetworkPolicy
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-gridkv
spec:
  podSelector:
    matchLabels:
      app: myapp
  ingress:
  - from:
    - podSelector:
        matchLabels:
          app: myapp
    ports:
    - protocol: TCP
      port: 8001
```

---

## 📈 Scaling

### Horizontal Scaling

```bash
# Kubernetes: Scale up
kubectl scale statefulset myapp --replicas=5

# GridKV automatically:
# - New instances join via seed nodes
# - Data rebalances via consistent hashing
# - ~1/M keys move (M = new number of instances)
```

### Vertical Scaling

```yaml
# Increase resources per instance
resources:
  limits:
    memory: "4Gi"   # More memory for larger cache
    cpu: "2"        # More CPU for higher throughput
  requests:
    memory: "2Gi"
    cpu: "1"
```

---

## 🎯 Best Practices

1. **Use StatefulSet** - Provides stable network identities
2. **Set Memory Limits** - Prevent OOM
3. **Configure Liveness Probe** - Detect unhealthy instances
4. **Use Multiple Seed Nodes** - Redundancy in discovery
5. **Monitor Metrics** - Export to Prometheus
6. **Tune Shard Count** - 2-4x CPU cores
7. **Set Appropriate Quorum** - Balance consistency and latency

---

**GridKV Deployment Guide** - Production-Ready Deployment ✅

**Last Updated**: 2025-11-09  
**GridKV Version**: v3.1
