package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/feellmoose/gridkv"
	"github.com/feellmoose/gridkv/internal/network"
)

// This example demonstrates the advanced adaptive network features:
// 1. RTT monitoring and latency matrix
// 2. Adaptive timeout and gossip interval adjustment
// 3. LAN/WAN node detection and grouping
// 4. Multi-DC architecture support
// 5. Locality-aware read/write optimization
// 6. Metrics collection and anomaly detection

func main() {
	fmt.Println("=== GridKV Adaptive Network Example ===")
	fmt.Println()

	// Example 1: Basic setup with adaptive network
	example1BasicSetup()
	
	// Example 2: Multi-DC setup
	example2MultiDC()
	
	// Example 3: Metrics and monitoring
	example3MetricsMonitoring()
}

func example1BasicSetup() {
	fmt.Println("Example 1: Basic Adaptive Network Setup")
	fmt.Println("----------------------------------------")

	// Create a simple network probe function
	probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
		start := time.Now()
		conn, err := net.DialTimeout("tcp", address, 2*time.Second)
		if err != nil {
			return 0, err
		}
		defer conn.Close()
		return time.Since(start), nil
	}

	// Configure adaptive network
	adaptiveConfig := &network.AdaptiveNetworkConfig{
		LocalNodeID:          "node1",
		LocalDCID:            "dc1",
		ProbeInterval:        5 * time.Second,
		ProbeSampleWindow:    12,
		EnableAnomalyDetect:  true,
		BaseGossipInterval:   200 * time.Millisecond,
		BaseFanout:           3,
		BaseTimeout:          1 * time.Second,
		AdaptiveTimeout:      true,
		AdaptiveFanout:       true,
		AdaptiveInterval:     true,
		LocalityAwareRouting: true,
		ProbeFunc:            probeFunc,
		AnomalyCallback: func(anomaly *network.NetworkAnomaly) {
			fmt.Printf("[ANOMALY] %s: %s (Severity: %s)\n",
				anomaly.Type.String(),
				anomaly.Description,
				anomaly.Severity.String())
		},
		AlertCallback: func(alert *network.Alert) {
			fmt.Printf("[ALERT] %s: %s (Severity: %s)\n",
				alert.Title,
				alert.Description,
				alert.Severity.String())
		},
	}

	// Create adaptive network manager
	adaptiveNet, err := network.NewAdaptiveNetworkManager(adaptiveConfig)
	if err != nil {
		log.Fatalf("Failed to create adaptive network: %v", err)
	}

	// Register some nodes
	nodes := []struct {
		id      string
		address string
		dcID    string
	}{
		{"node1", "localhost:8001", "dc1"},
		{"node2", "localhost:8002", "dc1"},
		{"node3", "localhost:8003", "dc2"},
	}

	for _, node := range nodes {
		if err := adaptiveNet.RegisterNode(node.id, node.address, node.dcID); err != nil {
			log.Printf("Failed to register node %s: %v", node.id, err)
		}
	}

	// Start adaptive network
	if err := adaptiveNet.Start(); err != nil {
		log.Fatalf("Failed to start adaptive network: %v", err)
	}
	defer adaptiveNet.Stop()

	// Wait for some probes to complete
	time.Sleep(10 * time.Second)

	// Show adaptive parameters
	fmt.Printf("Current Gossip Interval: %v\n", adaptiveNet.GetGossipInterval())
	fmt.Printf("Current Fanout: %d\n", adaptiveNet.GetFanout())

	// Show network environment
	env := adaptiveNet.GetNetworkEnvironment()
	fmt.Printf("\nNetwork Environment:\n")
	fmt.Printf("  Local DC: %s\n", env.LocalDC)
	fmt.Printf("  Total DCs: %d\n", env.TotalDCs)
	fmt.Printf("  LAN Nodes: %d\n", len(env.LANNodes))
	fmt.Printf("  WAN Nodes: %d\n", len(env.WANNodes))
	fmt.Printf("  Cross-Region Nodes: %d\n", len(env.CrossRegionNodes))
	fmt.Printf("  Average RTT: %v\n", env.AverageRTT)
	fmt.Printf("  Network Health: %.2f%%\n", env.NetworkHealth*100)

	fmt.Println()
}

func example2MultiDC() {
	fmt.Println("Example 2: Multi-DC Architecture")
	fmt.Println("---------------------------------")

	// Create adaptive network with multi-DC support
	adaptiveConfig := &network.AdaptiveNetworkConfig{
		LocalNodeID:            "node1",
		LocalDCID:              "us-east",
		IntraDCGossipInterval:  100 * time.Millisecond, // Fast within DC
		InterDCGossipInterval:  1 * time.Second,        // Slower between DCs
		AsyncReplicationDelay:  500 * time.Millisecond,
		LocalReadPreference:    true,
		LocalWriteOptimization: true,
		BaseGossipInterval:     200 * time.Millisecond,
		BaseFanout:             3,
		LocalityAwareRouting:   true,
	}

	adaptiveNet, err := network.NewAdaptiveNetworkManager(adaptiveConfig)
	if err != nil {
		log.Fatalf("Failed to create adaptive network: %v", err)
	}

	// Register data centers
	dcs := []*network.DataCenter{
		{ID: "us-east", Name: "US East", Region: "us-east-1", Priority: 1},
		{ID: "us-west", Name: "US West", Region: "us-west-1", Priority: 2},
		{ID: "eu-central", Name: "EU Central", Region: "eu-central-1", Priority: 3},
	}

	for _, dc := range dcs {
		adaptiveNet.RegisterDataCenter(dc)
		fmt.Printf("Registered DC: %s (%s)\n", dc.Name, dc.Region)
	}

	// Register nodes in different DCs
	nodes := []struct {
		id      string
		address string
		dcID    string
	}{
		{"node1", "10.0.1.10:8001", "us-east"},
		{"node2", "10.0.1.11:8002", "us-east"},
		{"node3", "10.0.2.10:8003", "us-west"},
		{"node4", "10.0.2.11:8004", "us-west"},
		{"node5", "10.0.3.10:8005", "eu-central"},
	}

	for _, node := range nodes {
		adaptiveNet.RegisterNode(node.id, node.address, node.dcID)
		fmt.Printf("Registered node: %s in %s\n", node.id, node.dcID)
	}

	// Demonstrate locality-aware routing
	allNodes := []string{"node1", "node2", "node3", "node4", "node5"}
	
	readNodes := adaptiveNet.SelectReadNodes(allNodes, 3)
	fmt.Printf("\nSelected nodes for read (locality-aware): %v\n", readNodes)
	
	writeNodes := adaptiveNet.SelectWriteNodes(allNodes, 3)
	fmt.Printf("Selected nodes for write (locality-aware): %v\n", writeNodes)

	// Show different gossip intervals for different targets
	localTargets := []string{"node1", "node2"}
	remoteTargets := []string{"node3", "node4", "node5"}
	
	localInterval := adaptiveNet.GetGossipIntervalForNodes(localTargets)
	remoteInterval := adaptiveNet.GetGossipIntervalForNodes(remoteTargets)
	
	fmt.Printf("\nGossip interval for local DC nodes: %v\n", localInterval)
	fmt.Printf("Gossip interval for remote DC nodes: %v\n", remoteInterval)

	fmt.Println()
}

func example3MetricsMonitoring() {
	fmt.Println("Example 3: Metrics and Monitoring")
	fmt.Println("----------------------------------")

	// Create adaptive network with full monitoring
	alertCount := 0
	adaptiveConfig := &network.AdaptiveNetworkConfig{
		LocalNodeID:         "node1",
		LocalDCID:           "dc1",
		EnableAnomalyDetect: true,
		AlertCallback: func(alert *network.Alert) {
			alertCount++
			fmt.Printf("[Alert #%d] %s - %s (Severity: %s)\n",
				alertCount,
				alert.Title,
				alert.Description,
				alert.Severity.String())
		},
	}

	adaptiveNet, err := network.NewAdaptiveNetworkManager(adaptiveConfig)
	if err != nil {
		log.Fatalf("Failed to create adaptive network: %v", err)
	}

	// Start monitoring
	if err := adaptiveNet.Start(); err != nil {
		log.Fatalf("Failed to start adaptive network: %v", err)
	}
	defer adaptiveNet.Stop()

	// Simulate some metrics
	for i := 0; i < 10; i++ {
		adaptiveNet.IncrementMetric("requests.total", 100)
		adaptiveNet.RecordMetric("latency.avg", int64(time.Millisecond*time.Duration(50+i*10)))
		time.Sleep(1 * time.Second)
	}

	// Get comprehensive dashboard
	dashboard := adaptiveNet.GetDashboard()
	fmt.Println("\n=== Dashboard ===")
	
	if metrics, ok := dashboard["metrics"].(map[string]interface{}); ok {
		fmt.Println("Metrics:")
		for name, value := range metrics {
			fmt.Printf("  %s: %v\n", name, value)
		}
	}

	// Get detailed statistics
	stats := adaptiveNet.GetStats()
	fmt.Println("\n=== Detailed Statistics ===")
	for key, value := range stats {
		fmt.Printf("%s: %v\n", key, value)
	}

	// Show recent alerts
	alerts := adaptiveNet.GetRecentAlerts(5)
	fmt.Printf("\n=== Recent Alerts (%d) ===\n", len(alerts))
	for i, alert := range alerts {
		fmt.Printf("%d. [%s] %s: %s\n",
			i+1,
			alert.Severity.String(),
			alert.Title,
			alert.Description)
	}

	fmt.Println()
}

// Complete example: Full GridKV setup with adaptive network
func exampleFullIntegration() {
	fmt.Println("Complete Example: GridKV with Adaptive Network")
	fmt.Println("===============================================")

	nodeID := "node1"
	nodeAddr := "localhost:8001"
	dcID := "dc1"

	// Create storage
	storageOpts := &gridkv.StorageOptions{
		Backend: gridkv.BackendMemory,
	}

	// Create network options
	networkOpts := &gridkv.NetworkOptions{
		BindAddr:     nodeAddr,
		MaxConns:     100,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	// Create probe function for RTT monitoring
	probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
		start := time.Now()
		dialer := &net.Dialer{Timeout: 2 * time.Second}
		conn, err := dialer.DialContext(ctx, "tcp", address)
		if err != nil {
			return 0, err
		}
		defer conn.Close()
		return time.Since(start), nil
	}

	// Create adaptive network manager
	adaptiveConfig := &network.AdaptiveNetworkConfig{
		LocalNodeID:            nodeID,
		LocalDCID:              dcID,
		ProbeInterval:          5 * time.Second,
		ProbeSampleWindow:      12,
		EnableAnomalyDetect:    true,
		IntraDCGossipInterval:  100 * time.Millisecond,
		InterDCGossipInterval:  1 * time.Second,
		AsyncReplicationDelay:  500 * time.Millisecond,
		LocalReadPreference:    true,
		LocalWriteOptimization: true,
		BaseGossipInterval:     200 * time.Millisecond,
		BaseFanout:             3,
		BaseTimeout:            1 * time.Second,
		AdaptiveTimeout:        true,
		AdaptiveFanout:         true,
		AdaptiveInterval:       true,
		LocalityAwareRouting:   true,
		ProbeFunc:              probeFunc,
		AnomalyCallback: func(anomaly *network.NetworkAnomaly) {
			log.Printf("[ANOMALY] %s on %s: %s",
				anomaly.Type.String(),
				anomaly.NodeID,
				anomaly.Description)
		},
		AlertCallback: func(alert *network.Alert) {
			log.Printf("[ALERT] %s: %s (Severity: %s)",
				alert.Title,
				alert.Description,
				alert.Severity.String())
		},
	}

	adaptiveNet, err := network.NewAdaptiveNetworkManager(adaptiveConfig)
	if err != nil {
		log.Fatalf("Failed to create adaptive network: %v", err)
	}

	// Start adaptive network
	if err := adaptiveNet.Start(); err != nil {
		log.Fatalf("Failed to start adaptive network: %v", err)
	}
	defer adaptiveNet.Stop()

	// Create GridKV instance
	opts := &gridkv.GridKVOptions{
		LocalNodeID:  nodeID,
		LocalAddress: nodeAddr,
		Storage:      storageOpts,
		Network:      networkOpts,
		ReplicaCount: 3,
		WriteQuorum:  2,
		ReadQuorum:   2,
	}

	kv, err := gridkv.NewGridKV(opts)
	if err != nil {
		log.Fatalf("Failed to create GridKV: %v", err)
	}
	defer kv.Close()

	fmt.Printf("GridKV started on %s with adaptive network features\n", nodeAddr)
	fmt.Println("\nAdaptive Network Features Enabled:")
	fmt.Println("  ✓ RTT monitoring and latency matrix")
	fmt.Println("  ✓ Adaptive timeout adjustment")
	fmt.Println("  ✓ Dynamic gossip interval")
	fmt.Println("  ✓ LAN/WAN node detection")
	fmt.Println("  ✓ Multi-DC support")
	fmt.Println("  ✓ Locality-aware routing")
	fmt.Println("  ✓ Metrics collection")
	fmt.Println("  ✓ Anomaly detection and alerts")

	// Wait for interrupt
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start metrics reporting
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				// Report metrics
				env := adaptiveNet.GetNetworkEnvironment()
				fmt.Printf("\n=== Network Status ===\n")
				fmt.Printf("Gossip Interval: %v\n", adaptiveNet.GetGossipInterval())
				fmt.Printf("Fanout: %d\n", adaptiveNet.GetFanout())
				fmt.Printf("Network Health: %.2f%%\n", env.NetworkHealth*100)
				fmt.Printf("Average RTT: %v\n", env.AverageRTT)
				fmt.Printf("LAN/WAN/CrossRegion: %d/%d/%d\n",
					len(env.LANNodes),
					len(env.WANNodes),
					len(env.CrossRegionNodes))

			case <-sigCh:
				return
			}
		}
	}()

	// Perform some operations
	ctx := context.Background()

	// Write some data
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("key%d", i)
		value := []byte(fmt.Sprintf("value%d", i))
		
		if err := kv.Set(ctx, key, value); err != nil {
			log.Printf("Set failed: %v", err)
		} else {
			fmt.Printf("Set %s = %s\n", key, string(value))
		}
		
		time.Sleep(1 * time.Second)
	}

	// Read the data back
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("key%d", i)
		
		value, err := kv.Get(ctx, key)
		if err != nil {
			log.Printf("Get failed: %v", err)
		} else {
			fmt.Printf("Get %s = %s\n", key, string(value))
		}
	}

	// Show final dashboard
	fmt.Println("\n=== Final Dashboard ===")
	dashboard := adaptiveNet.GetDashboard()
	
	if latency, ok := dashboard["latency"].(map[string]interface{}); ok {
		fmt.Println("Latency Stats:")
		for k, v := range latency {
			fmt.Printf("  %s: %v\n", k, v)
		}
	}

	if multidc, ok := dashboard["multidc"].(map[string]interface{}); ok {
		fmt.Println("Multi-DC Stats:")
		for k, v := range multidc {
			fmt.Printf("  %s: %v\n", k, v)
		}
	}

	// Wait for shutdown
	<-sigCh
	fmt.Println("\nShutting down...")
}

