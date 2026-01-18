package cluster

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/feellmoose/gridkv/internal/mem_storage"
	"github.com/feellmoose/gridkv/internal/utils/executor"
	"github.com/feellmoose/gridkv/internal/utils/hlc"
	"github.com/feellmoose/gridkv/internal/utils/logging"
	"github.com/feellmoose/gridkv/internal/utils/zerocopy"
)

// Object pools for gossip operations
var (
	// String slice pool for aliveMembers
	aliveMembersPool = sync.Pool{
		New: func() interface{} {
			return make([]string, 0, 64)
		},
	}

	// Map pool for nodeIDToAddr in gossip
	gossipNodeIDToAddrPool = sync.Pool{
		New: func() interface{} {
			return make(map[string]string, 64)
		},
	}
)

// Gossip handles epidemic propagation of sync operations
type Gossip interface {
	Push(ops []*mem_storage.SyncOperation, targets []string) error
	Pull(target string) ([]*mem_storage.SyncOperation, error)
	HandleMessage(data []byte) error // Handle incoming gossip message
}

type gossip struct {
	nodeID       string
	store        *mem_storage.MemStorage
	ring         HashRing
	replicaCount int
	executor     *executor.Exec
	hlc          *hlc.HLC
	interval     time.Duration
	stopCh       chan struct{}
	stopOnce     sync.Once
	wg           sync.WaitGroup

	sendFunc func(address string, data []byte) error
	recvFunc func() ([]byte, error)

	member MemberMgr // For selecting random targets
}

type gossipConfig struct {
	NodeID       string
	Store        *mem_storage.MemStorage
	Ring         HashRing
	ReplicaCount int
	Executor     *executor.Exec
	Interval     time.Duration
	HLC          *hlc.HLC
	SendFunc     func(address string, data []byte) error
	RecvFunc     func() ([]byte, error)
	Member       MemberMgr
}

func newGossip(cfg gossipConfig) (*gossip, error) {
	if cfg.Interval <= 0 {
		cfg.Interval = 100 * time.Millisecond // Balanced gossip interval for high concurrency
	}
	if cfg.ReplicaCount <= 0 {
		cfg.ReplicaCount = 3
	}

	g := &gossip{
		nodeID:       cfg.NodeID,
		store:        cfg.Store,
		ring:         cfg.Ring,
		replicaCount: cfg.ReplicaCount,
		executor:     cfg.Executor,
		hlc:          cfg.HLC,
		interval:     cfg.Interval,
		stopCh:       make(chan struct{}),
		sendFunc:     cfg.SendFunc,
		recvFunc:     cfg.RecvFunc,
		member:       cfg.Member,
	}

	return g, nil
}

// lifecycle.Component implementation
func (g *gossip) Name() string { return "gossip" }

func (g *gossip) Start(ctx context.Context) error {
	g.wg.Add(1)
	go g.gossipLoop()
	return nil
}

func (g *gossip) Close(ctx context.Context) error {
	g.stopOnce.Do(func() {
		close(g.stopCh)
	})
	g.wg.Wait()
	return nil
}

func (g *gossip) gossipLoop() {
	defer g.wg.Done()

	ticker := time.NewTicker(g.interval)
	defer ticker.Stop()

	consecutiveFailures := 0
	originalInterval := g.interval
	lastGC := time.Now()
	gcInterval := 5 * time.Minute // Periodic GC hint for long-running processes

	for {
		select {
		case <-g.stopCh:
			return
		case <-ticker.C:
			// Periodic GC hint for long-running processes
			if time.Since(lastGC) > gcInterval {
				runtime.GC()
				lastGC = time.Now()
			}

			if err := g.executor.Do(func() {
				g.doGossip()
			}); err != nil {
				consecutiveFailures++
				// On executor failures, increase frequency temporarily for high concurrency
				if consecutiveFailures >= 3 && g.interval > 50*time.Millisecond {
					g.interval = g.interval / 2
					if g.interval < 50*time.Millisecond {
						g.interval = 50 * time.Millisecond
					}
					ticker.Reset(g.interval)
					logging.Info("Gossip increasing frequency due to executor pressure", "node", g.nodeID, "new_interval", g.interval)
				}
				continue
			} else {
				consecutiveFailures = 0
				// Gradually restore normal interval
				if g.interval < originalInterval {
					g.interval = g.interval * 5 / 4
					if g.interval > originalInterval {
						g.interval = originalInterval
					}
					ticker.Reset(g.interval)
				}
			}
		}
	}
}

func (g *gossip) doGossip() {
	// Periodic pull from random nodes (log(N) fan-out)
	if g.member == nil {
		return
	}

	members := g.member.Members()
	if len(members) == 0 {
		return
	}

	// Select log(N) random targets for fan-out
	// Use object pool to reduce allocations
	aliveMembers := aliveMembersPool.Get().([]string)
	aliveMembers = aliveMembers[:0] // Reset length
	defer aliveMembersPool.Put(aliveMembers)

	for _, m := range members {
		if m.NodeID != g.nodeID && m.State == NodeStateAlive {
			aliveMembers = append(aliveMembers, m.NodeID)
		}
	}

	if len(aliveMembers) == 0 {
		return
	}

	// Fan-out = log2(N), but at least 1
	fanOut := 1
	if len(aliveMembers) > 1 {
		n := len(aliveMembers)
		// Calculate log2(n) efficiently
		fanOut = 0
		for n > 0 {
			fanOut++
			n >>= 1
		}
		if fanOut > len(aliveMembers) {
			fanOut = len(aliveMembers)
		}
	}

	// Random selection to avoid hotspots
	// Shuffle first fanOut positions
	selected := make([]string, fanOut)
	copy(selected, aliveMembers[:fanOut])

	// Simple shuffle using current time as seed (in production, use crypto/rand)
	for i := len(selected) - 1; i > 0; i-- {
		j := (i * 7) % (i + 1) // Pseudo-random using simple hash
		selected[i], selected[j] = selected[j], selected[i]
	}

	nodeIDToAddr := gossipNodeIDToAddrPool.Get().(map[string]string)
	defer func() {
		for k := range nodeIDToAddr {
			delete(nodeIDToAddr, k)
		}
		gossipNodeIDToAddrPool.Put(nodeIDToAddr)
	}()
	memberMap := make(map[string]NodeInfo, len(members))
	for _, m := range members {
		nodeIDToAddr[m.NodeID] = m.Address
		memberMap[m.NodeID] = m
	}
	for _, targetNodeID := range selected {
		targetAddr := nodeIDToAddr[targetNodeID]
		if targetAddr == "" {
			continue
		}
		if member, ok := memberMap[targetNodeID]; ok && member.State != NodeStateAlive {
			continue
		}
		addr := targetAddr
		if err := g.executor.Do(func() {
			_, _ = g.Pull(addr)
		}); err != nil {
			return
		}
	}
}

func (g *gossip) Push(ops []*mem_storage.SyncOperation, targets []string) error {
	if len(ops) == 0 || len(targets) == 0 {
		return nil
	}

	// Log for large batches only
	if len(ops) > 50 {
		logging.Debug("Gossip Push large batch", "node", g.nodeID, "ops_count", len(ops), "targets_count", len(targets))
	}

	data, err := SerializeSyncOps(ops)
	if err != nil {
		logging.Warn("Gossip Push serialization failed", "node", g.nodeID, "ops_count", len(ops), "error", err)
		return err
	}

	if len(ops) > 50 {
		logging.Debug("Gossip Push data serialized", "node", g.nodeID, "data_size", len(data), "ops_count", len(ops))
	}

	// For direct replication (writer calls Push with specific targets),
	// push to ALL targets to ensure data replication completeness
	// Fan-out is only used for epidemic gossip propagation
	fanOut := len(targets)

	// Push to all targets to ensure complete replication
	var lastErr error
	for i := 0; i < fanOut; i++ {
		target := targets[i]
		if g.member != nil {
			members := g.member.Members()
			isAlive := false
			for _, m := range members {
				if m.Address == target && m.State == NodeStateAlive {
					isAlive = true
					break
				}
			}
			if !isAlive {
				continue
			}
		}
		if err := g.executor.Do(func() {
			g.pushToTarget(target, data, 15)
		}); err != nil {
			logging.Warn("Gossip Push executor error", "node", g.nodeID, "target", target, "error", err)
			lastErr = err
		}
	}

	// Return last error if any, but don't fail the entire push
	if lastErr != nil {
		logging.Warn("Gossip Push completed with errors", "node", g.nodeID, "fanOut", fanOut, "last_error", lastErr)
	}

	return nil
}

func (g *gossip) pushToTarget(target string, data []byte, maxRetries int) {
	if g.sendFunc == nil {
		logging.Debug("Gossip pushToTarget: no sendFunc", "node", g.nodeID, "target", target)
		return
	}

	// Use constant to avoid repeated allocation
	const pushPrefix = "PUSH:"
	prefix := zerocopy.StringToBytes(pushPrefix)
	msg := make([]byte, len(prefix)+len(data))
	copy(msg, prefix)
	copy(msg[len(prefix):], data)

	for i := 0; i < maxRetries; i++ {
		if g.member != nil {
			members := g.member.Members()
			isAlive := false
			for _, m := range members {
				if m.Address == target && m.State == NodeStateAlive {
					isAlive = true
					break
				}
			}
			if !isAlive {
				return
			}
		}

		// Use timeout channel directly instead of context for better performance
		timeout := time.NewTimer(8 * time.Second)
		defer timeout.Stop()
		done := errorChanPool.Get().(chan error)

		go func() {
			done <- g.sendFunc(target, msg)
		}()

		select {
		case err := <-done:
			errorChanPool.Put(done)
			if err == nil {
				return
			}
			if isConnectionRefused(err) && i == 0 {
				return
			}
			if i == maxRetries-1 || i%3 == 0 {
				logging.Warn("Gossip pushToTarget failed", "node", g.nodeID, "target", target, "attempt", i+1, "error", err)
			}
		case <-timeout.C:
			errorChanPool.Put(done)
			if i == maxRetries-1 || i%3 == 0 {
				logging.Warn("Gossip pushToTarget timeout", "node", g.nodeID, "target", target, "attempt", i+1)
			}
		}

		if i < maxRetries-1 {
			backoff := time.Duration(1<<uint(i)) * 50 * time.Millisecond
			if backoff > 500*time.Millisecond {
				backoff = 500 * time.Millisecond
			}
			time.Sleep(backoff)
		}
	}
	logging.Debug("Gossip pushToTarget exhausted retries", "node", g.nodeID, "target", target)
}

func isConnectionRefused(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "connect: connection refused")
}

func (g *gossip) Pull(target string) ([]*mem_storage.SyncOperation, error) {
	if g.sendFunc == nil {
		return nil, nil
	}

	// Get local sync buffer to send
	localOps, err := g.store.GetSyncBuffer()
	logging.Debug("Gossip Pull sync buffer", "node", g.nodeID, "target", target, "ops_count", len(localOps), "error", err)
	if err != nil || len(localOps) == 0 {
		return nil, nil
	}

	// Serialize and send
	data, err := SerializeSyncOps(localOps)
	if err != nil {
		return nil, err
	}

	// Send pull request with local ops (push-pull pattern)
	// Use zerocopy to avoid string allocation
	pullPrefix := zerocopy.StringToBytes("PULL:" + g.nodeID + ":")
	pullReq := make([]byte, len(pullPrefix)+len(data))
	copy(pullReq, pullPrefix)
	copy(pullReq[len(pullPrefix):], data)
	if err := g.sendFunc(target, pullReq); err != nil {
		return nil, err
	}

	return nil, nil
}

func (g *gossip) applyOps(ops []*mem_storage.SyncOperation) error {
	if len(ops) == 0 {
		return nil
	}
	if len(ops) > 100 {
		logging.Debug("Gossip applyOps large batch", "node", g.nodeID, "ops_count", len(ops))
	}

	// Cache replica checks to reduce ring lookups
	keyReplicaCache := make(map[string]bool, len(ops))
	getReplica := func(key string) bool {
		if cached, ok := keyReplicaCache[key]; ok {
			return cached
		}
		if g.ring == nil {
			return false
		}
		targets := g.ring.GetN(key, g.replicaCount)
		isReplica := false
		for _, target := range targets {
			if target == g.nodeID {
				isReplica = true
				break
			}
		}
		keyReplicaCache[key] = isReplica
		return isReplica
	}

	appliedCount := 0
	skippedCount := 0
	for _, op := range ops {
		if op.Item == nil {
			skippedCount++
			continue
		}

		if !getReplica(op.Key) {
			skippedCount++
			continue
		}

		// Version check: skip if existing version >= incoming (LWW)
		existing, err := g.store.GetNoCopy(op.Key)
		if err == nil && existing != nil && existing.Version >= op.Item.Version {
			skippedCount++
			continue
		}

		// Store: new version wins or key doesn't exist
		if err := g.store.Set(op.Key, op.Item); err != nil {
			logging.Warn("Gossip store failed", "node", g.nodeID, "key", op.Key, "error", err)
			return err
		}
		appliedCount++
	}

	if len(ops) > 50 {
		logging.Debug("Gossip applyOps completed", "node", g.nodeID, "total", len(ops), "applied", appliedCount, "skipped", skippedCount)
	}
	return nil
}

// HandleMessage processes incoming gossip messages
// Expected format:
//   - Push: "PUSH:" + serializedOps
//   - Pull: "PULL:nodeID:" + serializedOps
func (g *gossip) HandleMessage(data []byte) error {
	if len(data) < 5 {
		logging.Debug("Gossip HandleMessage: message too short", "node", g.nodeID, "dataLen", len(data))
		return nil
	}

	// Use byte comparison instead of string conversion (zero allocation)
	// Check prefix: "PUSH:" or "PULL:"
	isPush := len(data) >= 5 && data[0] == 'P' && data[1] == 'U' && data[2] == 'S' && data[3] == 'H' && data[4] == ':'
	isPull := len(data) >= 5 && data[0] == 'P' && data[1] == 'U' && data[2] == 'L' && data[3] == 'L' && data[4] == ':'

	if !isPush && !isPull {
		prefixLen := 10
		if len(data) < prefixLen {
			prefixLen = len(data)
		}
		logging.Debug("Gossip HandleMessage: unknown message format", "node", g.nodeID, "prefix", string(data[:prefixLen]))
		return nil
	}

	payload := data[5:]

	if isPush {
		ops, err := DeserializeSyncOps(payload)
		if err != nil {
			return err
		}
		return g.applyOps(ops)
	}

	if isPull {
		// Extract nodeID and ops from pull request
		// Format: "PULL:nodeID:serializedOps"
		// Find first colon after "PULL:"
		idx := 0
		for idx < len(payload) && payload[idx] != ':' {
			idx++
		}
		if idx < len(payload) {
			remoteOps, err := DeserializeSyncOps(payload[idx+1:])
			if err == nil && len(remoteOps) > 0 {
				// Apply remote operations (error ignored as this is async gossip path)
				if err := g.applyOps(remoteOps); err != nil {
					logging.Debug("Failed to apply remote ops in gossip pull", "node", g.nodeID, "ops_count", len(remoteOps), "error", err)
				}
			}
		}
		// Send back local ops (push-pull pattern)
		// Use executor to avoid blocking
		if g.executor != nil && g.sendFunc != nil {
			targetNodeIDBytes := payload[:idx]
			// Use zerocopy to avoid allocation
			targetNodeID := zerocopy.BytesToString(targetNodeIDBytes)
			_ = g.executor.Do(func() {
				localOps, err := g.store.GetSyncBuffer()
				if err != nil || len(localOps) == 0 {
					return
				}
				serializedData, err := SerializeSyncOps(localOps)
				if err != nil || serializedData == nil {
					return
				}
				// Convert nodeID to address
				var targetAddr string
				if g.member != nil {
					members := g.member.Members()
					for _, m := range members {
						if m.NodeID == targetNodeID {
							targetAddr = m.Address
							break
						}
					}
				}
				if targetAddr == "" {
					return
				}
				// Pre-allocate message to avoid repeated allocation
				pushPrefix := []byte("PUSH:")
				responseMsg := make([]byte, len(pushPrefix)+len(serializedData))
				copy(responseMsg, pushPrefix)
				copy(responseMsg[len(pushPrefix):], serializedData)
				// Send response (error ignored as this is async gossip path)
				if err := g.sendFunc(targetAddr, responseMsg); err != nil {
					logging.Debug("Failed to send gossip pull response", "node", g.nodeID, "target", targetAddr, "error", err)
				}
			})
		}
	}
	return nil
}
