package main

import (
	"encoding/json"
	"fmt"
	"os"
)

type BenchmarkResult struct {
	TestName          string  `json:"test_name"`
	Timestamp         string  `json:"timestamp"`
	WriteQPS          float64 `json:"write_qps"`
	ReadQPS           float64 `json:"read_qps"`
	DeleteQPS         float64 `json:"delete_qps"`
	TotalQPS          float64 `json:"total_qps"`
	WriteSuccessRate  float64 `json:"write_success_rate"`
	ReadSuccessRate   float64 `json:"read_success_rate"`
	DeleteSuccessRate float64 `json:"delete_success_rate"`
	P50LatencyMs      float64 `json:"p50_latency_ms"`
	P95LatencyMs      float64 `json:"p95_latency_ms"`
	P99LatencyMs      float64 `json:"p99_latency_ms"`
	ReadLatencyP50Ms  float64 `json:"read_latency_p50_ms"`
	ReadLatencyP95Ms  float64 `json:"read_latency_p95_ms"`
	ReadLatencyP99Ms  float64 `json:"read_latency_p99_ms"`
	PeakGoroutines    int     `json:"peak_goroutines"`
	FinalGoroutines   int     `json:"final_goroutines"`
	PeakMemoryMB      float64 `json:"peak_memory_mb"`
	FinalMemoryMB     float64 `json:"final_memory_mb"`
}

func loadResult(filename string) (*BenchmarkResult, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	
	var result BenchmarkResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	
	return &result, nil
}

func formatPercentChange(old, new float64) string {
	if old == 0 {
		return "N/A"
	}
	change := ((new - old) / old) * 100
	if change > 0 {
		return fmt.Sprintf("+%.1f%%", change)
	}
	return fmt.Sprintf("%.1f%%", change)
}

func formatChange(old, new float64) string {
	if old == 0 {
		return "N/A"
	}
	change := ((new - old) / old) * 100
	if change > 0 {
		return fmt.Sprintf("+%.1f%% ⬆️", change)
	} else if change < 0 {
		return fmt.Sprintf("%.1f%% ⬇️", change)
	}
	return "0%"
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: %s <baseline.json> <current.json>\n", os.Args[0])
		os.Exit(1)
	}
	
	baseline, err := loadResult(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load baseline: %v\n", err)
		os.Exit(1)
	}
	
	current, err := loadResult(os.Args[2])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load current: %v\n", err)
		os.Exit(1)
	}
	
	fmt.Printf("Baseline:  %s (%s)\n", baseline.TestName, baseline.Timestamp)
	fmt.Printf("Current:   %s (%s)\n", current.TestName, current.Timestamp)
	fmt.Println()
	
	// QPS comparison
	fmt.Println("QPS Metrics:")
	fmt.Printf("  Total QPS:     %8.2f → %8.2f  %s\n",
		baseline.TotalQPS, current.TotalQPS,
		formatChange(baseline.TotalQPS, current.TotalQPS))
	fmt.Printf("  Write QPS:     %8.2f → %8.2f  %s\n",
		baseline.WriteQPS, current.WriteQPS,
		formatChange(baseline.WriteQPS, current.WriteQPS))
	fmt.Printf("  Read QPS:      %8.2f → %8.2f  %s\n",
		baseline.ReadQPS, current.ReadQPS,
		formatChange(baseline.ReadQPS, current.ReadQPS))
	fmt.Printf("  Delete QPS:    %8.2f → %8.2f  %s\n",
		baseline.DeleteQPS, current.DeleteQPS,
		formatChange(baseline.DeleteQPS, current.DeleteQPS))
	fmt.Println()
	
	// Success rate comparison
	fmt.Println("Success Rates:")
	fmt.Printf("  Write:         %6.1f%% → %6.1f%%  %s\n",
		baseline.WriteSuccessRate, current.WriteSuccessRate,
		formatChange(baseline.WriteSuccessRate, current.WriteSuccessRate))
	fmt.Printf("  Read:          %6.1f%% → %6.1f%%  %s\n",
		baseline.ReadSuccessRate, current.ReadSuccessRate,
		formatChange(baseline.ReadSuccessRate, current.ReadSuccessRate))
	fmt.Printf("  Delete:        %6.1f%% → %6.1f%%  %s\n",
		baseline.DeleteSuccessRate, current.DeleteSuccessRate,
		formatChange(baseline.DeleteSuccessRate, current.DeleteSuccessRate))
	fmt.Println()
	
	// Latency comparison (lower is better, so invert the change)
	fmt.Println("Latency (ms) - Lower is Better:")
	fmt.Printf("  P50:           %7.2f → %7.2f  %s\n",
		baseline.P50LatencyMs, current.P50LatencyMs,
		invertFormatChange(baseline.P50LatencyMs, current.P50LatencyMs))
	fmt.Printf("  P95:           %7.2f → %7.2f  %s\n",
		baseline.P95LatencyMs, current.P95LatencyMs,
		invertFormatChange(baseline.P95LatencyMs, current.P95LatencyMs))
	fmt.Printf("  P99:           %7.2f → %7.2f  %s\n",
		baseline.P99LatencyMs, current.P99LatencyMs,
		invertFormatChange(baseline.P99LatencyMs, current.P99LatencyMs))
	fmt.Println()
	
	fmt.Println("Read Latency (ms) - Lower is Better:")
	fmt.Printf("  P50:           %7.2f → %7.2f  %s\n",
		baseline.ReadLatencyP50Ms, current.ReadLatencyP50Ms,
		invertFormatChange(baseline.ReadLatencyP50Ms, current.ReadLatencyP50Ms))
	fmt.Printf("  P95:           %7.2f → %7.2f  %s\n",
		baseline.ReadLatencyP95Ms, current.ReadLatencyP95Ms,
		invertFormatChange(baseline.ReadLatencyP95Ms, current.ReadLatencyP95Ms))
	fmt.Printf("  P99:           %7.2f → %7.2f  %s\n",
		baseline.ReadLatencyP99Ms, current.ReadLatencyP99Ms,
		invertFormatChange(baseline.ReadLatencyP99Ms, current.ReadLatencyP99Ms))
	fmt.Println()
	
	// Resource comparison
	fmt.Println("Resource Usage:")
	fmt.Printf("  Peak Goroutines:  %4d → %4d  %s\n",
		baseline.PeakGoroutines, current.PeakGoroutines,
		formatChange(float64(baseline.PeakGoroutines), float64(current.PeakGoroutines)))
	fmt.Printf("  Final Goroutines: %4d → %4d  %s\n",
		baseline.FinalGoroutines, current.FinalGoroutines,
		formatChange(float64(baseline.FinalGoroutines), float64(current.FinalGoroutines)))
	fmt.Printf("  Peak Memory (MB): %6.1f → %6.1f  %s\n",
		baseline.PeakMemoryMB, current.PeakMemoryMB,
		formatChange(baseline.PeakMemoryMB, current.PeakMemoryMB))
	fmt.Println()
	
	// Summary
	improvements := 0
	regressions := 0
	
	if current.TotalQPS > baseline.TotalQPS*1.01 {
		improvements++
	} else if current.TotalQPS < baseline.TotalQPS*0.99 {
		regressions++
	}
	
	if current.ReadQPS > baseline.ReadQPS*1.01 {
		improvements++
	} else if current.ReadQPS < baseline.ReadQPS*0.99 {
		regressions++
	}
	
	if current.ReadSuccessRate > baseline.ReadSuccessRate+0.5 {
		improvements++
	} else if current.ReadSuccessRate < baseline.ReadSuccessRate-0.5 {
		regressions++
	}
	
	if current.P99LatencyMs < baseline.P99LatencyMs*0.99 {
		improvements++
	} else if current.P99LatencyMs > baseline.P99LatencyMs*1.01 {
		regressions++
	}
	
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	if improvements > regressions {
		fmt.Printf("✅ Overall: Improvements detected (%d improvements, %d regressions)\n", improvements, regressions)
	} else if regressions > improvements {
		fmt.Printf("⚠️  Overall: Regressions detected (%d improvements, %d regressions)\n", improvements, regressions)
	} else {
		fmt.Printf("➡️  Overall: Similar performance (%d improvements, %d regressions)\n", improvements, regressions)
	}
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
}

func invertFormatChange(old, new float64) string {
	if old == 0 {
		return "N/A"
	}
	change := ((new - old) / old) * 100
	if change < 0 {
		return fmt.Sprintf("%.1f%% ⬇️ (improved)", change)
	} else if change > 0 {
		return fmt.Sprintf("+%.1f%% ⬆️ (worse)", change)
	}
	return "0%"
}

