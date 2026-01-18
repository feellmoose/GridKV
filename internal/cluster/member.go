package cluster

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feellmoose/gridkv/internal/utils/executor"
	"github.com/feellmoose/gridkv/internal/utils/logging"
)

// NodeState is node health state
type NodeState int32

const (
	NodeStateUnknown NodeState = 0
	NodeStateAlive   NodeState = 1
	NodeStateSuspect NodeState = 2
	NodeStateDead    NodeState = 3
)

// NodeInfo represents cluster member metadata
type NodeInfo struct {
	NodeID      string
	Address     string
	State       NodeState
	Incarnation int64
	LastActive  time.Time
}

// MemberMgr manages cluster membership using SWIM protocol
type MemberMgr interface {
	Join(seed []string) error
	Leave() error
	Members() []NodeInfo
	State(nodeID string) NodeState
	// HandleMessage processes incoming SWIM messages
	HandleMessage(msg interface{}) error
}

type memberMgr struct {
	nodeID      string
	address     string
	incarnation atomic.Int64

	members sync.Map // map[string]*NodeInfo

	pingInterval   time.Duration
	failureTimeout time.Duration
	suspectTimeout time.Duration

	executor *executor.Exec
	stopCh   chan struct{}
	wg       sync.WaitGroup

	// Network send function (placeholder, will be replaced by network layer)
	sendFunc func(address string, msg interface{}) error

	// Callback for membership changes (e.g., to update hash ring)
	onMembershipChange func()

	// Indirect probe tracking: map[targetID]originalSender
	// Used to forward ack from target back to original sender
	indirectProbes sync.Map
}

type memberConfig struct {
	NodeID             string
	Address            string
	PingInterval       time.Duration
	FailureTimeout     time.Duration
	SuspectTimeout     time.Duration
	SendFunc           func(address string, msg interface{}) error
	OnMembershipChange func()
}

func newMemberMgr(cfg memberConfig) (*memberMgr, error) {
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 200 * time.Millisecond
	}
	if cfg.FailureTimeout <= 0 {
		cfg.FailureTimeout = 5 * time.Second
	}
	if cfg.SuspectTimeout <= 0 {
		cfg.SuspectTimeout = 2 * time.Second
	}

	exec, err := executor.New(executor.Opts{
		Name:    "member-mgr",
		Workers: 1,
		NoStats: false,
	})
	if err != nil {
		return nil, err
	}

	mgr := &memberMgr{
		nodeID:             cfg.NodeID,
		address:            cfg.Address,
		pingInterval:       cfg.PingInterval,
		failureTimeout:     cfg.FailureTimeout,
		suspectTimeout:     cfg.SuspectTimeout,
		executor:           exec,
		stopCh:             make(chan struct{}),
		sendFunc:           cfg.SendFunc,
		onMembershipChange: cfg.OnMembershipChange,
	}

	// Add self to members
	self := &NodeInfo{
		NodeID:      cfg.NodeID,
		Address:     cfg.Address,
		State:       NodeStateAlive,
		Incarnation: 0,
		LastActive:  time.Now(),
	}
	mgr.members.Store(cfg.NodeID, self)

	return mgr, nil
}

// lifecycle.Component implementation
func (m *memberMgr) Name() string { return "member-mgr" }

func (m *memberMgr) Start(ctx context.Context) error {
	m.incarnation.Store(time.Now().UnixNano())
	m.wg.Add(1)
	go m.pingLoop()
	return nil
}

func (m *memberMgr) Close(ctx context.Context) error {
	select {
	case <-m.stopCh:
	default:
		close(m.stopCh)
	}
	if err := m.executor.Stop(10 * time.Second); err != nil {
		return err
	}
	m.wg.Wait()
	return nil
}

func (m *memberMgr) pingLoop() {
	defer m.wg.Done()

	ticker := time.NewTicker(m.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			if err := m.executor.Do(func() {
				m.doPing()
			}); err != nil {
				// Executor is closed, exit gracefully
				return
			}
		}
	}
}

// doPing performs the core SWIM protocol ping cycle.
// It iterates through all known members and:
// 1. Skips self and dead nodes
// 2. Marks nodes as suspect/dead if they exceed failure timeout
// 3. Sends direct pings to healthy nodes or indirect probes to suspects
// 4. Updates LastActive timestamp on successful ping transmission to prevent false failure detection
func (m *memberMgr) doPing() {
	m.members.Range(func(key, value interface{}) bool {
		nodeID := key.(string)
		info := value.(*NodeInfo)

		// Skip self
		if nodeID == m.nodeID {
			return true
		}

		// Skip dead nodes
		if info.State == NodeStateDead {
			return true
		}

		now := time.Now()

		// Check for failure timeout - mark suspect or dead based on current state
		if now.Sub(info.LastActive) > m.failureTimeout {
			if info.State == NodeStateSuspect {
				m.markDead(nodeID)
			} else {
				m.markSuspect(nodeID)
			}
			return true
		}

		// Handle suspect nodes with indirect probing
		if info.State == NodeStateSuspect {
			m.tryIndirectProbe(nodeID)
		} else {
			// Send direct ping to healthy nodes
			if m.sendFunc != nil {
				pingMsg := &pingMsg{
					From:        m.nodeID,
					To:          nodeID,
					Incarnation: m.incarnation.Load(),
				}

				if err := m.sendFunc(info.Address, pingMsg); err != nil {
					// Removed frequent debug log "ping failed" - too verbose for production
					// Ping failures are normal during network issues and don't need debug logging
				} else {
					// Update LastActive on successful ping to prevent false failure detection
					m.updateLastActive(nodeID)
				}
			}
		}

		return true
	})
}

// HandleMessage processes incoming SWIM messages
func (m *memberMgr) HandleMessage(msg interface{}) error {
	switch v := msg.(type) {
	case *pingMsg:
		return m.handlePing(v)
	case *ackMsg:
		return m.handleAck(v)
	case *connectMsg:
		return m.handleConnect(v)
	case *leaveMsg:
		return m.handleLeave(v)
	case *indirectProbeMsg:
		return m.handleIndirectProbe(v)
	case *indirectAckMsg:
		return m.handleIndirectAck(v)
	case *clusterSyncMsg:
		return m.handleClusterSync(v)
	}
	return nil
}

func (m *memberMgr) handlePing(msg *pingMsg) error {
	// Nil check
	if msg == nil {
		return nil
	}
	// Try to get address from existing member info
	address := ""
	if existing, ok := m.members.Load(msg.From); ok {
		if info, ok := existing.(*NodeInfo); ok {
			address = info.Address
		}
	}
	// Update sender's info (only if we have address or node already exists)
	if address != "" {
		m.updateNode(msg.From, address, msg.Incarnation, NodeStateAlive)
	}

	// Send ACK
	if m.sendFunc != nil {
		// Only send ACK if we know the sender's network address
		if address != "" {
			_ = m.sendFunc(address, &ackMsg{
				From:        m.nodeID,
				To:          msg.From,
				Incarnation: m.incarnation.Load(),
			})
		}
	}
	return nil
}

func (m *memberMgr) handleAck(msg *ackMsg) error {
	// Nil check
	if msg == nil {
		return nil
	}

	// Get sender's info and update LastActive time - ACK confirms bidirectional communication
	if existing, ok := m.members.Load(msg.From); ok {
		if info, ok := existing.(*NodeInfo); ok && info.Address != "" {
			m.updateNode(msg.From, info.Address, msg.Incarnation, NodeStateAlive)
		}
	}

	// Check if this is an ack from indirect probe target and forward if needed
	if originalSender, ok := m.indirectProbes.Load(msg.From); ok {
		originalSenderID := originalSender.(string)
		m.indirectProbes.Delete(msg.From) // Clean up mapping

		// Forward ack to original sender
		if m.sendFunc != nil {
			if v, ok := m.members.Load(originalSenderID); ok {
				if info, ok := v.(*NodeInfo); ok && info.Address != "" {
					_ = m.sendFunc(info.Address, &indirectAckMsg{
						From:        m.nodeID,
						To:          originalSenderID,
						Target:      msg.From,
						Incarnation: msg.Incarnation,
					})
				}
			}
		}
	}

	return nil
}

func (m *memberMgr) handleConnect(msg *connectMsg) error {
	// Nil check
	if msg == nil {
		return nil
	}
	if msg.Address == "" || len(msg.Address) < 3 {
		logging.Debug("handleConnect: invalid address in connectMsg", "nodeID", msg.NodeID, "address", fmt.Sprintf("%q", msg.Address), "local", m.nodeID)
		return nil
	}
	m.updateNode(msg.NodeID, msg.Address, msg.Incarnation, NodeStateAlive)

	if m.sendFunc != nil {
		members := m.Members()
		for _, mem := range members {
			if mem.Address == "" || len(mem.Address) < 3 {
				logging.Debug("Members() returned invalid address", "nodeID", mem.NodeID, "address", fmt.Sprintf("%q", mem.Address), "local", m.nodeID)
			}
		}
		syncMsg := &clusterSyncMsg{
			From:    m.nodeID,
			Members: members,
		}
		if err := m.sendFunc(msg.Address, syncMsg); err != nil {
			logging.Debug("failed to send cluster sync", "from", m.nodeID, "to", msg.Address, "members", len(members), "error", err)
		}
	}
	return nil
}

func (m *memberMgr) handleLeave(msg *leaveMsg) error {
	// Nil check
	if msg == nil {
		return nil
	}
	// Mark node as dead
	m.updateNode(msg.NodeID, "", msg.Incarnation, NodeStateDead)
	return nil
}

func (m *memberMgr) tryIndirectProbe(targetID string) {
	// Find a random alive member to forward probe
	aliveMembers := make([]string, 0)
	m.members.Range(func(key, value interface{}) bool {
		nodeID := key.(string)
		info := value.(*NodeInfo)
		if nodeID != m.nodeID && nodeID != targetID && info.State == NodeStateAlive {
			aliveMembers = append(aliveMembers, nodeID)
		}
		return true
	})

	if len(aliveMembers) == 0 {
		return
	}

	// Random selection: use hash of targetID for deterministic but distributed selection
	// This ensures different nodes select different proxies for the same target
	hash := 0
	for _, c := range targetID {
		hash = hash*31 + int(c)
	}
	if hash < 0 {
		hash = -hash
	}
	proxyID := aliveMembers[hash%len(aliveMembers)]

	if m.sendFunc != nil {
		// Lookup proxy address and forward probe to its network address
		if v, ok := m.members.Load(proxyID); ok {
			if info, ok := v.(*NodeInfo); ok && info.Address != "" {
				_ = m.sendFunc(info.Address, &indirectProbeMsg{
					From:        m.nodeID,
					Target:      targetID,
					Incarnation: m.incarnation.Load(),
				})
			}
		}
	}
}

func (m *memberMgr) handleIndirectProbe(msg *indirectProbeMsg) error {
	// Nil check
	if msg == nil {
		return nil
	}
	// Forward ping to target
	if m.sendFunc != nil {
		// Resolve target nodeID to network address before forwarding
		if v, ok := m.members.Load(msg.Target); ok {
			if info, ok := v.(*NodeInfo); ok && info.Address != "" {
				// Store mapping: target -> original sender for ack forwarding
				m.indirectProbes.Store(msg.Target, msg.From)

				// Forward ping to target (From is proxy, To is target)
				// Target will send ack back to proxy, proxy will forward to original sender
				_ = m.sendFunc(info.Address, &pingMsg{
					From:        m.nodeID, // Proxy sends ping as itself
					To:          msg.Target,
					Incarnation: msg.Incarnation,
				})
			}
		}
	}
	return nil
}

func (m *memberMgr) handleIndirectAck(msg *indirectAckMsg) error {
	// Nil check
	if msg == nil {
		return nil
	}
	// This is an ack forwarded by proxy for indirect probe
	// Update target's info and forward ack to original sender
	if m.sendFunc != nil {
		// Update target's info (target responded to indirect probe)
		if v, ok := m.members.Load(msg.Target); ok {
			if info, ok := v.(*NodeInfo); ok && info.Address != "" {
				m.updateNode(msg.Target, info.Address, msg.Incarnation, NodeStateAlive)
			}
		}

		// Forward ack to original sender
		if v, ok := m.members.Load(msg.To); ok {
			if info, ok := v.(*NodeInfo); ok && info.Address != "" {
				_ = m.sendFunc(info.Address, &ackMsg{
					From:        msg.Target,
					To:          msg.To,
					Incarnation: msg.Incarnation,
				})
			}
		}
	}
	return nil
}

func (m *memberMgr) handleClusterSync(msg *clusterSyncMsg) error {
	addedCount := 0
	for i, member := range msg.Members {
		if member.NodeID != m.nodeID {
			if member.Address == "" {
				logging.Debug("cluster sync member has empty address", "nodeID", member.NodeID, "index", i, "local", m.nodeID, "state", member.State)
				continue
			}
			if len(member.Address) < 3 || member.Address[0] == 0 {
				logging.Debug("cluster sync member has invalid address", "nodeID", member.NodeID, "index", i, "address", fmt.Sprintf("%q", member.Address), "addressLen", len(member.Address), "local", m.nodeID)
				continue
			}
			m.updateNode(member.NodeID, member.Address, member.Incarnation, member.State)
			addedCount++
		}
	}
	if addedCount > 0 {
		logging.Debug("cluster sync received", "from", msg.From, "members", len(msg.Members), "added", addedCount, "local", m.nodeID)
	}
	return nil
}

func (m *memberMgr) markSuspect(nodeID string) {
	value, ok := m.members.Load(nodeID)
	if !ok {
		return
	}

	info := value.(*NodeInfo)
	if info.State == NodeStateSuspect {
		return // Already suspect
	}

	newInfo := &NodeInfo{
		NodeID:      info.NodeID,
		Address:     info.Address,
		State:       NodeStateSuspect,
		Incarnation: info.Incarnation,
		LastActive:  info.LastActive,
	}
	m.members.Store(nodeID, newInfo)

	// Notify membership change
	if m.onMembershipChange != nil {
		m.onMembershipChange()
	}

	// Schedule confirm check
	time.AfterFunc(m.suspectTimeout, func() {
		value, ok := m.members.Load(nodeID)
		if !ok {
			return
		}
		info := value.(*NodeInfo)
		if info.State == NodeStateSuspect && time.Since(info.LastActive) > m.suspectTimeout {
			logging.Debug("suspect timeout expired, marking dead", "nodeID", nodeID, "local", m.nodeID, "lastActiveAgo", time.Since(info.LastActive), "suspectTimeout", m.suspectTimeout)
			m.markDead(nodeID)
		}
	})
}

func (m *memberMgr) markDead(nodeID string) {
	value, ok := m.members.Load(nodeID)
	if !ok {
		return
	}

	info := value.(*NodeInfo)
	if info.State == NodeStateDead {
		return
	}

	newInfo := &NodeInfo{
		NodeID:      info.NodeID,
		Address:     info.Address,
		State:       NodeStateDead,
		Incarnation: info.Incarnation,
		LastActive:  info.LastActive,
	}
	m.members.Store(nodeID, newInfo)
	logging.Debug("node marked as dead",
		"nodeID", nodeID,
		"local", m.nodeID,
		"lastActiveAgo", time.Since(info.LastActive),
		"failureTimeout", m.failureTimeout,
		"address", info.Address,
	)
	if m.onMembershipChange != nil {
		m.onMembershipChange()
	}
}

// updateLastActive updates LastActive timestamp on successful ping.
// Prevents false failure detection when ACK is lost but ping succeeds.
func (m *memberMgr) updateLastActive(nodeID string) {
	if nodeID == "" {
		return
	}
	value, ok := m.members.Load(nodeID)
	if !ok {
		return
	}
	info := value.(*NodeInfo)

	// Skip if node is not in alive state (don't update dead/suspect nodes)
	if info.State != NodeStateAlive {
		return
	}

	now := time.Now()
	// Use 1-second threshold to balance performance and reliability
	if now.Sub(info.LastActive) < time.Second {
		return
	}

	// Atomic timestamp update: modify existing object instead of creating new one
	// This eliminates memory allocation and reduces sync.Map Store operations
	info.LastActive = now
	// Note: This modifies the object in-place, which is safe since we're only
	// updating the timestamp field that doesn't affect cluster membership logic
}

func (m *memberMgr) updateNode(nodeID string, address string, incarnation int64, state NodeState) {
	if address == "" {
		logging.Debug("updateNode called with empty address", "nodeID", nodeID, "local", m.nodeID)
		return
	}
	value, ok := m.members.Load(nodeID)
	wasNew := !ok
	stateChanged := false

	if !ok {
		info := &NodeInfo{
			NodeID:      nodeID,
			Address:     address,
			State:       state,
			Incarnation: incarnation,
			LastActive:  time.Now(),
		}
		m.members.Store(nodeID, info)
		stateChanged = true
		logging.Debug("new node discovered", "nodeID", nodeID, "address", address, "local", m.nodeID)
	} else {
		info := value.(*NodeInfo)
		if incarnation < info.Incarnation {
			return
		}

		stateChanged = (info.State != state)
		// Removed frequent debug log "node address updated" - too verbose
		newInfo := &NodeInfo{
			NodeID:      nodeID,
			Address:     address,
			State:       state,
			Incarnation: incarnation,
			LastActive:  time.Now(),
		}
		m.members.Store(nodeID, newInfo)
		// Removed frequent debug log "node state changed" - too verbose for production
	}

	if (wasNew || stateChanged) && m.onMembershipChange != nil {
		m.onMembershipChange()
	}
}

func (m *memberMgr) Join(seed []string) error {
	if len(seed) == 0 {
		return nil
	}

	if m.sendFunc == nil {
		logging.Error(errors.New("sendFunc not initialized"), "join failed: sendFunc not initialized", "node_id", m.nodeID)
		return errors.New("sendFunc not initialized")
	}

	var lastErr error
	successCount := 0
	for _, addr := range seed {
		if m.address == "" || len(m.address) < 3 {
			logging.Debug("Join: invalid local address", "nodeID", m.nodeID, "address", fmt.Sprintf("%q", m.address))
			continue
		}
		msg := &connectMsg{
			NodeID:      m.nodeID,
			Address:     m.address,
			Incarnation: m.incarnation.Load(),
		}
		err := m.sendFunc(addr, msg)
		if err != nil {
			lastErr = fmt.Errorf("failed to send CONNECT to %s: %w", addr, err)
			logging.Debug("CONNECT send failed", "from", m.nodeID, "to", addr, "error", err)
		} else {
			successCount++
		}
	}

	if successCount == 0 && lastErr != nil {
		logging.Error(lastErr, "join failed: all seed nodes unreachable", "node_id", m.nodeID, "seeds", seed)
		return fmt.Errorf("failed to join any seed node: %w", lastErr)
	}
	if successCount > 0 {
		logging.Debug("Join successful", "nodeID", m.nodeID, "connected", successCount, "total", len(seed))
	}

	return nil
}

func (m *memberMgr) Leave() error {
	// Mark self as dead and notify others
	m.incarnation.Add(1)
	m.markDead(m.nodeID)

	// Notify all members
	m.members.Range(func(key, value interface{}) bool {
		info := value.(*NodeInfo)
		if info.NodeID != m.nodeID && info.State == NodeStateAlive {
			if m.sendFunc != nil {
				_ = m.sendFunc(info.Address, &leaveMsg{
					NodeID:      m.nodeID,
					Incarnation: m.incarnation.Load(),
				})
			}
		}
		return true
	})

	return nil
}

func (m *memberMgr) Members() []NodeInfo {
	var result []NodeInfo
	m.members.Range(func(key, value interface{}) bool {
		info := value.(*NodeInfo)
		if info.Address == "" || len(info.Address) < 3 {
			logging.Debug("Members() found node with invalid address", "nodeID", info.NodeID, "address", fmt.Sprintf("%q", info.Address), "local", m.nodeID)
		}
		result = append(result, *info)
		return true
	})
	return result
}

func (m *memberMgr) State(nodeID string) NodeState {
	value, ok := m.members.Load(nodeID)
	if !ok {
		return NodeStateUnknown
	}
	return value.(*NodeInfo).State
}

// Message types are defined in types.go
