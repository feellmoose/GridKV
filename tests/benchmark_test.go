// Package tests provides simplified benchmarks for GridKV testing
package tests

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/feellmoose/gridkv/tests/simulator"
)

// BenchmarkBasicOps benchmarks basic operations using the simulator
func BenchmarkBasicOps(b *testing.B) {
	b.StopTimer()

	// Create a small simulator for benchmarking
	sim := simulator.NewSimulator(&simulator.Config{
		NodeCount:     3,
		ReplicaCount:  2,
		BasePort:      29000,
		MemoryMB:      128,
		ShardCount:    32,
		SetupTimeout:  20 * time.Second,
		StabilizeTime: 5 * time.Second,
	})

	if err := sim.SetupCluster(); err != nil {
		b.Fatalf("Failed to setup cluster: %v", err)
	}
	defer sim.Cleanup()

	nodes := sim.GetNodes()
	if len(nodes) == 0 {
		b.Fatal("No nodes available")
	}

	ctx := context.Background()
	keyCount := 1000
	if testing.Short() {
		keyCount = 100
	}

	b.ResetTimer()
	b.StartTimer()

	// Benchmark write operations
	b.Run("Write", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			key := fmt.Sprintf("bench-write-%d", i%keyCount)
			value := make([]byte, 256)
			for j := range value {
				value[j] = byte(i % 256)
			}
			node := nodes[i%len(nodes)]
			if err := node.Set(ctx, key, value); err != nil {
				b.Errorf("Write failed: %v", err)
			}
		}
	})

	// Benchmark read operations (need to prepopulate first)
	b.Run("Read", func(b *testing.B) {
		// Pre-populate some data
		for i := 0; i < keyCount; i++ {
			key := fmt.Sprintf("bench-read-%d", i)
			value := make([]byte, 256)
			for j := range value {
				value[j] = byte(i % 256)
			}
			nodes[i%len(nodes)].Set(ctx, key, value)
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			key := fmt.Sprintf("bench-read-%d", i%keyCount)
			node := nodes[i%len(nodes)]
			_, err := node.Get(ctx, key)
			if err != nil {
				b.Errorf("Read failed: %v", err)
			}
		}
	})
}

// BenchmarkSimulatorWorkload benchmarks using the workload executor
func BenchmarkSimulatorWorkload(b *testing.B) {
	b.StopTimer()

	sim := simulator.NewSimulator(&simulator.Config{
		NodeCount:     3,
		ReplicaCount:  2,
		BasePort:      29100,
		MemoryMB:      128,
		ShardCount:    32,
		SetupTimeout:  20 * time.Second,
		StabilizeTime: 5 * time.Second,
	})

	if err := sim.SetupCluster(); err != nil {
		b.Fatalf("Failed to setup cluster: %v", err)
	}
	defer sim.Cleanup()

	// Limit duration to prevent long-running benchmarks
	// Scale with b.N but cap at 5 seconds to prevent resource exhaustion
	duration := time.Duration(b.N/1000) * time.Millisecond
	if duration > 5*time.Second {
		duration = 5 * time.Second
	}
	if duration < 100*time.Millisecond {
		duration = 100 * time.Millisecond
	}

	executor := simulator.NewWorkloadExecutor(&simulator.WorkloadConfig{
		WorkerCount:  10,
		Duration:     duration,
		WriteRatio:   0.8,
		ReadRatio:    0.2,
		KeySpaceSize: 1000,
		ValueSize:    256,
	}, sim)

	b.StartTimer()
	if err := executor.ExecuteWorkload(); err != nil {
		b.Fatalf("Workload execution failed: %v", err)
	}
	b.StopTimer()

	completed, failed, _, qps := executor.GetStats()
	b.ReportMetric(qps, "qps")
	b.ReportMetric(float64(completed)/float64(completed+failed)*100, "success-rate-percent")
}
