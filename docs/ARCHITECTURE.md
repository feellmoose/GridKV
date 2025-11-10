# GridKV Architecture

**Version**: v3.1  
**Type**: System Design

---

## 🏗️ Overview

GridKV is an **embedded distributed key-value cache** built on proven distributed systems principles:
- Consistent hashing (Dynamo)
- Gossip protocol (SWIM)
- Quorum replication
- Hybrid Logical Clock (HLC)

---

## 📐 System Architecture

```
┌─────────────────────────────────────────────────────────────┐
│             GridKV Instance (Embedded in App)                │
├─────────────────────────────────────────────────────────────┤
│                                                               │
│  ┌────────────────────────────────────────────────────────┐ │
│  │                    Public API                           │ │
│  │  Set(key, value) │ Get(key) │ Delete(key)             │ │
│  └──────────────────┬─────────────────────────────────────┘ │
│                     │                                         │
│  ┌──────────────────▼─────────────────────────────────────┐ │
│  │              Gossip Manager                             │ │
│  │  • Cluster membership (SWIM)                           │ │
│  │  • Failure detection (<1s)                             │ │
│  │  • Quorum replication (N/W/R)                          │ │
│  │  • Data synchronization                                │ │
│  └──────┬────────────────────┬──────────────────┬─────────┘ │
│         │                    │                  │            │
│  ┌──────▼────────┐  ┌────────▼────────┐  ┌─────▼────────┐  │
│  │ Consistent    │  │  Storage        │  │  Network     │  │
│  │ Hash Ring     │  │  Backend        │  │  Transport   │  │
│  │               │  │                 │  │              │  │
│  │ • 150 vnodes  │  │ • MemSharded    │  │ • TCP        │  │
│  │ • O(log n)    │  │ • 32-64 shards  │  │ • Gossip     │  │
│  │   lookup      │  │ • 1M+ ops/s     │  │ • Adaptive   │  │
│  └───────────────┘  └─────────────────┘  └──────────────┘  │
│                                                               │
└─────────────────────────────────────────────────────────────┘
```

---

## 🔄 Core Components

### 1. Consistent Hash Ring

**Purpose**: Distribute keys evenly across instances

**Implementation**: Dynamo-style with virtual nodes

**Algorithm**:
```
1. Hash each instance R times (R = 150 virtual nodes)
2. Place virtual nodes on hash ring (0 to 2^32-1)
3. For a key: hash(key) → find next node clockwise
4. For replication: find N consecutive unique nodes
```

**Properties**:
- Load balance: Virtual nodes improve uniformity
- Minimal disruption: Only 1/M keys move when M nodes change
- Deterministic: Same key always maps to same nodes

**Performance**:
- Lookup: O(log n) binary search
- Add node: O(R + N) sorted merge
- Remove node: O(R + N) filtered scan

See: [CONSISTENT_HASHING paper](https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf)

### 2. Gossip Protocol (SWIM)

**Purpose**: Cluster membership and failure detection

**Components**:
- **Membership**: Track which instances are alive/suspect/dead
- **Failure Detection**: Probe instances, detect failures in <1s
- **Dissemination**: Spread membership updates via epidemic broadcast

**How it Works**:
```
Every 1 second (GossipInterval):
  1. Select K random peers (fanout)
  2. Send membership state
  3. Receive their state
  4. Merge states (newest wins)
  5. Mark unresponsive instances as suspect
  6. Mark long-suspect instances as dead
```

**Adaptive**: Adjusts interval based on network latency (LAN vs WAN)

See: [GOSSIP_PROTOCOL.md](GOSSIP_PROTOCOL.md)

### 3. Storage Backend

**Purpose**: Local key-value storage on each instance

**Implementations**:
- **Memory**: Simple sync.Map (dev/test)
- **MemorySharded**: 32-64 sharded maps (production)

**Features**:
- Thread-safe concurrent access
- Deep-copy semantics (returned data safe to modify)
- TTL support (optional expiration)
- Sync buffer for Gossip replication

**Performance**:
- MemorySharded: 1-2M+ ops/s (recommended)
- Memory: 600-700K ops/s

See: [STORAGE_BACKENDS.md](STORAGE_BACKENDS.md)

### 4. Network Transport

**Purpose**: Inter-instance communication

**Protocol**: TCP (reliable, ordered delivery)

**Features**:
- Connection pooling (reuse connections)
- Adaptive timeouts (based on RTT)
- Auto-reconnect on failure

See: [TRANSPORT_LAYER.md](TRANSPORT_LAYER.md)

### 5. Hybrid Logical Clock (HLC)

**Purpose**: Distributed timestamps for conflict resolution

**Properties**:
- Causality: A → B implies HLC(A) < HLC(B)
- Bounded drift: Stays within ε of physical time
- Monotonic: Never decreases

**Usage**: Version numbers for last-write-wins

See: [HYBRID_LOGICAL_CLOCK.md](HYBRID_LOGICAL_CLOCK.md)

---

## 🔀 Data Flow

### Write Operation (Set)

```
Application calls kv.Set(ctx, "user:123", data)
  ↓
1. Consistent Hash: Find N instances for "user:123"
   → ["instance-2", "instance-1", "instance-3"]
  ↓
2. Generate HLC timestamp (version)
  ↓
3. Write to N instances in parallel
  ↓
4. Wait for W confirmations (quorum)
  ↓
5. Return success to application
  ↓
6. Continue async replication to remaining instances
```

**Latency**: ~2ms for W=2 (LAN)

### Read Operation (Get)

```
Application calls kv.Get(ctx, "user:123")
  ↓
1. Consistent Hash: Find N instances for "user:123"
   → ["instance-2", "instance-1", "instance-3"]
  ↓
2. Check if local instance is in N
   → Yes: Read locally (43ns, in-process) ✅
   → No: Read from R remote instances
  ↓
3. If remote: Read from R instances in parallel
  ↓
4. Return value with highest version (newest)
  ↓
5. If versions differ: Trigger read-repair (async)
```

**Latency**: 43ns (local) or ~1ms (remote, LAN)

### Failure Detection

```
Every 1 second:
  ↓
1. Select random instance to probe
  ↓
2. Send ping
  ↓
3a. Response received → Mark as alive ✅
3b. No response → Mark as suspect ⚠️
  ↓
4. If suspect > SuspectTimeout (10s) → Mark as dead ❌
  ↓
5. Broadcast state change via Gossip
  ↓
6. Other instances re-route traffic
```

---

## 🌍 Multi-Datacenter Support

GridKV automatically detects and optimizes for multi-DC deployments:

```
Instance A (US-East) ←→ Instance B (US-West):
  Measure RTT: 20ms → Classify as LAN
  → Fast Gossip interval (1s)
  → Sync reads preferred

Instance A (US-East) ←→ Instance C (EU-West):
  Measure RTT: 150ms → Classify as WAN
  → Slow Gossip interval (4s)
  → Async replication
  → Nearest DC reads
```

**Automatic Adaptation**:
- ✅ RTT measurement between all instances
- ✅ Dynamic Gossip interval adjustment
- ✅ Locality-aware read routing
- ✅ Cross-DC async replication

---

## 🔐 Security

### Message Signing (Optional)

```go
GridKVOptions{
    EnableCrypto: true,  // Enable Ed25519 signatures
}
```

**When enabled**:
- All Gossip messages signed with Ed25519
- Prevents message tampering
- Authenticates sender identity
- ~15µs overhead per message

**When to use**:
- Enable: Untrusted networks, public cloud
- Disable: Private networks, trusted LANs

---

## 📈 Scalability

### Horizontal Scaling

```
1 instance:   1-2M ops/s
3 instances:  3-6M ops/s (linear)
10 instances: 10-20M ops/s (linear)
```

**Scales linearly** because:
- Data partitioned via consistent hashing
- Each instance handles its partition
- No central bottleneck

### Cluster Size Limits

| Cluster Size | Gossip Overhead | Use Case |
|--------------|----------------|----------|
| 1-10 instances | Negligible | Small deployments |
| 10-50 instances | Low (~1% network) | Medium deployments |
| 50-100 instances | Moderate (~2-3%) | Large deployments |
| 100+ instances | Higher | Consider hierarchical |

**Recommended**: 3-20 instances per datacenter

---

## 🎯 Design Principles

### 1. Simplicity Over Features

**GridKV focuses on**:
- ✅ Simple KV operations (Set, Get, Delete)
- ✅ Automatic clustering
- ✅ Embedded deployment

**GridKV does NOT provide**:
- ❌ Rich data structures (List, Set, ZSet)
- ❌ Complex queries
- ❌ Lua scripting

**Philosophy**: Do one thing well (distributed KV cache)

### 2. Operational Simplicity

- ✅ Zero external dependencies
- ✅ Auto-clustering (no manual setup)
- ✅ Self-healing (automatic failover)
- ✅ One system to manage (not two)

### 3. Go-Native Integration

- ✅ Import as Go library
- ✅ Type-safe APIs
- ✅ Compile into single binary
- ✅ No FFI/RPC overhead

---

## 📚 Related Documentation

- [Embedded Architecture](EMBEDDED_ARCHITECTURE.md) - Why embedded?
- [Consistency Model](CONSISTENCY_MODEL.md) - Quorum details
- [Gossip Protocol](GOSSIP_PROTOCOL.md) - SWIM specification
- [Performance](PERFORMANCE.md) - Benchmarks

---

**GridKV Architecture** - Embedded, Distributed, Simple ✅

**Last Updated**: 2025-11-09  
**GridKV Version**: v3.1
