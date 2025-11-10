#!/bin/bash

# Long-Running Stability Test for GridKV Adaptive Network
# This script runs extended stability tests (1+ hours)

set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

# Configuration
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
REPORT_DIR="stability_reports/${TIMESTAMP}"
mkdir -p "${REPORT_DIR}"

LOG_FILE="${REPORT_DIR}/stability_test.log"
exec > >(tee -a "${LOG_FILE}")
exec 2>&1

echo -e "${BLUE}========================================${NC}"
echo -e "${BLUE}GridKV Stability Test Suite${NC}"
echo -e "${BLUE}========================================${NC}"
echo ""
echo "Timestamp: ${TIMESTAMP}"
echo "Report Directory: ${REPORT_DIR}"
echo ""

cd "$(dirname "$0")/.."

# Parse duration argument
DURATION="1h"
if [ -n "$1" ]; then
    DURATION="$1"
fi

echo -e "${YELLOW}Test Configuration:${NC}"
echo "Duration: ${DURATION}"
echo ""

# Prompt for confirmation
echo -e "${YELLOW}Warning: This test will run for ${DURATION}${NC}"
echo "Press Enter to continue or Ctrl+C to cancel..."
read -r

# Test 1: Continuous Probing
echo -e "${BLUE}Test 1: Continuous Probing (${DURATION})${NC}"
echo -e "${BLUE}========================================${NC}"
echo ""

START_TIME=$(date +%s)

echo "Starting continuous probing test..."
echo "Monitoring system health every minute..."
echo ""

# Run the test in background and monitor
go test -v -run TestStabilityLongRunning/ContinuousProbing ./tests/ -timeout $((2 * 3600))s \
    > "${REPORT_DIR}/continuous_probing.log" 2>&1 &

TEST_PID=$!

# Monitor the test
while kill -0 $TEST_PID 2>/dev/null; do
    sleep 60
    echo "$(date '+%Y-%m-%d %H:%M:%S') - Test still running (PID: $TEST_PID)"
    
    # Check system resources
    if command -v free > /dev/null; then
        echo "Memory usage:"
        free -h | grep -E "Mem|Swap"
    fi
    
    echo ""
done

wait $TEST_PID
TEST_EXIT_CODE=$?

END_TIME=$(date +%s)
ELAPSED=$((END_TIME - START_TIME))

if [ $TEST_EXIT_CODE -eq 0 ]; then
    echo -e "${GREEN}✓ Continuous probing test PASSED${NC}"
    TEST1_STATUS="PASSED"
else
    echo -e "${RED}✗ Continuous probing test FAILED (exit code: $TEST_EXIT_CODE)${NC}"
    TEST1_STATUS="FAILED"
fi

echo "Duration: ${ELAPSED}s ($(printf '%02d:%02d:%02d' $((ELAPSED/3600)) $((ELAPSED%3600/60)) $((ELAPSED%60))))"
echo ""

# Test 2: Memory Leak Detection
echo -e "${BLUE}Test 2: Memory Leak Detection${NC}"
echo -e "${BLUE}=============================${NC}"
echo ""

START_TIME=$(date +%s)

echo "Starting memory leak detection test..."
go test -v -run TestStabilityLongRunning/MemoryLeakDetection ./tests/ -timeout 30m \
    > "${REPORT_DIR}/memory_leak.log" 2>&1

if [ $? -eq 0 ]; then
    echo -e "${GREEN}✓ Memory leak test PASSED${NC}"
    TEST2_STATUS="PASSED"
else
    echo -e "${RED}✗ Memory leak test FAILED${NC}"
    TEST2_STATUS="FAILED"
fi

END_TIME=$(date +%s)
ELAPSED=$((END_TIME - START_TIME))
echo "Duration: ${ELAPSED}s"
echo ""

# Test 3: Node Churn
echo -e "${BLUE}Test 3: Node Churn${NC}"
echo -e "${BLUE}==================${NC}"
echo ""

START_TIME=$(date +%s)

echo "Starting node churn test..."
go test -v -run TestStabilityLongRunning/NodeChurn ./tests/ -timeout 15m \
    > "${REPORT_DIR}/node_churn.log" 2>&1

if [ $? -eq 0 ]; then
    echo -e "${GREEN}✓ Node churn test PASSED${NC}"
    TEST3_STATUS="PASSED"
else
    echo -e "${RED}✗ Node churn test FAILED${NC}"
    TEST3_STATUS="FAILED"
fi

END_TIME=$(date +%s)
ELAPSED=$((END_TIME - START_TIME))
echo "Duration: ${ELAPSED}s"
echo ""

# Test 4: High Load
echo -e "${BLUE}Test 4: High Concurrent Load${NC}"
echo -e "${BLUE}=============================${NC}"
echo ""

START_TIME=$(date +%s)

echo "Starting high load test..."
go test -v -run TestStabilityHighLoad/HighConcurrency ./tests/ -timeout 10m \
    > "${REPORT_DIR}/high_load.log" 2>&1

if [ $? -eq 0 ]; then
    echo -e "${GREEN}✓ High load test PASSED${NC}"
    TEST4_STATUS="PASSED"
else
    echo -e "${RED}✗ High load test FAILED${NC}"
    TEST4_STATUS="FAILED"
fi

END_TIME=$(date +%s)
ELAPSED=$((END_TIME - START_TIME))
echo "Duration: ${ELAPSED}s"
echo ""

# Test 5: Recovery
echo -e "${BLUE}Test 5: Failure Recovery${NC}"
echo -e "${BLUE}========================${NC}"
echo ""

START_TIME=$(date +%s)

echo "Starting recovery tests..."
go test -v -run TestStabilityRecovery ./tests/ -timeout 10m \
    > "${REPORT_DIR}/recovery.log" 2>&1

if [ $? -eq 0 ]; then
    echo -e "${GREEN}✓ Recovery tests PASSED${NC}"
    TEST5_STATUS="PASSED"
else
    echo -e "${RED}✗ Recovery tests FAILED${NC}"
    TEST5_STATUS="FAILED"
fi

END_TIME=$(date +%s)
ELAPSED=$((END_TIME - START_TIME))
echo "Duration: ${ELAPSED}s"
echo ""

# Generate Summary Report
echo -e "${BLUE}========================================${NC}"
echo -e "${BLUE}Stability Test Summary${NC}"
echo -e "${BLUE}========================================${NC}"
echo ""

SUMMARY_FILE="${REPORT_DIR}/STABILITY_SUMMARY.md"

cat > "${SUMMARY_FILE}" << EOF
# GridKV Stability Test Report

**Timestamp:** ${TIMESTAMP}  
**Test Duration:** ${DURATION}

## Test Results

| Test | Status | Log File |
|------|--------|----------|
| Continuous Probing | ${TEST1_STATUS} | continuous_probing.log |
| Memory Leak Detection | ${TEST2_STATUS} | memory_leak.log |
| Node Churn | ${TEST3_STATUS} | node_churn.log |
| High Concurrent Load | ${TEST4_STATUS} | high_load.log |
| Failure Recovery | ${TEST5_STATUS} | recovery.log |

## Detailed Results

### Continuous Probing

\`\`\`
EOF

tail -50 "${REPORT_DIR}/continuous_probing.log" >> "${SUMMARY_FILE}"

cat >> "${SUMMARY_FILE}" << EOF
\`\`\`

### Memory Leak Detection

\`\`\`
EOF

tail -30 "${REPORT_DIR}/memory_leak.log" >> "${SUMMARY_FILE}"

cat >> "${SUMMARY_FILE}" << EOF
\`\`\`

### Node Churn

\`\`\`
EOF

tail -30 "${REPORT_DIR}/node_churn.log" >> "${SUMMARY_FILE}"

cat >> "${SUMMARY_FILE}" << EOF
\`\`\`

### High Load

\`\`\`
EOF

tail -30 "${REPORT_DIR}/high_load.log" >> "${SUMMARY_FILE}"

cat >> "${SUMMARY_FILE}" << EOF
\`\`\`

### Recovery

\`\`\`
EOF

tail -30 "${REPORT_DIR}/recovery.log" >> "${SUMMARY_FILE}"

cat >> "${SUMMARY_FILE}" << EOF
\`\`\`

## Analysis

EOF

# Analyze results
if [ "${TEST1_STATUS}" = "PASSED" ] && [ "${TEST2_STATUS}" = "PASSED" ] && \
   [ "${TEST3_STATUS}" = "PASSED" ] && [ "${TEST4_STATUS}" = "PASSED" ] && \
   [ "${TEST5_STATUS}" = "PASSED" ]; then
    cat >> "${SUMMARY_FILE}" << EOF
**Overall Assessment:** ✅ PASS

The system has demonstrated excellent stability across all test scenarios:
- Continuous operation without degradation
- No memory leaks detected
- Handles node churn gracefully
- Maintains performance under high load
- Recovers properly from failures

**Recommendation:** System is READY for production deployment.
EOF
    OVERALL_STATUS="PASS"
else
    cat >> "${SUMMARY_FILE}" << EOF
**Overall Assessment:** ⚠️ ISSUES DETECTED

The following issues were identified:
EOF
    
    [ "${TEST1_STATUS}" = "FAILED" ] && echo "- Continuous probing test failed" >> "${SUMMARY_FILE}"
    [ "${TEST2_STATUS}" = "FAILED" ] && echo "- Memory leak detected" >> "${SUMMARY_FILE}"
    [ "${TEST3_STATUS}" = "FAILED" ] && echo "- Node churn handling issues" >> "${SUMMARY_FILE}"
    [ "${TEST4_STATUS}" = "FAILED" ] && echo "- High load performance degradation" >> "${SUMMARY_FILE}"
    [ "${TEST5_STATUS}" = "FAILED" ] && echo "- Recovery issues detected" >> "${SUMMARY_FILE}"
    
    cat >> "${SUMMARY_FILE}" << EOF

**Recommendation:** Review and address the identified issues before production deployment.
EOF
    OVERALL_STATUS="FAIL"
fi

cat >> "${SUMMARY_FILE}" << EOF

## Next Steps

1. Review detailed logs in ${REPORT_DIR}
2. Address any identified issues
3. Re-run stability tests after fixes
4. Consider extended testing (24+ hours) for critical deployments

---
*Report generated: $(date)*
EOF

# Display summary
cat "${SUMMARY_FILE}"

echo ""
echo -e "${BLUE}========================================${NC}"
echo ""
echo "Stability test report saved to: ${REPORT_DIR}"
echo "Summary: ${SUMMARY_FILE}"
echo ""

if [ "${OVERALL_STATUS}" = "PASS" ]; then
    echo -e "${GREEN}Overall Status: PASS ✅${NC}"
    exit 0
else
    echo -e "${RED}Overall Status: FAIL ⚠️${NC}"
    exit 1
fi

