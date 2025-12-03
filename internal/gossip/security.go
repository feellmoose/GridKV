package gossip

// File: security.go  
// Consolidated security and bootstrap functionality
// Merged from: bootstrap.go, security_tracker.go

import (
	"sync"
	"sync/atomic"
	"time"
)

// BootstrapConfig configuration for secure cluster formation
type BootstrapConfig struct {
	Enabled bool
	Token   string
	Timeout time.Duration
}

// BootstrapTokenManager manages bootstrap tokens
type BootstrapTokenManager struct {
	mu      sync.RWMutex
	enabled bool
	token   string
}

// NewBootstrapTokenManager creates token manager
func NewBootstrapTokenManager(config *BootstrapConfig) *BootstrapTokenManager {
	if config == nil {
		return &BootstrapTokenManager{enabled: false}
	}
	return &BootstrapTokenManager{
		enabled: config.Enabled,
		token:   config.Token,
	}
}

// ValidateToken validates token
func (btm *BootstrapTokenManager) ValidateToken(token string) bool {
	if btm == nil || !btm.enabled {
		return true
	}
	btm.mu.RLock()
	defer btm.mu.RUnlock()
	return token == btm.token
}

// GetToken gets token
func (btm *BootstrapTokenManager) GetToken() string {
	if btm == nil {
		return ""
	}
	btm.mu.RLock()
	defer btm.mu.RUnlock()
	return btm.token
}

// VerifyBootstrapToken alias for ValidateToken
func (btm *BootstrapTokenManager) VerifyBootstrapToken(token string) bool {
	return btm.ValidateToken(token)
}

// UseBootstrapToken uses token
func (btm *BootstrapTokenManager) UseBootstrapToken(token string) bool {
	return btm.ValidateToken(token)
}

// IsBootstrapModeActive checks if enabled
func (btm *BootstrapTokenManager) IsBootstrapModeActive() bool {
	if btm == nil {
		return false
	}
	btm.mu.RLock()
	defer btm.mu.RUnlock()
	return btm.enabled
}

// SuspiciousNodeTracker tracks suspicious nodes
type SuspiciousNodeTracker struct {
	mu              sync.RWMutex
	failures        map[string]int64
	threshold       int
	timeWindow      time.Duration
	suspiciousCount atomic.Int64
}

// NewSuspiciousNodeTracker creates tracker
func NewSuspiciousNodeTracker(threshold int, timeWindow time.Duration) *SuspiciousNodeTracker {
	return &SuspiciousNodeTracker{
		failures:   make(map[string]int64),
		threshold:  threshold,
		timeWindow: timeWindow,
	}
}

// RecordFailure records failure
func (st *SuspiciousNodeTracker) RecordFailure(nodeID string) {
	if st == nil {
		return
	}
	st.mu.Lock()
	st.failures[nodeID]++
	if st.failures[nodeID] >= int64(st.threshold) {
		st.suspiciousCount.Store(int64(len(st.failures)))
	}
	st.mu.Unlock()
}

// GetSuspiciousCount gets suspicious count
func (st *SuspiciousNodeTracker) GetSuspiciousCount() int64 {
	if st == nil {
		return 0
	}
	return st.suspiciousCount.Load()
}

// IsSuspicious checks if node is suspicious
func (st *SuspiciousNodeTracker) IsSuspicious(nodeID string) bool {
	if st == nil {
		return false
	}
	st.mu.RLock()
	count := st.failures[nodeID]
	st.mu.RUnlock()
	return count >= int64(st.threshold)
}

// Clear clears records
func (st *SuspiciousNodeTracker) Clear(nodeID string) {
	if st == nil {
		return
	}
	st.mu.Lock()
	delete(st.failures, nodeID)
	st.suspiciousCount.Store(int64(len(st.failures)))
	st.mu.Unlock()
}

// RecordUnauthenticatedMessage records unauthenticated message
func (st *SuspiciousNodeTracker) RecordUnauthenticatedMessage(nodeID string) {
	st.RecordFailure(nodeID)
}

// RecordSignatureFailure records signature failure
func (st *SuspiciousNodeTracker) RecordSignatureFailure(nodeID string) {
	st.RecordFailure(nodeID)
}

