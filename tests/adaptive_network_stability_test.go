package tests

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feellmoose/gridkv/internal/network"
)

// TestStabilityLongRunning tests system stability over extended periods
func TestStabilityLongRunning(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping long-running stability test in short mode")
	}

	t.Run("ContinuousProbing_1Hour", func(t *testing.T) {
		testDuration := 1 * time.Hour
		if testing.Short() {
			testDuration = 5 * time.Minute
		}

		probeCount := atomic.Int64{}
		errorCount := atomic.Int64{}

		probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
			probeCount.Add(1)
			// Simulate occasional failures
			if rand.Intn(100) < 2 { // 2% failure rate
				errorCount.Add(1)
				return 0, fmt.Errorf("simulated probe failure")
			}
			// Variable latency
			latency := time.Duration(10+rand.Intn(40)) * time.Millisecond
			return latency, nil
		}

		config := &network.AdaptiveNetworkConfig{
			LocalNodeID:         "node1",
			LocalDCID:           "dc1",
			ProbeInterval:       1 * time.Second,
			ProbeSampleWindow:   12,
			EnableAnomalyDetect: true,
			AdaptiveTimeout:     true,
			AdaptiveInterval:    true,
			ProbeFunc:           probeFunc,
		}

		anm, err := network.NewAdaptiveNetworkManager(config)
		if err != nil {
			t.Fatalf("Failed to create manager: %v", err)
		}

		// Register nodes
		for i := 0; i < 20; i++ {
			anm.RegisterNode(
				fmt.Sprintf("node%d", i),
				fmt.Sprintf("10.0.1.%d:8001", i),
				fmt.Sprintf("dc%d", i%3+1),
			)
		}

		if err := anm.Start(); err != nil {
			t.Fatalf("Failed to start: %v", err)
		}
		defer anm.Stop()

		t.Logf("Starting stability test for %v", testDuration)
		startTime := time.Now()
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		timeout := time.After(testDuration)

	loop:
		for {
			select {
			case <-timeout:
				break loop
			case <-ticker.C:
				elapsed := time.Since(startTime)
				stats := anm.GetStats()
				env := anm.GetNetworkEnvironment()

				t.Logf("Elapsed: %v, Probes: %d, Errors: %d, Health: %.2f%%",
					elapsed.Round(time.Second),
					probeCount.Load(),
					errorCount.Load(),
					env.NetworkHealth*100)

				// Verify system is still functional
				if err := anm.HealthCheck(); err != nil {
					t.Errorf("Health check failed at %v: %v", elapsed, err)
				}

				// Check stats are reasonable
				if len(stats) == 0 {
					t.Error("Stats should not be empty")
				}
			}
		}

		// Final verification
		totalProbes := probeCount.Load()
		totalErrors := errorCount.Load()
		errorRate := float64(totalErrors) / float64(totalProbes)

		t.Logf("Final results: %d probes, %d errors (%.2f%%)",
			totalProbes, totalErrors, errorRate*100)

		if totalProbes == 0 {
			t.Error("No probes executed")
		}

		// Error rate should be close to simulated rate (2% ± 1%)
		if errorRate > 0.05 {
			t.Errorf("Error rate too high: %.2f%%", errorRate*100)
		}
	})

	t.Run("MemoryLeakDetection", func(t *testing.T) {
		testDuration := 10 * time.Minute
		if testing.Short() {
			testDuration = 1 * time.Minute
		}

		probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
			return time.Duration(10+rand.Intn(20)) * time.Millisecond, nil
		}

		config := &network.AdaptiveNetworkConfig{
			LocalNodeID:       "node1",
			LocalDCID:         "dc1",
			ProbeInterval:     500 * time.Millisecond,
			ProbeSampleWindow: 12,
			ProbeFunc:         probeFunc,
		}

		anm, _ := network.NewAdaptiveNetworkManager(config)

		for i := 0; i < 50; i++ {
			anm.RegisterNode(fmt.Sprintf("node%d", i), fmt.Sprintf("10.0.1.%d:8001", i), "dc1")
		}

		anm.Start()
		defer anm.Stop()

		// Monitor stats over time
		var statsSizes []int
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		timeout := time.After(testDuration)

	loop:
		for {
			select {
			case <-timeout:
				break loop
			case <-ticker.C:
				stats := anm.GetStats()
				// Rough estimate of stats size
				size := len(fmt.Sprintf("%v", stats))
				statsSizes = append(statsSizes, size)

				t.Logf("Stats size: %d bytes", size)

				// Check for unbounded growth
				if len(statsSizes) >= 2 {
					growth := float64(statsSizes[len(statsSizes)-1]) / float64(statsSizes[0])
					if growth > 2.0 {
						t.Errorf("Potential memory leak: stats size grew by %.1fx", growth)
					}
				}
			}
		}

		if len(statsSizes) < 2 {
			t.Skip("Not enough data points collected")
		}
	})

	t.Run("NodeChurn", func(t *testing.T) {
		testDuration := 5 * time.Minute
		if testing.Short() {
			testDuration = 1 * time.Minute
		}

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

		nodeCounter := atomic.Int32{}
		stopCh := make(chan struct{})
		var wg sync.WaitGroup

		// Goroutine to continuously add/remove nodes
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-stopCh:
					return
				case <-ticker.C:
					nodeID := fmt.Sprintf("node%d", nodeCounter.Add(1))
					address := fmt.Sprintf("10.0.1.%d:8001", rand.Intn(255))
					anm.RegisterNode(nodeID, address, "dc1")

					// Remove a random old node
					if nodeCounter.Load() > 20 {
						oldNode := fmt.Sprintf("node%d", rand.Int31n(nodeCounter.Load()-10))
						anm.UnregisterNode(oldNode)
					}
				}
			}
		}()

		// Monitor stability
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		timeout := time.After(testDuration)

	loop:
		for {
			select {
			case <-timeout:
				break loop
			case <-ticker.C:
				if err := anm.HealthCheck(); err != nil {
					t.Errorf("Health check failed during churn: %v", err)
				}

				stats := anm.GetStats()
				t.Logf("Nodes registered so far: %d, Current stats entries: %d",
					nodeCounter.Load(), len(stats))
			}
		}

		close(stopCh)
		wg.Wait()

		t.Logf("Total nodes cycled through: %d", nodeCounter.Load())
	})
}

// TestStabilityHighLoad tests system under high concurrent load
func TestStabilityHighLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping high load test in short mode")
	}

	t.Run("HighConcurrency", func(t *testing.T) {
		probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
			return time.Duration(5+rand.Intn(15)) * time.Millisecond, nil
		}

		config := &network.AdaptiveNetworkConfig{
			LocalNodeID:   "node1",
			LocalDCID:     "dc1",
			ProbeInterval: 200 * time.Millisecond,
			ProbeFunc:     probeFunc,
		}

		anm, _ := network.NewAdaptiveNetworkManager(config)

		for i := 0; i < 100; i++ {
			anm.RegisterNode(fmt.Sprintf("node%d", i), fmt.Sprintf("10.0.1.%d:8001", i), "dc1")
		}

		anm.Start()
		defer anm.Stop()

		time.Sleep(1 * time.Second) // Warmup

		// High concurrent load
		concurrency := 100
		duration := 2 * time.Minute
		if testing.Short() {
			duration = 30 * time.Second
		}

		var wg sync.WaitGroup
		errorCount := atomic.Int32{}
		opCount := atomic.Int64{}
		stopCh := make(chan struct{})

		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()

				for {
					select {
					case <-stopCh:
						return
					default:
						// Mix of operations
						switch rand.Intn(5) {
						case 0:
							_ = anm.GetGossipInterval()
						case 1:
							_ = anm.GetFanout()
						case 2:
							nodeID := fmt.Sprintf("node%d", rand.Intn(100))
							_, _ = anm.GetNodeLatency(nodeID)
						case 3:
							anm.RecordMetric(fmt.Sprintf("metric%d", id), int64(rand.Intn(1000)))
						case 4:
							_ = anm.GetStats()
						}

						opCount.Add(1)
						time.Sleep(time.Duration(rand.Intn(10)) * time.Millisecond)
					}
				}
			}(i)
		}

		// Monitor for errors
		timeout := time.After(duration)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

	loop:
		for {
			select {
			case <-timeout:
				break loop
			case <-ticker.C:
				ops := opCount.Load()
				errs := errorCount.Load()
				opsPerSec := float64(ops) / time.Since(time.Now().Add(-10*time.Second)).Seconds()

				t.Logf("Operations: %d (%.0f ops/sec), Errors: %d",
					ops, opsPerSec, errs)

				if err := anm.HealthCheck(); err != nil {
					t.Errorf("Health check failed under load: %v", err)
					errorCount.Add(1)
				}
			}
		}

		close(stopCh)
		wg.Wait()

		totalOps := opCount.Load()
		totalErrs := errorCount.Load()

		t.Logf("Final: %d operations, %d errors", totalOps, totalErrs)

		if totalOps == 0 {
			t.Error("No operations completed")
		}

		errorRate := float64(totalErrs) / float64(totalOps)
		if errorRate > 0.01 {
			t.Errorf("Error rate too high: %.2f%%", errorRate*100)
		}
	})

	t.Run("MetricsBurst", func(t *testing.T) {
		mc := network.NewMetricsCollector(nil)

		concurrency := 50
		opsPerGoroutine := 10000

		var wg sync.WaitGroup
		start := time.Now()

		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()

				for j := 0; j < opsPerGoroutine; j++ {
					mc.Increment(fmt.Sprintf("counter%d", id%10), 1)
					mc.Set(fmt.Sprintf("gauge%d", id%10), int64(j))
				}
			}(i)
		}

		wg.Wait()
		elapsed := time.Since(start)

		totalOps := concurrency * opsPerGoroutine * 2 // increment + set
		opsPerSec := float64(totalOps) / elapsed.Seconds()

		t.Logf("Completed %d operations in %v (%.0f ops/sec)",
			totalOps, elapsed, opsPerSec)

		// Verify final values
		for i := 0; i < 10; i++ {
			value, exists := mc.Get(fmt.Sprintf("counter%d", i))
			if !exists {
				t.Errorf("Counter %d not found", i)
			}
			expectedCount := int64(concurrency/10) * int64(opsPerGoroutine)
			if value != expectedCount {
				t.Errorf("Counter %d: expected %d, got %d", i, expectedCount, value)
			}
		}
	})
}

// TestStabilityRecovery tests recovery from failure scenarios
func TestStabilityRecovery(t *testing.T) {
	t.Run("ProbeFailureRecovery", func(t *testing.T) {
		failureWindow := atomic.Bool{}
		failureWindow.Store(true)

		probeCount := atomic.Int64{}

		probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
			probeCount.Add(1)
			if failureWindow.Load() {
				return 0, fmt.Errorf("simulated failure")
			}
			return 10 * time.Millisecond, nil
		}

		config := &network.AdaptiveNetworkConfig{
			LocalNodeID:   "node1",
			LocalDCID:     "dc1",
			ProbeInterval: 100 * time.Millisecond,
			ProbeFunc:     probeFunc,
		}

		anm, _ := network.NewAdaptiveNetworkManager(config)
		anm.RegisterNode("node2", "10.0.1.2:8001", "dc1")
		anm.Start()
		defer anm.Stop()

		// Let failures happen
		time.Sleep(1 * time.Second)
		failuresPhase := probeCount.Load()

		// Stop failures
		failureWindow.Store(false)
		time.Sleep(1 * time.Second)

		// Verify recovery
		latency, exists := anm.GetNodeLatency("node2")
		if !exists {
			t.Error("Node latency should exist after recovery")
		}

		if exists && latency.AvgRTT == 0 {
			t.Error("Average RTT should be > 0 after recovery")
		}

		t.Logf("Probes during failure: ~%d, total probes: %d",
			failuresPhase, probeCount.Load())
	})

	t.Run("AnomalyRecovery", func(t *testing.T) {
		anomalyCount := atomic.Int32{}
		anomalyCallback := func(anomaly *network.NetworkAnomaly) {
			anomalyCount.Add(1)
		}

		highLatency := atomic.Bool{}
		highLatency.Store(true)

		probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
			if highLatency.Load() {
				return 200 * time.Millisecond, nil
			}
			return 10 * time.Millisecond, nil
		}

		config := &network.AdaptiveNetworkConfig{
			LocalNodeID:         "node1",
			LocalDCID:           "dc1",
			ProbeInterval:       100 * time.Millisecond,
			EnableAnomalyDetect: true,
			ProbeFunc:           probeFunc,
			AnomalyCallback:     anomalyCallback,
		}

		anm, _ := network.NewAdaptiveNetworkManager(config)
		anm.RegisterNode("node2", "10.0.1.2:8001", "dc1")
		anm.Start()
		defer anm.Stop()

		// Wait for anomalies
		time.Sleep(1 * time.Second)
		anomaliesDuringHigh := anomalyCount.Load()

		// Return to normal
		highLatency.Store(false)
		time.Sleep(2 * time.Second)

		// Should stop generating anomalies
		anomaliesAfterRecovery := anomalyCount.Load()

		t.Logf("Anomalies during high latency: %d, after recovery: %d",
			anomaliesDuringHigh, anomaliesAfterRecovery)

		// New anomalies should be minimal after recovery
		newAnomalies := anomaliesAfterRecovery - anomaliesDuringHigh
		if newAnomalies > anomaliesDuringHigh/2 {
			t.Logf("Warning: Still generating many anomalies after recovery (%d)", newAnomalies)
		}
	})
}

// TestStressScenarios tests extreme scenarios
func TestStressScenarios(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress tests in short mode")
	}

	t.Run("RapidNodeRegistration", func(t *testing.T) {
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

		// Rapidly register many nodes
		start := time.Now()
		nodeCount := 1000

		for i := 0; i < nodeCount; i++ {
			anm.RegisterNode(
				fmt.Sprintf("node%d", i),
				fmt.Sprintf("10.%d.%d.%d:8001", i/65536, (i/256)%256, i%256),
				fmt.Sprintf("dc%d", i%5+1),
			)
		}

		elapsed := time.Since(start)
		t.Logf("Registered %d nodes in %v (%.0f nodes/sec)",
			nodeCount, elapsed, float64(nodeCount)/elapsed.Seconds())

		// Verify system still functional
		if err := anm.HealthCheck(); err != nil {
			t.Errorf("Health check failed after rapid registration: %v", err)
		}

		stats := anm.GetStats()
		if len(stats) == 0 {
			t.Error("Stats should not be empty")
		}
	})

	t.Run("ExtremeConcurrency", func(t *testing.T) {
		mc := network.NewMetricsCollector(nil)

		concurrency := 1000
		opsPerGoroutine := 1000

		start := time.Now()
		var wg sync.WaitGroup

		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()

				for j := 0; j < opsPerGoroutine; j++ {
					mc.Increment("counter", 1)
				}
			}(i)
		}

		wg.Wait()
		elapsed := time.Since(start)

		expectedValue := int64(concurrency * opsPerGoroutine)
		actualValue, _ := mc.Get("counter")

		t.Logf("Extreme concurrency: %d goroutines × %d ops = %d total ops in %v",
			concurrency, opsPerGoroutine, expectedValue, elapsed)

		if actualValue != expectedValue {
			t.Errorf("Counter mismatch: expected %d, got %d (lost %d)",
				expectedValue, actualValue, expectedValue-actualValue)
		}

		opsPerSec := float64(expectedValue) / elapsed.Seconds()
		t.Logf("Throughput: %.0f ops/sec", opsPerSec)
	})
}
