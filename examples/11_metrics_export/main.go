package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/feellmoose/gridkv"
	"github.com/feellmoose/gridkv/internal/metrics"
)

func main() {
	fmt.Println("=== GridKV Enterprise Metrics Example ===")

	// Example 1: Prometheus Format Export
	prometheusExample()

	// Example 2: OTLP JSON Format Export
	otlpJSONExample()

	// Example 3: Real-time Metrics Dashboard
	realTimeDashboard()
}

// Example 1: Export metrics in Prometheus format
func prometheusExample() {
	fmt.Println("--- Example 1: Prometheus Format ---")

	// Create exporter with Prometheus format
	prometheusExporter := metrics.PrometheusExporter(func(text string) error {
		fmt.Println("Prometheus Metrics:")
		fmt.Println(text)
		return nil
	})

	// Create GridKV metrics
	gkMetrics := metrics.NewGridKVMetrics(prometheusExporter)

	// Record some metrics
	gkMetrics.SetClusterNodesTotal(5)
	gkMetrics.SetClusterNodesAlive(5)
	gkMetrics.IncrementRequestsTotal()
	gkMetrics.IncrementRequestsSuccess()
	gkMetrics.IncrementSet()
	gkMetrics.IncrementGet()
	gkMetrics.SetStorageKeys(1000)
	gkMetrics.SetLatencyP50(50000)  // 50µs
	gkMetrics.SetLatencyP95(200000) // 200µs

	// Export
	gkMetrics.Export(context.Background())

	fmt.Println()
}

// Example 2: Export metrics in OTLP JSON format
func otlpJSONExample() {
	fmt.Println("--- Example 2: OTLP JSON Format ---")

	// Create exporter with OTLP JSON format
	otlpExporter := metrics.OTLPJSONExporter("gridkv", "3.1.0", func(data []byte) error {
		fmt.Println("OTLP JSON Metrics:")
		// Pretty print JSON
		fmt.Println(string(data))
		return nil
	})

	// Create GridKV metrics
	gkMetrics := metrics.NewGridKVMetrics(otlpExporter)

	// Record metrics
	gkMetrics.SetClusterNodesTotal(3)
	gkMetrics.SetClusterNodesAlive(3)
	gkMetrics.IncrementRequestsTotal()
	gkMetrics.IncrementRequestsTotal()
	gkMetrics.IncrementRequestsSuccess()
	gkMetrics.IncrementRequestsSuccess()
	gkMetrics.IncrementSet()
	gkMetrics.IncrementGet()

	// Export
	gkMetrics.Export(context.Background())

	fmt.Println()
}

// Example 3: Real-time metrics with GridKV integration
func realTimeDashboard() {
	fmt.Println("--- Example 3: Real-time Metrics Dashboard ---")

	ctx := context.Background()

	// Create metrics exporter (write to file)
	file, _ := os.Create("gridkv_metrics.prom")
	defer file.Close()

	prometheusExporter := metrics.PrometheusExporter(func(text string) error {
		file.Truncate(0)
		file.Seek(0, 0)
		_, err := file.WriteString(text)
		return err
	})

	gkMetrics := metrics.NewGridKVMetrics(prometheusExporter)

	// Create GridKV instance
	kv, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
		LocalNodeID:  "metrics-demo",
		LocalAddress: "localhost:29001",
		SeedAddrs:    nil, // Single node
		Network: &gridkv.NetworkOptions{
			BindAddr: "localhost:29001",
			Type:     gridkv.TCP,
		},
		Storage: &gridkv.StorageOptions{
			Backend: gridkv.BackendMemory,
		},
	})
	if err != nil {
		panic(err)
	}
	defer kv.Close()

	// Simulate operations and collect metrics
	fmt.Println("Simulating operations...")

	for i := 0; i < 100; i++ {
		// Set operation
		key := fmt.Sprintf("key-%d", i)
		value := []byte(fmt.Sprintf("value-%d", i))

		start := time.Now()
		err := kv.Set(ctx, key, value)
		latency := time.Since(start).Nanoseconds()

		// Record metrics
		gkMetrics.IncrementRequestsTotal()
		gkMetrics.IncrementSet()

		if err != nil {
			gkMetrics.IncrementRequestsErrors()
		} else {
			gkMetrics.IncrementRequestsSuccess()
		}

		// Update latency (simplified, should use histogram)
		if i == 0 {
			gkMetrics.SetLatencyP50(latency)
		}
	}

	// Update cluster metrics (simplified for demo)
	gkMetrics.SetClusterNodesTotal(1) // Single node demo
	gkMetrics.SetClusterNodesAlive(1)

	// Update storage metrics (simplified for demo)
	gkMetrics.SetStorageKeys(100) // 100 keys written

	// Export metrics
	gkMetrics.Export(ctx)

	fmt.Println("Metrics exported to gridkv_metrics.prom")
	fmt.Println("\nYou can scrape this file with Prometheus:")
	fmt.Println("  - Add to prometheus.yml:")
	fmt.Println("    scrape_configs:")
	fmt.Println("      - job_name: 'gridkv'")
	fmt.Println("        file_sd_configs:")
	fmt.Println("          - files: ['gridkv_metrics.prom']")
	fmt.Println()
}
