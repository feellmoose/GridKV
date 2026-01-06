package cluster

import (
	"context"
	"sync"
	"time"

	"github.com/feellmoose/gridkv/internal/mem_storage"
	"github.com/feellmoose/gridkv/internal/utils/executor"
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
	SendFunc     func(address string, data []byte) error
	RecvFunc     func() ([]byte, error)
	Member       MemberMgr
}

func newGossip(cfg gossipConfig) (*gossip, error) {
	if cfg.Interval <= 0 {
		cfg.Interval = 400 * time.Millisecond
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

	for {
		select {
		case <-g.stopCh:
			return
		case <-ticker.C:
			_ = g.executor.Do(func() {
				g.doGossip()
			})
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

	// Convert nodeID to address
	nodeIDToAddr := gossipNodeIDToAddrPool.Get().(map[string]string)
	defer func() {
		for k := range nodeIDToAddr {
			delete(nodeIDToAddr, k)
		}
		gossipNodeIDToAddrPool.Put(nodeIDToAddr)
	}()
	for _, m := range members {
		nodeIDToAddr[m.NodeID] = m.Address
	}
	for _, targetNodeID := range selected {
		targetAddr := nodeIDToAddr[targetNodeID]
		if targetAddr == "" {
			continue // Skip if address not found
		}
		// Capture for goroutine
		addr := targetAddr
		_ = g.executor.Do(func() {
			_, _ = g.Pull(addr)
		})
	}
}

func (g *gossip) Push(ops []*mem_storage.SyncOperation, targets []string) error {
	if len(ops) == 0 || len(targets) == 0 {
		return nil
	}

	data, err := SerializeSyncOps(ops)
	if err != nil {
		return err
	}

	// Fan-out = log2(N) to avoid full broadcast
	fanOut := 1
	if len(targets) > 1 {
		n := len(targets)
		fanOut = 0
		for n > 0 {
			fanOut++
			n >>= 1
		}
		if fanOut > len(targets) {
			fanOut = len(targets)
		}
	}

	// Push to first fanOut targets
	for i := 0; i < fanOut; i++ {
		target := targets[i]
		_ = g.executor.Do(func() {
			g.pushToTarget(target, data, 3) // Max 3 retries
		})
	}

	return nil
}

func (g *gossip) pushToTarget(target string, data []byte, maxRetries int) {
	if g.sendFunc == nil {
		return
	}

	// Pre-allocate message with prefix to avoid repeated allocation on retry
	prefix := []byte("PUSH:")
	msg := make([]byte, len(prefix)+len(data))
	copy(msg, prefix)
	copy(msg[len(prefix):], data)

	for i := 0; i < maxRetries; i++ {
		if err := g.sendFunc(target, msg); err == nil {
			return
		}

		if i < maxRetries-1 {
			backoff := time.Duration(i+1) * 100 * time.Millisecond
			time.Sleep(backoff)
		}
	}
}

func (g *gossip) Pull(target string) ([]*mem_storage.SyncOperation, error) {
	if g.sendFunc == nil {
		return nil, nil
	}

	// Get local sync buffer to send
	localOps, err := g.store.GetSyncBuffer()
	if err != nil || len(localOps) == 0 {
		return nil, nil
	}

	// Serialize and send
	data, err := SerializeSyncOps(localOps)
	if err != nil {
		return nil, err
	}

	// Send pull request with local ops (push-pull pattern)
	pullReq := append([]byte("PULL:"+g.nodeID+":"), data...)
	if err := g.sendFunc(target, pullReq); err != nil {
		return nil, err
	}

	return nil, nil
}

func (g *gossip) applyOps(ops []*mem_storage.SyncOperation) error {
	if len(ops) == 0 {
		return nil
	}

	// Cache GetN results for same keys to reduce ring lookups
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

	for _, op := range ops {
		if op.Item == nil {
			continue
		}

		// Check if local node is a replica for this key (use cache)
		if !getReplica(op.Key) {
			continue
		}

		existing, err := g.store.Get(op.Key)
		if err == nil && existing != nil {
			if !op.Item.ResolveConflict(existing) {
				continue // Existing version wins
			}
		}

		if err := g.store.Set(op.Key, op.Item); err != nil {
			return err
		}
	}
	return nil
}

// HandleMessage processes incoming gossip messages
func (g *gossip) HandleMessage(data []byte) error {
	if len(data) < 5 {
		return nil
	}

	// Use byte comparison instead of string conversion (zero allocation)
	// Check prefix: "PUSH:" or "PULL:"
	isPush := len(data) >= 5 && data[0] == 'P' && data[1] == 'U' && data[2] == 'S' && data[3] == 'H' && data[4] == ':'
	isPull := len(data) >= 5 && data[0] == 'P' && data[1] == 'U' && data[2] == 'L' && data[3] == 'L' && data[4] == ':'

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
				_ = g.applyOps(remoteOps)
			}
		}
		// Send back local ops (push-pull)
		localOps, err := g.store.GetSyncBuffer()
		if err == nil && len(localOps) > 0 {
			serializedData, _ := SerializeSyncOps(localOps)
			if serializedData != nil && g.sendFunc != nil {
				// Extract target nodeID from pull request (use bytes, avoid string conversion)
				targetNodeIDBytes := payload[:idx]
				targetNodeID := string(targetNodeIDBytes) // Only convert when needed
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
				if targetAddr != "" {
					// Pre-allocate message to avoid repeated allocation
					pushPrefix := []byte("PUSH:")
					responseMsg := make([]byte, len(pushPrefix)+len(serializedData))
					copy(responseMsg, pushPrefix)
					copy(responseMsg[len(pushPrefix):], serializedData)
					_ = g.sendFunc(targetAddr, responseMsg)
				}
			}
		}
	}
	return nil
}
