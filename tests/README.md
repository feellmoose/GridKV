# GridKV Test Suite

This directory contains the test suite for GridKV distributed key-value store.

## Quick Start

```bash
# Run all tests
go test ./tests/ -v

# Run short tests only
go test ./tests/ -short -v

# Run benchmarks
go test ./tests/ -bench=. -benchmem
```

## Profiling Long-Running Stability

`TestStabilityLongRun` is designed for 30-minute stability and GC/memory
behaviour validation. You can enable standard Go CPU/heap profiles:

```bash
go test ./tests -run TestStabilityLongRun -timeout 40m \
  -cpuprofile=cpu_long.out -memprofile=heap_long.out
```

Then inspect profiles with:

```bash
go tool pprof cpu_long.out
go tool pprof heap_long.out
```

## Test Types

| Test | Description | Duration |
|------|-------------|----------|
| `TestBasicCluster` | Basic consistency validation | ~12s |
| `TestReplication` | Replication correctness | ~15s |
| `TestPerformance` | Performance validation | ~15s |
| `TestFaultTolerance` | Fault tolerance validation | ~10s |
| `TestLargeScaleStress` | Large-scale stress test | ~2m |
| `TestLongRunningStress` | Extended duration test | ~5m |
| `TestStabilityLongRun` | 30-minute stability test | ~30m |

## Test Configuration

Tests can be configured via environment variables:

```bash
# Memory configuration
export TEST_MAX_MEMORY_MB=512
export BENCH_MAX_MEMORY_MB=256

# Cluster configuration
export TEST_NODE_COUNT=10
export TEST_REPLICA_COUNT=3
export TEST_BASE_PORT=30000

# Run tests
go test ./tests/ -v
```

## Test Structure

```
tests/
├── simulator/           # Test simulator framework
│   ├── simulator.go     # Cluster simulator
│   ├── workload.go      # Workload executor
│   ├── config.go        # Test configurations
│   └── runner.go        # Test runner
├── cluster_test.go      # Cluster tests
├── stress_test.go       # Stress tests
└── benchmark_test.go    # Benchmarks
```

## Performance Targets

- **Success Rate**: ≥90% for performance tests
- **QPS**: ≥1,000 ops/s
- **Consistency**: ≥80% for consistency tests
- **Replication**: ≥90% for replication tests

For detailed architecture documentation, see [internal/cluster/README.md](../internal/cluster/README.md).
