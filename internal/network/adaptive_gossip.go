package network

import (
	"context"
	"sync"
	"time"
)

// AdaptiveGossipManager manages adaptive gossip behavior based on network conditions
type AdaptiveGossipManager struct {
	mu sync.RWMutex

	// Core components
	latencyMatrix  *LatencyMatrix
	multiDCManager *MultiDCManager
	metrics        *IntegratedNetworkMetrics

	// Adaptive parameters
	baseGossipInterval    time.Duration
	currentGossipInterval time.Duration
	baseFanout            int
	currentFanout         int
	baseTimeout           time.Duration

	// Adaptive strategies
	adaptiveTimeout       bool
	adaptiveFanout        bool
	adaptiveInterval      bool
	localityAwareRouting  bool
	priorityBasedGossip   bool

	// State tracking
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// AdaptiveGossipOptions configures the adaptive gossip manager
type AdaptiveGossipOptions struct {
	BaseGossipInterval   time.Duration
	BaseFanout           int
	BaseTimeout          time.Duration
	AdaptiveTimeout      bool
	AdaptiveFanout       bool
	AdaptiveInterval     bool
	LocalityAwareRouting bool
	PriorityBasedGossip  bool

	LatencyMatrix  *LatencyMatrix
	MultiDCManager *MultiDCManager
	Metrics        *IntegratedNetworkMetrics
}

// NewAdaptiveGossipManager creates a new adaptive gossip manager
func NewAdaptiveGossipManager(opts *AdaptiveGossipOptions) *AdaptiveGossipManager {
	if opts.BaseGossipInterval == 0 {
		opts.BaseGossipInterval = 200 * time.Millisecond
	}
	if opts.BaseFanout == 0 {
		opts.BaseFanout = 3
	}
	if opts.BaseTimeout == 0 {
		opts.BaseTimeout = 1 * time.Second
	}

	agm := &AdaptiveGossipManager{
		latencyMatrix:         opts.LatencyMatrix,
		multiDCManager:        opts.MultiDCManager,
		metrics:               opts.Metrics,
		baseGossipInterval:    opts.BaseGossipInterval,
		currentGossipInterval: opts.BaseGossipInterval,
		baseFanout:            opts.BaseFanout,
		currentFanout:         opts.BaseFanout,
		baseTimeout:           opts.BaseTimeout,
		adaptiveTimeout:       opts.AdaptiveTimeout,
		adaptiveFanout:        opts.AdaptiveFanout,
		adaptiveInterval:      opts.AdaptiveInterval,
		localityAwareRouting:  opts.LocalityAwareRouting,
		priorityBasedGossip:   opts.PriorityBasedGossip,
		stopCh:                make(chan struct{}),
	}

	return agm
}

// Start begins adaptive adjustment
func (agm *AdaptiveGossipManager) Start() {
	agm.wg.Add(1)
	go agm.adaptiveLoop()
}

// Stop halts adaptive adjustment
func (agm *AdaptiveGossipManager) Stop() {
	close(agm.stopCh)
	agm.wg.Wait()
}

// adaptiveLoop periodically adjusts gossip parameters
func (agm *AdaptiveGossipManager) adaptiveLoop() {
	defer agm.wg.Done()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-agm.stopCh:
			return
		case <-ticker.C:
			agm.adjust()
		}
	}
}

// adjust performs adaptive adjustments based on network conditions
func (agm *AdaptiveGossipManager) adjust() {
	if agm.latencyMatrix == nil {
		return
	}

	stats := agm.latencyMatrix.GetStats()

	// Adjust gossip interval based on network quality
	if agm.adaptiveInterval {
		agm.adjustGossipInterval(stats)
	}

	// Adjust fanout based on network capacity
	if agm.adaptiveFanout {
		agm.adjustFanout(stats)
	}

	// Update metrics
	if agm.metrics != nil {
		agm.metrics.Update()
	}
}

// adjustGossipInterval adjusts gossip interval based on network conditions
func (agm *AdaptiveGossipManager) adjustGossipInterval(stats map[string]interface{}) {
	agm.mu.Lock()
	defer agm.mu.Unlock()

	// Parse average RTT
	avgRTTStr, ok := stats["average_global_rtt"].(string)
	if !ok {
		return
	}

	avgRTT, err := time.ParseDuration(avgRTTStr)
	if err != nil {
		return
	}

	// Adjust interval based on RTT
	// Low latency (< 5ms): Use faster interval
	// Medium latency (5-50ms): Use base interval
	// High latency (> 50ms): Use slower interval
	
	if avgRTT < 5*time.Millisecond {
		// LAN environment - gossip faster
		agm.currentGossipInterval = agm.baseGossipInterval / 2
	} else if avgRTT > 50*time.Millisecond {
		// WAN/Cross-region - gossip slower to reduce bandwidth
		agm.currentGossipInterval = agm.baseGossipInterval * 2
	} else {
		// Normal - use base interval
		agm.currentGossipInterval = agm.baseGossipInterval
	}

	// Consider packet loss
	totalProbes, _ := stats["total_probes"].(int64)
	totalFailures, _ := stats["total_failures"].(int64)
	
	if totalProbes > 0 {
		lossRate := float64(totalFailures) / float64(totalProbes)
		if lossRate > 0.1 {
			// High packet loss - slow down gossip to reduce congestion
			agm.currentGossipInterval = time.Duration(float64(agm.currentGossipInterval) * 1.5)
		}
	}

	// Cap the interval
	minInterval := agm.baseGossipInterval / 4
	maxInterval := agm.baseGossipInterval * 4

	if agm.currentGossipInterval < minInterval {
		agm.currentGossipInterval = minInterval
	}
	if agm.currentGossipInterval > maxInterval {
		agm.currentGossipInterval = maxInterval
	}
}

// adjustFanout adjusts fanout based on network capacity
func (agm *AdaptiveGossipManager) adjustFanout(stats map[string]interface{}) {
	agm.mu.Lock()
	defer agm.mu.Unlock()

	totalNodes, _ := stats["total_nodes"].(int)
	totalFailures, _ := stats["total_failures"].(int64)
	totalProbes, _ := stats["total_probes"].(int64)

	if totalNodes == 0 {
		return
	}

	// Calculate network health score (0-1)
	healthScore := 1.0
	if totalProbes > 0 {
		healthScore = 1.0 - (float64(totalFailures) / float64(totalProbes))
	}

	// Adjust fanout based on health and cluster size
	// Good health & small cluster: Lower fanout
	// Good health & large cluster: Base fanout
	// Poor health: Increase fanout for redundancy

	if healthScore > 0.95 {
		// Excellent health - use base or lower fanout
		if totalNodes < 20 {
			agm.currentFanout = agm.baseFanout - 1
			if agm.currentFanout < 2 {
				agm.currentFanout = 2
			}
		} else {
			agm.currentFanout = agm.baseFanout
		}
	} else if healthScore < 0.8 {
		// Poor health - increase fanout for redundancy
		agm.currentFanout = agm.baseFanout + 2
	} else {
		// Normal health - use base fanout
		agm.currentFanout = agm.baseFanout
	}

	// Cap fanout
	maxFanout := 10
	if totalNodes < maxFanout {
		maxFanout = totalNodes
	}

	if agm.currentFanout > maxFanout {
		agm.currentFanout = maxFanout
	}
	if agm.currentFanout < 1 {
		agm.currentFanout = 1
	}
}

// GetGossipInterval returns the current adaptive gossip interval
func (agm *AdaptiveGossipManager) GetGossipInterval() time.Duration {
	agm.mu.RLock()
	defer agm.mu.RUnlock()
	return agm.currentGossipInterval
}

// GetFanout returns the current adaptive fanout
func (agm *AdaptiveGossipManager) GetFanout() int {
	agm.mu.RLock()
	defer agm.mu.RUnlock()
	return agm.currentFanout
}

// GetTimeoutForNode returns adaptive timeout for a specific node
func (agm *AdaptiveGossipManager) GetTimeoutForNode(nodeID string) time.Duration {
	if !agm.adaptiveTimeout || agm.latencyMatrix == nil {
		return agm.baseTimeout
	}

	return agm.latencyMatrix.GetAdaptiveTimeout(nodeID, agm.baseTimeout)
}

// SelectGossipTargets selects optimal gossip targets using locality and priority
func (agm *AdaptiveGossipManager) SelectGossipTargets(allNodes []string, count int) []string {
	if len(allNodes) <= count {
		return allNodes
	}

	// If not using locality-aware routing, return random selection
	if !agm.localityAwareRouting || agm.multiDCManager == nil {
		return agm.randomSelect(allNodes, count)
	}

	// Locality-aware selection
	return agm.localityAwareSelect(allNodes, count)
}

// randomSelect performs random selection
func (agm *AdaptiveGossipManager) randomSelect(nodes []string, count int) []string {
	if len(nodes) <= count {
		return nodes
	}

	// Simple random selection (shuffle and take first N)
	selected := make([]string, len(nodes))
	copy(selected, nodes)

	// Fisher-Yates shuffle
	for i := len(selected) - 1; i > 0; i-- {
		j := int(time.Now().UnixNano()) % (i + 1)
		selected[i], selected[j] = selected[j], selected[i]
	}

	return selected[:count]
}

// localityAwareSelect performs locality-aware selection
func (agm *AdaptiveGossipManager) localityAwareSelect(allNodes []string, count int) []string {
	// Separate nodes by locality
	var localNodes, remoteNodes []string

	for _, nodeID := range allNodes {
		if agm.multiDCManager.IsLocalNode(nodeID) {
			localNodes = append(localNodes, nodeID)
		} else {
			remoteNodes = append(remoteNodes, nodeID)
		}
	}

	selected := make([]string, 0, count)

	// Prefer local nodes (2/3 of selection)
	localCount := (count * 2) / 3
	if localCount > len(localNodes) {
		localCount = len(localNodes)
	}

	// Add local nodes
	for i := 0; i < localCount && i < len(localNodes); i++ {
		selected = append(selected, localNodes[i])
	}

	// Fill remaining with remote nodes
	remaining := count - len(selected)
	for i := 0; i < remaining && i < len(remoteNodes); i++ {
		selected = append(selected, remoteNodes[i])
	}

	return selected
}

// SelectReadNodes selects optimal nodes for reading
func (agm *AdaptiveGossipManager) SelectReadNodes(candidates []string, count int) []string {
	if !agm.localityAwareRouting || agm.multiDCManager == nil {
		return agm.randomSelect(candidates, count)
	}

	return agm.multiDCManager.SelectReadNodes(candidates, count)
}

// SelectWriteNodes selects optimal nodes for writing
func (agm *AdaptiveGossipManager) SelectWriteNodes(candidates []string, count int) []string {
	if !agm.localityAwareRouting || agm.multiDCManager == nil {
		return agm.randomSelect(candidates, count)
	}

	return agm.multiDCManager.SelectWriteNodes(candidates, count)
}

// GetGossipIntervalForNodes returns appropriate gossip interval for target nodes
func (agm *AdaptiveGossipManager) GetGossipIntervalForNodes(targetNodes []string) time.Duration {
	if agm.multiDCManager == nil {
		agm.mu.RLock()
		interval := agm.currentGossipInterval
		agm.mu.RUnlock()
		return interval
	}

	// Use multi-DC aware interval
	return agm.multiDCManager.GetGossipInterval(targetNodes)
}

// GetStats returns adaptive gossip statistics
func (agm *AdaptiveGossipManager) GetStats() map[string]interface{} {
	agm.mu.RLock()
	defer agm.mu.RUnlock()

	stats := map[string]interface{}{
		"base_gossip_interval":    agm.baseGossipInterval.String(),
		"current_gossip_interval": agm.currentGossipInterval.String(),
		"base_fanout":             agm.baseFanout,
		"current_fanout":          agm.currentFanout,
		"base_timeout":            agm.baseTimeout.String(),
		"adaptive_timeout":        agm.adaptiveTimeout,
		"adaptive_fanout":         agm.adaptiveFanout,
		"adaptive_interval":       agm.adaptiveInterval,
		"locality_aware_routing":  agm.localityAwareRouting,
		"priority_based_gossip":   agm.priorityBasedGossip,
	}

	return stats
}

// RTTProber implements network RTT probing
type RTTProber struct {
	dialFunc func(ctx context.Context, address string) error
}

// NewRTTProber creates a new RTT prober
func NewRTTProber(dialFunc func(ctx context.Context, address string) error) *RTTProber {
	return &RTTProber{
		dialFunc: dialFunc,
	}
}

// Probe measures RTT to an address
func (rp *RTTProber) Probe(ctx context.Context, address string) (time.Duration, error) {
	start := time.Now()
	
	err := rp.dialFunc(ctx, address)
	if err != nil {
		return 0, err
	}

	return time.Since(start), nil
}

// CreateProbeFunc creates a probe function for LatencyMatrix
func CreateProbeFunc(dialFunc func(ctx context.Context, address string) error) ProbeFunc {
	return func(ctx context.Context, address string) (time.Duration, error) {
		start := time.Now()
		err := dialFunc(ctx, address)
		if err != nil {
			return 0, err
		}
		return time.Since(start), nil
	}
}

