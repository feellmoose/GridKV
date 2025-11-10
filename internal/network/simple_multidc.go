package network

import (
	"sync"
	"time"
)

// SimpleMultiDC provides basic multi-DC awareness without complex async replication
// Simplified version that focuses on locality-aware routing
type SimpleMultiDC struct {
	mu        sync.RWMutex
	localDCID string
	nodeToDC  map[string]string // node ID -> DC ID
	metrics   *SimpleMetrics
}

// NewSimpleMultiDC creates a simplified multi-DC manager
func NewSimpleMultiDC(localDCID string, metrics *SimpleMetrics) *SimpleMultiDC {
	return &SimpleMultiDC{
		localDCID: localDCID,
		nodeToDC:  make(map[string]string, 100), // Pre-allocate for performance
		metrics:   metrics,
	}
}

// RegisterNode registers a node in a specific DC
//
//go:inline
func (sm *SimpleMultiDC) RegisterNode(nodeID, dcID string) {
	sm.mu.Lock()
	sm.nodeToDC[nodeID] = dcID
	sm.mu.Unlock()
}

// UnregisterNode removes a node
//
//go:inline
func (sm *SimpleMultiDC) UnregisterNode(nodeID string) {
	sm.mu.Lock()
	delete(sm.nodeToDC, nodeID)
	sm.mu.Unlock()
}

// IsLocalNode checks if a node is in the local DC
//
//go:inline
func (sm *SimpleMultiDC) IsLocalNode(nodeID string) bool {
	sm.mu.RLock()
	dcID, exists := sm.nodeToDC[nodeID]
	sm.mu.RUnlock()

	if !exists {
		return true // Unknown nodes treated as local for safety
	}

	return dcID == sm.localDCID
}

// SelectPreferLocal selects nodes with local DC preference
// Returns local nodes first, then remote nodes
func (sm *SimpleMultiDC) SelectPreferLocal(candidates []string, count int) []string {
	if count <= 0 || len(candidates) == 0 {
		return nil
	}

	if len(candidates) <= count {
		return candidates
	}

	sm.mu.RLock()
	defer sm.mu.RUnlock()

	// Separate local and remote
	local := make([]string, 0, count)
	remote := make([]string, 0, count)

	for _, nodeID := range candidates {
		dcID, exists := sm.nodeToDC[nodeID]
		if !exists || dcID == sm.localDCID {
			if len(local) < count {
				local = append(local, nodeID)
				if sm.metrics != nil {
					sm.metrics.RecordOperation(true)
				}
			}
		} else {
			remote = append(remote, nodeID)
		}
	}

	// Fill with local first, then remote
	result := make([]string, 0, count)
	result = append(result, local...)

	remaining := count - len(result)
	for i := 0; i < remaining && i < len(remote); i++ {
		result = append(result, remote[i])
		if sm.metrics != nil {
			sm.metrics.RecordOperation(false)
		}
	}

	return result
}

// GetGossipInterval returns gossip interval based on target nodes
// DC内快速（100ms），DC间慢速（1s）
func (sm *SimpleMultiDC) GetGossipInterval(targetNodes []string) time.Duration {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	// Check if any target is remote
	for _, nodeID := range targetNodes {
		if dcID, exists := sm.nodeToDC[nodeID]; exists {
			if dcID != sm.localDCID {
				return 1 * time.Second // Inter-DC: slower
			}
		}
	}

	return 100 * time.Millisecond // Intra-DC: faster
}

// GetStats returns simple statistics
func (sm *SimpleMultiDC) GetStats() map[string]interface{} {
	sm.mu.RLock()
	total := len(sm.nodeToDC)

	localCount := 0
	for _, dcID := range sm.nodeToDC {
		if dcID == sm.localDCID {
			localCount++
		}
	}
	sm.mu.RUnlock()

	return map[string]interface{}{
		"local_dc_id":  sm.localDCID,
		"total_nodes":  total,
		"local_nodes":  localCount,
		"remote_nodes": total - localCount,
	}
}
