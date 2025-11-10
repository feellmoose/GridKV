#!/bin/bash

# GridKV Adaptive Network Testing Suite
# This script runs comprehensive tests: functional, performance, and stability

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Test configuration
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
REPORT_DIR="test_reports/${TIMESTAMP}"
mkdir -p "${REPORT_DIR}"

# Logging
LOG_FILE="${REPORT_DIR}/test_execution.log"
exec > >(tee -a "${LOG_FILE}")
exec 2>&1

echo -e "${BLUE}========================================${NC}"
echo -e "${BLUE}GridKV Adaptive Network Testing Suite${NC}"
echo -e "${BLUE}========================================${NC}"
echo ""
echo "Timestamp: ${TIMESTAMP}"
echo "Report Directory: ${REPORT_DIR}"
echo ""

cd "$(dirname "$0")/.."

# Phase 1: Functional Tests
echo -e "${YELLOW}Phase 1: Functional Verification Tests${NC}"
echo -e "${YELLOW}========================================${NC}"
echo ""

FUNCTIONAL_START=$(date +%s)

echo "Running functional tests..."
if go test -v -run TestAdaptiveNetworkFunctional ./tests/ -timeout 10m > "${REPORT_DIR}/functional_tests.log" 2>&1; then
    echo -e "${GREEN}✓ Functional tests PASSED${NC}"
    FUNCTIONAL_STATUS="PASSED"
else
    echo -e "${RED}✗ Functional tests FAILED${NC}"
    FUNCTIONAL_STATUS="FAILED"
    echo "See ${REPORT_DIR}/functional_tests.log for details"
fi

FUNCTIONAL_END=$(date +%s)
FUNCTIONAL_DURATION=$((FUNCTIONAL_END - FUNCTIONAL_START))
echo "Duration: ${FUNCTIONAL_DURATION}s"
echo ""

# Phase 2: Performance Benchmarks (Baseline)
echo -e "${YELLOW}Phase 2: Performance Benchmarks (Baseline)${NC}"
echo -e "${YELLOW}==========================================${NC}"
echo ""

BENCH_START=$(date +%s)

echo "Running performance benchmarks..."
go test -bench=. -benchmem -benchtime=5s -run=^$ ./tests/ \
    > "${REPORT_DIR}/baseline_benchmarks.txt" 2>&1

if [ $? -eq 0 ]; then
    echo -e "${GREEN}✓ Benchmarks completed${NC}"
    BENCH_STATUS="COMPLETED"
    
    # Extract key metrics
    echo ""
    echo "Key Performance Metrics (Baseline):"
    echo "-----------------------------------"
    
    # Latency Matrix operations
    echo "Latency Matrix:"
    grep "BenchmarkLatencyMatrix" "${REPORT_DIR}/baseline_benchmarks.txt" | head -5
    
    # Multi-DC operations
    echo ""
    echo "Multi-DC Operations:"
    grep "BenchmarkMultiDC" "${REPORT_DIR}/baseline_benchmarks.txt" | head -5
    
    # Metrics collector
    echo ""
    echo "Metrics Collector:"
    grep "BenchmarkMetricsCollector" "${REPORT_DIR}/baseline_benchmarks.txt" | head -5
    
    # Integration
    echo ""
    echo "Integration:"
    grep "BenchmarkIntegration.*ConcurrentOperations" "${REPORT_DIR}/baseline_benchmarks.txt"
    
else
    echo -e "${RED}✗ Benchmarks FAILED${NC}"
    BENCH_STATUS="FAILED"
fi

BENCH_END=$(date +%s)
BENCH_DURATION=$((BENCH_END - BENCH_START))
echo ""
echo "Duration: ${BENCH_DURATION}s"
echo ""

# Phase 3: Stress Testing
echo -e "${YELLOW}Phase 3: Stress Testing${NC}"
echo -e "${YELLOW}=======================${NC}"
echo ""

STRESS_START=$(date +%s)

echo "Running stress tests..."
go test -v -run TestStressScenarios ./tests/ -timeout 30m \
    > "${REPORT_DIR}/stress_tests.log" 2>&1

if [ $? -eq 0 ]; then
    echo -e "${GREEN}✓ Stress tests PASSED${NC}"
    STRESS_STATUS="PASSED"
else
    echo -e "${RED}✗ Stress tests FAILED${NC}"
    STRESS_STATUS="FAILED"
fi

STRESS_END=$(date +%s)
STRESS_DURATION=$((STRESS_END - STRESS_START))
echo "Duration: ${STRESS_DURATION}s"
echo ""

# Phase 4: Scalability Testing
echo -e "${YELLOW}Phase 4: Scalability Testing${NC}"
echo -e "${YELLOW}=============================${NC}"
echo ""

SCALE_START=$(date +%s)

echo "Running scalability benchmarks..."
go test -bench=BenchmarkScalability -benchtime=3s -run=^$ ./tests/ \
    > "${REPORT_DIR}/scalability_benchmarks.txt" 2>&1

if [ $? -eq 0 ]; then
    echo -e "${GREEN}✓ Scalability benchmarks completed${NC}"
    SCALE_STATUS="COMPLETED"
    
    echo ""
    echo "Scalability Results:"
    echo "-------------------"
    grep "BenchmarkScalability" "${REPORT_DIR}/scalability_benchmarks.txt"
else
    echo -e "${RED}✗ Scalability benchmarks FAILED${NC}"
    SCALE_STATUS="FAILED"
fi

SCALE_END=$(date +%s)
SCALE_DURATION=$((SCALE_END - SCALE_START))
echo ""
echo "Duration: ${SCALE_DURATION}s"
echo ""

# Phase 5: Memory and Allocation Analysis
echo -e "${YELLOW}Phase 5: Memory Allocation Analysis${NC}"
echo -e "${YELLOW}====================================${NC}"
echo ""

echo "Analyzing memory allocations..."
go test -bench=BenchmarkMemoryAllocation -benchmem -benchtime=5s -run=^$ ./tests/ \
    > "${REPORT_DIR}/memory_analysis.txt" 2>&1

if [ $? -eq 0 ]; then
    echo -e "${GREEN}✓ Memory analysis completed${NC}"
    
    echo ""
    echo "Memory Allocation Results:"
    echo "-------------------------"
    grep "BenchmarkMemoryAllocation" "${REPORT_DIR}/memory_analysis.txt"
else
    echo -e "${RED}✗ Memory analysis FAILED${NC}"
fi
echo ""

# Phase 6: Short Stability Test (Optional - can be run separately)
if [ "$1" = "--with-stability" ]; then
    echo -e "${YELLOW}Phase 6: Stability Testing (Short)${NC}"
    echo -e "${YELLOW}===================================${NC}"
    echo ""
    
    STABILITY_START=$(date +%s)
    
    echo "Running short stability tests (5 minutes)..."
    echo "For full 1-hour stability test, run separately with: go test -run TestStabilityLongRunning -timeout 2h ./tests/"
    
    go test -v -run TestStabilityHighLoad -short ./tests/ -timeout 15m \
        > "${REPORT_DIR}/stability_tests_short.log" 2>&1
    
    if [ $? -eq 0 ]; then
        echo -e "${GREEN}✓ Short stability tests PASSED${NC}"
        STABILITY_STATUS="PASSED"
    else
        echo -e "${RED}✗ Short stability tests FAILED${NC}"
        STABILITY_STATUS="FAILED"
    fi
    
    STABILITY_END=$(date +%s)
    STABILITY_DURATION=$((STABILITY_END - STABILITY_START))
    echo "Duration: ${STABILITY_DURATION}s"
    echo ""
fi

# Generate Summary Report
echo -e "${BLUE}========================================${NC}"
echo -e "${BLUE}Test Summary Report${NC}"
echo -e "${BLUE}========================================${NC}"
echo ""

TOTAL_END=$(date +%s)
TOTAL_START=${FUNCTIONAL_START}
TOTAL_DURATION=$((TOTAL_END - TOTAL_START))

# Create summary file
SUMMARY_FILE="${REPORT_DIR}/SUMMARY.md"

cat > "${SUMMARY_FILE}" << EOF
# GridKV Adaptive Network Test Report

**Timestamp:** ${TIMESTAMP}  
**Total Duration:** ${TOTAL_DURATION}s ($(printf '%02d:%02d:%02d' $((TOTAL_DURATION/3600)) $((TOTAL_DURATION%3600/60)) $((TOTAL_DURATION%60))))

## Test Results

| Phase | Status | Duration |
|-------|--------|----------|
| Functional Tests | ${FUNCTIONAL_STATUS} | ${FUNCTIONAL_DURATION}s |
| Performance Benchmarks | ${BENCH_STATUS} | ${BENCH_DURATION}s |
| Stress Tests | ${STRESS_STATUS} | ${STRESS_DURATION}s |
| Scalability Tests | ${SCALE_STATUS} | ${SCALE_DURATION}s |
EOF

if [ "$1" = "--with-stability" ]; then
    echo "| Stability Tests | ${STABILITY_STATUS} | ${STABILITY_DURATION}s |" >> "${SUMMARY_FILE}"
fi

cat >> "${SUMMARY_FILE}" << EOF

## Test Artifacts

- Full execution log: \`test_execution.log\`
- Functional tests: \`functional_tests.log\`
- Baseline benchmarks: \`baseline_benchmarks.txt\`
- Stress tests: \`stress_tests.log\`
- Scalability benchmarks: \`scalability_benchmarks.txt\`
- Memory analysis: \`memory_analysis.txt\`
EOF

if [ "$1" = "--with-stability" ]; then
    echo "- Stability tests: \`stability_tests_short.log\`" >> "${SUMMARY_FILE}"
fi

cat >> "${SUMMARY_FILE}" << EOF

## Key Metrics

### Performance (Baseline)

\`\`\`
EOF

grep "BenchmarkIntegration.*ConcurrentOperations" "${REPORT_DIR}/baseline_benchmarks.txt" >> "${SUMMARY_FILE}" || echo "N/A" >> "${SUMMARY_FILE}"

cat >> "${SUMMARY_FILE}" << EOF
\`\`\`

### Scalability

\`\`\`
EOF

grep "BenchmarkScalability" "${REPORT_DIR}/scalability_benchmarks.txt" >> "${SUMMARY_FILE}" || echo "N/A" >> "${SUMMARY_FILE}"

cat >> "${SUMMARY_FILE}" << EOF
\`\`\`

### Memory Allocations

\`\`\`
EOF

grep "BenchmarkMemoryAllocation" "${REPORT_DIR}/memory_analysis.txt" | head -3 >> "${SUMMARY_FILE}" || echo "N/A" >> "${SUMMARY_FILE}"

cat >> "${SUMMARY_FILE}" << EOF
\`\`\`

## Notes

- Run with \`--with-stability\` flag to include short stability tests
- For full 1-hour stability test: \`go test -run TestStabilityLongRunning -timeout 2h ./tests/\`
- For detailed results, see individual log files

## Recommendations

EOF

# Add recommendations based on results
if [ "${FUNCTIONAL_STATUS}" = "FAILED" ]; then
    echo "- **CRITICAL**: Fix functional test failures before proceeding" >> "${SUMMARY_FILE}"
fi

if [ "${STRESS_STATUS}" = "FAILED" ]; then
    echo "- **WARNING**: Stress tests failed - review system stability under load" >> "${SUMMARY_FILE}"
fi

cat >> "${SUMMARY_FILE}" << EOF
- Review benchmark results for performance regressions
- Check memory allocation patterns for optimization opportunities
- Consider running full stability tests for production readiness
EOF

# Display summary
cat "${SUMMARY_FILE}"

echo ""
echo -e "${BLUE}========================================${NC}"
echo -e "${BLUE}Report Location${NC}"
echo -e "${BLUE}========================================${NC}"
echo ""
echo "All test reports saved to: ${REPORT_DIR}"
echo "Summary report: ${SUMMARY_FILE}"
echo ""

# Overall status
OVERALL_STATUS="SUCCESS"
if [ "${FUNCTIONAL_STATUS}" = "FAILED" ] || [ "${STRESS_STATUS}" = "FAILED" ]; then
    OVERALL_STATUS="FAILURE"
    echo -e "${RED}Overall Status: FAILURE${NC}"
    exit 1
else
    echo -e "${GREEN}Overall Status: SUCCESS${NC}"
    exit 0
fi

