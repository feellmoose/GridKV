# GridKV Test Suite

## Overview

The test suite includes functional tests, performance benchmarks, failure recovery tests, and production environment simulation tests.

## Quick Start

### Basic Tests
```bash
# Build check
go build ./tests/...

# List all tests
go test ./tests/... -list .

# Run quick benchmarks (using -short flag)
go test ./tests/... -short -bench=. -benchtime=1s

# Run full benchmarks
go test ./tests/... -bench=. -benchtime=2s
```

### Functional Tests
```bash
# Run quick tests (skip large tests)
go test ./tests/... -short -v

# Run full tests (including large tests)
go test ./tests/... -v -run TestFailureRecovery -timeout 15m

# Run production workload tests
go test ./tests/... -v -run TestProductionWorkload -timeout 20m

# Run consistency tests
go test ./tests/... -v -run TestEventualConsistency -timeout 20m

# Run concurrent stress benchmark
go test ./tests/... -bench=BenchmarkConcurrentStress -benchtime=5s
```

## Test File Description

### Core Test Files
- `test_helpers.go` - Test helper functions and utilities
- `network_profile.go` - Network configuration file

### Functional Tests
- `simulation_test.go` - **Merged file**: Production workload tests, eventual consistency tests, stress tests
  - Production tests: `TestProductionWorkload_Realistic`, `TestProductionWorkload_HighStress`, `TestProductionWorkload_MixedPatterns`
  - Consistency tests: `TestEventualConsistency_Basic`, `TestEventualConsistency_GossipPropagation`, `TestEventualConsistency_HLCCausality`
  - Stress tests: `RunStressTest` and related helper functions
- `failure_recovery_test.go` - Failure recovery tests
- `long_stability_test.go` - Long-term stability tests

### Performance Tests
- `benchmark_test.go` - **Merged file**: All performance benchmarks (18 benchmark functions)
  - Basic performance: `BenchmarkWriteThroughput`, `BenchmarkReadLatency`, `BenchmarkWriteReadMixed`, `BenchmarkGossipConvergence`, `BenchmarkBatchWrite`, `BenchmarkConcurrentWrites`
  - QPS tests: `BenchmarkWriteQPS`, `BenchmarkReadQPS`, `BenchmarkDeleteQPS`, `BenchmarkMixedOpsQPS`
  - Cluster QPS: `BenchmarkClusterWriteQPS`, `BenchmarkClusterReadQPS`, `BenchmarkClusterMixedQPS`
  - Large scale cluster: `BenchmarkLargeClusterWriteThroughput`, `BenchmarkLargeClusterDataSeeding`, `BenchmarkSmallClusterDataSeeding`, `BenchmarkMixedWorkloadLargeCluster`
  - Concurrent stress: `BenchmarkConcurrentStress` (new)

### Test Tools
- `production_simulator.go` - Production environment simulator
- `performance_monitor.go` - Performance monitoring tool

## Test Configuration

### Test Flags
- `-short` - Skip large tests, run quick tests (default behavior)
- Without `-short` - Run complete test suite (including large tests)

### Environment Variables
- `GRIDKV_NETWORK=tcp|quic` - Select network type
- `GRIDKV_NO_THROTTLE=1` - Disable throttling (performance tests)

### Test Timeouts
- Small tests: `-timeout 5m`
- Medium tests: `-timeout 10m`
- Large tests: `-timeout 30m`

## Performance Benchmarks

### Cluster Creation
- 100 nodes: ~500ms
- Node ready: <1 second

### Failure Recovery
- Failure detection: <5 seconds
- Data redistribution: 2-5 seconds

### Reliability
- Node failure data accessibility: >95%
- Network partition data accessibility: >40%

## Test Results

Latest test results:
- TestFailureRecovery_NodeFailure: ✅ Passed (96.1% data accessibility)
- TestFailureRecovery_NetworkPartition: ✅ Passed (48.4% partition data accessibility)

For detailed test reports, see: `COMPREHENSIVE_TEST_REPORT.md`
