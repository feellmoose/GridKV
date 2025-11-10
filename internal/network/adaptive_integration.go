package network

import (
	"fmt"
	"sync"
	"time"
)

// AdaptiveNetworkManager is the main integration point for all adaptive network features
type AdaptiveNetworkManager struct {
	mu sync.RWMutex

	// Core components
	latencyMatrix      *LatencyMatrix
	multiDCManager     *MultiDCManager
	metricsCollector   *MetricsCollector
	adaptiveGossip     *AdaptiveGossipManager
	integratedMetrics  *IntegratedNetworkMetrics
	alertManager       *AlertManager
	asyncReplicationQ  *AsyncReplicationQueue

	// Configuration
	localNodeID string
	localDCID   string
	enabled     bool

	// Control
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// AdaptiveNetworkConfig configures the adaptive network manager
type AdaptiveNetworkConfig struct {
	LocalNodeID string
	LocalDCID   string

	// Monitoring configuration
	ProbeInterval        time.Duration
	ProbeSampleWindow    int
	EnableAnomalyDetect  bool
	
	// Multi-DC configuration
	IntraDCGossipInterval  time.Duration
	InterDCGossipInterval  time.Duration
	AsyncReplicationDelay  time.Duration
	LocalReadPreference    bool
	LocalWriteOptimization bool

	// Adaptive gossip configuration
	BaseGossipInterval   time.Duration
	BaseFanout           int
	BaseTimeout          time.Duration
	AdaptiveTimeout      bool
	AdaptiveFanout       bool
	AdaptiveInterval     bool
	LocalityAwareRouting bool

	// Callbacks
	ProbeFunc        ProbeFunc
	ReplicationFunc  ReplicationFunc
	AnomalyCallback  AnomalyCallback
	AlertCallback    AlertCallback
}

// NewAdaptiveNetworkManager creates a new adaptive network manager with all features
func NewAdaptiveNetworkManager(config *AdaptiveNetworkConfig) (*AdaptiveNetworkManager, error) {
	if config == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	if config.LocalNodeID == "" {
		return nil, fmt.Errorf("LocalNodeID is required")
	}

	// Apply defaults
	applyDefaults(config)

	anm := &AdaptiveNetworkManager{
		localNodeID: config.LocalNodeID,
		localDCID:   config.LocalDCID,
		enabled:     true,
		stopCh:      make(chan struct{}),
	}

	// Initialize metrics collector
	anm.metricsCollector = NewMetricsCollector(&MetricsCollectorOptions{
		EnableTimeSeries: true,
		TimeSeriesWindow: config.ProbeSampleWindow,
		AnomalyDetection: config.EnableAnomalyDetect,
	})

	// Initialize alert manager
	anm.alertManager = NewAlertManager(1000, 5*time.Minute)

	// Register alert callbacks
	if config.AlertCallback != nil {
		anm.alertManager.RegisterCallback(config.AlertCallback)
	}

	// Create combined anomaly callback
	anomalyCallback := func(anomaly *NetworkAnomaly) {
		// Convert to alert
		alert := &Alert{
			Timestamp:   anomaly.Timestamp,
			Severity:    convertAnomalySeverity(anomaly.Severity),
			Title:       fmt.Sprintf("%s: %s", anomaly.Type.String(), anomaly.NodeID),
			Description: anomaly.Description,
			MetricName:  "network.anomaly",
			Labels: map[string]string{
				"node_id": anomaly.NodeID,
				"type":    anomaly.Type.String(),
			},
		}
		anm.alertManager.TriggerAlert(alert)

		// Call user callback if provided
		if config.AnomalyCallback != nil {
			config.AnomalyCallback(anomaly)
		}
	}

	// Initialize latency matrix
	anm.latencyMatrix = NewLatencyMatrix(&LatencyMatrixOptions{
		LocalNodeID:     config.LocalNodeID,
		ProbeInterval:   config.ProbeInterval,
		SampleWindow:    config.ProbeSampleWindow,
		ProbeFunc:       config.ProbeFunc,
		AnomalyCallback: anomalyCallback,
	})

	// Initialize multi-DC manager
	anm.multiDCManager = NewMultiDCManager(&MultiDCOptions{
		LocalDCID:              config.LocalDCID,
		IntraDCGossipInterval:  config.IntraDCGossipInterval,
		InterDCGossipInterval:  config.InterDCGossipInterval,
		AsyncReplicationDelay:  config.AsyncReplicationDelay,
		LocalReadPreference:    config.LocalReadPreference,
		LocalWriteOptimization: config.LocalWriteOptimization,
		LatencyMatrix:          anm.latencyMatrix,
	})

	// Initialize integrated metrics
	anm.integratedMetrics = NewIntegratedNetworkMetrics(
		anm.metricsCollector,
		anm.latencyMatrix,
		anm.multiDCManager,
	)

	// Initialize adaptive gossip manager
	anm.adaptiveGossip = NewAdaptiveGossipManager(&AdaptiveGossipOptions{
		BaseGossipInterval:   config.BaseGossipInterval,
		BaseFanout:           config.BaseFanout,
		BaseTimeout:          config.BaseTimeout,
		AdaptiveTimeout:      config.AdaptiveTimeout,
		AdaptiveFanout:       config.AdaptiveFanout,
		AdaptiveInterval:     config.AdaptiveInterval,
		LocalityAwareRouting: config.LocalityAwareRouting,
		LatencyMatrix:        anm.latencyMatrix,
		MultiDCManager:       anm.multiDCManager,
		Metrics:              anm.integratedMetrics,
	})

	// Initialize async replication queue
	if config.ReplicationFunc != nil {
		anm.asyncReplicationQ = NewAsyncReplicationQueue(anm.multiDCManager, config.ReplicationFunc)
	}

	return anm, nil
}

// applyDefaults applies default values to config
func applyDefaults(config *AdaptiveNetworkConfig) {
	if config.ProbeInterval == 0 {
		config.ProbeInterval = 5 * time.Second
	}
	if config.ProbeSampleWindow == 0 {
		config.ProbeSampleWindow = 12
	}
	if config.IntraDCGossipInterval == 0 {
		config.IntraDCGossipInterval = 100 * time.Millisecond
	}
	if config.InterDCGossipInterval == 0 {
		config.InterDCGossipInterval = 1 * time.Second
	}
	if config.AsyncReplicationDelay == 0 {
		config.AsyncReplicationDelay = 500 * time.Millisecond
	}
	if config.BaseGossipInterval == 0 {
		config.BaseGossipInterval = 200 * time.Millisecond
	}
	if config.BaseFanout == 0 {
		config.BaseFanout = 3
	}
	if config.BaseTimeout == 0 {
		config.BaseTimeout = 1 * time.Second
	}
}

// convertAnomalySeverity converts network anomaly severity to alert severity
func convertAnomalySeverity(severity AnomalySeverity) AlertSeverity {
	switch severity {
	case SeverityInfo:
		return AlertSeverityInfo
	case SeverityWarning:
		return AlertSeverityWarning
	case SeverityError:
		return AlertSeverityError
	case SeverityCritical:
		return AlertSeverityCritical
	default:
		return AlertSeverityInfo
	}
}

// Start starts all adaptive network components
func (anm *AdaptiveNetworkManager) Start() error {
	anm.mu.Lock()
	defer anm.mu.Unlock()

	if !anm.enabled {
		return fmt.Errorf("adaptive network manager is disabled")
	}

	// Start latency monitoring
	if anm.latencyMatrix != nil {
		anm.latencyMatrix.Start()
	}

	// Start adaptive gossip
	if anm.adaptiveGossip != nil {
		anm.adaptiveGossip.Start()
	}

	// Start async replication queue
	if anm.asyncReplicationQ != nil {
		anm.asyncReplicationQ.Start()
	}

	// Start metrics update loop
	anm.wg.Add(1)
	go anm.metricsUpdateLoop()

	return nil
}

// Stop stops all adaptive network components
func (anm *AdaptiveNetworkManager) Stop() {
	anm.mu.Lock()
	anm.enabled = false
	anm.mu.Unlock()

	close(anm.stopCh)
	anm.wg.Wait()

	if anm.latencyMatrix != nil {
		anm.latencyMatrix.Stop()
	}

	if anm.adaptiveGossip != nil {
		anm.adaptiveGossip.Stop()
	}

	if anm.asyncReplicationQ != nil {
		anm.asyncReplicationQ.Stop()
	}
}

// metricsUpdateLoop periodically updates integrated metrics
func (anm *AdaptiveNetworkManager) metricsUpdateLoop() {
	defer anm.wg.Done()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-anm.stopCh:
			return
		case <-ticker.C:
			if anm.integratedMetrics != nil {
				anm.integratedMetrics.Update()
			}
		}
	}
}

// RegisterNode registers a node for monitoring
func (anm *AdaptiveNetworkManager) RegisterNode(nodeID, address, dcID string) error {
	anm.mu.RLock()
	defer anm.mu.RUnlock()

	if !anm.enabled {
		return fmt.Errorf("adaptive network manager is disabled")
	}

	// Add to multi-DC manager
	if anm.multiDCManager != nil {
		if err := anm.multiDCManager.AddNodeToDC(dcID, nodeID, address); err != nil {
			return err
		}
	}

	// Add to latency matrix (if not local node)
	if nodeID != anm.localNodeID && anm.latencyMatrix != nil {
		anm.latencyMatrix.AddNode(nodeID, address)
	}

	return nil
}

// UnregisterNode removes a node from monitoring
func (anm *AdaptiveNetworkManager) UnregisterNode(nodeID string) {
	if anm.multiDCManager != nil {
		anm.multiDCManager.RemoveNode(nodeID)
	}

	if anm.latencyMatrix != nil {
		anm.latencyMatrix.RemoveNode(nodeID)
	}
}

// RegisterDataCenter registers a data center
func (anm *AdaptiveNetworkManager) RegisterDataCenter(dc *DataCenter) {
	if anm.multiDCManager != nil {
		anm.multiDCManager.RegisterDataCenter(dc)
	}
}

// GetGossipInterval returns the current adaptive gossip interval
func (anm *AdaptiveNetworkManager) GetGossipInterval() time.Duration {
	if anm.adaptiveGossip != nil {
		return anm.adaptiveGossip.GetGossipInterval()
	}
	return 200 * time.Millisecond
}

// GetGossipIntervalForNodes returns gossip interval for specific nodes
func (anm *AdaptiveNetworkManager) GetGossipIntervalForNodes(targetNodes []string) time.Duration {
	if anm.adaptiveGossip != nil {
		return anm.adaptiveGossip.GetGossipIntervalForNodes(targetNodes)
	}
	return 200 * time.Millisecond
}

// GetFanout returns the current adaptive fanout
func (anm *AdaptiveNetworkManager) GetFanout() int {
	if anm.adaptiveGossip != nil {
		return anm.adaptiveGossip.GetFanout()
	}
	return 3
}

// GetTimeoutForNode returns adaptive timeout for a node
func (anm *AdaptiveNetworkManager) GetTimeoutForNode(nodeID string) time.Duration {
	if anm.adaptiveGossip != nil {
		return anm.adaptiveGossip.GetTimeoutForNode(nodeID)
	}
	return 1 * time.Second
}

// SelectGossipTargets selects optimal gossip targets
func (anm *AdaptiveNetworkManager) SelectGossipTargets(allNodes []string, count int) []string {
	if anm.adaptiveGossip != nil {
		return anm.adaptiveGossip.SelectGossipTargets(allNodes, count)
	}
	
	// Fallback to simple selection
	if len(allNodes) <= count {
		return allNodes
	}
	return allNodes[:count]
}

// SelectReadNodes selects optimal nodes for reading
func (anm *AdaptiveNetworkManager) SelectReadNodes(candidates []string, count int) []string {
	if anm.adaptiveGossip != nil {
		return anm.adaptiveGossip.SelectReadNodes(candidates, count)
	}

	if len(candidates) <= count {
		return candidates
	}
	return candidates[:count]
}

// SelectWriteNodes selects optimal nodes for writing
func (anm *AdaptiveNetworkManager) SelectWriteNodes(candidates []string, count int) []string {
	if anm.adaptiveGossip != nil {
		return anm.adaptiveGossip.SelectWriteNodes(candidates, count)
	}

	if len(candidates) <= count {
		return candidates
	}
	return candidates[:count]
}

// GetNodeLatency returns latency info for a node
func (anm *AdaptiveNetworkManager) GetNodeLatency(nodeID string) (*NodeLatencyInfo, bool) {
	if anm.latencyMatrix != nil {
		return anm.latencyMatrix.GetNodeLatency(nodeID)
	}
	return nil, false
}

// GetClosestNodes returns N closest nodes
func (anm *AdaptiveNetworkManager) GetClosestNodes(n int) []string {
	if anm.latencyMatrix != nil {
		return anm.latencyMatrix.GetClosestNodes(n)
	}
	return nil
}

// IsLocalNode checks if a node is in the local DC
func (anm *AdaptiveNetworkManager) IsLocalNode(nodeID string) bool {
	if anm.multiDCManager != nil {
		return anm.multiDCManager.IsLocalNode(nodeID)
	}
	return true
}

// ShouldAsyncReplicate determines if replication should be async
func (anm *AdaptiveNetworkManager) ShouldAsyncReplicate(nodeID string) bool {
	if anm.multiDCManager != nil {
		return anm.multiDCManager.ShouldAsyncReplicate(nodeID)
	}
	return false
}

// EnqueueAsyncReplication enqueues an async replication task
func (anm *AdaptiveNetworkManager) EnqueueAsyncReplication(nodeID, key string, value []byte) {
	if anm.asyncReplicationQ == nil {
		return
	}

	delay := time.Duration(0)
	if anm.multiDCManager != nil {
		delay = anm.multiDCManager.GetAsyncReplicationDelay(nodeID)
	}

	task := &AsyncReplicationTask{
		NodeID:    nodeID,
		Key:       key,
		Value:     value,
		Timestamp: time.Now(),
		Delay:     delay,
	}

	anm.asyncReplicationQ.Enqueue(task)
}

// RecordMetric records a metric value
func (anm *AdaptiveNetworkManager) RecordMetric(name string, value int64) {
	if anm.metricsCollector != nil {
		anm.metricsCollector.Set(name, value)
	}
}

// IncrementMetric increments a counter metric
func (anm *AdaptiveNetworkManager) IncrementMetric(name string, delta int64) {
	if anm.metricsCollector != nil {
		anm.metricsCollector.Increment(name, delta)
	}
}

// GetDashboard returns a comprehensive dashboard of all metrics
func (anm *AdaptiveNetworkManager) GetDashboard() map[string]interface{} {
	if anm.integratedMetrics != nil {
		return anm.integratedMetrics.GetDashboard()
	}
	return make(map[string]interface{})
}

// GetStats returns statistics from all components
func (anm *AdaptiveNetworkManager) GetStats() map[string]interface{} {
	stats := make(map[string]interface{})

	if anm.latencyMatrix != nil {
		stats["latency_matrix"] = anm.latencyMatrix.GetStats()
	}

	if anm.multiDCManager != nil {
		stats["multidc"] = anm.multiDCManager.GetStats()
	}

	if anm.adaptiveGossip != nil {
		stats["adaptive_gossip"] = anm.adaptiveGossip.GetStats()
	}

	if anm.metricsCollector != nil {
		stats["metrics"] = anm.metricsCollector.GetAllMetrics()
	}

	if anm.asyncReplicationQ != nil {
		stats["async_replication_queue_size"] = anm.asyncReplicationQ.GetQueueSize()
	}

	stats["enabled"] = anm.enabled
	stats["local_node_id"] = anm.localNodeID
	stats["local_dc_id"] = anm.localDCID

	return stats
}

// GetRecentAlerts returns recent alerts
func (anm *AdaptiveNetworkManager) GetRecentAlerts(count int) []*Alert {
	if anm.alertManager != nil {
		return anm.alertManager.GetRecentAlerts(count)
	}
	return nil
}

// HealthCheck performs a health check of all components
func (anm *AdaptiveNetworkManager) HealthCheck() error {
	anm.mu.RLock()
	defer anm.mu.RUnlock()

	if !anm.enabled {
		return fmt.Errorf("adaptive network manager is disabled")
	}

	// Check if critical components are initialized
	if anm.latencyMatrix == nil {
		return fmt.Errorf("latency matrix not initialized")
	}

	if anm.multiDCManager == nil {
		return fmt.Errorf("multi-DC manager not initialized")
	}

	if anm.adaptiveGossip == nil {
		return fmt.Errorf("adaptive gossip not initialized")
	}

	return nil
}

// NetworkEnvironmentInfo provides information about the detected network environment
type NetworkEnvironmentInfo struct {
	LANNodes         []string
	WANNodes         []string
	CrossRegionNodes []string
	LocalDC          string
	TotalDCs         int
	AverageRTT       time.Duration
	NetworkHealth    float64 // 0-1 score
}

// GetNetworkEnvironment returns information about the network environment
func (anm *AdaptiveNetworkManager) GetNetworkEnvironment() *NetworkEnvironmentInfo {
	info := &NetworkEnvironmentInfo{
		LocalDC: anm.localDCID,
	}

	if anm.latencyMatrix != nil {
		info.LANNodes = anm.latencyMatrix.GetLANNodes()
		info.WANNodes = anm.latencyMatrix.GetWANNodes()
		info.CrossRegionNodes = anm.latencyMatrix.GetCrossRegionNodes()

		stats := anm.latencyMatrix.GetStats()
		if avgRTTStr, ok := stats["average_global_rtt"].(string); ok {
			if duration, err := time.ParseDuration(avgRTTStr); err == nil {
				info.AverageRTT = duration
			}
		}

		// Calculate network health
		totalProbes, _ := stats["total_probes"].(int64)
		totalFailures, _ := stats["total_failures"].(int64)
		if totalProbes > 0 {
			info.NetworkHealth = 1.0 - (float64(totalFailures) / float64(totalProbes))
		}
	}

	if anm.multiDCManager != nil {
		dcs := anm.multiDCManager.GetDataCenters()
		info.TotalDCs = len(dcs)
	}

	return info
}

