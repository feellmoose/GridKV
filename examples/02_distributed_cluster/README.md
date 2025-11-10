# Scenario 2: Strong Consistency Configuration

## 📖 Scenario Description

This scenario provides strong consistency guarantees, suitable for scenarios with extremely high data consistency requirements, such as financial transactions, order systems, inventory management, etc.

### Use Cases

✅ **Financial Transactions**: Transfers, payments, settlements  
✅ **Order Systems**: Order status, payment status  
✅ **Inventory Management**: Stock deduction, stock queries  
✅ **Critical Business Data**: Configuration, permissions, state  

---

## 🎯 Performance Expectations

| Metric | Performance |
|--------|-------------|
| **Reads** | 2-3M ops/s |
| **Writes** | 1-2M ops/s |
| **Consistency** | **Strong consistency** |
| **P99 Latency** | < 2ms |

---

## ⚙️ Core Configuration

### Strong Consistency Guarantee

```go
ReplicaCount: 3,  // 3 replicas
WriteQuorum:  2,  // Must write to 2 replicas
ReadQuorum:   2,  // Must read from 2 replicas ⚡
```

**Consistency Proof**:
```
W + R > N
2 + 2 > 3  ✅

This ensures that the read and write sets must overlap,
so read operations must be able to read the latest written data.
```

### Fast Fail Strategy

```go
ReplicationTimeout: 1 * time.Second,  // Short timeout
ReadTimeout:        1 * time.Second,
```

**Design Philosophy**:
- Fast fail, avoid long waits
- 1 second timeout is sufficient for 99% of normal requests
- Can retry after failure

---

## 📐 Consistency Model

### Strong Consistency vs Eventual Consistency

| Comparison | Strong Consistency (This Config) | Eventual Consistency |
|-----------|----------------------------------|---------------------|
| **R+W vs N** | R+W > N | R+W ≤ N |
| **Read Guarantee** | Must read latest data | May read stale data |
| **Write Guarantee** | Write immediately visible | Write visible with delay |
| **Performance** | Lower | Higher |
| **Use Case** | Finance, Orders | Cache, Sessions |

### Consistency Diagram

```
Strong consistency guarantee with 3 replicas, W=2, R=2:

Timeline:
t0: Write begins
    Node1: [empty] → Writing
    Node2: [empty] → Writing
    Node3: [empty]

t1: Write completes (W=2)
    Node1: [v1] ✅
    Node2: [v1] ✅
    Node3: [empty] (Async syncing)

t2: Read (R=2)
    Read Node1: [v1]
    Read Node2: [v1]
    Compare: v1 == v1 ✅
    Return: v1 (Guaranteed to be latest)

Conclusion: Even if Node3 hasn't synced yet,
           reads can still guarantee reading the latest value
```

---

## 🚀 Run Example

```bash
cd examples/02_strong_consistency
go run main.go
```

### Expected Output

```
═══════════════════════════════════════════════════════
  GridKV Scenario 2: Strong Consistency Configuration
═══════════════════════════════════════════════════════

📦 Creating node 1...
✅ Node 1 created successfully
📦 Creating node 2...
✅ Node 2 created successfully
📦 Creating node 3...
✅ Node 3 created successfully

⏳ Waiting for cluster convergence...
✅ Strong consistency cluster ready

═══════════════════════════════════════════════════════
  Demo: Strong Consistency Guarantee
═══════════════════════════════════════════════════════

1️⃣  Writing data to node 1...
   ✅ Write successful (written to 2 replicas)

2️⃣  Reading data from node 2...
   ✅ Read successful: status:pending
   ✅ Data consistent (read from 2 replicas and compared)

3️⃣  Updating order status...
   ✅ Update successful (written to 2 replicas)

4️⃣  Verifying consistency (reading from all nodes)...
   Node 1: status:completed
   Node 2: status:completed
   Node 3: status:completed
   ✅ All nodes data consistent

═══════════════════════════════════════════════════════
  Performance Testing
═══════════════════════════════════════════════════════

📊 Test: Sequential writes
─────────────────────────────────────────────────────
✅ Completed 10000/10000 operations
✅ Throughput: 1850432.90 ops/s (1.85M ops/s)
✅ Average latency: 540ns

📊 Test: Sequential reads
─────────────────────────────────────────────────────
✅ Completed 10000/10000 operations
✅ Throughput: 2654867.26 ops/s (2.65M ops/s)
✅ Average latency: 376ns
```

---

## 💡 Use Case Details

### 1. Financial Transaction System

```go
// Transfer operation - requires strong consistency
func transfer(from, to string, amount float64) error {
    // Read balance (R=2, guaranteed to read latest balance)
    balance, _ := kv.Get(ctx, "balance:"+from)
    
    // Deduct balance
    newBalance := currentBalance - amount
    
    // Write balance (W=2, guaranteed write success)
    kv.Set(ctx, "balance:"+from, newBalance)
    kv.Set(ctx, "balance:"+to, toBalance+amount)
    
    // Strong consistency guarantee: 
    // Subsequent reads must see the updated balance
    return nil
}
```

### 2. Order System

```go
// Order status update
func updateOrderStatus(orderID, status string) error {
    // W=2 guarantees write success
    err := kv.Set(ctx, "order:"+orderID, status)
    
    // R=2 guarantees reading latest status
    currentStatus, _ := kv.Get(ctx, "order:"+orderID)
    
    // Guarantee: currentStatus == status
    return err
}
```

### 3. Inventory Management

```go
// Stock deduction - prevent overselling
func decreaseStock(productID string, quantity int) error {
    // R=2 read current stock (guaranteed latest)
    stock, _ := kv.Get(ctx, "stock:"+productID)
    
    if stock >= quantity {
        // W=2 update stock (guaranteed write)
        newStock := stock - quantity
        return kv.Set(ctx, "stock:"+productID, newStock)
    }
    
    return errors.New("insufficient stock")
}
```

---

## 🔍 Performance Analysis

### Performance Comparison

| Configuration | Read Performance | Write Performance | Consistency |
|--------------|-----------------|-------------------|-------------|
| **Strong Consistency (R=2,W=2)** | 2-3M | 1-2M | Strong |
| Eventual Consistency (R=1,W=1) | 6-7M | 4-5M | Eventual |
| Single Replica (R=1,W=1) | 7M+ | 5M+ | No guarantee |

### Why Lower Performance?

1. **Need to wait for multiple replicas**
   - Writes must wait for 2 replicas
   - Reads must access 2 replicas
   
2. **Need to compare data**
   - Compare version numbers when reading
   - May trigger read repair

3. **Network overhead**
   - Double the network requests
   - RPC call latency accumulates

---

## ⚠️ Considerations

### Availability Requirements

```
3 replicas + W=2, R=2:
- Need at least 2 nodes online
- 1 node failure: ✅ Still available
- 2 node failures: ❌ Unavailable

Recommendation: 5 replicas + W=3, R=3
- Can tolerate 2 node failures
```

### Performance Trade-offs

```
If performance is insufficient:
1. Increase node count (horizontal scaling)
2. Use 5 replicas + W=3, R=2 (improve read performance)
3. Optimize network configuration
```

### Timeout Settings

```
Too short: Network jitter causes failures
Too long: Blocking time too long

Recommendations:
- Intranet: 1-2 seconds
- Cross data center: 3-5 seconds
```

---

## 📚 Related Resources

- [Consistency Model Details](../../docs/CONSISTENCY_MODEL.md)
- [Quorum Read/Write Principles](../../docs/QUORUM.md)
- [Scenario Comparison](../README.md)

---

**Use Case**: Finance, Orders, Inventory, Critical Business  
**Consistency Level**: ⭐⭐⭐⭐⭐ (Strong Consistency)  
**Recommendation**: ⭐⭐⭐⭐⭐
