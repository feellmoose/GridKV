package network

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// NetworkType represents the type of network connection between nodes
type NetworkType int

const (
	NetworkTypeUnknown     NetworkType = iota
	NetworkTypeLAN                     // Local Area Network (< 2ms RTT)
	NetworkTypeWAN                     // Wide Area Network (> 2ms RTT)
	NetworkTypeCrossRegion             // Cross-region (> 50ms RTT)
)

func (nt NetworkType) String() string {
	switch nt {
	case NetworkTypeLAN:
		return "LAN"
	case NetworkTypeWAN:
		return "WAN"
	case NetworkTypeCrossRegion:
		return "CrossRegion"
	default:
		return "Unknown"
	}
}

// RTTSample represents a single RTT measurement
type RTTSample struct {
	Timestamp time.Time
	RTT       time.Duration
	Success   bool
}

// NodeLatencyInfo stores latency information for a single node
type NodeLatencyInfo struct {
	NodeID      string
	Address     string
	LastProbe   time.Time
	NetworkType NetworkType

	// RTT statistics (exported for testing)
	mu            sync.RWMutex
	samples       []RTTSample
	AvgRTT        time.Duration
	MinRTT        time.Duration
	MaxRTT        time.Duration
	Jitter        time.Duration
	PacketLoss    float64
	TotalProbes   int64
	FailedProbes  int64
	lastSuccessAt time.Time

	// Adaptive parameters
	TimeoutMultiplier float64 // Multiplier for timeout calculations
}

// LatencyMatrix maintains RTT and latency information for all nodes
type LatencyMatrix struct {
	mu      sync.RWMutex
	nodes   map[string]*NodeLatencyInfo
	localID string

	// Monitoring configuration
	probeInterval   time.Duration
	sampleWindow    int
	stopCh          chan struct{}
	wg              sync.WaitGroup
	probeFunc       ProbeFunc
	anomalyCallback AnomalyCallback

	// Network grouping
	lanNodes         map[string]bool
	wanNodes         map[string]bool
	crossRegionNodes map[string]bool

	// Statistics
	stats *LatencyMatrixStats
}

// LatencyMatrixStats tracks overall statistics
type LatencyMatrixStats struct {
	totalProbes      atomic.Int64
	totalFailures    atomic.Int64
	totalAnomalies   atomic.Int64
	lastUpdateTime   atomic.Int64
	averageGlobalRTT atomic.Int64 // In nanoseconds
}

// ProbeFunc is called to measure RTT to a specific node
type ProbeFunc func(ctx context.Context, address string) (time.Duration, error)

// AnomalyCallback is called when a network anomaly is detected
type AnomalyCallback func(anomaly *NetworkAnomaly)

// NetworkAnomaly represents a detected network issue
type NetworkAnomaly struct {
	Timestamp   time.Time
	NodeID      string
	Address     string
	Type        AnomalyType
	Description string
	Severity    AnomalySeverity
	Metrics     map[string]interface{}
}

type AnomalyType int

const (
	AnomalyTypeHighLatency AnomalyType = iota
	AnomalyTypeHighJitter
	AnomalyTypePacketLoss
	AnomalyTypeTimeout
	AnomalyTypeNetworkPartition
	AnomalyTypeFlapping
)

func (at AnomalyType) String() string {
	switch at {
	case AnomalyTypeHighLatency:
		return "HighLatency"
	case AnomalyTypeHighJitter:
		return "HighJitter"
	case AnomalyTypePacketLoss:
		return "PacketLoss"
	case AnomalyTypeTimeout:
		return "Timeout"
	case AnomalyTypeNetworkPartition:
		return "NetworkPartition"
	case AnomalyTypeFlapping:
		return "Flapping"
	default:
		return "Unknown"
	}
}

type AnomalySeverity int

const (
	SeverityInfo AnomalySeverity = iota
	SeverityWarning
	SeverityError
	SeverityCritical
)

func (as AnomalySeverity) String() string {
	switch as {
	case SeverityInfo:
		return "Info"
	case SeverityWarning:
		return "Warning"
	case SeverityError:
		return "Error"
	case SeverityCritical:
		return "Critical"
	default:
		return "Unknown"
	}
}

// LatencyMatrixOptions configures the latency matrix
type LatencyMatrixOptions struct {
	LocalNodeID     string
	ProbeInterval   time.Duration
	SampleWindow    int
	ProbeFunc       ProbeFunc
	AnomalyCallback AnomalyCallback
}

// NewLatencyMatrix creates a new latency matrix
func NewLatencyMatrix(opts *LatencyMatrixOptions) *LatencyMatrix {
	if opts.ProbeInterval == 0 {
		opts.ProbeInterval = 5 * time.Second
	}
	if opts.SampleWindow == 0 {
		opts.SampleWindow = 12 // Keep last 12 samples (1 minute at 5s interval)
	}

	lm := &LatencyMatrix{
		nodes:            make(map[string]*NodeLatencyInfo),
		localID:          opts.LocalNodeID,
		probeInterval:    opts.ProbeInterval,
		sampleWindow:     opts.SampleWindow,
		stopCh:           make(chan struct{}),
		probeFunc:        opts.ProbeFunc,
		anomalyCallback:  opts.AnomalyCallback,
		lanNodes:         make(map[string]bool),
		wanNodes:         make(map[string]bool),
		crossRegionNodes: make(map[string]bool),
		stats:            &LatencyMatrixStats{},
	}

	return lm
}

// Start begins the periodic probing
func (lm *LatencyMatrix) Start() {
	lm.wg.Add(1)
	go lm.probeLoop()
}

// Stop halts the probing
func (lm *LatencyMatrix) Stop() {
	close(lm.stopCh)
	lm.wg.Wait()
}

// AddNode adds a node to be monitored
func (lm *LatencyMatrix) AddNode(nodeID, address string) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	if nodeID == lm.localID {
		return // Don't monitor ourselves
	}

	if _, exists := lm.nodes[nodeID]; !exists {
		lm.nodes[nodeID] = &NodeLatencyInfo{
			NodeID:            nodeID,
			Address:           address,
			NetworkType:       NetworkTypeUnknown,
			samples:           make([]RTTSample, 0, lm.sampleWindow),
			TimeoutMultiplier: 3.0, // Start with 3x RTT timeout
			MinRTT:            time.Duration(math.MaxInt64),
		}
	}
}

// RemoveNode removes a node from monitoring
func (lm *LatencyMatrix) RemoveNode(nodeID string) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	delete(lm.nodes, nodeID)
	delete(lm.lanNodes, nodeID)
	delete(lm.wanNodes, nodeID)
	delete(lm.crossRegionNodes, nodeID)
}

// probeLoop periodically probes all nodes
func (lm *LatencyMatrix) probeLoop() {
	defer lm.wg.Done()

	ticker := time.NewTicker(lm.probeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-lm.stopCh:
			return
		case <-ticker.C:
			lm.probeAllNodes()
		}
	}
}

// probeAllNodes sends probes to all registered nodes
func (lm *LatencyMatrix) probeAllNodes() {
	lm.mu.RLock()
	nodes := make([]*NodeLatencyInfo, 0, len(lm.nodes))
	for _, node := range lm.nodes {
		nodes = append(nodes, node)
	}
	lm.mu.RUnlock()

	// Probe nodes in parallel
	var wg sync.WaitGroup
	for _, node := range nodes {
		wg.Add(1)
		go func(n *NodeLatencyInfo) {
			defer wg.Done()
			lm.probeNode(n)
		}(node)
	}
	wg.Wait()

	// Update network grouping after all probes
	lm.updateNetworkGrouping()
	lm.updateGlobalStats()
}

// probeNode measures RTT to a specific node
func (lm *LatencyMatrix) probeNode(node *NodeLatencyInfo) {
	if lm.probeFunc == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	rtt, err := lm.probeFunc(ctx, node.Address)

	lm.stats.totalProbes.Add(1)

	sample := RTTSample{
		Timestamp: start,
		Success:   err == nil,
	}

	node.mu.Lock()
	defer node.mu.Unlock()

	node.LastProbe = start
	node.TotalProbes++

	if err != nil {
		node.FailedProbes++
		lm.stats.totalFailures.Add(1)
		sample.RTT = 0

		// Add failed sample
		node.samples = append(node.samples, sample)
		if len(node.samples) > lm.sampleWindow {
			node.samples = node.samples[1:]
		}

		// Check for anomalies
		lm.checkAnomalies(node)
		return
	}

	sample.RTT = rtt
	node.lastSuccessAt = start

	// Add sample
	node.samples = append(node.samples, sample)
	if len(node.samples) > lm.sampleWindow {
		node.samples = node.samples[1:]
	}

	// Update statistics
	lm.updateNodeStats(node)

	// Check for anomalies
	lm.checkAnomalies(node)
}

// updateNodeStats recalculates node statistics from samples
func (lm *LatencyMatrix) updateNodeStats(node *NodeLatencyInfo) {
	if len(node.samples) == 0 {
		return
	}

	var sum, min, max time.Duration
	min = time.Duration(math.MaxInt64)
	var successCount, failureCount int64

	for _, s := range node.samples {
		if s.Success {
			successCount++
			sum += s.RTT
			if s.RTT < min {
				min = s.RTT
			}
			if s.RTT > max {
				max = s.RTT
			}
		} else {
			failureCount++
		}
	}

	if successCount > 0 {
		node.AvgRTT = sum / time.Duration(successCount)
		node.MinRTT = min
		node.MaxRTT = max

		// Calculate jitter (mean absolute deviation)
		var jitterSum time.Duration
		for _, s := range node.samples {
			if s.Success {
				diff := s.RTT - node.AvgRTT
				if diff < 0 {
					diff = -diff
				}
				jitterSum += diff
			}
		}
		node.Jitter = jitterSum / time.Duration(successCount)

		// Update network type based on RTT
		if node.AvgRTT < 2*time.Millisecond {
			node.NetworkType = NetworkTypeLAN
		} else if node.AvgRTT < 50*time.Millisecond {
			node.NetworkType = NetworkTypeWAN
		} else {
			node.NetworkType = NetworkTypeCrossRegion
		}

		// Adaptive timeout multiplier based on jitter
		jitterRatio := float64(node.Jitter) / float64(node.AvgRTT)
		if jitterRatio > 0.5 {
			node.TimeoutMultiplier = 5.0 // High jitter
		} else if jitterRatio > 0.2 {
			node.TimeoutMultiplier = 4.0 // Medium jitter
		} else {
			node.TimeoutMultiplier = 3.0 // Low jitter
		}
	}

	// Calculate packet loss
	total := successCount + failureCount
	if total > 0 {
		node.PacketLoss = float64(failureCount) / float64(total)
	}
}

// updateNetworkGrouping categorizes nodes by network type
func (lm *LatencyMatrix) updateNetworkGrouping() {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	// Clear existing grouping
	lm.lanNodes = make(map[string]bool)
	lm.wanNodes = make(map[string]bool)
	lm.crossRegionNodes = make(map[string]bool)

	for nodeID, node := range lm.nodes {
		node.mu.RLock()
		switch node.NetworkType {
		case NetworkTypeLAN:
			lm.lanNodes[nodeID] = true
		case NetworkTypeWAN:
			lm.wanNodes[nodeID] = true
		case NetworkTypeCrossRegion:
			lm.crossRegionNodes[nodeID] = true
		}
		node.mu.RUnlock()
	}
}

// updateGlobalStats updates global statistics
func (lm *LatencyMatrix) updateGlobalStats() {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	var totalRTT time.Duration
	var count int

	for _, node := range lm.nodes {
		node.mu.RLock()
		if node.AvgRTT > 0 {
			totalRTT += node.AvgRTT
			count++
		}
		node.mu.RUnlock()
	}

	if count > 0 {
		avgRTT := totalRTT / time.Duration(count)
		lm.stats.averageGlobalRTT.Store(int64(avgRTT))
	}

	lm.stats.lastUpdateTime.Store(time.Now().Unix())
}

// checkAnomalies detects and reports network anomalies
func (lm *LatencyMatrix) checkAnomalies(node *NodeLatencyInfo) {
	if lm.anomalyCallback == nil {
		return
	}

	// Check high latency (> 100ms for LAN/WAN, > 500ms for cross-region)
	if node.AvgRTT > 0 {
		threshold := 100 * time.Millisecond
		if node.NetworkType == NetworkTypeCrossRegion {
			threshold = 500 * time.Millisecond
		}

		if node.AvgRTT > threshold {
			anomaly := &NetworkAnomaly{
				Timestamp:   time.Now(),
				NodeID:      node.NodeID,
				Address:     node.Address,
				Type:        AnomalyTypeHighLatency,
				Description: fmt.Sprintf("High latency detected: %v (threshold: %v)", node.AvgRTT, threshold),
				Severity:    SeverityWarning,
				Metrics: map[string]interface{}{
					"avg_rtt":   node.AvgRTT.String(),
					"threshold": threshold.String(),
				},
			}
			lm.stats.totalAnomalies.Add(1)
			lm.anomalyCallback(anomaly)
		}
	}

	// Check high jitter (> 50% of average RTT)
	if node.AvgRTT > 0 && node.Jitter > node.AvgRTT/2 {
		anomaly := &NetworkAnomaly{
			Timestamp:   time.Now(),
			NodeID:      node.NodeID,
			Address:     node.Address,
			Type:        AnomalyTypeHighJitter,
			Description: fmt.Sprintf("High jitter detected: %v (avg RTT: %v)", node.Jitter, node.AvgRTT),
			Severity:    SeverityWarning,
			Metrics: map[string]interface{}{
				"jitter":  node.Jitter.String(),
				"avg_rtt": node.AvgRTT.String(),
			},
		}
		lm.stats.totalAnomalies.Add(1)
		lm.anomalyCallback(anomaly)
	}

	// Check packet loss (> 10%)
	if node.PacketLoss > 0.1 {
		severity := SeverityWarning
		if node.PacketLoss > 0.5 {
			severity = SeverityCritical
		}

		anomaly := &NetworkAnomaly{
			Timestamp:   time.Now(),
			NodeID:      node.NodeID,
			Address:     node.Address,
			Type:        AnomalyTypePacketLoss,
			Description: fmt.Sprintf("Packet loss detected: %.2f%%", node.PacketLoss*100),
			Severity:    severity,
			Metrics: map[string]interface{}{
				"packet_loss": node.PacketLoss,
			},
		}
		lm.stats.totalAnomalies.Add(1)
		lm.anomalyCallback(anomaly)
	}

	// Check for flapping (rapid state changes)
	if len(node.samples) >= 6 {
		recentSamples := node.samples[len(node.samples)-6:]
		var transitions int
		for i := 1; i < len(recentSamples); i++ {
			if recentSamples[i].Success != recentSamples[i-1].Success {
				transitions++
			}
		}

		if transitions >= 3 {
			anomaly := &NetworkAnomaly{
				Timestamp:   time.Now(),
				NodeID:      node.NodeID,
				Address:     node.Address,
				Type:        AnomalyTypeFlapping,
				Description: fmt.Sprintf("Network flapping detected: %d state changes in last 6 samples", transitions),
				Severity:    SeverityError,
				Metrics: map[string]interface{}{
					"transitions": transitions,
				},
			}
			lm.stats.totalAnomalies.Add(1)
			lm.anomalyCallback(anomaly)
		}
	}
}

// GetNodeLatency returns latency info for a specific node
func (lm *LatencyMatrix) GetNodeLatency(nodeID string) (*NodeLatencyInfo, bool) {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	node, exists := lm.nodes[nodeID]
	if !exists {
		return nil, false
	}

	// Return a copy to avoid race conditions
	node.mu.RLock()
	defer node.mu.RUnlock()

	nodeCopy := &NodeLatencyInfo{
		NodeID:            node.NodeID,
		Address:           node.Address,
		LastProbe:         node.LastProbe,
		NetworkType:       node.NetworkType,
		AvgRTT:            node.AvgRTT,
		MinRTT:            node.MinRTT,
		MaxRTT:            node.MaxRTT,
		Jitter:            node.Jitter,
		PacketLoss:        node.PacketLoss,
		TotalProbes:       node.TotalProbes,
		FailedProbes:      node.FailedProbes,
		lastSuccessAt:     node.lastSuccessAt,
		TimeoutMultiplier: node.TimeoutMultiplier,
	}

	return nodeCopy, true
}

// GetAdaptiveTimeout calculates an adaptive timeout for a node
func (lm *LatencyMatrix) GetAdaptiveTimeout(nodeID string, baseTimeout time.Duration) time.Duration {
	node, exists := lm.GetNodeLatency(nodeID)
	if !exists {
		return baseTimeout
	}

	// Calculate timeout as: avgRTT * multiplier, but at least baseTimeout
	adaptiveTimeout := time.Duration(float64(node.AvgRTT) * node.TimeoutMultiplier)
	if adaptiveTimeout < baseTimeout {
		adaptiveTimeout = baseTimeout
	}

	// Cap at 10x base timeout to avoid excessive delays
	maxTimeout := baseTimeout * 10
	if adaptiveTimeout > maxTimeout {
		adaptiveTimeout = maxTimeout
	}

	return adaptiveTimeout
}

// GetLANNodes returns all nodes in the LAN
func (lm *LatencyMatrix) GetLANNodes() []string {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	nodes := make([]string, 0, len(lm.lanNodes))
	for nodeID := range lm.lanNodes {
		nodes = append(nodes, nodeID)
	}
	return nodes
}

// GetWANNodes returns all nodes in the WAN
func (lm *LatencyMatrix) GetWANNodes() []string {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	nodes := make([]string, 0, len(lm.wanNodes))
	for nodeID := range lm.wanNodes {
		nodes = append(nodes, nodeID)
	}
	return nodes
}

// GetCrossRegionNodes returns all cross-region nodes
func (lm *LatencyMatrix) GetCrossRegionNodes() []string {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	nodes := make([]string, 0, len(lm.crossRegionNodes))
	for nodeID := range lm.crossRegionNodes {
		nodes = append(nodes, nodeID)
	}
	return nodes
}

// GetClosestNodes returns N closest nodes to this node
func (lm *LatencyMatrix) GetClosestNodes(n int) []string {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	type nodeRTT struct {
		nodeID string
		rtt    time.Duration
	}

	nodes := make([]nodeRTT, 0, len(lm.nodes))
	for nodeID, node := range lm.nodes {
		node.mu.RLock()
		if node.AvgRTT > 0 {
			nodes = append(nodes, nodeRTT{nodeID, node.AvgRTT})
		}
		node.mu.RUnlock()
	}

	// Simple selection sort for top N (good enough for small N)
	if n > len(nodes) {
		n = len(nodes)
	}

	for i := 0; i < n; i++ {
		minIdx := i
		for j := i + 1; j < len(nodes); j++ {
			if nodes[j].rtt < nodes[minIdx].rtt {
				minIdx = j
			}
		}
		nodes[i], nodes[minIdx] = nodes[minIdx], nodes[i]
	}

	result := make([]string, n)
	for i := 0; i < n; i++ {
		result[i] = nodes[i].nodeID
	}

	return result
}

// GetStats returns global statistics
func (lm *LatencyMatrix) GetStats() map[string]interface{} {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	return map[string]interface{}{
		"total_nodes":        len(lm.nodes),
		"lan_nodes":          len(lm.lanNodes),
		"wan_nodes":          len(lm.wanNodes),
		"cross_region_nodes": len(lm.crossRegionNodes),
		"total_probes":       lm.stats.totalProbes.Load(),
		"total_failures":     lm.stats.totalFailures.Load(),
		"total_anomalies":    lm.stats.totalAnomalies.Load(),
		"average_global_rtt": time.Duration(lm.stats.averageGlobalRTT.Load()).String(),
		"last_update":        time.Unix(lm.stats.lastUpdateTime.Load(), 0).Format(time.RFC3339),
	}
}
