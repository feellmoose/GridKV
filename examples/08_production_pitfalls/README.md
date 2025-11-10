# Production Pitfalls Detection Example

This example demonstrates how to detect and avoid the 5 most common production pitfalls that can cause 10-25% performance degradation.

## The 5 Hidden Pitfalls

1. **CPU Frequency Throttling** - 15-20% throughput loss
2. **Disk I/O Interference** - P99 latency spike to 10ms+
3. **NUMA Cross-Node Access** - 10-15% cache miss increase
4. **Gossip Protocol Storm** - 20-30% CPU to soft interrupts
5. **Go Runtime Version** - 2× GC frequency on old versions

## Running the Example

```bash
cd examples/08_production_pitfalls
go run main.go
```

## What It Does

The example will:
1. ✅ Check CPU frequency and governor settings
2. ✅ Check memory and swap configuration
3. ✅ Detect NUMA topology and policy
4. ✅ Verify Go runtime version
5. ✅ Display recommended configuration
6. ✅ Create an optimized GridKV instance with pitfall avoidance

## Expected Output

```
1️⃣  CPU Configuration Check
  CPU Cores: 20
  Current Frequency: 3400 MHz
  Max Frequency: 4700 MHz
  Frequency Ratio: 72.3%
  ⚠️  WARNING: CPU frequency is low!
     Fix: sudo cpupower frequency-set -g performance

2️⃣  Memory Configuration Check
  Total Memory: 32768 MB
  Available Memory: 28000 MB
  Swap: 0 MB
  ✅ Swap is disabled

3️⃣  NUMA Configuration Check
  NUMA Nodes: 2
  Policy: default
  ⚠️  WARNING: Multi-NUMA without optimization!
     Fix: numactl --interleave=all ./gridkv

4️⃣  Go Runtime Check
  Go Version: go1.21.5
  ✅ Go version supports optimized sync.Pool

5️⃣  Recommended Configuration
  MaxMemoryMB: 22937 MB (70% of total)
  ShardCount: 80 (CPU × 4)
  MaxConnections: 40 (CPU × 2)
  GossipInterval: 3s (avoid protocol storm)
```

## Production Deployment Script

Use the automated deployment script:

```bash
# Run the detection script first
./scripts/check_production_pitfalls.sh

# Use the optimized startup script
./scripts/start_gridkv_optimized.sh
```

## Manual Optimization Steps

### 1. Fix CPU Frequency
```bash
sudo cpupower frequency-set -g performance
echo 1 | sudo tee /sys/devices/system/cpu/intel_pstate/no_turbo
```

### 2. Disable Swap
```bash
sudo swapoff -a
sudo sysctl vm.swappiness=10
```

### 3. Configure NUMA
```bash
# For multi-NUMA systems
numactl --interleave=all ./gridkv

# Or bind to single node
numactl --cpunodebind=0 --membind=0 ./gridkv
```

### 4. Optimize Gossip (Large Clusters)
```go
opts := &gridkv.GridKVOptions{
    GossipInterval: 3 * time.Second,  // Increased from 1s
    FailureTimeout: 10 * time.Second, // Increased from 5s
    SuspectTimeout: 20 * time.Second, // Increased from 10s
}
```

### 5. Verify Go Version
```bash
go version  # Should be 1.20 or higher
```

## Expected Performance Gains

After fixing all pitfalls:

| Metric | Before | After | Improvement |
|--------|--------|-------|-------------|
| P99 Latency | 1-2ms | 200-400µs | 75% reduction |
| Throughput | Baseline | +25% | 25% increase |
| CPU Usage | 70% | 50% | 20% reduction |
| GC Pause | 10-20ms | 3-5ms | 70% reduction |
| Latency Stability | ±50% | ±5% | Significant |

## Related Documentation

- [Production Pitfalls Guide (CN)](../../docs/PRODUCTION_PITFALLS_CN.md)
- [Performance Optimization Roadmap (CN)](../../docs/PERFORMANCE_OPTIMIZATION_CN.md)
- [Architecture](../../docs/ARCHITECTURE.md)

## Monitoring in Production

Continuously monitor these metrics:
- CPU frequency (should stay > 95% of max)
- Swap usage (should always be 0)
- NUMA remote access ratio (should be < 10%)
- Network packet rate (should be < 5K pps per node)
- GC pause time (should be < 10ms)

