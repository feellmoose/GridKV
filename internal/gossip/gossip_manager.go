package gossip

import (
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feellmoose/gridkv/internal/utils/crypto"
	"github.com/feellmoose/gridkv/internal/utils/hlc"
	"github.com/feellmoose/gridkv/internal/utils/logging"
	"github.com/feellmoose/gridkv/internal/utils/opid"
	"github.com/panjf2000/ants/v2"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Constants for node states and message types for cleaner code
const (
	Alive   NodeState = NodeState_NODE_STATE_ALIVE   // Node is healthy and responsive
	Suspect NodeState = NodeState_NODE_STATE_SUSPECT // Node failed heartbeat and is suspected dead
	Dead    NodeState = NodeState_NODE_STATE_DEAD    // Node confirmed dead and removed from the ring
)

const (
	CACHE_SYNC         GossipMessageType = GossipMessageType_MESSAGE_TYPE_CACHE_SYNC
	CLUSTER_SYNC       GossipMessageType = GossipMessageType_MESSAGE_TYPE_CLUSTER_SYNC
	CONNECT            GossipMessageType = GossipMessageType_MESSAGE_TYPE_CONNECT
	PROBE_REQUEST      GossipMessageType = GossipMessageType_MESSAGE_TYPE_PROBE_REQUEST
	PROBE_RESPONSE     GossipMessageType = GossipMessageType_MESSAGE_TYPE_PROBE_RESPONSE
	CACHE_SYNC_ACK     GossipMessageType = GossipMessageType_MESSAGE_TYPE_CACHE_SYNC_ACK
	FULL_SYNC_REQUEST  GossipMessageType = GossipMessageType_MESSAGE_TYPE_FULL_SYNC_REQUEST
	FULL_SYNC_RESPONSE GossipMessageType = GossipMessageType_MESSAGE_TYPE_FULL_SYNC_RESPONSE
	READ_REQUEST       GossipMessageType = GossipMessageType_MESSAGE_TYPE_READ_REQUEST
	READ_RESPONSE      GossipMessageType = GossipMessageType_MESSAGE_TYPE_READ_RESPONSE
)

// GossipOptions contains configuration for the GossipManager.
type GossipOptions struct {
	LocalNodeID        string        // Unique identifier for this node
	LocalAddress       string        // Network address for this node (host:port)
	SeedAddrs          []string      // Bootstrap nodes for cluster formation
	FailureTimeout     time.Duration // Timeout before marking node as suspect
	SuspectTimeout     time.Duration // Timeout before marking suspect node as dead
	GossipInterval     time.Duration // Interval for periodic gossip broadcasts
	ReplicaCount       int           // N: Number of replicas for each key
	WriteQuorum        int           // W: Write quorum (must ack W replicas)
	ReadQuorum         int           // R: Read quorum (must read from R replicas)
	MaxReplicators     int           // Max concurrent replication goroutines
	ReplicationTimeout time.Duration // Timeout for replication operations
	ReadTimeout        time.Duration // Timeout for read operations
	DisableAuth        bool          // Disable message authentication (use with caution)
}

// GossipManager is the core component that manages cluster membership, failure detection,
// and data replication using a gossip protocol.
//
// Key responsibilities:
//   - Maintain cluster membership using SWIM-based failure detection
//   - Coordinate distributed reads/writes with quorum
//   - Replicate data across nodes using consistent hashing
//   - Synchronize state via incremental and full sync
//   - Sign and verify messages for security
type GossipManager struct {
	mu           sync.RWMutex
	localNodeID  string
	localAddress string
	seedAddrs    []string             // Bootstrap seed addresses
	liveNodes    map[string]*NodeInfo // Active cluster members

	hashRing *ConsistentHash // Consistent hash ring for data distribution
	store    KVStore         // Local storage backend
	network  Network         // Network layer for communication

	inputCh chan *GossipMessage // Incoming message queue
	stopCh  chan struct{}       // Shutdown signal
	wg      sync.WaitGroup      // Wait group for graceful shutdown

	// Message statistics (atomic counters for diagnostics)
	messagesTotal   atomic.Int64
	messagesDropped atomic.Int64

	// Configuration
	failureTimeout time.Duration
	suspectTimeout time.Duration
	gossipInterval time.Duration

	replicaCount       int
	writeQuorum        int
	readQuorum         int
	maxReplicators     int
	replicationTimeout time.Duration
	readTimeout        time.Duration

	// Versioning and timing
	localVersion int64           // Atomic counter for local version
	hlc          *hlc.HLC        // Hybrid logical clock
	opidGen      *opid.Generator // Operation ID generator

	// Cryptography
	keypair     *crypto.KeyPair             // Local key pair for signing
	peerPubkeys map[string]crypto.PublicKey // Peer public keys for verification
	disableAuth bool                        // Disable message authentication

	// Read operation tracking
	pendingReads sync.Map // map[requestId]chan *ReadResponsePayload

	// Adaptive batching (OPTIMIZATION)
	msgRateCounter atomic.Int64 // Messages sent per second
	lastBatchSize  atomic.Int32 // Last calculated batch size
	lastRateCheck  atomic.Int64 // Last time we checked message rate

	// Goroutine pool for replication workers (OPTIMIZATION)
	replicationPool *ants.Pool

	// Replication batching (OPTIMIZATION)
	batchBuffer map[string]*replicationBatch // Per-target batching
	batchMutex  sync.Mutex                   // Protects batchBuffer

	clusterReady   atomic.Bool  // Cached readiness status
	lastReadyCheck atomic.Int64 // Last time readiness was checked (Unix nano)
}

// NewGossipManager creates a new GossipManager instance with the specified configuration.
//
// This function initializes all subsystems including:
//   - Hybrid logical clock for distributed timestamps
//   - Operation ID generator for request tracking
//   - Goroutine pool for bounded concurrency
//   - Cryptographic signing for message authentication
//
// Parameters:
//   - opts: Configuration options (required)
//   - hashRing: Consistent hash ring for key distribution (required)
//   - network: Network layer for communication (required)
//   - store: Local storage backend (required)
//   - keypair: Cryptographic key pair for signing (optional)
//   - peerPubkeys: Map of peer public keys for verification (optional)
//
// Returns:
//   - *GossipManager: The initialized gossip manager
//   - error: Any initialization error
func NewGossipManager(opts *GossipOptions, hashRing *ConsistentHash, network Network, store KVStore, keypair *crypto.KeyPair, peerPubkeys map[string]crypto.PublicKey) (*GossipManager, error) {
	// Validate required parameters
	if opts == nil {
		return nil, errors.New("gossip options nil")
	}
	if opts.LocalNodeID == "" || opts.LocalAddress == "" {
		return nil, errors.New("LocalNodeID and LocalAddress required")
	}
	if hashRing == nil || network == nil || store == nil {
		return nil, errors.New("hash ring, network, and store cannot be nil")
	}

	// Set defaults for optional parameters
	if opts.FailureTimeout == 0 {
		opts.FailureTimeout = 15 * time.Second // Increased from 5s to handle slow startup
	}
	if opts.SuspectTimeout == 0 {
		opts.SuspectTimeout = 30 * time.Second // Increased from 10s for larger clusters
	}
	if opts.GossipInterval == 0 {
		opts.GossipInterval = 500 * time.Millisecond // Faster gossip for quicker convergence
	}
	if opts.ReplicaCount <= 0 {
		opts.ReplicaCount = 3
	}
	if opts.WriteQuorum <= 0 {
		opts.WriteQuorum = (opts.ReplicaCount / 2) + 1
	}
	if opts.ReadQuorum <= 0 {
		opts.ReadQuorum = (opts.ReplicaCount / 2) + 1
	}
	if opts.MaxReplicators <= 0 {
		opts.MaxReplicators = 4
	}
	if opts.ReplicationTimeout == 0 {
		opts.ReplicationTimeout = 2 * time.Second
	}
	if opts.ReadTimeout == 0 {
		opts.ReadTimeout = 2 * time.Second
	}

	rand.Seed(time.Now().UnixNano())

	gm := &GossipManager{
		localNodeID:        opts.LocalNodeID,
		localAddress:       opts.LocalAddress,
		seedAddrs:          opts.SeedAddrs,
		liveNodes:          make(map[string]*NodeInfo),
		hashRing:           hashRing,
		store:              store,
		network:            network,
		inputCh:            make(chan *GossipMessage, 1024), // Increased from 256 for better burst handling
		stopCh:             make(chan struct{}),
		failureTimeout:     opts.FailureTimeout,
		suspectTimeout:     opts.SuspectTimeout,
		gossipInterval:     opts.GossipInterval,
		replicaCount:       opts.ReplicaCount,
		writeQuorum:        opts.WriteQuorum,
		readQuorum:         opts.ReadQuorum,
		maxReplicators:     opts.MaxReplicators,
		replicationTimeout: opts.ReplicationTimeout,
		readTimeout:        opts.ReadTimeout,
		localVersion:       1,
		hlc:                hlc.NewHLC(opts.LocalNodeID),
		opidGen:            opid.NewGenerator(opts.LocalNodeID),
		keypair:            keypair,
		peerPubkeys:        peerPubkeys,
		disableAuth:        opts.DisableAuth,
		batchBuffer:        make(map[string]*replicationBatch),
	}

	// Add local node to liveNodes and hash ring
	gm.liveNodes[gm.localNodeID] = &NodeInfo{
		NodeId:       gm.localNodeID,
		Address:      gm.localAddress,
		LastActiveTs: timestamppb.Now(),
		State:        NodeState_NODE_STATE_ALIVE,
		Version:      gm.localVersion,
	}

	// CRITICAL: Add local node to hash ring during initialization
	hashRing.Add(gm.localNodeID)

	gm.clusterReady.Store(true)
	gm.lastReadyCheck.Store(time.Now().UnixNano())

	// Initialize adaptive batching
	gm.lastBatchSize.Store(100) // Start with default batch size
	gm.lastRateCheck.Store(time.Now().Unix())

	// Initialize goroutine pool for replication (OPTIMIZATION)
	poolSize := opts.MaxReplicators * 4 // 4x buffer for burst traffic
	if poolSize < 16 {
		poolSize = 16 // Minimum pool size
	}
	pool, err := ants.NewPool(poolSize,
		ants.WithPreAlloc(true),     // Pre-allocate workers
		ants.WithNonblocking(false), // Block when pool is full
		ants.WithPanicHandler(func(err interface{}) {
			logging.Error(fmt.Errorf("panic in replication worker: %v", err), "replication pool panic")
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create replication pool: %w", err)
	}
	gm.replicationPool = pool

	return gm, nil
}

// Start initiates the GossipManager's background processes.
// This includes:
//   - Message processing loop
//   - Periodic gossip broadcasts
//   - Failure detection
//
// This method is non-blocking and returns immediately.
func (gm *GossipManager) Start() {
	gm.wg.Add(1)
	go gm.processLoop()
	logging.Debug("Gossip Manager started", "node", gm.localNodeID)

	// Send initial CONNECT message to announce presence
	var pubKey []byte
	if gm.keypair != nil {
		pubKey = gm.keypair.Pub
	}
	join := &GossipMessage{
		Type:   GossipMessageType_MESSAGE_TYPE_CONNECT,
		Sender: gm.localNodeID,
		Payload: &GossipMessage_ConnectPayload{
			ConnectPayload: &ConnectPayload{
				NodeId:    gm.localNodeID,
				Address:   gm.localAddress,
				Version:   gm.incrementLocalVersion(),
				Hlc:       gm.hlc.Now(),
				PublicKey: pubKey, // Include public key for automatic exchange (may be nil)
			},
		},
	}
	// SAFETY: signMessageCanonical will handle nil keypair gracefully
	if err := gm.signMessageCanonical(join); err != nil && !gm.disableAuth {
		logging.Warn("Failed to sign CONNECT message", "error", err)
	}
	gm.SimulateReceive(join)

	// CRITICAL FIX: Connect to seed nodes on startup
	// This ensures nodes discover each other and build the hash ring
	go gm.connectToSeeds()
}

// Stop gracefully shuts down the GossipManager.
// This blocks until all background goroutines have exited.
func (gm *GossipManager) Stop() {
	close(gm.stopCh)
	gm.wg.Wait()

	// Release goroutine pool
	if gm.replicationPool != nil {
		gm.replicationPool.Release()
	}

	// CRITICAL: Clean up pending reads to prevent memory leaks
	// Close all pending channels and remove from map
	gm.pendingReads.Range(func(key, value interface{}) bool {
		if ch, ok := value.(chan *ReadResponsePayload); ok {
			// SAFETY: Use recover to handle potential panic when closing channel
			func() {
				defer func() {
					if r := recover(); r != nil {
						// Channel already closed, ignore
					}
				}()
				close(ch)
			}()
		}
		gm.pendingReads.Delete(key)
		return true
	})

	logging.Debug("Gossip Manager stopped", "node", gm.localNodeID)
}

// isCriticalMessage checks if a message type requires immediate processing.
// Critical messages bypass the input queue to ensure zero latency and guaranteed delivery.
//
//go:inline
func isCriticalMessage(msgType GossipMessageType) bool {
	switch msgType {
	case CLUSTER_SYNC, CONNECT, PROBE_REQUEST, PROBE_RESPONSE:
		return true
	default:
		return false
	}
}

// SimulateReceive enqueues a message for processing.
// Critical messages are processed directly to bypass the queue.
// This is the main entry point for incoming messages.
//
// Parameters:
//   - msg: The message to process
func (gm *GossipManager) SimulateReceive(msg *GossipMessage) {
	if msg == nil {
		return
	}

	// Track incoming attempt
	gm.messagesTotal.Add(1)

	// Critical messages: direct processing (bypass queue)
	if isCriticalMessage(msg.Type) {
		// Verify signature before processing (no lock needed for verification)
		if ok := gm.verifyMessageCanonical(msg); ok {
			// Process directly - handlers will manage their own locks
			gm.processGossipMessage(msg)
		} else {
			logging.Warn("Dropped critical msg: invalid signature", "sender", msg.Sender, "type", msg.Type)
			gm.messagesDropped.Add(1)
		}
		return
	}

	// Data messages: queue to inputCh
	select {
	case gm.inputCh <- msg:
		// Enqueued successfully
	default:
		logging.Warn("Dropped gossip message: input full", "type", msg.Type, "sender", msg.Sender)
		gm.messagesDropped.Add(1)
	}
}

// MessageStats returns aggregated gossip message statistics.
// total: Total messages received (critical + queued attempts)
// dropped: Messages dropped due to queue saturation or validation failures.
func (gm *GossipManager) MessageStats() (total, dropped int64) {
	return gm.messagesTotal.Load(), gm.messagesDropped.Load()
}

// processLoop is the main event loop that processes messages and runs periodic tasks.
func (gm *GossipManager) processLoop() {
	defer gm.wg.Done()

	// SAFETY: Recover from panics and restart the loop
	defer func() {
		if r := recover(); r != nil {
			logging.Error(fmt.Errorf("panic in processLoop: %v", r),
				"Gossip processLoop panic recovered - restarting")

			// Wait a bit before restarting to avoid rapid panic loops
			time.Sleep(1 * time.Second)

			// Restart the process loop
			gm.wg.Add(1)
			go gm.processLoop()
		}
	}()

	failureTick := time.NewTicker(gm.failureTimeout / 4)
	defer failureTick.Stop()

	gossipTick := time.NewTicker(gm.gossipInterval)
	defer gossipTick.Stop()

	for {
		select {
		case msg := <-gm.inputCh:
			// SAFETY: Recover from individual message processing panics
			func() {
				defer func() {
					if r := recover(); r != nil {
						logging.Error(fmt.Errorf("panic processing message: %v", r),
							"Message processing panic recovered",
							"sender", msg.GetSender(),
							"type", msg.GetType())
					}
				}()

				// Verify message signature
				if ok := gm.verifyMessageCanonical(msg); !ok {
					logging.Warn("Dropping msg: invalid signature", "sender", msg.Sender)
					return
				}
				gm.processGossipMessage(msg)
			}()

		case <-failureTick.C:
			// SAFETY: Protect failure detection
			func() {
				defer func() {
					if r := recover(); r != nil {
						logging.Error(fmt.Errorf("panic in failure detection: %v", r),
							"Failure detection panic recovered")
					}
				}()
				gm.runFailureDetection()
			}()

		case <-gossipTick.C:
			// SAFETY: Protect gossip broadcasting
			func() {
				defer func() {
					if r := recover(); r != nil {
						logging.Error(fmt.Errorf("panic in gossip periodic: %v", r),
							"Gossip periodic panic recovered")
					}
				}()
				gm.gossipPeriodically()
			}()

		case <-gm.stopCh:
			return
		}
	}
}

// Helper methods

// getNode retrieves node info by ID (thread-safe).
func (gm *GossipManager) getNode(id string) (*NodeInfo, bool) {
	gm.mu.RLock()
	defer gm.mu.RUnlock()
	n, ok := gm.liveNodes[id]
	return n, ok
}

// isNodeLocallyAlive checks if a node is alive locally.
func (gm *GossipManager) isNodeLocallyAlive(nodeID string) bool {
	n, ok := gm.getNode(nodeID)
	return ok && n.State == NodeState_NODE_STATE_ALIVE
}

// incrementLocalVersion atomically increments and returns the local version counter.
func (gm *GossipManager) incrementLocalVersion() int64 {
	return atomic.AddInt64(&gm.localVersion, 1)
}

// generateOpID creates a unique operation ID for request tracking.
func (gm *GossipManager) generateOpID() string {
	return gm.opidGen.Generate()
}

// hasMultipleNodes checks if cluster has multiple nodes.
func (gm *GossipManager) hasMultipleNodes() bool {
	gm.mu.RLock()
	defer gm.mu.RUnlock()
	return len(gm.liveNodes) > 1
}

// getRandomPeerID returns a random peer ID, excluding the specified ID.
// OPTIMIZATION: Fast path for small clusters.
func (gm *GossipManager) getRandomPeerID(exclude string) string {
	gm.mu.RLock()
	defer gm.mu.RUnlock()

	// Fast path for small clusters
	if len(gm.liveNodes) <= 2 {
		for id := range gm.liveNodes {
			if id != gm.localNodeID && id != exclude {
				return id
			}
		}
		return ""
	}

	// Pre-allocate with exact capacity
	ids := make([]string, 0, len(gm.liveNodes))
	for id := range gm.liveNodes {
		if id == gm.localNodeID || id == exclude {
			continue
		}
		ids = append(ids, id)
	}

	if len(ids) == 0 {
		return ""
	}

	return ids[rand.Intn(len(ids))]
}

// connectToSeeds initiates connections to all seed nodes on startup.
//
// This is critical for cluster formation and node discovery.
// The function sends CONNECT messages to all seed addresses to announce
// this node's presence and trigger mutual discovery. It retries periodically
// until at least one seed responds or the cluster is formed.
func (gm *GossipManager) connectToSeeds() {
	if len(gm.seedAddrs) == 0 {
		logging.Debug("No seed nodes configured - running as standalone or first node")
		return
	}

	// Prepare CONNECT message
	var pubKey []byte
	if gm.keypair != nil {
		pubKey = gm.keypair.Pub
	}
	connectMsg := &GossipMessage{
		Type:   GossipMessageType_MESSAGE_TYPE_CONNECT,
		Sender: gm.localNodeID,
		Payload: &GossipMessage_ConnectPayload{
			ConnectPayload: &ConnectPayload{
				NodeId:    gm.localNodeID,
				Address:   gm.localAddress,
				Version:   gm.incrementLocalVersion(),
				Hlc:       gm.hlc.Now(),
				PublicKey: pubKey, // Include public key for automatic exchange (may be nil)
			},
		},
	}
	// SAFETY: signMessageCanonical will handle nil keypair gracefully
	if err := gm.signMessageCanonical(connectMsg); err != nil && !gm.disableAuth {
		logging.Warn("Failed to sign CONNECT message to seed", "error", err)
	}

	logging.Debug("Connecting to seed nodes", "seeds", len(gm.seedAddrs))

	// Send initial CONNECT to all seeds immediately
	var wg sync.WaitGroup
	for _, seedAddr := range gm.seedAddrs {
		// Skip if seed address is our own address
		if seedAddr == gm.localAddress {
			continue
		}

		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			// Use shorter timeout for faster failure detection
			if err := gm.network.SendWithTimeout(addr, connectMsg, 1*time.Second); err != nil {
				logging.Debug("Failed to connect to seed", "seed", addr, "error", err)
			} else {
				logging.Debug("Sent CONNECT to seed", "seed", addr)
			}
		}(seedAddr)
	}
	wg.Wait()

	maxRetries := 5                     // Reduced from 10
	retryDelay := 50 * time.Millisecond // Reduced from 100ms

	for attempt := 0; attempt < maxRetries; attempt++ {
		// Check if we've discovered any peers
		gm.mu.RLock()
		peerCount := len(gm.liveNodes) - 1 // Exclude self
		gm.mu.RUnlock()

		if peerCount > 0 {
			logging.Debug("Successfully connected to cluster", "peers", peerCount)
			return
		}

		// Wait briefly before retry
		time.Sleep(retryDelay)
		retryDelay = retryDelay * 2
		if retryDelay > 1*time.Second {
			retryDelay = 1 * time.Second // Cap at 1s instead of 5s
		}

		// Retry failed connections
		for _, seedAddr := range gm.seedAddrs {
			if seedAddr == gm.localAddress {
				continue
			}
			// Quick retry
			gm.network.SendWithTimeout(seedAddr, connectMsg, 500*time.Millisecond)
		}
	}

	// Check final peer count
	gm.mu.RLock()
	peerCount := len(gm.liveNodes) - 1
	gm.mu.RUnlock()

	if peerCount == 0 {
		logging.Warn("Failed to connect to any seed nodes after retries - running as standalone node")
	} else {
		logging.Debug("Connected to cluster", "peers", peerCount)
	}
}

// IsReady performs a fast readiness check using cached atomic status.
//
// Returns:
//   - bool: true if cluster is ready, false otherwise
func (gm *GossipManager) IsReady() bool {
	// OPTIMIZATION: Use cached atomic status (no lock needed)
	// Cache is updated periodically by GetReplicaStatus()
	const cacheTTL = 100 * time.Millisecond
	now := time.Now().UnixNano()
	lastCheck := gm.lastReadyCheck.Load()

	// If cache is fresh, use it
	if now-lastCheck < int64(cacheTTL) {
		return gm.clusterReady.Load()
	}

	// Cache expired - do quick check
	gm.mu.RLock()
	healthyNodes := 0
	for _, node := range gm.liveNodes {
		if node.State == NodeState_NODE_STATE_ALIVE {
			healthyNodes++
		}
	}
	ready := healthyNodes > 0
	gm.mu.RUnlock()

	// Update cache
	gm.clusterReady.Store(ready)
	gm.lastReadyCheck.Store(now)

	return ready
}

// GetReplicaStatus returns the current state of the replica system.
// This provides visibility into cluster health and formation status.
//
// Returns:
//   - ReplicaStatus: Current cluster state
func (gm *GossipManager) GetReplicaStatus() ReplicaStatus {
	gm.mu.RLock()
	clusterSize := len(gm.liveNodes)
	healthyNodes := 0

	// Count healthy (ALIVE) nodes
	for _, node := range gm.liveNodes {
		if node.State == NodeState_NODE_STATE_ALIVE {
			healthyNodes++
		}
	}

	// Determine effective write quorum
	effectiveQuorum := gm.writeQuorum
	if healthyNodes < gm.writeQuorum {
		effectiveQuorum = (healthyNodes / 2) + 1
		if effectiveQuorum < 1 {
			effectiveQuorum = 1
		}
	}

	// System is ready if at least one node is available
	ready := healthyNodes > 0
	gm.mu.RUnlock()

	gm.clusterReady.Store(ready)
	gm.lastReadyCheck.Store(time.Now().UnixNano())

	// Check public key status (needs lock)
	gm.mu.RLock()
	pubkeyCount, peerCount, pubkeysReady := gm.HasPublicKeysForPeers()
	gm.mu.RUnlock()

	// Only log if debug is enabled
	if logging.Log.IsDebugEnabled() {
		ringMembers := gm.hashRing.Members()
		logging.Debug("Hash ring members", "count", len(ringMembers), "members", ringMembers)
	}

	return ReplicaStatus{
		Ready:           ready,
		ClusterSize:     clusterSize,
		HealthyNodes:    healthyNodes,
		ReplicaFactor:   gm.replicaCount,
		EffectiveQuorum: effectiveQuorum,
		LocalNodeID:     gm.localNodeID,
		PubkeysReady:    pubkeysReady,
		PubkeyCount:     pubkeyCount,
		PeerCount:       peerCount,
	}
}

// HasPublicKeysForPeers checks if public keys are obtained for all peer nodes.
// Returns: (keys obtained, total peers, all ready)
func (gm *GossipManager) HasPublicKeysForPeers() (int, int, bool) {
	// If authentication is disabled, always return ready
	if gm.disableAuth {
		return 0, 0, true
	}

	gm.mu.RLock()
	defer gm.mu.RUnlock()

	peerCount := 0
	pubkeyCount := 0

	// Count peer nodes and available public keys
	for nodeID, node := range gm.liveNodes {
		// Skip self
		if nodeID == gm.localNodeID {
			continue
		}

		// Only count alive nodes
		if node.State != NodeState_NODE_STATE_ALIVE {
			continue
		}

		peerCount++

		// Check if we have this node's public key
		if _, ok := gm.peerPubkeys[nodeID]; ok {
			pubkeyCount++
		}
	}

	// System is ready if no peers or all peer keys obtained
	allReady := (peerCount == 0) || (pubkeyCount >= peerCount)
	return pubkeyCount, peerCount, allReady
}

// ReplicaStatus represents the current state of the replica system.
// Defined here to avoid circular import with main package.
type ReplicaStatus struct {
	Ready           bool
	ClusterSize     int
	HealthyNodes    int
	ReplicaFactor   int
	EffectiveQuorum int
	LocalNodeID     string
	// New fields for public key status
	PubkeysReady bool // True if all peer public keys are obtained
	PubkeyCount  int  // Number of peer public keys obtained
	PeerCount    int  // Total number of peer nodes
}
