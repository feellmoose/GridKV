# GridKV Cluster Component

Distributed protocol layer implementation providing eventually consistent distributed key-value storage.

## Design Principles

- **Algebraic Soundness**: HLC guarantees causality (HLC(A) < HLC(B) when A → B), LWW guarantees eventual consistency
- **Throughput Optimization**: Eliminate write amplification (from 2N down to N), asynchronous propagation (latency from 2RTT down to 0)
- **Simplified Path**: Unified gossip propagation, eliminate primary fan-out redundancy

## Core Components

### 1. Membership and Topology (SWIM + HashRing)

#### MemberMgr (Membership Manager)
- **Protocol**: SWIM (periodic ping/ack, indirect probing, suspect->confirm state machine)
- **Version Control**: Node metadata with version numbers (incarnation) to prevent old message overwrites
- **Implementation**:
  - Uses `lifecycle.Component` for lifecycle management
  - `executor.Executor` executes ping/ack/indirect-probe (single goroutine polling, avoids concurrency conflicts)
  - Node state: `sync.Map[nodeID]*NodeInfo` (lock-free reads, CAS on writes)
  - Incarnation version number: `atomic.Int64` (prevents old message overwrites)

#### HashRing (Consistent Hash Ring)
- **Features**: Virtual nodes + replica factor
- **Update Mechanism**: Ring updates via atomic snapshot broadcast (monotonically increasing version numbers), supports concurrent lock-free reads
- **Implementation**:
  - Virtual nodes: `[]ringNode{hash, nodeID}`, sorted with binary search (O(log N*R))
  - Version number: `atomic.Int64` (lock-free reads)
  - `Get`: hash(key) -> binary search -> return nodeID (lock-free read)
  - `GetN`: Take N nodes clockwise from Get position, deduplicate (max O(N))
  - `Update`: Version comparison (CAS), rebuild ring on new version (copy-on-write, atomic pointer replacement)
  - Concurrency safety: RWMutex (read-heavy), brief write lock during Update

**Topology Changes**: SWIM detects join/leave/fail and generates new ring version, gossip propagates; consumers use version comparison for lock-free reads (ignore versions < current version).

### 2. Write Protocol (Unified Epidemic Propagation, Eliminate Write Amplification)

#### Writer
- **Routing**: client/hash router -> local buffer (immediate return); hash ring locates replica list (for gossip target selection)
- **Versioning**: HLC generates write timestamps; storage layer LWW (HLC guarantees causality, no complex merging needed)
- **Implementation**:
  - `Set`:
    1. `hlc.Now()` generates version number -> `item.Version = hlcInt64` (parse HLC string to int64)
    2. `mem_storage.Set(key, item)` -> immediate return (already written to syncBuffer in local buffer)
    3. Trigger batch check (atomic counter, executor.Do async flush when threshold reached)
  - `BatchSet`: Batch call Set (reuse single logic, reduce lock contention)
  - `Delete`: Same as Set, but item is tombstone (Version>0, Value=nil)

#### Gossip (Epidemic Propagation)
- **Propagation Mechanism**: Local buffer -> batch trigger (time window T + batch threshold N) -> gossip push/pull diffusion to replicas (fan-out = log(N))
- **Algebraic Optimization**: Write amplification from 2N (primary fan-out + gossip) down to N (gossip only); latency from 2RTT (primary + fan-out) down to 0 (async propagation)
- **Implementation**:
  - `Push`:
    1. Serialize ops (zerocopy.BytesToString reduces allocations)
    2. executor.Do async send to targets (fan-out = log(N), avoids full broadcast)
    3. Each target independent goroutine, failure retry (exponential backoff, max 3 times)
  - `Pull`:
    1. Send PULL request to target
    2. Receive response, deserialize ops
    3. Apply ops (mem_storage.Set, conflicts resolved by ResolveConflict)
  - `Start`: Start periodic gossip (executor scheduled task, T_gossip interval)
  - `Stop`: Wait for all Push/Pull to complete (sync.WaitGroup)

**Batch Triggering** (Embedded in Writer, not exposed separately):
- Trigger conditions: Time window (T_window) or batch threshold (N_threshold)
- Implementation: atomic counter + time.Timer, executor.Do(flush) when threshold reached
- flush: `mem_storage.GetSyncBuffer()` -> `Gossip.Push(ops, targets)`
- targets: `HashRing.GetN(key, replicaCount)` gets replica list

**Consistency**: Eventual consistency (epidemic guarantees convergence); optional sync writes (wait for W replica acks, reduces throughput).

### 3. Read Protocol (Minimize Network Hops)

#### Reader
- **Routing**: Hash ring locates preferred replica; fast fallback to next replica when suspect/unreachable (O(log N) lookup)
- **Consistency**: `R=1` default (eventual consistency); configurable `QUORUM/ALL` (strong consistency, reduces throughput)
- **Implementation**:
  - `Get`:
    1. Hot key cache: `cache.Get(key)` -> return if hit (TTL lease)
    2. `HashRing.Get(key)` locates preferred replica
    3. Local: `mem_storage.Get(key)`, remote: network request
    4. Cache result (`cache.Set(key, item, TTL)`)
  - `BatchGet`: Parallel Get calls (executor concurrent execution, sync.WaitGroup aggregates results)
  - `GetSpeculative`:
    1. `HashRing.GetN(key, n)` gets N replicas
    2. executor parallel queries (context.WithCancel, cancel others on first success)
    3. Version conflict detection: compare all returned versions, trigger async ReadRepair
    4. Return highest version (HLC comparison)

#### ReadRepair
- **Mechanism**: Async repair on version conflicts (HLC comparison, repair only outdated versions); avoids write amplification for non-conflicting cases
- **Implementation**:
  - `Repair`:
    1. Version comparison: iterate versions, find highest version (StoredItem.CompareVersion)
    2. Async repair: executor.Do executes Writer.Set(key, maxVersionItem)
    3. Throttling: rate limiter (avoid repair storms, ReadRepairRateLimitPerSec)
  - Trigger: Automatically called when GetSpeculative detects version conflicts

**Latency Optimization**: Speculative read (parallel query to multiple replicas, first responder wins); hot key caching (TTL leases).

### 4. Anti-Entropy and Replay

#### AntiEntropy
- **Mechanism**: Periodic comparison of key-range digests (bloom + version vector), batch repair of differences
- **Implementation**:
  - `Digest`:
    1. Scan key-range (mem_storage.Keys() with prefix filtering)
    2. Bloom filter: hash all keys (xxh3, fast and uniformly distributed)
    3. Version vector: map[nodeID]maxVersion (extract nodeID from StoredItem.Version)
  - `Sync`:
    1. Compare blooms (bit operations, fast difference detection)
    2. Compare version vectors (find keys with outdated versions)
    3. Return list of differing keys
  - Periodic execution: executor scheduled task (T_anti_entropy interval, default 5min)

#### Replay
- **Mechanism**: Post-recovery hinted-handoff retransmission; resharding migration with checkpoints
- **Implementation**:
  - `SaveCheckpoint`:
    1. Serialize ops (zerocopy reduces allocations)
    2. Persist to local file (atomic write: temp file + rename)
    3. Periodic execution (executor scheduled task, T_checkpoint interval)
  - `LoadCheckpoint`:
    1. Read checkpoint file on startup
    2. Deserialize ops
    3. Apply ops (Writer.BatchSet, conflicts auto-resolved)
  - `HintedHandoff`:
    1. After node recovery, read unacknowledged ops from hinted-handoff queue
    2. executor.Do async retry (exponential backoff, max 10 times)
    3. Delete on success, retain on failure (retry next time)

Write buffer can be persistent/memory queue, continue from checkpoint after crashes.

## Unified Component (Cluster)

`Cluster` merges all interfaces, providing unified distributed operation entry point.

### Component Structure
```go
type Cluster struct {
    // Core components
    member  *MemberMgr
    ring    *HashRing
    writer  *Writer
    gossip  *Gossip
    reader  *Reader
    repair  *ReadRepair
    entropy *AntiEntropy
    replay  *Replay

    // Dependency components (reuse utils)
    hlc       *hlc.HLC
    store     *mem_storage.MemStorage
    executor  *executor.Executor
    cache     *cache.Cache
    lifecycle *lifecycle.LifecycleManager
}
```

### Lifecycle Management
- Uses `lifecycle.LifecycleManager` to manage all component start/stop ordering
- Dependencies: `member -> ring -> writer/gossip -> reader/repair -> entropy/replay`
- Unified error handling: errors package aggregates errors
- Unified logging: logging package records key operations

### Concurrency Safety
- All concurrent operations use `sync.Map/atomic/executor` (avoid data races)
- All public methods are concurrently callable, internal uses executor to serialize write operations
- Memory safety: lock-free reads, CAS/version comparison on writes

## Storage Layer Interface

### MemStorage
- `Set(key string, item *StoredItem) error` - Sharding + LWW conflict resolution
- `Get(key string) (*StoredItem, error)` - Sharded lookup + decompression
- `Delete(key string, version int64) error` - Version check + tombstone
- `GetSyncBuffer() ([]*SyncOperation, error)` - Collect from each shard's ring buffer

### StoredItem
- `ResolveConflict(other *StoredItem) bool` - HLC version comparison (higher version wins)

## HLC (Hybrid Logical Clock)

### Interface
- `Now() string` - Physical time + logical counter
- `Update(remote string)` - Merge remote HLC (guarantees causality)

## Performance Optimizations

1. **Write Amplification Optimization**: From 2N down to N (gossip only, no primary fan-out)
2. **Latency Optimization**: From 2RTT down to 0 (asynchronous propagation)
3. **Read Optimization**: Speculative read + hot key caching
4. **Zero Copy**: Serialization/deserialization uses zerocopy
5. **Lock-Free Reads**: Hash ring version comparison, sync.Map lock-free reads

## Consistency Guarantees

- **Eventual Consistency**: Epidemic gossip guarantees convergence
- **Causality**: HLC guarantees causal ordering
- **Conflict Resolution**: LWW (Last Write Wins) based on HLC versions
- **Read Repair**: Automatic detection and repair of version conflicts
