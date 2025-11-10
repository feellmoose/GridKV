package tests

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/feellmoose/gridkv/internal/network"
)

// BenchmarkLatencyMatrix benchmarks the latency matrix operations
func BenchmarkLatencyMatrix(b *testing.B) {
	b.Run("AddNode", func(b *testing.B) {
		lm := network.NewLatencyMatrix(&network.LatencyMatrixOptions{
			LocalNodeID:   "local",
			ProbeInterval: 10 * time.Second, // Slow to avoid probing
			SampleWindow:  10,
		})

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			lm.AddNode(fmt.Sprintf("node%d", i), fmt.Sprintf("node%d:8001", i))
		}
	})

	b.Run("GetNodeLatency", func(b *testing.B) {
		probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
			return 10 * time.Millisecond, nil
		}

		lm := network.NewLatencyMatrix(&network.LatencyMatrixOptions{
			LocalNodeID:   "local",
			ProbeInterval: 100 * time.Millisecond,
			SampleWindow:  10,
			ProbeFunc:     probeFunc,
		})

		// Pre-populate
		for i := 0; i < 100; i++ {
			lm.AddNode(fmt.Sprintf("node%d", i), fmt.Sprintf("node%d:8001", i))
		}

		lm.Start()
		defer lm.Stop()
		time.Sleep(500 * time.Millisecond)

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = lm.GetNodeLatency(fmt.Sprintf("node%d", i%100))
		}
	})

	b.Run("GetAdaptiveTimeout", func(b *testing.B) {
		probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
			return 50 * time.Millisecond, nil
		}

		lm := network.NewLatencyMatrix(&network.LatencyMatrixOptions{
			LocalNodeID:   "local",
			ProbeInterval: 100 * time.Millisecond,
			SampleWindow:  10,
			ProbeFunc:     probeFunc,
		})

		lm.AddNode("node1", "node1:8001")
		lm.Start()
		defer lm.Stop()
		time.Sleep(300 * time.Millisecond)

		baseTimeout := 100 * time.Millisecond
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = lm.GetAdaptiveTimeout("node1", baseTimeout)
		}
	})

	b.Run("GetClosestNodes", func(b *testing.B) {
		probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
			return 10 * time.Millisecond, nil
		}

		lm := network.NewLatencyMatrix(&network.LatencyMatrixOptions{
			LocalNodeID:   "local",
			ProbeInterval: 200 * time.Millisecond,
			SampleWindow:  10,
			ProbeFunc:     probeFunc,
		})

		for i := 0; i < 100; i++ {
			lm.AddNode(fmt.Sprintf("node%d", i), fmt.Sprintf("node%d:8001", i))
		}

		lm.Start()
		defer lm.Stop()
		time.Sleep(500 * time.Millisecond)

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = lm.GetClosestNodes(10)
		}
	})

	b.Run("ConcurrentAccess", func(b *testing.B) {
		probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
			return 10 * time.Millisecond, nil
		}

		lm := network.NewLatencyMatrix(&network.LatencyMatrixOptions{
			LocalNodeID:   "local",
			ProbeInterval: 200 * time.Millisecond,
			SampleWindow:  10,
			ProbeFunc:     probeFunc,
		})

		for i := 0; i < 50; i++ {
			lm.AddNode(fmt.Sprintf("node%d", i), fmt.Sprintf("node%d:8001", i))
		}

		lm.Start()
		defer lm.Stop()
		time.Sleep(500 * time.Millisecond)

		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				_, _ = lm.GetNodeLatency(fmt.Sprintf("node%d", i%50))
				i++
			}
		})
	})
}

// BenchmarkMultiDC benchmarks multi-DC operations
func BenchmarkMultiDC(b *testing.B) {
	b.Run("AddNodeToDC", func(b *testing.B) {
		mdm := network.NewMultiDCManager(&network.MultiDCOptions{
			LocalDCID: "dc1",
		})

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			dcID := fmt.Sprintf("dc%d", i%3+1)
			_ = mdm.AddNodeToDC(dcID, fmt.Sprintf("node%d", i), fmt.Sprintf("10.0.%d.%d:8001", i%3+1, i%255))
		}
	})

	b.Run("SelectReadNodes", func(b *testing.B) {
		mdm := network.NewMultiDCManager(&network.MultiDCOptions{
			LocalDCID:           "dc1",
			LocalReadPreference: true,
		})

		// Pre-populate
		for i := 0; i < 100; i++ {
			dcID := fmt.Sprintf("dc%d", i%3+1)
			_ = mdm.AddNodeToDC(dcID, fmt.Sprintf("node%d", i), fmt.Sprintf("10.0.%d.%d:8001", i%3+1, i))
		}

		candidates := make([]string, 100)
		for i := 0; i < 100; i++ {
			candidates[i] = fmt.Sprintf("node%d", i)
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = mdm.SelectReadNodes(candidates, 10)
		}
	})

	b.Run("IsLocalNode", func(b *testing.B) {
		mdm := network.NewMultiDCManager(&network.MultiDCOptions{
			LocalDCID: "dc1",
		})

		for i := 0; i < 1000; i++ {
			dcID := fmt.Sprintf("dc%d", i%3+1)
			_ = mdm.AddNodeToDC(dcID, fmt.Sprintf("node%d", i), fmt.Sprintf("10.0.1.%d:8001", i%255))
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = mdm.IsLocalNode(fmt.Sprintf("node%d", i%1000))
		}
	})

	b.Run("GetGossipInterval", func(b *testing.B) {
		mdm := network.NewMultiDCManager(&network.MultiDCOptions{
			LocalDCID:             "dc1",
			IntraDCGossipInterval: 100 * time.Millisecond,
			InterDCGossipInterval: 1 * time.Second,
		})

		for i := 0; i < 100; i++ {
			dcID := fmt.Sprintf("dc%d", i%3+1)
			_ = mdm.AddNodeToDC(dcID, fmt.Sprintf("node%d", i), fmt.Sprintf("10.0.1.%d:8001", i))
		}

		targets := []string{"node1", "node2", "node50"}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = mdm.GetGossipInterval(targets)
		}
	})
}

// BenchmarkMetricsCollector benchmarks metrics collection
func BenchmarkMetricsCollector(b *testing.B) {
	b.Run("Increment", func(b *testing.B) {
		mc := network.NewMetricsCollector(nil)

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			mc.Increment("test.counter", 1)
		}
	})

	b.Run("Set", func(b *testing.B) {
		mc := network.NewMetricsCollector(nil)

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			mc.Set("test.gauge", int64(i))
		}
	})

	b.Run("Get", func(b *testing.B) {
		mc := network.NewMetricsCollector(nil)
		mc.Set("test.metric", 100)

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = mc.Get("test.metric")
		}
	})

	b.Run("ConcurrentIncrement", func(b *testing.B) {
		mc := network.NewMetricsCollector(nil)

		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				mc.Increment("test.counter", 1)
			}
		})
	})

	b.Run("ConcurrentSet", func(b *testing.B) {
		mc := network.NewMetricsCollector(nil)

		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				mc.Set("test.gauge", int64(i))
				i++
			}
		})
	})

	b.Run("MixedOperations", func(b *testing.B) {
		mc := network.NewMetricsCollector(nil)

		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				switch i % 3 {
				case 0:
					mc.Increment("counter", 1)
				case 1:
					mc.Set("gauge", int64(i))
				case 2:
					_, _ = mc.Get("counter")
				}
				i++
			}
		})
	})
}

// BenchmarkAnomalyDetector benchmarks anomaly detection
func BenchmarkAnomalyDetector(b *testing.B) {
	b.Run("CheckValue", func(b *testing.B) {
		detector := network.NewAnomalyDetector(&network.AnomalyDetectorOptions{
			Window: 20,
		})

		// Pre-populate with normal values
		for i := 0; i < 20; i++ {
			detector.CheckValue("test.metric", 100.0)
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			detector.CheckValue("test.metric", 100.0+float64(i%10))
		}
	})

	b.Run("MultipleMetrics", func(b *testing.B) {
		detector := network.NewAnomalyDetector(&network.AnomalyDetectorOptions{
			Window: 20,
		})

		// Pre-populate
		for j := 0; j < 10; j++ {
			for i := 0; i < 20; i++ {
				detector.CheckValue(fmt.Sprintf("metric%d", j), 100.0)
			}
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			metricName := fmt.Sprintf("metric%d", i%10)
			detector.CheckValue(metricName, 100.0+float64(i%10))
		}
	})
}

// BenchmarkAdaptiveGossip benchmarks adaptive gossip operations
func BenchmarkAdaptiveGossip(b *testing.B) {
	b.Run("GetGossipInterval", func(b *testing.B) {
		agm := network.NewAdaptiveGossipManager(&network.AdaptiveGossipOptions{
			BaseGossipInterval: 200 * time.Millisecond,
		})

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = agm.GetGossipInterval()
		}
	})

	b.Run("GetFanout", func(b *testing.B) {
		agm := network.NewAdaptiveGossipManager(&network.AdaptiveGossipOptions{
			BaseFanout: 3,
		})

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = agm.GetFanout()
		}
	})

	b.Run("SelectGossipTargets", func(b *testing.B) {
		mdm := network.NewMultiDCManager(&network.MultiDCOptions{
			LocalDCID: "dc1",
		})

		for i := 0; i < 100; i++ {
			dcID := fmt.Sprintf("dc%d", i%3+1)
			_ = mdm.AddNodeToDC(dcID, fmt.Sprintf("node%d", i), fmt.Sprintf("10.0.1.%d:8001", i))
		}

		agm := network.NewAdaptiveGossipManager(&network.AdaptiveGossipOptions{
			BaseFanout:           3,
			LocalityAwareRouting: true,
			MultiDCManager:       mdm,
		})

		allNodes := make([]string, 100)
		for i := 0; i < 100; i++ {
			allNodes[i] = fmt.Sprintf("node%d", i)
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = agm.SelectGossipTargets(allNodes, 5)
		}
	})
}

// BenchmarkIntegration benchmarks integrated operations
func BenchmarkIntegration(b *testing.B) {
	b.Run("CreateManager", func(b *testing.B) {
		probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
			return 10 * time.Millisecond, nil
		}

		config := &network.AdaptiveNetworkConfig{
			LocalNodeID:   "node1",
			LocalDCID:     "dc1",
			ProbeInterval: 10 * time.Second, // Slow to avoid overhead
			ProbeFunc:     probeFunc,
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			anm, err := network.NewAdaptiveNetworkManager(config)
			if err != nil {
				b.Fatal(err)
			}
			_ = anm
		}
	})

	b.Run("RegisterNode", func(b *testing.B) {
		probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
			return 10 * time.Millisecond, nil
		}

		config := &network.AdaptiveNetworkConfig{
			LocalNodeID:   "node1",
			LocalDCID:     "dc1",
			ProbeInterval: 10 * time.Second,
			ProbeFunc:     probeFunc,
		}

		anm, _ := network.NewAdaptiveNetworkManager(config)

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = anm.RegisterNode(
				fmt.Sprintf("node%d", i),
				fmt.Sprintf("10.0.1.%d:8001", i%255),
				"dc1",
			)
		}
	})

	b.Run("GetStats", func(b *testing.B) {
		probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
			return 10 * time.Millisecond, nil
		}

		config := &network.AdaptiveNetworkConfig{
			LocalNodeID:   "node1",
			LocalDCID:     "dc1",
			ProbeInterval: 1 * time.Second,
			ProbeFunc:     probeFunc,
		}

		anm, _ := network.NewAdaptiveNetworkManager(config)
		anm.Start()
		defer anm.Stop()

		for i := 0; i < 10; i++ {
			_ = anm.RegisterNode(fmt.Sprintf("node%d", i), fmt.Sprintf("10.0.1.%d:8001", i), "dc1")
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = anm.GetStats()
		}
	})

	b.Run("GetDashboard", func(b *testing.B) {
		probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
			return 10 * time.Millisecond, nil
		}

		config := &network.AdaptiveNetworkConfig{
			LocalNodeID:   "node1",
			LocalDCID:     "dc1",
			ProbeInterval: 1 * time.Second,
			ProbeFunc:     probeFunc,
		}

		anm, _ := network.NewAdaptiveNetworkManager(config)
		anm.Start()
		defer anm.Stop()

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = anm.GetDashboard()
		}
	})

	b.Run("ConcurrentOperations", func(b *testing.B) {
		probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
			return 10 * time.Millisecond, nil
		}

		config := &network.AdaptiveNetworkConfig{
			LocalNodeID:   "node1",
			LocalDCID:     "dc1",
			ProbeInterval: 500 * time.Millisecond,
			ProbeFunc:     probeFunc,
		}

		anm, _ := network.NewAdaptiveNetworkManager(config)
		anm.Start()
		defer anm.Stop()

		for i := 0; i < 50; i++ {
			_ = anm.RegisterNode(fmt.Sprintf("node%d", i), fmt.Sprintf("10.0.1.%d:8001", i), "dc1")
		}

		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				switch i % 5 {
				case 0:
					_ = anm.GetGossipInterval()
				case 1:
					_ = anm.GetFanout()
				case 2:
					_, _ = anm.GetNodeLatency(fmt.Sprintf("node%d", i%50))
				case 3:
					anm.RecordMetric("test.metric", int64(i))
				case 4:
					_ = anm.GetStats()
				}
				i++
			}
		})
	})
}

// BenchmarkScalability tests performance at different scales
func BenchmarkScalability(b *testing.B) {
	scales := []int{10, 50, 100, 500, 1000}

	for _, nodeCount := range scales {
		b.Run(fmt.Sprintf("Nodes_%d", nodeCount), func(b *testing.B) {
			probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
				return 10 * time.Millisecond, nil
			}

			config := &network.AdaptiveNetworkConfig{
				LocalNodeID:   "node1",
				LocalDCID:     "dc1",
				ProbeInterval: 10 * time.Second, // Slow to focus on read performance
				ProbeFunc:     probeFunc,
			}

			anm, _ := network.NewAdaptiveNetworkManager(config)

			// Register nodes
			for i := 0; i < nodeCount; i++ {
				_ = anm.RegisterNode(
					fmt.Sprintf("node%d", i),
					fmt.Sprintf("10.0.%d.%d:8001", i/255, i%255),
					fmt.Sprintf("dc%d", i%3+1),
				)
			}

			anm.Start()
			defer anm.Stop()

			time.Sleep(100 * time.Millisecond) // Brief warmup

			candidates := make([]string, nodeCount)
			for i := 0; i < nodeCount; i++ {
				candidates[i] = fmt.Sprintf("node%d", i)
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = anm.SelectReadNodes(candidates, 10)
			}

			b.StopTimer()
			b.ReportMetric(float64(nodeCount), "nodes")
		})
	}
}

// BenchmarkMemoryAllocation tests memory allocation patterns
func BenchmarkMemoryAllocation(b *testing.B) {
	b.Run("LatencyMatrixAllocation", func(b *testing.B) {
		b.ReportAllocs()
		probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
			return 10 * time.Millisecond, nil
		}

		for i := 0; i < b.N; i++ {
			lm := network.NewLatencyMatrix(&network.LatencyMatrixOptions{
				LocalNodeID:   "local",
				ProbeInterval: 10 * time.Second,
				SampleWindow:  10,
				ProbeFunc:     probeFunc,
			})
			_ = lm
		}
	})

	b.Run("MetricsCollectorAllocation", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			mc := network.NewMetricsCollector(nil)
			mc.Increment("test", 1)
			mc.Set("gauge", 100)
		}
	})

	b.Run("AdaptiveManagerAllocation", func(b *testing.B) {
		b.ReportAllocs()
		probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
			return 10 * time.Millisecond, nil
		}

		config := &network.AdaptiveNetworkConfig{
			LocalNodeID:   "node1",
			LocalDCID:     "dc1",
			ProbeInterval: 10 * time.Second,
			ProbeFunc:     probeFunc,
		}

		for i := 0; i < b.N; i++ {
			anm, _ := network.NewAdaptiveNetworkManager(config)
			_ = anm
		}
	})
}
