# Scenario 3: High Availability Configuration

## 📖 Scenario Description

This scenario provides 99.99%+ high availability guarantee, suitable for 7x24 hour core services.

### Use Cases

✅ **Core Services**: 24/7 running critical services  
✅ **Financial Systems**: Payment, clearing systems  
✅ **E-commerce Platforms**: Order, shopping cart services  
✅ **SaaS Services**: Multi-tenant core services  

---

## 🎯 Performance Expectations

| Metric | Performance |
|--------|-------------|
| **Reads** | 3-4M ops/s |
| **Writes** | 2-3M ops/s |
| **Availability** | 99.99%+ |
| **Fault Tolerance** | 2 nodes |

---

## ⚙️ Core Configuration

### High Availability Guarantee

```go
ReplicaCount: 5,  // 5 replicas
WriteQuorum:  3,  // Write to 3 replicas
ReadQuorum:   2,  // Read from 2 replicas
```

**Fault Tolerance Capability**:
```
5 replica configuration:
- 0 failures: ✅ Normal service
- 1 failure: ✅ Normal service
- 2 failures: ✅ Still serviceable (degraded)
- 3 failures: ❌ Unavailable

Tolerance: 2/5 = 40%
```

### Long Timeout Configuration

```go
ReadTimeout:  10 * time.Second,
WriteTimeout: 10 * time.Second,
FailureTimeout: 10 * time.Second,
SuspectTimeout: 20 * time.Second,
```

**Design Philosophy**:
- Tolerate network jitter and latency
- Avoid false positives in node failure detection
- Suitable for cross-data center deployment

---

## 📊 Availability Calculation

### SLA Guarantee

```
Single node availability: 99%
5 replica availability: 1 - (0.01)^5 = 99.99999%

Actual considerations:
- Network partitions
- Maintenance windows
- Software bugs

Expected availability: 99.99% (Four nines)
```

### Fault Recovery Time

| Scenario | Detection Time | Recovery Time | Total |
|----------|---------------|---------------|-------|
| Single node failure | 10s | Immediate | 10s |
| 2 node failures | 10s | Immediate | 10s |
| Node restart | 20s | 30s | 50s |

---

## 🚀 Run Example

```bash
cd examples/03_high_availability
go run main.go
```

### Expected Output

```
═══════════════════════════════════════════════════════
  GridKV Scenario 3: High Availability Configuration
═══════════════════════════════════════════════════════

📦 Creating node 1...
✅ Node 1 created successfully
...
📦 Creating node 5...
✅ Node 5 created successfully

⏳ Waiting for cluster convergence...
✅ High availability cluster ready (5 nodes)

═══════════════════════════════════════════════════════
  Demo: High Availability Scenario
═══════════════════════════════════════════════════════

1️⃣  Writing test data...
   ✅ Write successful (written to 3 replicas)

2️⃣  Verifying data replicated to all nodes...
   ✅ Node 1: running
   ✅ Node 2: running
   ✅ Node 3: running
   ✅ Node 4: running
   ✅ Node 5: running
   ✅ All nodes data consistent

3️⃣  Simulating node failures (shutting down nodes 4 and 5)...
   ✅ Nodes 4 and 5 shut down

4️⃣  Verifying service still available...
   ✅ Read successful: running
   ✅ Service normal (3 nodes remaining)

5️⃣  Continue writing new data...
   ✅ Write successful (still can write to 3 replicas)

6️⃣  Verifying data on remaining nodes...
   ✅ Node 1: healthy
   ✅ Node 2: healthy
   ✅ Node 3: healthy
```

---

## 💡 Configuration Trade-offs

### R/W Configuration Comparison

| Configuration | Read Performance | Write Performance | Consistency | Availability |
|--------------|-----------------|-------------------|-------------|--------------|
| R=1,W=1 | High | High | Eventual | Medium |
| R=2,W=2 | Medium | Medium | Strong | Medium |
| **R=2,W=3** | **Medium** | **Medium-Low** | **Strong** | **High** |
| R=3,W=3 | Low | Low | Strong | Highest |

**This Configuration Choice R=2,W=3**:
- W=3 ensures data durability
- R=2 ensures consistency
- R+W=5 > N, strong consistency
- Can tolerate 2 node failures

---

## 🔍 Monitoring and Alerting

### Key Metrics

```go
// Need to monitor
- Node health status
- Cluster size
- Available node count
- Write success rate
- Read success rate
- P99 latency
```

### Alert Rules

```
Critical:
- Available nodes ≤ 3 (cannot tolerate more failures)
- Write success rate < 95%

Warning:
- Available nodes = 4
- Write success rate < 98%
- P99 latency > 5s
```

---

## 📚 Related Documentation

- [Fault Tolerance Report](../../FAULT_TOLERANCE_REPORT.md)
- [SWIM Protocol](../../docs/GOSSIP_PROTOCOL.md)
- [Scenario Comparison](../README.md)

---

**Use Case**: Core services, critical business  
**Availability Level**: ⭐⭐⭐⭐⭐  
**Recommendation**: ⭐⭐⭐⭐
