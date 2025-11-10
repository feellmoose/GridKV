#!/bin/bash

# Benchmark Comparison Tool
# Compares two benchmark results to measure optimization improvements

set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

if [ $# -lt 2 ]; then
    echo "Usage: $0 <baseline_file> <optimized_file>"
    echo ""
    echo "Example:"
    echo "  $0 test_reports/20250101_120000/baseline_benchmarks.txt test_reports/20250101_140000/baseline_benchmarks.txt"
    exit 1
fi

BASELINE_FILE="$1"
OPTIMIZED_FILE="$2"

if [ ! -f "$BASELINE_FILE" ]; then
    echo -e "${RED}Error: Baseline file not found: $BASELINE_FILE${NC}"
    exit 1
fi

if [ ! -f "$OPTIMIZED_FILE" ]; then
    echo -e "${RED}Error: Optimized file not found: $OPTIMIZED_FILE${NC}"
    exit 1
fi

TIMESTAMP=$(date +%Y%m%d_%H%M%S)
REPORT_DIR="comparison_reports/${TIMESTAMP}"
mkdir -p "${REPORT_DIR}"

COMPARISON_FILE="${REPORT_DIR}/comparison.md"

echo -e "${BLUE}========================================${NC}"
echo -e "${BLUE}Benchmark Comparison${NC}"
echo -e "${BLUE}========================================${NC}"
echo ""
echo "Baseline:  $BASELINE_FILE"
echo "Optimized: $OPTIMIZED_FILE"
echo "Report:    $COMPARISON_FILE"
echo ""

# Extract benchmark results function
extract_benchmarks() {
    local file=$1
    grep "^Benchmark" "$file" | \
        awk '{print $1, $3, $4, $5, $6}' | \
        sort
}

# Create comparison report
cat > "$COMPARISON_FILE" << EOF
# GridKV Benchmark Comparison Report

**Date:** $(date)  
**Baseline:**  \`$(basename $BASELINE_FILE)\`  
**Optimized:** \`$(basename $OPTIMIZED_FILE)\`

## Executive Summary

EOF

# Calculate improvements
TEMP_BASELINE=$(mktemp)
TEMP_OPTIMIZED=$(mktemp)

extract_benchmarks "$BASELINE_FILE" > "$TEMP_BASELINE"
extract_benchmarks "$OPTIMIZED_FILE" > "$TEMP_OPTIMIZED"

# Compare key metrics
echo "## Performance Comparison" >> "$COMPARISON_FILE"
echo "" >> "$COMPARISON_FILE"
echo "| Benchmark | Baseline (ns/op) | Optimized (ns/op) | Change | Improvement |" >> "$COMPARISON_FILE"
echo "|-----------|------------------|-------------------|--------|-------------|" >> "$COMPARISON_FILE"

IMPROVEMENTS=0
REGRESSIONS=0
TOTAL_COMPARED=0

while IFS= read -r baseline_line; do
    bench_name=$(echo "$baseline_line" | awk '{print $1}')
    baseline_time=$(echo "$baseline_line" | awk '{print $2}')
    
    # Find corresponding optimized benchmark
    optimized_line=$(grep "^$bench_name " "$TEMP_OPTIMIZED" || echo "")
    
    if [ -n "$optimized_line" ]; then
        optimized_time=$(echo "$optimized_line" | awk '{print $2}')
        
        # Calculate change (only if both are numbers)
        if [[ "$baseline_time" =~ ^[0-9]+\.?[0-9]*$ ]] && [[ "$optimized_time" =~ ^[0-9]+\.?[0-9]*$ ]]; then
            change=$(echo "scale=2; (($optimized_time - $baseline_time) / $baseline_time) * 100" | bc)
            improvement=$(echo "scale=2; (($baseline_time - $optimized_time) / $baseline_time) * 100" | bc)
            
            # Format output
            if (( $(echo "$improvement > 0" | bc -l) )); then
                status="${GREEN}+${improvement}%${NC}"
                marker="✓"
                IMPROVEMENTS=$((IMPROVEMENTS + 1))
            elif (( $(echo "$improvement < -5" | bc -l) )); then
                status="${RED}${improvement}%${NC}"
                marker="⚠"
                REGRESSIONS=$((REGRESSIONS + 1))
            else
                status="${YELLOW}${improvement}%${NC}"
                marker="~"
            fi
            
            # Add to report
            printf "| %-40s | %15s | %15s | %8s | %s |\n" \
                "$bench_name" "$baseline_time" "$optimized_time" "$change%" "$marker" \
                >> "$COMPARISON_FILE"
            
            TOTAL_COMPARED=$((TOTAL_COMPARED + 1))
        fi
    fi
done < "$TEMP_BASELINE"

echo "" >> "$COMPARISON_FILE"
echo "**Legend:** ✓ = Improvement, ⚠ = Regression, ~ = No significant change" >> "$COMPARISON_FILE"
echo "" >> "$COMPARISON_FILE"

# Summary statistics
cat >> "$COMPARISON_FILE" << EOF
## Summary Statistics

- **Total benchmarks compared:** $TOTAL_COMPARED
- **Improvements:** $IMPROVEMENTS
- **Regressions:** $REGRESSIONS
- **Neutral:** $((TOTAL_COMPARED - IMPROVEMENTS - REGRESSIONS))

EOF

# Memory allocation comparison
echo "## Memory Allocation Comparison" >> "$COMPARISON_FILE"
echo "" >> "$COMPARISON_FILE"
echo "| Benchmark | Baseline (B/op) | Optimized (B/op) | Change |" >> "$COMPARISON_FILE"
echo "|-----------|-----------------|------------------|--------|" >> "$COMPARISON_FILE"

grep "^Benchmark" "$BASELINE_FILE" | while read -r line; do
    bench_name=$(echo "$line" | awk '{print $1}')
    baseline_alloc=$(echo "$line" | awk '{print $5}')
    
    if [ "$baseline_alloc" != "B/op" ] && [[ "$baseline_alloc" =~ ^[0-9]+$ ]]; then
        optimized_line=$(grep "^$bench_name " "$OPTIMIZED_FILE" || echo "")
        if [ -n "$optimized_line" ]; then
            optimized_alloc=$(echo "$optimized_line" | awk '{print $5}')
            
            if [[ "$optimized_alloc" =~ ^[0-9]+$ ]]; then
                if [ "$baseline_alloc" -ne 0 ]; then
                    change=$(echo "scale=2; (($optimized_alloc - $baseline_alloc) / $baseline_alloc) * 100" | bc)
                    printf "| %-40s | %15s | %15s | %8s%% |\n" \
                        "$bench_name" "$baseline_alloc" "$optimized_alloc" "$change" \
                        >> "$COMPARISON_FILE"
                fi
            fi
        fi
    fi
done

echo "" >> "$COMPARISON_FILE"

# Allocations per operation comparison
echo "## Allocations Per Operation" >> "$COMPARISON_FILE"
echo "" >> "$COMPARISON_FILE"
echo "| Benchmark | Baseline (allocs/op) | Optimized (allocs/op) | Change |" >> "$COMPARISON_FILE"
echo "|-----------|----------------------|-----------------------|--------|" >> "$COMPARISON_FILE"

grep "^Benchmark" "$BASELINE_FILE" | while read -r line; do
    bench_name=$(echo "$line" | awk '{print $1}')
    baseline_allocs=$(echo "$line" | awk '{print $6}')
    
    if [ "$baseline_allocs" != "allocs/op" ] && [[ "$baseline_allocs" =~ ^[0-9]+$ ]]; then
        optimized_line=$(grep "^$bench_name " "$OPTIMIZED_FILE" || echo "")
        if [ -n "$optimized_line" ]; then
            optimized_allocs=$(echo "$optimized_line" | awk '{print $6}')
            
            if [[ "$optimized_allocs" =~ ^[0-9]+$ ]]; then
                if [ "$baseline_allocs" -ne 0 ]; then
                    change=$(echo "scale=2; (($optimized_allocs - $baseline_allocs) / $baseline_allocs) * 100" | bc)
                    printf "| %-40s | %20s | %20s | %8s%% |\n" \
                        "$bench_name" "$baseline_allocs" "$optimized_allocs" "$change" \
                        >> "$COMPARISON_FILE"
                fi
            fi
        fi
    fi
done

echo "" >> "$COMPARISON_FILE"

# Recommendations
cat >> "$COMPARISON_FILE" << EOF
## Analysis and Recommendations

EOF

if [ $IMPROVEMENTS -gt $REGRESSIONS ]; then
    cat >> "$COMPARISON_FILE" << EOF
**Overall Assessment:** ✅ POSITIVE

The optimizations have resulted in net performance improvements:
- ${IMPROVEMENTS} benchmarks improved
- ${REGRESSIONS} benchmarks regressed
- Net benefit: $((IMPROVEMENTS - REGRESSIONS)) improved benchmarks

**Recommendation:** Proceed with the optimizations. Review regressions to ensure they are acceptable trade-offs.
EOF
elif [ $REGRESSIONS -gt $IMPROVEMENTS ]; then
    cat >> "$COMPARISON_FILE" << EOF
**Overall Assessment:** ⚠️ MIXED RESULTS

The optimizations have resulted in mixed outcomes:
- ${IMPROVEMENTS} benchmarks improved
- ${REGRESSIONS} benchmarks regressed
- Net impact: $((REGRESSIONS - IMPROVEMENTS)) more regressions than improvements

**Recommendation:** Review the regressions carefully. Consider selective application of optimizations or further tuning.
EOF
else
    cat >> "$COMPARISON_FILE" << EOF
**Overall Assessment:** ~ NEUTRAL

The optimizations have balanced impacts:
- ${IMPROVEMENTS} benchmarks improved
- ${REGRESSIONS} benchmarks regressed
- No significant net change

**Recommendation:** Evaluate specific use cases to determine if optimizations align with target workloads.
EOF
fi

cat >> "$COMPARISON_FILE" << EOF

### Key Observations

1. **Latency Matrix Operations:** $(grep "LatencyMatrix" "$TEMP_OPTIMIZED" | wc -l) benchmarks
2. **Multi-DC Operations:** $(grep "MultiDC" "$TEMP_OPTIMIZED" | wc -l) benchmarks
3. **Metrics Collection:** $(grep "MetricsCollector" "$TEMP_OPTIMIZED" | wc -l) benchmarks
4. **Integration Tests:** $(grep "Integration" "$TEMP_OPTIMIZED" | wc -l) benchmarks

### Next Steps

1. Analyze regressions to understand root causes
2. Profile CPU and memory usage for regressed benchmarks
3. Consider architectural changes for consistently slow operations
4. Re-run benchmarks with different workload patterns
5. Validate optimizations with production-like scenarios

---
*Report generated by compare_benchmarks.sh*
EOF

# Display summary to terminal
cat "$COMPARISON_FILE"

# Cleanup temp files
rm -f "$TEMP_BASELINE" "$TEMP_OPTIMIZED"

echo ""
echo -e "${BLUE}========================================${NC}"
echo ""
echo "Comparison report saved to: $COMPARISON_FILE"
echo ""

if [ $IMPROVEMENTS -gt $REGRESSIONS ]; then
    echo -e "${GREEN}Overall: IMPROVEMENTS > REGRESSIONS ✅${NC}"
    exit 0
elif [ $REGRESSIONS -gt $IMPROVEMENTS ]; then
    echo -e "${YELLOW}Overall: REGRESSIONS > IMPROVEMENTS ⚠️${NC}"
    exit 1
else
    echo -e "${YELLOW}Overall: NEUTRAL ~${NC}"
    exit 0
fi

