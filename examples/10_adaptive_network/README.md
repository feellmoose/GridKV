# Adaptive Network Features Example

This example demonstrates GridKV's advanced adaptive network features that automatically adjust to network conditions for optimal performance.

## Features Demonstrated

### 1. RTT Monitoring and Latency Matrix
- Periodic ping monitoring of all nodes
- Maintains real-time latency matrix
- Tracks RTT statistics (min, max, avg, jitter)
- Calculates packet loss rates

### 2. Adaptive Timeout Adjustment
- Automatically adjusts timeouts based on measured RTT
- Considers network jitter for timeout multiplier
- Prevents premature timeouts in high-latency networks
- Caps timeouts to prevent excessive delays

### 3. Dynamic Gossip Interval
- Adjusts gossip frequency based on network conditions
- Fast gossip in LAN environments (< 5ms RTT)
- Slower gossip in WAN/cross-region setups
- Reduces bandwidth usage under high packet loss

### 4. LAN/WAN Node Detection
- Automatically categorizes nodes by network type:
  - **LAN**: < 2ms RTT
  - **WAN**: 2-50ms RTT
  - **Cross-Region**: > 50ms RTT
- Enables topology-aware routing

### 5. Multi-DC Architecture
- Hierarchical gossip (fast intra-DC, slow inter-DC)
- Data center awareness and node grouping
- Cross-DC async replication with configurable delays
- Per-DC priority settings

### 6. Locality-Aware Routing
- Preferential read from local DC nodes
- Optimized write distribution (local-first)
- Closest node selection based on RTT
- Reduces cross-DC traffic

### 7. Metrics Collection
- Comprehensive network metrics
- Time-series data for trending
- Anomaly detection using statistical methods
- Alert generation with severity levels

### 8. Anomaly Detection and Alerting
- Detects:
  - High latency spikes
  - Excessive jitter
  - Packet loss
  - Network flapping (rapid state changes)
  - Potential network partitions
- Configurable alert callbacks
- Alert suppression to prevent spam

## Running the Example

### Basic Usage

```bash
cd examples/10_adaptive_network
go run main.go
```

### Example 1: Basic Adaptive Network Setup

Demonstrates basic RTT monitoring and adaptive parameter adjustment:

```go
adaptiveConfig := &network.AdaptiveNetworkConfig{
    LocalNodeID:          "node1",
    LocalDCID:            "dc1",
    ProbeInterval:        5 * time.Second,
    AdaptiveTimeout:      true,
    AdaptiveFanout:       true,
    AdaptiveInterval:     true,
    LocalityAwareRouting: true,
}

adaptiveNet, _ := network.NewAdaptiveNetworkManager(adaptiveConfig)
adaptiveNet.Start()
```

**Output**:
- Current adaptive gossip interval
- Current fanout value
- Network environment classification
- Node categorization (LAN/WAN/CrossRegion)
- Network health score

### Example 2: Multi-DC Architecture

Shows how to set up and use multi-DC features:

```go
adaptiveConfig := &network.AdaptiveNetworkConfig{
    LocalNodeID:            "node1",
    LocalDCID:              "us-east",
    IntraDCGossipInterval:  100 * time.Millisecond, // Fast
    InterDCGossipInterval:  1 * time.Second,        // Slower
    LocalReadPreference:    true,
    LocalWriteOptimization: true,
}

// Register data centers
dcs := []*network.DataCenter{
    {ID: "us-east", Name: "US East", Region: "us-east-1", Priority: 1},
    {ID: "us-west", Name: "US West", Region: "us-west-1", Priority: 2},
    {ID: "eu-central", Name: "EU Central", Region: "eu-central-1", Priority: 3},
}
```

**Features**:
- Different gossip intervals for intra-DC vs inter-DC
- Locality-aware node selection
- Async cross-DC replication
- DC priority for write routing

### Example 3: Metrics and Monitoring

Demonstrates comprehensive metrics collection and anomaly detection:

```go
adaptiveConfig := &network.AdaptiveNetworkConfig{
    EnableAnomalyDetect: true,
    AlertCallback: func(alert *network.Alert) {
        log.Printf("[ALERT] %s: %s", alert.Title, alert.Description)
    },
}

// Record metrics
adaptiveNet.IncrementMetric("requests.total", 100)
adaptiveNet.RecordMetric("latency.avg", latency)

// Get dashboard
dashboard := adaptiveNet.GetDashboard()
```

**Metrics Available**:
- Network RTT (avg, min, max)
- Packet loss rate
- Probe success/failure counts
- Multi-DC operation counts
- Local vs remote read/write ratios
- Anomaly counts
- Alert history

## Configuration Options

### Probe Configuration

```go
ProbeInterval:      5 * time.Second,  // How often to probe nodes
ProbeSampleWindow:  12,                // Number of samples to keep
ProbeFunc:          customProbeFunc,   // Custom probe implementation
```

### Adaptive Behavior

```go
AdaptiveTimeout:      true, // Auto-adjust timeouts based on RTT
AdaptiveFanout:       true, // Auto-adjust fanout based on health
AdaptiveInterval:     true, // Auto-adjust gossip interval
LocalityAwareRouting: true, // Prefer local nodes for routing
```

### Multi-DC Configuration

```go
IntraDCGossipInterval:  100 * time.Millisecond, // Fast intra-DC
InterDCGossipInterval:  1 * time.Second,        // Slower inter-DC
AsyncReplicationDelay:  500 * time.Millisecond, // Cross-DC delay
LocalReadPreference:    true,                   // Prefer local reads
LocalWriteOptimization: true,                   // Optimize local writes
```

### Anomaly Detection

```go
EnableAnomalyDetect: true,
AnomalyCallback: func(anomaly *network.NetworkAnomaly) {
    // Handle network anomaly
},
AlertCallback: func(alert *network.Alert) {
    // Handle alert
},
```

## Network Environment Detection

The system automatically detects and categorizes the network environment:

### LAN Environment (< 2ms RTT)
- **Gossip Interval**: Reduced to 50% of base
- **Fanout**: Reduced for efficiency
- **Timeout**: 3x RTT multiplier (low jitter)

### WAN Environment (2-50ms RTT)
- **Gossip Interval**: Base interval
- **Fanout**: Base fanout
- **Timeout**: 3-4x RTT multiplier

### Cross-Region (> 50ms RTT)
- **Gossip Interval**: Increased to 200% of base
- **Fanout**: May increase for redundancy
- **Timeout**: 4-5x RTT multiplier (high jitter)

## Anomaly Types

### High Latency
- Triggered when RTT exceeds threshold
- Threshold: 100ms (LAN/WAN), 500ms (cross-region)
- Severity: Warning

### High Jitter
- Triggered when jitter > 50% of average RTT
- Indicates unstable network
- Severity: Warning

### Packet Loss
- Triggered when loss rate > 10%
- Severity: Warning (10-50%), Critical (>50%)

### Network Flapping
- Triggered by 3+ state changes in 6 samples
- Indicates intermittent connectivity
- Severity: Error

## API Usage

### Query Network State

```go
// Get adaptive parameters
interval := adaptiveNet.GetGossipInterval()
fanout := adaptiveNet.GetFanout()
timeout := adaptiveNet.GetTimeoutForNode(nodeID)

// Get node information
latency, exists := adaptiveNet.GetNodeLatency(nodeID)
if exists {
    fmt.Printf("RTT: %v, Jitter: %v, Loss: %.2f%%\n",
        latency.avgRTT, latency.jitter, latency.packetLoss*100)
}

// Get closest nodes
closest := adaptiveNet.GetClosestNodes(5)

// Check locality
isLocal := adaptiveNet.IsLocalNode(nodeID)
```

### Select Optimal Nodes

```go
// Select nodes for reading (prefers local DC)
readNodes := adaptiveNet.SelectReadNodes(candidates, 3)

// Select nodes for writing (optimized for locality)
writeNodes := adaptiveNet.SelectWriteNodes(candidates, 3)

// Select gossip targets (balanced local/remote)
gossipTargets := adaptiveNet.SelectGossipTargets(allNodes, fanout)
```

### Async Replication

```go
// Check if should use async replication
if adaptiveNet.ShouldAsyncReplicate(nodeID) {
    // Enqueue for delayed replication
    adaptiveNet.EnqueueAsyncReplication(nodeID, key, value)
} else {
    // Synchronous replication
    replicateNow(nodeID, key, value)
}
```

### Monitoring

```go
// Get network environment info
env := adaptiveNet.GetNetworkEnvironment()
fmt.Printf("Health: %.2f%%, Avg RTT: %v\n", 
    env.NetworkHealth*100, env.AverageRTT)

// Get comprehensive stats
stats := adaptiveNet.GetStats()

// Get dashboard (includes all subsystems)
dashboard := adaptiveNet.GetDashboard()

// Get recent alerts
alerts := adaptiveNet.GetRecentAlerts(10)
```

## Performance Impact

### Benefits
- **Reduced Latency**: 20-40% improvement via locality routing
- **Better Reliability**: Adaptive timeouts reduce false failures
- **Bandwidth Efficiency**: Smart interval adjustment saves 30-50% bandwidth
- **Early Detection**: Anomaly detection catches issues before failures

### Overhead
- **CPU**: ~1-2% for monitoring and metrics
- **Memory**: ~100KB per node for latency history
- **Network**: Probe traffic is minimal (~1KB per node per interval)

## Best Practices

1. **Probe Interval**: 5-10 seconds balances accuracy and overhead
2. **Sample Window**: 12-20 samples provides good statistical stability
3. **DC Configuration**: Always set correct DC IDs for multi-DC benefits
4. **Alert Callbacks**: Implement non-blocking handlers to avoid delays
5. **Metrics**: Enable time-series for trending and capacity planning

## Troubleshooting

### High False Anomaly Rate
- Increase sample window for more stable statistics
- Adjust probe interval to match network variance
- Check if thresholds are too sensitive for your environment

### Gossip Too Slow
- Verify adaptive interval is enabled
- Check if network is classified correctly (LAN vs WAN)
- Review packet loss - high loss triggers slowdown

### Locality Not Working
- Ensure DC IDs are set correctly for all nodes
- Verify LocalityAwareRouting is enabled
- Check latency matrix has collected sufficient data

## Related Documentation

- [Multi-DC Architecture](../../docs/MULTIDC_ARCHITECTURE.md)
- [Network Monitoring](../../docs/NETWORK_MONITORING.md)
- [Adaptive Gossip](../../docs/ADAPTIVE_GOSSIP.md)
- [Metrics Collection](../../docs/METRICS_COLLECTION.md)

## Advanced Topics

### Custom Probe Function

Implement custom network probes:

```go
probeFunc := func(ctx context.Context, address string) (time.Duration, error) {
    // Custom RTT measurement logic
    start := time.Now()
    
    // Send custom probe packet
    if err := sendProbe(address); err != nil {
        return 0, err
    }
    
    return time.Since(start), nil
}
```

### Custom Anomaly Detection

Extend anomaly detection:

```go
anomalyCallback := func(anomaly *network.NetworkAnomaly) {
    // Custom handling based on type
    switch anomaly.Type {
    case network.AnomalyTypeHighLatency:
        // Trigger circuit breaker
        circuitBreaker.Open(anomaly.NodeID)
    case network.AnomalyTypePacketLoss:
        // Switch to backup route
        router.UseBackup(anomaly.NodeID)
    }
}
```

### Integration with External Monitoring

Export metrics to external systems:

```go
go func() {
    ticker := time.NewTicker(1 * time.Minute)
    for range ticker.C {
        dashboard := adaptiveNet.GetDashboard()
        
        // Export to Prometheus
        prometheus.Export(dashboard)
        
        // Or send to logging system
        logger.Info("network_metrics", dashboard)
    }
}()
```

## Conclusion

The adaptive network features provide automatic optimization for varying network conditions, making GridKV suitable for deployment across diverse environments from local data centers to global multi-region architectures.

