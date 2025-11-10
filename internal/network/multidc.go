package network

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// DataCenter represents a single data center in the multi-DC architecture
type DataCenter struct {
	ID       string
	Name     string
	Region   string
	Priority int // Lower number = higher priority for writes

	mu    sync.RWMutex
	nodes map[string]*DCNode // Nodes in this DC
}

// DCNode represents a node within a data center
type DCNode struct {
	NodeID   string
	Address  string
	IsLocal  bool // Whether this node is in the local DC
	LastSeen time.Time
	Healthy  bool
}

// MultiDCManager manages multiple data centers
type MultiDCManager struct {
	mu          sync.RWMutex
	localDCID   string
	datacenters map[string]*DataCenter
	nodeToDC    map[string]string // Maps node ID to DC ID

	// Configuration
	intraDCGossipInterval  time.Duration // Fast gossip within DC
	interDCGossipInterval  time.Duration // Slow gossip between DCs
	asyncReplicationDelay  time.Duration // Delay for cross-DC async replication
	localReadPreference    bool          // Prefer local DC for reads
	localWriteOptimization bool          // Optimize writes to local DC first

	// Network monitoring integration
	latencyMatrix *LatencyMatrix

	// Statistics
	stats *MultiDCStats
}

// MultiDCStats tracks multi-DC statistics
type MultiDCStats struct {
	intraDCOperations   atomic.Int64
	interDCOperations   atomic.Int64
	localReads          atomic.Int64
	remoteReads         atomic.Int64
	localWrites         atomic.Int64
	remoteWrites        atomic.Int64
	crossDCReplications atomic.Int64
	replicationLatency  atomic.Int64 // Average in nanoseconds
}

// MultiDCOptions configures the multi-DC manager
type MultiDCOptions struct {
	LocalDCID              string
	IntraDCGossipInterval  time.Duration
	InterDCGossipInterval  time.Duration
	AsyncReplicationDelay  time.Duration
	LocalReadPreference    bool
	LocalWriteOptimization bool
	LatencyMatrix          *LatencyMatrix
}

// NewMultiDCManager creates a new multi-DC manager
func NewMultiDCManager(opts *MultiDCOptions) *MultiDCManager {
	if opts.IntraDCGossipInterval == 0 {
		opts.IntraDCGossipInterval = 100 * time.Millisecond // Fast within DC
	}
	if opts.InterDCGossipInterval == 0 {
		opts.InterDCGossipInterval = 1 * time.Second // Slower between DCs
	}
	if opts.AsyncReplicationDelay == 0 {
		opts.AsyncReplicationDelay = 500 * time.Millisecond
	}

	return &MultiDCManager{
		localDCID:              opts.LocalDCID,
		datacenters:            make(map[string]*DataCenter),
		nodeToDC:               make(map[string]string),
		intraDCGossipInterval:  opts.IntraDCGossipInterval,
		interDCGossipInterval:  opts.InterDCGossipInterval,
		asyncReplicationDelay:  opts.AsyncReplicationDelay,
		localReadPreference:    opts.LocalReadPreference,
		localWriteOptimization: opts.LocalWriteOptimization,
		latencyMatrix:          opts.LatencyMatrix,
		stats:                  &MultiDCStats{},
	}
}

// RegisterDataCenter adds a new data center
func (mdm *MultiDCManager) RegisterDataCenter(dc *DataCenter) {
	mdm.mu.Lock()
	defer mdm.mu.Unlock()

	if dc.nodes == nil {
		dc.nodes = make(map[string]*DCNode)
	}
	mdm.datacenters[dc.ID] = dc
}

// AddNodeToDC adds a node to a specific data center
func (mdm *MultiDCManager) AddNodeToDC(dcID, nodeID, address string) error {
	mdm.mu.Lock()
	defer mdm.mu.Unlock()

	dc, exists := mdm.datacenters[dcID]
	if !exists {
		// Auto-create DC if it doesn't exist
		dc = &DataCenter{
			ID:    dcID,
			Name:  dcID,
			nodes: make(map[string]*DCNode),
		}
		mdm.datacenters[dcID] = dc
	}

	isLocal := dcID == mdm.localDCID

	dc.nodes[nodeID] = &DCNode{
		NodeID:   nodeID,
		Address:  address,
		IsLocal:  isLocal,
		LastSeen: time.Now(),
		Healthy:  true,
	}

	mdm.nodeToDC[nodeID] = dcID

	// Add to latency matrix if available
	if mdm.latencyMatrix != nil && !isLocal {
		mdm.latencyMatrix.AddNode(nodeID, address)
	}

	return nil
}

// RemoveNode removes a node from its DC
func (mdm *MultiDCManager) RemoveNode(nodeID string) {
	mdm.mu.Lock()
	defer mdm.mu.Unlock()

	dcID, exists := mdm.nodeToDC[nodeID]
	if !exists {
		return
	}

	if dc, ok := mdm.datacenters[dcID]; ok {
		delete(dc.nodes, nodeID)
	}

	delete(mdm.nodeToDC, nodeID)

	if mdm.latencyMatrix != nil {
		mdm.latencyMatrix.RemoveNode(nodeID)
	}
}

// GetNodeDC returns the DC ID for a node
func (mdm *MultiDCManager) GetNodeDC(nodeID string) (string, bool) {
	mdm.mu.RLock()
	defer mdm.mu.RUnlock()

	dcID, exists := mdm.nodeToDC[nodeID]
	return dcID, exists
}

// IsLocalNode returns true if the node is in the local DC
func (mdm *MultiDCManager) IsLocalNode(nodeID string) bool {
	mdm.mu.RLock()
	defer mdm.mu.RUnlock()

	dcID, exists := mdm.nodeToDC[nodeID]
	if !exists {
		return false
	}

	return dcID == mdm.localDCID
}

// GetLocalNodes returns all nodes in the local DC
func (mdm *MultiDCManager) GetLocalNodes() []string {
	mdm.mu.RLock()
	defer mdm.mu.RUnlock()

	dc, exists := mdm.datacenters[mdm.localDCID]
	if !exists {
		return nil
	}

	nodes := make([]string, 0, len(dc.nodes))
	for nodeID := range dc.nodes {
		nodes = append(nodes, nodeID)
	}
	return nodes
}

// GetRemoteNodes returns all nodes in remote DCs
func (mdm *MultiDCManager) GetRemoteNodes() []string {
	mdm.mu.RLock()
	defer mdm.mu.RUnlock()

	var nodes []string
	for dcID, dc := range mdm.datacenters {
		if dcID == mdm.localDCID {
			continue
		}
		for nodeID := range dc.nodes {
			nodes = append(nodes, nodeID)
		}
	}
	return nodes
}

// GetNodesInDC returns all nodes in a specific DC
func (mdm *MultiDCManager) GetNodesInDC(dcID string) []string {
	mdm.mu.RLock()
	defer mdm.mu.RUnlock()

	dc, exists := mdm.datacenters[dcID]
	if !exists {
		return nil
	}

	nodes := make([]string, 0, len(dc.nodes))
	for nodeID := range dc.nodes {
		nodes = append(nodes, nodeID)
	}
	return nodes
}

// GetGossipInterval returns the appropriate gossip interval for target nodes
func (mdm *MultiDCManager) GetGossipInterval(targetNodes []string) time.Duration {
	mdm.mu.RLock()
	defer mdm.mu.RUnlock()

	// If any target node is in a different DC, use inter-DC interval
	for _, nodeID := range targetNodes {
		if dcID, exists := mdm.nodeToDC[nodeID]; exists {
			if dcID != mdm.localDCID {
				mdm.stats.interDCOperations.Add(1)
				return mdm.interDCGossipInterval
			}
		}
	}

	mdm.stats.intraDCOperations.Add(1)
	return mdm.intraDCGossipInterval
}

// SelectReadNodes selects optimal nodes for reading based on locality
func (mdm *MultiDCManager) SelectReadNodes(candidates []string, count int) []string {
	if !mdm.localReadPreference {
		// No preference, return first N candidates
		if len(candidates) <= count {
			return candidates
		}
		return candidates[:count]
	}

	mdm.mu.RLock()
	defer mdm.mu.RUnlock()

	// Separate local and remote nodes
	var localNodes, remoteNodes []string
	for _, nodeID := range candidates {
		if dcID, exists := mdm.nodeToDC[nodeID]; exists {
			if dcID == mdm.localDCID {
				localNodes = append(localNodes, nodeID)
			} else {
				remoteNodes = append(remoteNodes, nodeID)
			}
		} else {
			remoteNodes = append(remoteNodes, nodeID)
		}
	}

	// Prefer local nodes first
	selected := make([]string, 0, count)

	// Add local nodes first
	for i := 0; i < len(localNodes) && len(selected) < count; i++ {
		selected = append(selected, localNodes[i])
		mdm.stats.localReads.Add(1)
	}

	// Fill with remote nodes if needed
	for i := 0; i < len(remoteNodes) && len(selected) < count; i++ {
		selected = append(selected, remoteNodes[i])
		mdm.stats.remoteReads.Add(1)
	}

	return selected
}

// SelectWriteNodes selects optimal nodes for writing based on locality
func (mdm *MultiDCManager) SelectWriteNodes(candidates []string, count int) []string {
	if !mdm.localWriteOptimization {
		// No optimization, return first N candidates
		if len(candidates) <= count {
			return candidates
		}
		return candidates[:count]
	}

	mdm.mu.RLock()
	defer mdm.mu.RUnlock()

	// Separate local and remote nodes
	var localNodes, remoteNodes []string
	for _, nodeID := range candidates {
		if dcID, exists := mdm.nodeToDC[nodeID]; exists {
			if dcID == mdm.localDCID {
				localNodes = append(localNodes, nodeID)
			} else {
				remoteNodes = append(remoteNodes, nodeID)
			}
		} else {
			remoteNodes = append(remoteNodes, nodeID)
		}
	}

	// Prefer local nodes first for lower latency
	selected := make([]string, 0, count)

	for i := 0; i < len(localNodes) && len(selected) < count; i++ {
		selected = append(selected, localNodes[i])
		mdm.stats.localWrites.Add(1)
	}

	for i := 0; i < len(remoteNodes) && len(selected) < count; i++ {
		selected = append(selected, remoteNodes[i])
		mdm.stats.remoteWrites.Add(1)
	}

	return selected
}

// ShouldAsyncReplicate determines if replication should be asynchronous
func (mdm *MultiDCManager) ShouldAsyncReplicate(nodeID string) bool {
	return !mdm.IsLocalNode(nodeID)
}

// GetAsyncReplicationDelay returns the delay for async replication to a node
func (mdm *MultiDCManager) GetAsyncReplicationDelay(nodeID string) time.Duration {
	if mdm.IsLocalNode(nodeID) {
		return 0 // No delay for local nodes
	}

	mdm.stats.crossDCReplications.Add(1)

	// Use latency-aware delay if available
	if mdm.latencyMatrix != nil {
		if latency, exists := mdm.latencyMatrix.GetNodeLatency(nodeID); exists {
			// Use 2x RTT as minimum delay
			delay := latency.AvgRTT * 2
			if delay < mdm.asyncReplicationDelay {
				delay = mdm.asyncReplicationDelay
			}
			return delay
		}
	}

	return mdm.asyncReplicationDelay
}

// GetDataCenters returns all registered data centers
func (mdm *MultiDCManager) GetDataCenters() []*DataCenter {
	mdm.mu.RLock()
	defer mdm.mu.RUnlock()

	dcs := make([]*DataCenter, 0, len(mdm.datacenters))
	for _, dc := range mdm.datacenters {
		dcs = append(dcs, dc)
	}
	return dcs
}

// GetStats returns multi-DC statistics
func (mdm *MultiDCManager) GetStats() map[string]interface{} {
	mdm.mu.RLock()
	totalDCs := len(mdm.datacenters)
	totalNodes := len(mdm.nodeToDC)
	mdm.mu.RUnlock()

	return map[string]interface{}{
		"total_datacenters":       totalDCs,
		"local_dc_id":             mdm.localDCID,
		"total_nodes":             totalNodes,
		"intra_dc_operations":     mdm.stats.intraDCOperations.Load(),
		"inter_dc_operations":     mdm.stats.interDCOperations.Load(),
		"local_reads":             mdm.stats.localReads.Load(),
		"remote_reads":            mdm.stats.remoteReads.Load(),
		"local_writes":            mdm.stats.localWrites.Load(),
		"remote_writes":           mdm.stats.remoteWrites.Load(),
		"cross_dc_replications":   mdm.stats.crossDCReplications.Load(),
		"intra_dc_interval":       mdm.intraDCGossipInterval.String(),
		"inter_dc_interval":       mdm.interDCGossipInterval.String(),
		"async_replication_delay": mdm.asyncReplicationDelay.String(),
	}
}

// AsyncReplicationTask represents a pending async replication
type AsyncReplicationTask struct {
	NodeID    string
	Key       string
	Value     []byte
	Timestamp time.Time
	Delay     time.Duration
}

// AsyncReplicationQueue manages async replication tasks
type AsyncReplicationQueue struct {
	mu     sync.Mutex
	tasks  []*AsyncReplicationTask
	stopCh chan struct{}
	wg     sync.WaitGroup

	mdcManager *MultiDCManager
	replicator ReplicationFunc
}

// ReplicationFunc performs the actual replication
type ReplicationFunc func(ctx context.Context, nodeID, key string, value []byte) error

// NewAsyncReplicationQueue creates a new async replication queue
func NewAsyncReplicationQueue(mdcManager *MultiDCManager, replicator ReplicationFunc) *AsyncReplicationQueue {
	return &AsyncReplicationQueue{
		tasks:      make([]*AsyncReplicationTask, 0),
		stopCh:     make(chan struct{}),
		mdcManager: mdcManager,
		replicator: replicator,
	}
}

// Start begins processing the replication queue
func (arq *AsyncReplicationQueue) Start() {
	arq.wg.Add(1)
	go arq.processLoop()
}

// Stop halts queue processing
func (arq *AsyncReplicationQueue) Stop() {
	close(arq.stopCh)
	arq.wg.Wait()
}

// Enqueue adds a replication task
func (arq *AsyncReplicationQueue) Enqueue(task *AsyncReplicationTask) {
	arq.mu.Lock()
	defer arq.mu.Unlock()

	arq.tasks = append(arq.tasks, task)
}

// processLoop processes queued replication tasks
func (arq *AsyncReplicationQueue) processLoop() {
	defer arq.wg.Done()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-arq.stopCh:
			return
		case <-ticker.C:
			arq.processTasks()
		}
	}
}

// processTasks executes ready replication tasks
func (arq *AsyncReplicationQueue) processTasks() {
	arq.mu.Lock()

	now := time.Now()
	ready := make([]*AsyncReplicationTask, 0)
	pending := make([]*AsyncReplicationTask, 0)

	for _, task := range arq.tasks {
		if now.Sub(task.Timestamp) >= task.Delay {
			ready = append(ready, task)
		} else {
			pending = append(pending, task)
		}
	}

	arq.tasks = pending
	arq.mu.Unlock()

	// Execute ready tasks in parallel
	var wg sync.WaitGroup
	for _, task := range ready {
		wg.Add(1)
		go func(t *AsyncReplicationTask) {
			defer wg.Done()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			startTime := time.Now()
			if err := arq.replicator(ctx, t.NodeID, t.Key, t.Value); err == nil {
				// Track replication latency
				latency := time.Since(startTime)
				if arq.mdcManager != nil {
					arq.mdcManager.stats.replicationLatency.Store(int64(latency))
				}
			}
		}(task)
	}

	wg.Wait()
}

// GetQueueSize returns the number of pending tasks
func (arq *AsyncReplicationQueue) GetQueueSize() int {
	arq.mu.Lock()
	defer arq.mu.Unlock()
	return len(arq.tasks)
}
