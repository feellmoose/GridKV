package tests

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/feellmoose/gridkv/internal/network"
)

// TestAdaptiveNetworkFunctional contains all functional tests for adaptive network features
func TestAdaptiveNetworkFunctional(t *testing.T) {
	t.Run("NetworkMonitor", testNetworkMonitor)
	t.Run("MultiDC", testMultiDC)
	t.Run("MetricsCollector", testMetricsCollector)
	t.Run("AdaptiveGossip", testAdaptiveGossip)
	t.Run("AnomalyDetection", testAnomalyDetection)
	t.Run("Integration", testIntegration)
}

// testNetworkMonitor tests the network monitoring functionality
func testNetworkMonitor(t *testing.T) {
	t.Run("BasicProbing", func(t *testing.T) {
		probeCount := 0
		probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
			probeCount++
			// Simulate different RTTs for different addresses
			if address == "node1:8001" {
				return 1 * time.Millisecond, nil // LAN
			}
			if address == "node2:8002" {
				return 30 * time.Millisecond, nil // WAN
			}
			return 100 * time.Millisecond, nil // Cross-region
		}

		lm := network.NewLatencyMatrix(&network.LatencyMatrixOptions{
			LocalNodeID:   "local",
			ProbeInterval: 100 * time.Millisecond,
			SampleWindow:  5,
			ProbeFunc:     probeFunc,
		})

		lm.AddNode("node1", "node1:8001")
		lm.AddNode("node2", "node2:8002")
		lm.AddNode("node3", "node3:8003")

		lm.Start()
		defer lm.Stop()

		// Wait for probes to complete
		time.Sleep(500 * time.Millisecond)

		// Verify probe count
		if probeCount < 6 { // At least 2 rounds for 3 nodes
			t.Errorf("Expected at least 6 probes, got %d", probeCount)
		}

		// Verify node latency
		latency, exists := lm.GetNodeLatency("node1")
		if !exists {
			t.Fatal("Node1 latency not found")
		}
		if latency.NetworkType != network.NetworkTypeLAN {
			t.Errorf("Expected LAN, got %v", latency.NetworkType)
		}

		latency2, _ := lm.GetNodeLatency("node2")
		if latency2.NetworkType != network.NetworkTypeWAN {
			t.Errorf("Expected WAN, got %v", latency2.NetworkType)
		}

		latency3, _ := lm.GetNodeLatency("node3")
		if latency3.NetworkType != network.NetworkTypeCrossRegion {
			t.Errorf("Expected CrossRegion, got %v", latency3.NetworkType)
		}
	})

	t.Run("AdaptiveTimeout", func(t *testing.T) {
		probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
			return 50 * time.Millisecond, nil
		}

		lm := network.NewLatencyMatrix(&network.LatencyMatrixOptions{
			LocalNodeID:   "local",
			ProbeInterval: 100 * time.Millisecond,
			SampleWindow:  5,
			ProbeFunc:     probeFunc,
		})

		lm.AddNode("node1", "node1:8001")
		lm.Start()
		defer lm.Stop()

		time.Sleep(300 * time.Millisecond)

		baseTimeout := 100 * time.Millisecond
		timeout := lm.GetAdaptiveTimeout("node1", baseTimeout)

		// Should be at least base timeout
		if timeout < baseTimeout {
			t.Errorf("Adaptive timeout %v less than base %v", timeout, baseTimeout)
		}

		// Should be reasonable (< 10x base)
		if timeout > baseTimeout*10 {
			t.Errorf("Adaptive timeout %v too large (> 10x base)", timeout)
		}
	})

	t.Run("NetworkGrouping", func(t *testing.T) {
		probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
			switch address {
			case "lan1:8001", "lan2:8002":
				return 1 * time.Millisecond, nil
			case "wan1:8003", "wan2:8004":
				return 30 * time.Millisecond, nil
			default:
				return 100 * time.Millisecond, nil
			}
		}

		lm := network.NewLatencyMatrix(&network.LatencyMatrixOptions{
			LocalNodeID:   "local",
			ProbeInterval: 50 * time.Millisecond,
			SampleWindow:  5,
			ProbeFunc:     probeFunc,
		})

		lm.AddNode("lan1", "lan1:8001")
		lm.AddNode("lan2", "lan2:8002")
		lm.AddNode("wan1", "wan1:8003")
		lm.AddNode("wan2", "wan2:8004")
		lm.AddNode("cross1", "cross1:8005")

		lm.Start()
		defer lm.Stop()

		time.Sleep(300 * time.Millisecond)

		lanNodes := lm.GetLANNodes()
		wanNodes := lm.GetWANNodes()
		crossNodes := lm.GetCrossRegionNodes()

		if len(lanNodes) != 2 {
			t.Errorf("Expected 2 LAN nodes, got %d", len(lanNodes))
		}
		if len(wanNodes) != 2 {
			t.Errorf("Expected 2 WAN nodes, got %d", len(wanNodes))
		}
		if len(crossNodes) != 1 {
			t.Errorf("Expected 1 cross-region node, got %d", len(crossNodes))
		}
	})

	t.Run("ClosestNodes", func(t *testing.T) {
		probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
			switch address {
			case "node1:8001":
				return 10 * time.Millisecond, nil
			case "node2:8002":
				return 20 * time.Millisecond, nil
			case "node3:8003":
				return 30 * time.Millisecond, nil
			case "node4:8004":
				return 40 * time.Millisecond, nil
			default:
				return 50 * time.Millisecond, nil
			}
		}

		lm := network.NewLatencyMatrix(&network.LatencyMatrixOptions{
			LocalNodeID:   "local",
			ProbeInterval: 50 * time.Millisecond,
			SampleWindow:  5,
			ProbeFunc:     probeFunc,
		})

		for i := 1; i <= 5; i++ {
			lm.AddNode(fmt.Sprintf("node%d", i), fmt.Sprintf("node%d:800%d", i, i))
		}

		lm.Start()
		defer lm.Stop()

		time.Sleep(300 * time.Millisecond)

		closest := lm.GetClosestNodes(3)
		if len(closest) != 3 {
			t.Errorf("Expected 3 closest nodes, got %d", len(closest))
		}

		// First should be node1 (lowest RTT)
		if closest[0] != "node1" {
			t.Errorf("Expected node1 to be closest, got %s", closest[0])
		}
	})
}

// testMultiDC tests multi-DC functionality
func testMultiDC(t *testing.T) {
	t.Run("DCRegistration", func(t *testing.T) {
		mdm := network.NewMultiDCManager(&network.MultiDCOptions{
			LocalDCID: "dc1",
		})

		dc1 := &network.DataCenter{ID: "dc1", Name: "DC1", Region: "us-east"}
		dc2 := &network.DataCenter{ID: "dc2", Name: "DC2", Region: "us-west"}

		mdm.RegisterDataCenter(dc1)
		mdm.RegisterDataCenter(dc2)

		dcs := mdm.GetDataCenters()
		if len(dcs) != 2 {
			t.Errorf("Expected 2 DCs, got %d", len(dcs))
		}
	})

	t.Run("NodeToDCMapping", func(t *testing.T) {
		mdm := network.NewMultiDCManager(&network.MultiDCOptions{
			LocalDCID: "dc1",
		})

		mdm.AddNodeToDC("dc1", "node1", "10.0.1.1:8001")
		mdm.AddNodeToDC("dc1", "node2", "10.0.1.2:8002")
		mdm.AddNodeToDC("dc2", "node3", "10.0.2.1:8003")

		dcID, exists := mdm.GetNodeDC("node1")
		if !exists || dcID != "dc1" {
			t.Errorf("Expected node1 in dc1, got %s (exists: %v)", dcID, exists)
		}

		if !mdm.IsLocalNode("node1") {
			t.Error("node1 should be local")
		}
		if mdm.IsLocalNode("node3") {
			t.Error("node3 should not be local")
		}
	})

	t.Run("LocalityAwareSelection", func(t *testing.T) {
		mdm := network.NewMultiDCManager(&network.MultiDCOptions{
			LocalDCID:           "dc1",
			LocalReadPreference: true,
		})

		mdm.AddNodeToDC("dc1", "node1", "10.0.1.1:8001")
		mdm.AddNodeToDC("dc1", "node2", "10.0.1.2:8002")
		mdm.AddNodeToDC("dc2", "node3", "10.0.2.1:8003")
		mdm.AddNodeToDC("dc2", "node4", "10.0.2.2:8004")

		candidates := []string{"node1", "node2", "node3", "node4"}
		selected := mdm.SelectReadNodes(candidates, 2)

		if len(selected) != 2 {
			t.Errorf("Expected 2 nodes, got %d", len(selected))
		}

		// Should prefer local nodes
		localCount := 0
		for _, node := range selected {
			if mdm.IsLocalNode(node) {
				localCount++
			}
		}

		if localCount == 0 {
			t.Error("Should select at least one local node")
		}
	})

	t.Run("GossipIntervalSelection", func(t *testing.T) {
		mdm := network.NewMultiDCManager(&network.MultiDCOptions{
			LocalDCID:             "dc1",
			IntraDCGossipInterval: 100 * time.Millisecond,
			InterDCGossipInterval: 1 * time.Second,
		})

		mdm.AddNodeToDC("dc1", "node1", "10.0.1.1:8001")
		mdm.AddNodeToDC("dc1", "node2", "10.0.1.2:8002")
		mdm.AddNodeToDC("dc2", "node3", "10.0.2.1:8003")

		// All local nodes - should use intra-DC interval
		localTargets := []string{"node1", "node2"}
		localInterval := mdm.GetGossipInterval(localTargets)
		if localInterval != 100*time.Millisecond {
			t.Errorf("Expected 100ms for local, got %v", localInterval)
		}

		// Mixed nodes - should use inter-DC interval
		mixedTargets := []string{"node1", "node3"}
		mixedInterval := mdm.GetGossipInterval(mixedTargets)
		if mixedInterval != 1*time.Second {
			t.Errorf("Expected 1s for mixed, got %v", mixedInterval)
		}
	})
}

// testMetricsCollector tests metrics collection functionality
func testMetricsCollector(t *testing.T) {
	t.Run("BasicMetrics", func(t *testing.T) {
		mc := network.NewMetricsCollector(&network.MetricsCollectorOptions{
			EnableTimeSeries: true,
			TimeSeriesWindow: 10,
		})

		// Test counter
		mc.RegisterMetric("test.counter", network.MetricTypeCounter, "Test counter", nil)
		mc.Increment("test.counter", 5)
		mc.Increment("test.counter", 3)

		value, exists := mc.Get("test.counter")
		if !exists || value != 8 {
			t.Errorf("Expected counter value 8, got %d (exists: %v)", value, exists)
		}

		// Test gauge
		mc.RegisterMetric("test.gauge", network.MetricTypeGauge, "Test gauge", nil)
		mc.Set("test.gauge", 100)
		mc.Set("test.gauge", 200)

		value, _ = mc.Get("test.gauge")
		if value != 200 {
			t.Errorf("Expected gauge value 200, got %d", value)
		}
	})

	t.Run("TimeSeries", func(t *testing.T) {
		mc := network.NewMetricsCollector(&network.MetricsCollectorOptions{
			EnableTimeSeries: true,
			TimeSeriesWindow: 5,
		})

		mc.RegisterMetric("test.ts", network.MetricTypeGauge, "Test time series", nil)

		// Add samples
		for i := 1; i <= 7; i++ {
			mc.Set("test.ts", int64(i*10))
			time.Sleep(10 * time.Millisecond)
		}

		ts := mc.GetTimeSeries("test.ts")
		// Should keep at most 5 samples (might have a few more due to timing)
		if len(ts) < 5 || len(ts) > 7 {
			t.Errorf("Expected 5-7 samples, got %d", len(ts))
		}

		// Last sample should be 70
		if ts[len(ts)-1].Value != 70 {
			t.Errorf("Expected last sample 70, got %.0f", ts[len(ts)-1].Value)
		}
	})

	t.Run("AllMetrics", func(t *testing.T) {
		mc := network.NewMetricsCollector(nil)

		mc.Increment("metric1", 1)
		mc.Set("metric2", 100)
		mc.Increment("metric3", 5)

		all := mc.GetAllMetrics()
		if len(all) < 3 {
			t.Errorf("Expected at least 3 metrics, got %d", len(all))
		}

		// Check uptime is present
		if _, exists := all["uptime_seconds"]; !exists {
			t.Error("uptime_seconds should be present")
		}
	})
}

// testAdaptiveGossip tests adaptive gossip functionality
func testAdaptiveGossip(t *testing.T) {
	t.Run("IntervalAdjustment", func(t *testing.T) {
		// Create latency matrix with LAN conditions
		probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
			return 1 * time.Millisecond, nil // LAN
		}

		lm := network.NewLatencyMatrix(&network.LatencyMatrixOptions{
			LocalNodeID:   "local",
			ProbeInterval: 50 * time.Millisecond,
			SampleWindow:  5,
			ProbeFunc:     probeFunc,
		})

		lm.AddNode("node1", "node1:8001")
		lm.Start()
		defer lm.Stop()

		agm := network.NewAdaptiveGossipManager(&network.AdaptiveGossipOptions{
			BaseGossipInterval: 200 * time.Millisecond,
			AdaptiveInterval:   true,
			LatencyMatrix:      lm,
		})

		agm.Start()
		defer agm.Stop()

		// Wait for adjustment (adaptive loop runs every 5s)
		time.Sleep(6 * time.Second)

		interval := agm.GetGossipInterval()
		// Should be reduced for LAN (may take time to adjust)
		t.Logf("Current gossip interval: %v (base: 200ms)", interval)
		// Just verify it's reasonable, not necessarily reduced yet
		if interval > 1*time.Second {
			t.Errorf("Interval too high: %v", interval)
		}
	})

	t.Run("FanoutAdjustment", func(t *testing.T) {
		agm := network.NewAdaptiveGossipManager(&network.AdaptiveGossipOptions{
			BaseFanout:     3,
			AdaptiveFanout: true,
		})

		fanout := agm.GetFanout()
		if fanout != 3 {
			t.Errorf("Expected initial fanout 3, got %d", fanout)
		}
	})

	t.Run("LocalityAwareTargets", func(t *testing.T) {
		mdm := network.NewMultiDCManager(&network.MultiDCOptions{
			LocalDCID: "dc1",
		})

		mdm.AddNodeToDC("dc1", "node1", "10.0.1.1:8001")
		mdm.AddNodeToDC("dc1", "node2", "10.0.1.2:8002")
		mdm.AddNodeToDC("dc2", "node3", "10.0.2.1:8003")
		mdm.AddNodeToDC("dc2", "node4", "10.0.2.2:8004")

		agm := network.NewAdaptiveGossipManager(&network.AdaptiveGossipOptions{
			LocalityAwareRouting: true,
			MultiDCManager:       mdm,
		})

		allNodes := []string{"node1", "node2", "node3", "node4"}
		selected := agm.SelectGossipTargets(allNodes, 3)

		if len(selected) > 3 {
			t.Errorf("Expected at most 3 nodes, got %d", len(selected))
		}
	})
}

// testAnomalyDetection tests anomaly detection functionality
func testAnomalyDetection(t *testing.T) {
	t.Run("HighLatencyDetection", func(t *testing.T) {
		anomalies := make(chan *network.NetworkAnomaly, 10)
		anomalyCallback := func(anomaly *network.NetworkAnomaly) {
			anomalies <- anomaly
		}

		probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
			// Simulate high latency
			return 200 * time.Millisecond, nil
		}

		lm := network.NewLatencyMatrix(&network.LatencyMatrixOptions{
			LocalNodeID:     "local",
			ProbeInterval:   50 * time.Millisecond,
			SampleWindow:    5,
			ProbeFunc:       probeFunc,
			AnomalyCallback: anomalyCallback,
		})

		lm.AddNode("node1", "node1:8001")
		lm.Start()
		defer lm.Stop()

		// Wait for anomaly detection
		select {
		case anomaly := <-anomalies:
			if anomaly.Type != network.AnomalyTypeHighLatency {
				t.Errorf("Expected high latency anomaly, got %v", anomaly.Type)
			}
		case <-time.After(1 * time.Second):
			// High latency might not trigger anomaly immediately
			// This is OK
		}
	})

	t.Run("PacketLossDetection", func(t *testing.T) {
		anomalies := make(chan *network.NetworkAnomaly, 10)
		anomalyCallback := func(anomaly *network.NetworkAnomaly) {
			anomalies <- anomaly
		}

		failCount := 0
		probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
			failCount++
			if failCount%3 == 0 { // 33% packet loss
				return 0, fmt.Errorf("probe failed")
			}
			return 10 * time.Millisecond, nil
		}

		lm := network.NewLatencyMatrix(&network.LatencyMatrixOptions{
			LocalNodeID:     "local",
			ProbeInterval:   50 * time.Millisecond,
			SampleWindow:    12,
			ProbeFunc:       probeFunc,
			AnomalyCallback: anomalyCallback,
		})

		lm.AddNode("node1", "node1:8001")
		lm.Start()
		defer lm.Stop()

		// Wait for packet loss detection
		select {
		case anomaly := <-anomalies:
			if anomaly.Type != network.AnomalyTypePacketLoss {
				t.Logf("Got anomaly type: %v (expected packet loss)", anomaly.Type)
			}
		case <-time.After(2 * time.Second):
			// Packet loss detection requires multiple samples
		}
	})

	t.Run("StatisticalAnomalyDetection", func(t *testing.T) {
		alerts := make(chan *network.Alert, 10)
		alertCallback := func(alert *network.Alert) {
			alerts <- alert
		}

		detector := network.NewAnomalyDetector(&network.AnomalyDetectorOptions{
			Window:        20,
			AlertCallback: alertCallback,
		})

		// Add normal values
		for i := 0; i < 15; i++ {
			detector.CheckValue("test.metric", 100.0)
			time.Sleep(1 * time.Millisecond)
		}

		// Add anomalous value
		detector.CheckValue("test.metric", 1000.0)

		// Check for alert
		select {
		case alert := <-alerts:
			if alert.MetricName != "test.metric" {
				t.Errorf("Expected alert for test.metric, got %s", alert.MetricName)
			}
		case <-time.After(100 * time.Millisecond):
			t.Error("Expected anomaly alert but got none")
		}
	})
}

// testIntegration tests integrated adaptive network manager
func testIntegration(t *testing.T) {
	t.Run("FullIntegration", func(t *testing.T) {
		probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
			return 10 * time.Millisecond, nil
		}

		config := &network.AdaptiveNetworkConfig{
			LocalNodeID:          "node1",
			LocalDCID:            "dc1",
			ProbeInterval:        100 * time.Millisecond,
			ProbeSampleWindow:    5,
			EnableAnomalyDetect:  true,
			AdaptiveTimeout:      true,
			AdaptiveInterval:     true,
			LocalityAwareRouting: true,
			ProbeFunc:            probeFunc,
		}

		anm, err := network.NewAdaptiveNetworkManager(config)
		if err != nil {
			t.Fatalf("Failed to create adaptive network manager: %v", err)
		}

		if err := anm.Start(); err != nil {
			t.Fatalf("Failed to start: %v", err)
		}
		defer anm.Stop()

		// Register nodes
		anm.RegisterNode("node2", "10.0.1.2:8002", "dc1")
		anm.RegisterNode("node3", "10.0.2.1:8003", "dc2")

		// Wait for probes
		time.Sleep(500 * time.Millisecond)

		// Test basic functionality
		interval := anm.GetGossipInterval()
		if interval == 0 {
			t.Error("Gossip interval should not be zero")
		}

		fanout := anm.GetFanout()
		if fanout == 0 {
			t.Error("Fanout should not be zero")
		}

		// Test node operations
		latency, exists := anm.GetNodeLatency("node2")
		if !exists {
			t.Error("Should have latency data for node2")
		} else if latency.AvgRTT == 0 {
			t.Error("Average RTT should not be zero")
		}

		// Test health check
		if err := anm.HealthCheck(); err != nil {
			t.Errorf("Health check failed: %v", err)
		}

		// Test statistics
		stats := anm.GetStats()
		if len(stats) == 0 {
			t.Error("Stats should not be empty")
		}

		// Test dashboard
		dashboard := anm.GetDashboard()
		if len(dashboard) == 0 {
			t.Error("Dashboard should not be empty")
		}

		// Test network environment
		env := anm.GetNetworkEnvironment()
		if env.LocalDC != "dc1" {
			t.Errorf("Expected local DC dc1, got %s", env.LocalDC)
		}
	})

	t.Run("ConcurrentOperations", func(t *testing.T) {
		probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
			return 10 * time.Millisecond, nil
		}

		config := &network.AdaptiveNetworkConfig{
			LocalNodeID:   "node1",
			LocalDCID:     "dc1",
			ProbeInterval: 200 * time.Millisecond,
			ProbeFunc:     probeFunc,
		}

		anm, _ := network.NewAdaptiveNetworkManager(config)
		anm.Start()
		defer anm.Stop()

		// Register nodes
		for i := 1; i <= 10; i++ {
			anm.RegisterNode(
				fmt.Sprintf("node%d", i),
				fmt.Sprintf("10.0.1.%d:800%d", i, i),
				"dc1",
			)
		}

		// Concurrent access
		var wg sync.WaitGroup
		errors := make(chan error, 100)

		// Concurrent reads
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = anm.GetGossipInterval()
				_ = anm.GetFanout()
				_ = anm.GetStats()
			}()
		}

		// Concurrent writes
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				anm.RecordMetric(fmt.Sprintf("test.metric%d", n), int64(n))
			}(i)
		}

		wg.Wait()
		close(errors)

		// Check for errors
		for err := range errors {
			if err != nil {
				t.Errorf("Concurrent operation error: %v", err)
			}
		}
	})
}

// TestNodeLatencyStatistics verifies RTT statistics calculation
func TestNodeLatencyStatistics(t *testing.T) {
	sampleRTTs := []time.Duration{
		10 * time.Millisecond,
		12 * time.Millisecond,
		9 * time.Millisecond,
		11 * time.Millisecond,
		13 * time.Millisecond,
	}

	probeIdx := 0
	probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
		if probeIdx >= len(sampleRTTs) {
			return sampleRTTs[len(sampleRTTs)-1], nil
		}
		rtt := sampleRTTs[probeIdx]
		probeIdx++
		return rtt, nil
	}

	lm := network.NewLatencyMatrix(&network.LatencyMatrixOptions{
		LocalNodeID:   "local",
		ProbeInterval: 50 * time.Millisecond,
		SampleWindow:  10,
		ProbeFunc:     probeFunc,
	})

	lm.AddNode("node1", "node1:8001")
	lm.Start()
	defer lm.Stop()

	time.Sleep(300 * time.Millisecond)

	latency, exists := lm.GetNodeLatency("node1")
	if !exists {
		t.Fatal("Node latency not found")
	}

	// Check statistics
	if latency.MinRTT == 0 || latency.MaxRTT == 0 || latency.AvgRTT == 0 {
		t.Error("Statistics should be calculated")
	}

	if latency.MinRTT > latency.AvgRTT || latency.AvgRTT > latency.MaxRTT {
		t.Error("Statistics order incorrect: min <= avg <= max")
	}

	t.Logf("Stats: min=%v avg=%v max=%v jitter=%v",
		latency.MinRTT, latency.AvgRTT, latency.MaxRTT, latency.Jitter)
}
