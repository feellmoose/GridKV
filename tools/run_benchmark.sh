#!/bin/bash
# Quick benchmark runner with baseline comparison

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
RESULTS_DIR="$PROJECT_ROOT/benchmark_results"
BASELINE_FILE="$RESULTS_DIR/baseline.json"

# Default config
BENCH_NAME="${1:-optimization_step}"
NODES="${GRIDKV_BENCH_NODES:-5}"
DURATION="${GRIDKV_BENCH_DURATION:-30s}"
CONCURRENT="${GRIDKV_BENCH_CONCURRENT:-100}"
NETWORK="${GRIDKV_BENCH_NETWORK:-TCP}"
BACKEND="${GRIDKV_BENCH_BACKEND:-MemorySharded}"

# Create results directory
mkdir -p "$RESULTS_DIR"

# Generate output filename
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
OUTPUT_FILE="$RESULTS_DIR/${BENCH_NAME}_${TIMESTAMP}.json"

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "   GridKV Quick Benchmark"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Test Name:    $BENCH_NAME"
echo "Nodes:        $NODES"
echo "Backend:      $BACKEND"
echo "Network:      $NETWORK"
echo "Concurrent:   $CONCURRENT"
echo "Duration:     $DURATION"
echo "Output:       $OUTPUT_FILE"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

# Run benchmark
export GRIDKV_BENCH_NODES="$NODES"
export GRIDKV_BENCH_DURATION="$DURATION"
export GRIDKV_BENCH_CONCURRENT="$CONCURRENT"
export GRIDKV_BENCH_NETWORK="$NETWORK"
export GRIDKV_BENCH_BACKEND="$BACKEND"

cd "$PROJECT_ROOT"
go run "$SCRIPT_DIR/benchmark.go" "$BENCH_NAME" "$OUTPUT_FILE" 2>&1 | tee "$OUTPUT_FILE.log"

if [ ! -f "$OUTPUT_FILE" ]; then
    echo "ERROR: Benchmark output file not created"
    exit 1
fi

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "   Comparison with Baseline"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

# Compare with baseline if exists
if [ -f "$BASELINE_FILE" ]; then
    go run "$SCRIPT_DIR/compare/main.go" "$BASELINE_FILE" "$OUTPUT_FILE"
else
    echo "No baseline found at $BASELINE_FILE"
    echo "To set a baseline, run:"
    echo "  cp $OUTPUT_FILE $BASELINE_FILE"
    echo ""
    echo "Setting current result as baseline..."
    cp "$OUTPUT_FILE" "$BASELINE_FILE"
    echo "Baseline set: $BASELINE_FILE"
fi

echo ""
echo "Done! Results saved to: $OUTPUT_FILE"

