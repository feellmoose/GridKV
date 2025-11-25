// Package gossip implements the SWIM-based gossip protocol for GridKV.
//
// This package provides cluster membership management, failure detection,
// and distributed data replication using a gossip protocol.
//
// Key Components:
//   - GossipManager: Core gossip protocol coordinator
//   - ConsistentHash: Data distribution ring with virtual nodes
//   - Replication: Batched eventual-consistency replication pipeline
//   - Failure Detection: SWIM-based node health monitoring
//   - Message Handling: Inbound message processing with priority queues
//
// Architecture:
//   - Membership: SWIM protocol for cluster state management
//   - Replication: High-throughput batching pipeline per target
//   - Consistency: Eventual consistency with read-repair
//   - Security: Ed25519 message signing and verification
//
// All public APIs are safe for concurrent access.
package gossip

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"runtime/debug"
	"unsafe"

	"github.com/feellmoose/gridkv/internal/metrics"
	"github.com/feellmoose/gridkv/internal/storage"
	"github.com/feellmoose/gridkv/internal/utils/crypto"
	"github.com/feellmoose/gridkv/internal/utils/hlc"
	"github.com/feellmoose/gridkv/internal/utils/logging"
	"github.com/feellmoose/gridkv/internal/utils/opid"
	"github.com/feellmoose/gridkv/internal/utils/workerpool"
)

// Constants for node states and message types for cleaner code
const (
	Alive   NodeState = NodeState_NODE_STATE_ALIVE   // Node is healthy and responsive
	Suspect NodeState = NodeState_NODE_STATE_SUSPECT // Node failed heartbeat and is suspected dead
	Dead    NodeState = NodeState_NODE_STATE_DEAD    // Node confirmed dead and removed from the ring
)

const (
	CACHE_SYNC          GossipMessageType = GossipMessageType_MESSAGE_TYPE_CACHE_SYNC
	CLUSTER_SYNC        GossipMessageType = GossipMessageType_MESSAGE_TYPE_CLUSTER_SYNC
	CONNECT             GossipMessageType = GossipMessageType_MESSAGE_TYPE_CONNECT
	PROBE_REQUEST       GossipMessageType = GossipMessageType_MESSAGE_TYPE_PROBE_REQUEST
	PROBE_RESPONSE      GossipMessageType = GossipMessageType_MESSAGE_TYPE_PROBE_RESPONSE
	FULL_SYNC_REQUEST   GossipMessageType = GossipMessageType_MESSAGE_TYPE_FULL_SYNC_REQUEST
	FULL_SYNC_RESPONSE  GossipMessageType = GossipMessageType_MESSAGE_TYPE_FULL_SYNC_RESPONSE
	READ_REQUEST        GossipMessageType = GossipMessageType_MESSAGE_TYPE_READ_REQUEST
	READ_RESPONSE       GossipMessageType = GossipMessageType_MESSAGE_TYPE_READ_RESPONSE
	BATCH_READ_REQUEST  GossipMessageType = GossipMessageType_MESSAGE_TYPE_BATCH_READ_REQUEST
	BATCH_READ_RESPONSE GossipMessageType = GossipMessageType_MESSAGE_TYPE_BATCH_READ_RESPONSE
)

const maxPendingReads = 32768 // Increased from 16384 for ultra-high concurrency

// pendingReadEntry stores pending read metadata for timeout cleanup
type pendingReadEntry struct {
	ch        chan *ReadResponsePayload
	createdAt time.Time
}

// GossipOptions contains configuration for the GossipManager.
type GossipOptions struct {
	LocalNodeID        string        // Unique identifier for this node
	LocalAddress       string        // Network address for this node (host:port)
	SeedAddrs          []string      // Bootstrap nodes for cluster formation
	FailureTimeout     time.Duration // Timeout before marking node as suspect
	SuspectTimeout     time.Duration // Timeout before marking suspect node as dead
	GossipInterval     time.Duration // Interval for periodic gossip broadcasts
	ReplicaCount       int           // N: Number of replicas for each key (eventual consistency)
	MaxReplicators     int           // Max concurrent replication goroutines
	ReplicationTimeout time.Duration // Timeout for replication operations
	ReadTimeout        time.Duration // Timeout for read operations
	DisableAuth        bool          // Disable message authentication (use with caution)
	StartupGracePeriod time.Duration // Grace period before marking nodes suspect (for startup/tests)
	// Gossip payload chunking to avoid oversized network messages
	ClusterSyncChunkSize   int // Max nodes per cluster sync message (default: 64)
	CacheSyncOpsPerMessage int // Max operations per CACHE_SYNC message (default: 512)
	// Rate limits (ops/sec per node). 0 => defaults (50k)
	MigrateRateLimitPerSec    int64
	ReadRepairRateLimitPerSec int64
	// Short-TTL hot read cache. 0 => disabled
	HotReadCacheTTL time.Duration
	// Metrics integration (optional)
	Metrics *metrics.GridKVMetrics

	// Advanced inbound scheduling options
	InboundNUMANodes int
}

// GossipManager is the core component that manages cluster membership, failure detection,
// and data replication using a gossip protocol.
//
// Key responsibilities:
//   - Maintain cluster membership using SWIM-based failure detection
//   - Coordinate distributed reads/writes with eventual consistency
//   - Replicate data across nodes using consistent hashing
//   - Synchronize state via incremental and full sync
//   - Sign and verify messages for security
type GossipManager struct {
	mu           sync.RWMutex
	localNodeID  string
	localAddress string
	seedAddrs    []string             // Bootstrap seed addresses
	liveNodes    map[string]*NodeInfo // Active cluster members
	liveNodesLF  *LockFreeNodeMap     // Lock-free node map

	hashRing      *ConsistentHash // Consistent hash ring for data distribution
	hashRingCache *HashRingCache  // Hash ring result cache
	store         KVStore         // Local storage backend
	network       Network         // Network layer for communication

	inputCh chan *GossipMessage // Incoming message queue
	stopCh  chan struct{}       // Shutdown signal
	wg      sync.WaitGroup      // Wait group for graceful shutdown

	// Message statistics (atomic counters for diagnostics)
	messagesTotal   atomic.Int64
	messagesDropped atomic.Int64

	// Configuration
	failureTimeout     time.Duration
	suspectTimeout     time.Duration
	gossipInterval     time.Duration
	startupGracePeriod time.Duration

	replicaCount         int
	maxReplicators       int
	replicationTimeout   time.Duration
	readTimeout          time.Duration
	clusterSyncChunkSize int
	options              *GossipOptions
	metrics              *metrics.GridKVMetrics

	// Versioning and timing
	localVersion int64           // Atomic counter for local version
	hlc          *hlc.HLC        // Hybrid logical clock
	opidGen      *opid.Generator // Operation ID generator

	// Cryptography
	keypair     *crypto.KeyPair             // Local key pair for signing
	peerPubkeys map[string]crypto.PublicKey // Peer public keys for verification
	disableAuth bool                        // Disable message authentication

	// Read operation tracking - stores pending read metadata (channel + timestamp) for timeout cleanup
	pendingReads      sync.Map // map[requestId]*pendingReadEntry
	pendingReadsCount atomic.Int64

	// Per-target replication pipelines (eventual consistency high-throughput)
	pipelineMu          sync.Mutex
	pipelines           map[string]*targetPipeline
	pipelineDropCounter atomic.Int64

	msgRateCounter atomic.Int64 // Messages sent per second
	lastBatchSize  atomic.Int32 // Last calculated batch size
	lastRateCheck  atomic.Int64 // Last time we checked message rate

	replicationPool     *workerpool.Pool
	inboundPool         *workerpool.Pool
	inboundPriorityPool *workerpool.PriorityPool
	inboundPoolSize     int // Current inbound pool size for dynamic scaling
	inboundNUMANodes    int
	inboundTaskCounter  atomic.Uint64

	// Pool saturation tracking for metrics
	inboundPoolSaturations atomic.Int64 // Count of times inbound pool was at capacity

	batchBuffer map[string]*replicationBatch // Per-target batching (keyed by target addr)
	batchMutex  sync.Mutex                   // Protects batchBuffer

	gradualMigration *gradualMigrationManager // Manages gradual data migration
	batchCleanup     *batchCleanupManager     // Manages periodic cleanup of batch buffers
	batchManager     *BatchManager            // Adaptive batching policies

	// Rate limiting for migration and read-repair
	migrateLimiter    tokenBucket
	readRepairLimiter tokenBucket

	clusterReady   atomic.Bool  // Cached readiness status
	lastReadyCheck atomic.Int64 // Last time readiness was checked (Unix nano)

	// Connection state tracking to prevent goroutine leaks
	connectingNodes    sync.Map      // map[string]*connectingState - tracks ongoing connection attempts
	connectRateLimiter chan struct{} // Rate limiter for connection attempts

	// Unified event loop for all periodic tasks
	eventLoop      *UnifiedEventLoop
	eventScheduler *EventScheduler

	// Hot read cache (short TTL, best-effort)
	hotCacheTTL time.Duration
	hotCache    sync.Map // key -> hotCacheEntry

	// Read batch manager for high-throughput reads
	readBatchManager *ReadBatchManager

	// Worker pool metrics recorders
	replicationPoolMetrics *metrics.WorkerPoolRecorder
	inboundPoolMetrics     *metrics.WorkerPoolRecorder

	// Adaptive pool resizers
	replicationPoolResizer *adaptivePoolResizer
	inboundPoolResizer     *adaptivePoolResizer

	// Performance optimizations
	useBinaryProtocol bool

	// Rate limiter for gossip messages to prevent network congestion
	gossipRateLimiter *gossipRateLimiter
}

// gossipRateLimiter limits the rate of non-critical gossip messages
type gossipRateLimiter struct {
	mu          sync.Mutex
	lastSend    time.Time
	minInterval time.Duration
}

// tokenBucket is a simple token bucket limiter
type tokenBucket struct {
	capacity int64
	tokens   atomic.Int64
	refill   int64         // tokens per tick
	interval time.Duration // tick interval
	once     sync.Once
	stopCh   chan struct{}
	ticker   *time.Ticker
	paused   atomic.Bool
}

func (tb *tokenBucket) start() {
	tb.once.Do(func() {
		tb.stopCh = make(chan struct{})
		tb.ticker = time.NewTicker(tb.interval)
		go func() {
			defer tb.ticker.Stop()
			for {
				select {
				case <-tb.stopCh:
					return
				case <-tb.ticker.C:
					cur := tb.tokens.Add(tb.refill)
					if cur > tb.capacity {
						tb.tokens.Store(tb.capacity)
					}
				}
			}
		}()
	})
}

func (tb *tokenBucket) stop() {
	if tb.stopCh != nil {
		close(tb.stopCh)
	}
	if tb.ticker != nil {
		tb.ticker.Stop()
	}
}

func (tb *tokenBucket) Allow(n int64) bool {
	if tb.paused.Load() {
		return false
	}
	for {
		cur := tb.tokens.Load()
		if cur < n {
			return false
		}
		if tb.tokens.CompareAndSwap(cur, cur-n) {
			return true
		}
	}
}

func (tb *tokenBucket) pause() {
	tb.paused.Store(true)
}

// hotCacheEntry stores a cached item with expiry
type hotCacheEntry struct {
	item     *storage.StoredItem
	expireAt time.Time
}

// connectingState tracks an ongoing connection attempt
type connectingState struct {
	mu          sync.Mutex
	lastAttempt time.Time
	attempts    int
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
		opts.FailureTimeout = 30 * time.Second
	}
	if opts.SuspectTimeout == 0 {
		opts.SuspectTimeout = 60 * time.Second
	}
	if opts.GossipInterval == 0 {
		// Optimized for fast convergence: 1s base interval
		// Balances convergence speed with network overhead
		opts.GossipInterval = 1000 * time.Millisecond
	}
	if opts.ReplicaCount <= 0 {
		opts.ReplicaCount = 3
	}
	// No Quorum parameters needed
	if opts.MaxReplicators <= 0 {
		opts.MaxReplicators = 16
	}
	if opts.ReplicationTimeout == 0 {
		// Increased timeout to reduce I/O timeout errors under high load
		// High concurrency scenarios need more time for TCP writes to complete
		opts.ReplicationTimeout = 2 * time.Second
	}
	if opts.ReadTimeout == 0 {
		opts.ReadTimeout = 3 * time.Second // Increased for better reliability with batch reads
	}
	if opts.StartupGracePeriod == 0 {
		opts.StartupGracePeriod = 60 * time.Second
	}
	if opts.ClusterSyncChunkSize <= 0 {
		opts.ClusterSyncChunkSize = 64
	}
	if opts.CacheSyncOpsPerMessage <= 0 {
		opts.CacheSyncOpsPerMessage = 512
	}

	rand.Seed(time.Now().UnixNano())

	gm := &GossipManager{
		localNodeID:          opts.LocalNodeID,
		localAddress:         opts.LocalAddress,
		seedAddrs:            opts.SeedAddrs,
		liveNodes:            make(map[string]*NodeInfo),
		liveNodesLF:          NewLockFreeNodeMap(),
		hashRing:             hashRing,
		hashRingCache:        NewHashRingCache(hashRing, 1*time.Second, 10000),
		store:                store,
		network:              network,
		inputCh:              make(chan *GossipMessage, 131072), // 128K buffer for ultra-high concurrency
		stopCh:               make(chan struct{}),
		failureTimeout:       opts.FailureTimeout,
		suspectTimeout:       opts.SuspectTimeout,
		gossipInterval:       opts.GossipInterval,
		replicaCount:         opts.ReplicaCount,
		maxReplicators:       opts.MaxReplicators,
		replicationTimeout:   opts.ReplicationTimeout,
		readTimeout:          opts.ReadTimeout,
		clusterSyncChunkSize: opts.ClusterSyncChunkSize,
		metrics:              opts.Metrics,
		options:              opts,
		startupGracePeriod:   opts.StartupGracePeriod,
		localVersion:         1,
		hlc:                  hlc.NewHLC(opts.LocalNodeID),
		opidGen:              opid.NewGenerator(opts.LocalNodeID),
		keypair:              keypair,
		peerPubkeys:          peerPubkeys,
		disableAuth:          opts.DisableAuth,
		batchBuffer:          make(map[string]*replicationBatch),
		gradualMigration:     newGradualMigrationManager(nil), // Will be set after gm is created
		batchCleanup:         newBatchCleanupManager(nil),     // Will be set after gm is created
		useBinaryProtocol:    true,                            // Enable binary protocol by default
		gossipRateLimiter: &gossipRateLimiter{
			minInterval: 50 * time.Millisecond, // Minimum interval between gossip messages
		},
	}

	gm.batchManager = newBatchManager()
	gm.batchManager.UpdateClusterSize(1)

	// Set self-reference for managers
	gm.gradualMigration.gm = gm
	gm.batchCleanup.gm = gm

	// Configure network to use binary protocol
	gm.network.SetUseBinaryProtocol(gm.useBinaryProtocol)

	// Set metrics for network byte tracking
	if networkImpl, ok := gm.network.(*NetworkImpl); ok && gm.metrics != nil {
		networkImpl.SetMetrics(gm.metrics)
	}

	// Add local node to liveNodes and hash ring
	localNode := &NodeInfo{
		NodeId:       gm.localNodeID,
		Address:      gm.localAddress,
		LastActiveTs: time.Now(),
		State:        NodeState_NODE_STATE_ALIVE,
		Version:      gm.localVersion,
	}
	gm.liveNodes[gm.localNodeID] = localNode
	gm.liveNodesLF.Update(gm.localNodeID, localNode)

	// Add local node to hash ring during initialization
	hashRing.Add(gm.localNodeID)

	gm.clusterReady.Store(true)
	gm.lastReadyCheck.Store(time.Now().UnixNano())

	// Initialize adaptive batching
	gm.lastBatchSize.Store(100) // Start with default batch size
	gm.lastRateCheck.Store(time.Now().Unix())

	// Optimize pool size: start small, let adaptive resizer scale up
	// Reduced initial size to prevent excessive goroutine creation at startup
	// Base size: MaxReplicators * 4 (reduced from 8)
	poolSize := opts.MaxReplicators * 4
	if poolSize < 32 { // Reduced from 128
		poolSize = 32
	}
	if poolSize > 128 { // Reduced from 512 - adaptive resizer will scale up as needed
		poolSize = 128
	}
	// Removed maxBlocking - using non-blocking mode to prevent goroutine leaks
	pool, err := workerpool.New(workerpool.Options{
		Name:        "replication",
		MaxWorkers:  poolSize,
		QueueSize:   poolSize,
		NonBlocking: true,
		PanicHandler: func(err interface{}) {
			logging.Error(fmt.Errorf("panic in replication worker: %v", err), "replication pool panic")
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create replication pool: %w", err)
	}
	gm.replicationPool = pool

	// Dynamic inbound pool sizing: optimized for high-throughput scenarios
	// Balanced approach: increase size but not too aggressively to avoid goroutine explosion
	// Base size: MaxReplicators * 48 (balanced between 32 and 64)
	inboundSize := opts.MaxReplicators * 48
	if inboundSize < 384 { // Balanced between 256 and 512
		inboundSize = 384
	}
	// Start with 1.5x base size for better initial concurrency
	initialSize := inboundSize + inboundSize/2 // 1.5x
	if initialSize < 768 {                     // Balanced between 512 and 1024
		initialSize = 768
	}
	const maxInitialSize = 12288 // Balanced between 8192 and 16384
	if initialSize > maxInitialSize {
		initialSize = maxInitialSize
	}

	gm.inboundPoolSize = initialSize // Store initial size for dynamic adjustment
	// Removed inboundMaxBlocking - using non-blocking mode to prevent goroutine leaks
	// Increased queue size to 2x workers for better buffering
	inboundPool, err := workerpool.New(workerpool.Options{
		Name:        "inbound",
		MaxWorkers:  initialSize,
		QueueSize:   initialSize * 2, // 2x queue size for better buffering
		NonBlocking: true,
		PanicHandler: func(err interface{}) {
			logPanicWithStack("Panic in inbound worker pool", err)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create inbound pool: %w", err)
	}
	gm.inboundPool = inboundPool
	gm.inboundNUMANodes = opts.InboundNUMANodes
	if gm.inboundNUMANodes <= 0 {
		gm.inboundNUMANodes = runtime.NumCPU() / 4
		if gm.inboundNUMANodes < 1 {
			gm.inboundNUMANodes = 1
		}
	}
	priorityPool, err := workerpool.NewPriorityPool(workerpool.PriorityPoolOptions{
		BasePool:           inboundPool,
		NUMANodes:          gm.inboundNUMANodes,
		EnableWorkStealing: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create inbound priority pool: %w", err)
	}
	gm.inboundPriorityPool = priorityPool

	// Initialize adaptive pool resizers with larger maxSize to allow scaling
	// Start small, scale up aggressively when needed
	gm.replicationPoolResizer = newAdaptivePoolResizer(
		gm.replicationPool,
		"replication",
		poolSize,
		poolSize/2,  // min: 50% of initial (more conservative)
		poolSize*20, // max: 20x initial (allow aggressive scaling when needed)
	)
	gm.inboundPoolResizer = newAdaptivePoolResizer(
		gm.inboundPool,
		"inbound",
		initialSize,
		initialSize/2,  // min: 50% of initial (more conservative)
		initialSize*20, // max: 20x initial (allow aggressive scaling when needed)
	)

	gm.initWorkerPoolMetrics()

	// Start goroutine monitor for leak detection
	globalGoroutineMonitor.SetBaseline(runtime.NumGoroutine())
	globalGoroutineMonitor.Start()

	// Initialize connection rate limiter - allow max 5 concurrent connection attempts
	// Reduced from 10 to prevent excessive goroutine creation under high load
	// This prevents connection storms and goroutine leaks
	gm.connectRateLimiter = make(chan struct{}, 20)

	// Initialize unified event loop for all periodic tasks
	gm.eventLoop = NewUnifiedEventLoop(8192)
	gm.eventScheduler = NewEventScheduler(gm.eventLoop)
	gm.eventLoop.Start()

	// Register periodic event handlers
	gm.registerEventHandlers()

	// Initialize rate limiters (defaults: 50k ops/s per node for both)
	migrateCap := opts.MigrateRateLimitPerSec
	if migrateCap <= 0 {
		migrateCap = 50000
	}
	gm.migrateLimiter = tokenBucket{capacity: migrateCap, refill: migrateCap, interval: time.Second}
	gm.migrateLimiter.tokens.Store(gm.migrateLimiter.capacity)
	gm.migrateLimiter.start()
	readRepairCap := opts.ReadRepairRateLimitPerSec
	if readRepairCap <= 0 {
		readRepairCap = 50000
	}
	gm.readRepairLimiter = tokenBucket{capacity: readRepairCap, refill: readRepairCap, interval: time.Second}
	gm.readRepairLimiter.tokens.Store(gm.readRepairLimiter.capacity)
	gm.readRepairLimiter.start()

	// Hot read cache TTL
	if opts.HotReadCacheTTL > 0 {
		gm.hotCacheTTL = opts.HotReadCacheTTL
	} else {
		// Default hot cache TTL: 1 second
		gm.hotCacheTTL = 1 * time.Second
	}

	// Initialize read batch manager
	// OPTIMIZED for 1M+ QPS: Larger batches, balanced window for maximum throughput
	// Increased batch size to 200 for better batching efficiency
	// Increased window to 10ms for better batching while maintaining low latency
	readBatchSize := 200                     // Increased from 50 for better throughput
	readBatchWindow := 10 * time.Millisecond // Increased from 5ms for better batching
	enableAdaptive := true                   // Enable adaptive tuning by default
	gm.readBatchManager = NewReadBatchManager(
		gm.sendBatchReadRequest,
		gm.generateOpID,
		readBatchSize,
		readBatchWindow,
		gm.metrics,
		enableAdaptive,
	)

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
	// signMessageCanonical will handle nil keypair gracefully
	if err := gm.signMessageCanonical(join); err != nil && !gm.disableAuth {
		logging.Warn("Failed to sign CONNECT message", "error", err)
	}
	gm.SimulateReceive(join)

	// Connect to seed nodes on startup with staggered backoff
	// This ensures nodes discover each other and build the hash ring
	// Staggered backoff prevents connection storms in large clusters (50-100 nodes)
	go gm.connectToSeedsWithBackoff()

	if gm.batchCleanup != nil {
		gm.batchCleanup.start()
	}
}

// Stop gracefully shuts down the GossipManager.
// This blocks until all background goroutines have exited.
func (gm *GossipManager) Stop() {
	close(gm.stopCh)

	// Wait for main loop to stop with timeout
	done := make(chan struct{})
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()

	go func() {
		gm.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Main loop stopped
	case <-timeout.C:
		// Timeout - continue anyway
		logging.Warn("Gossip manager stop timeout, continuing cleanup")
	}

	// Stop all components that use network BEFORE closing network
	// This prevents "connection pool closed" errors during shutdown
	// Use minimal waits with timeouts to speed up shutdown

	// Stop read batch manager
	if gm.readBatchManager != nil {
		gm.readBatchManager.Stop()
	}

	// Stop event scheduler and loop - may trigger network operations
	if gm.eventScheduler != nil {
		gm.eventScheduler.Stop()
		time.Sleep(10 * time.Millisecond)
	}

	if gm.eventLoop != nil {
		gm.eventLoop.Stop()
		time.Sleep(10 * time.Millisecond)
	}

	// Release worker pools with minimal wait - pools should drain quickly
	if gm.inboundPriorityPool != nil {
		gm.inboundPriorityPool.Release()
		time.Sleep(10 * time.Millisecond)
	}
	if gm.inboundPool != nil {
		gm.inboundPool.Release()
		time.Sleep(20 * time.Millisecond)
	}

	if gm.replicationPool != nil {
		gm.replicationPool.Release()
		time.Sleep(20 * time.Millisecond)
	}

	// Now safe to stop network - all components that use it are stopped
	if gm.network != nil {
		gm.network.Stop()
		// Increased wait time to ensure network goroutines exit
		time.Sleep(100 * time.Millisecond)
	}

	gm.FlushAllPipelines()
	// Stop all pipelines to prevent goroutine leaks
	gm.StopAllPipelines()
	// Additional wait to ensure all pipeline goroutines have exited
	time.Sleep(200 * time.Millisecond)

	// Flush and stop all batch timers to prevent goroutine leaks
	gm.flushAllBatches()

	if gm.batchCleanup != nil {
		gm.batchCleanup.stop()
	}

	// Stop gradual migration manager
	if gm.gradualMigration != nil {
		gm.gradualMigration.stop()
		time.Sleep(10 * time.Millisecond)
	}

	// Stop token bucket limiters
	gm.migrateLimiter.stop()
	gm.readRepairLimiter.stop()

	// Clean up pending reads to prevent memory leaks
	// Drain and recycle all pending channels
	gm.pendingReads.Range(func(key, value interface{}) bool {
		if entry, ok := value.(*pendingReadEntry); ok && entry != nil && entry.ch != nil {
			// Try to drain channel before recycling to prevent goroutine leaks
			func(ch chan *ReadResponsePayload) {
				defer func() {
					if r := recover(); r != nil {
						// Channel already closed, ignore
					}
				}()
				// Drain any pending responses
				select {
				case <-ch:
					// Drained response
				default:
					// Channel empty
				}
				// Recycle channel instead of closing (channel pool manages lifecycle)
				putReadResponseChannel(ch)
			}(entry.ch)
		}

		if strKey, ok := key.(string); ok {
			gm.removePendingRead(strKey)
		} else {
			gm.pendingReads.Delete(key)
			if value != nil {
				if entry, ok := value.(*pendingReadEntry); ok && entry.ch != nil {
					putReadResponseChannel(entry.ch)
				}
			}
		}
		return true
	})

	logging.Debug("Gossip Manager stopped", "node", gm.localNodeID)
}

// BeginShutdown prepares replication components for node shutdown.
func (gm *GossipManager) BeginShutdown() {
	if gm == nil {
		return
	}
	gm.FlushAllPipelines()
	if gm.gradualMigration != nil {
		gm.gradualMigration.startShutdownMigration()
	}
	gm.migrateLimiter.pause()
	gm.readRepairLimiter.pause()
}

// WaitForDrain blocks until replication pipelines and shutdown migrations finish or the context expires.
func (gm *GossipManager) WaitForDrain(ctx context.Context) error {
	if gm == nil {
		return nil
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		pending := gm.pendingReplicationOps() + gm.pendingShutdownMigrations()
		if gm.metrics != nil {
			gm.metrics.SetShutdownPendingShards(pending)
		}
		if pending == 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (gm *GossipManager) pendingReplicationOps() int64 {
	var total int64

	gm.pipelineMu.Lock()
	for _, pipeline := range gm.pipelines {
		if pipeline != nil && pipeline.ch != nil {
			total += int64(len(pipeline.ch))
		}
	}
	gm.pipelineMu.Unlock()

	unifiedPipelines.Range(func(_, value interface{}) bool {
		up, ok := value.(*UnifiedPipeline)
		if !ok || up == nil {
			return true
		}
		if up.manager == gm {
			total += up.Pending()
		}
		return true
	})

	return total
}

func (gm *GossipManager) pendingShutdownMigrations() int64 {
	if gm.gradualMigration == nil {
		return 0
	}
	return gm.gradualMigration.pendingShutdownKeys()
}

// Gossip and sync functions merged from sync.go for file consolidation

var (
	nodeInfoSlicePool = sync.Pool{
		New: func() interface{} {
			slice := make([]*NodeInfo, 0, 32)
			return &slice
		},
	}
)

//go:inline
func (gm *GossipManager) gossipPeriodically() {
	// Add nil checks to prevent panic
	if gm == nil {
		return
	}

	gm.mu.RLock()
	peerCount := len(gm.liveNodes) - 1
	gm.mu.RUnlock()

	if peerCount == 0 {
		return
	}

	gm.mu.RLock()
	membersPtr := nodeInfoSlicePool.Get().(*[]*NodeInfo)
	members := (*membersPtr)[:0]

	for _, n := range gm.liveNodes {
		if n == nil {
			continue
		}
		members = append(members, &NodeInfo{
			NodeId:       n.NodeId,
			Address:      n.Address,
			LastActiveTs: n.LastActiveTs,
			State:        n.State,
			Version:      n.Version,
		})
	}
	gm.mu.RUnlock()

	chunks := chunkClusterSyncNodes(members, gm.getClusterSyncChunkSize())

	gossipTargets := gm.getGossipTargets(peerCount)

	if len(gossipTargets) == 0 {
		nodeInfoSlicePool.Put(membersPtr)
		return
	}

	// Check network before creating message
	if gm.network == nil {
		nodeInfoSlicePool.Put(membersPtr)
		return
	}

	var wg sync.WaitGroup
	for _, target := range gossipTargets {
		target := target
		wg.Add(1)

		if err := gm.replicationPool.Submit(func() {
			defer wg.Done()
			if peer, ok := gm.getNode(target); ok && peer != nil && gm.network != nil {
				for _, chunk := range chunks {
					syncMsg := gm.buildClusterSyncMessage(chunk)
					// Increased timeout for gossip sync to avoid I/O timeout errors under load
					if err := gm.network.SendWithTimeout(peer.Address, syncMsg, 5*time.Second); err == nil {
						if gm.metrics != nil {
							gm.metrics.IncrementGossipSent()
						}
					}
					putGossipMessage(syncMsg)
				}
			}
		}); err != nil {
			// Pool full - don't create goroutine, just skip
			// This prevents goroutine leaks when pools are exhausted
			wg.Done()
			if logging.Log.IsDebugEnabled() {
				logging.Debug("Skipped gossip sync: pool exhausted", "target", target)
			}
		}
	}
	wg.Wait()

	nodeInfoSlicePool.Put(membersPtr)

	if len(gossipTargets) > 0 {
		gm.gossipCachePeriodically(gossipTargets[0])
	}
}

func (gm *GossipManager) newCacheSyncOperation(key string, item *storage.StoredItem) *CacheSyncOperation {
	if item == nil {
		return nil
	}
	setData := storageItemToProto(item)
	if setData == nil {
		return nil
	}
	return &CacheSyncOperation{
		Key:           key,
		ClientVersion: item.Version,
		Type:          OperationType_OP_SET,
		SetData:       setData,
		DataPayload: &CacheSyncOperation_SetData{
			SetData: setData,
		},
	}
}

func (gm *GossipManager) sendCacheSyncOps(ctx context.Context, address string, ops []*CacheSyncOperation) error {
	if len(ops) == 0 {
		return nil
	}
	if gm.useBinaryProtocol {
		// Check message size and split if needed to avoid TCP limit
		payload, _ := encodeCacheSyncPayload(ops)
		estimatedSize := len(payload) + 64 // Add overhead for binary message header

		if estimatedSize > maxSerializedMessageSize {
			// Split operations into smaller batches
			splitSize := len(ops) / 2
			if splitSize == 0 {
				splitSize = 1
			}
			var firstErr error
			for i := 0; i < len(ops); i += splitSize {
				end := i + splitSize
				if end > len(ops) {
					end = len(ops)
				}
				batch := ops[i:end]
				if err := gm.sendCacheSyncOps(ctx, address, batch); err != nil && firstErr == nil {
					firstErr = err
				}
			}
			return firstErr
		}

		msg := GetBinaryMessage()
		msg.Type = BinaryMsgTypeCacheSync
		senderBytes := []byte(gm.localNodeID)
		copy(msg.Sender[:], senderBytes)
		if len(senderBytes) < len(msg.Sender) {
			for i := len(senderBytes); i < len(msg.Sender); i++ {
				msg.Sender[i] = 0
			}
		}
		msg.Payload = payload
		data := msg.Marshal()
		PutBinaryMessage(msg)

		sendCtx, cancel := context.WithTimeout(ctx, gm.replicationTimeout)
		defer cancel()
		return gm.network.SendRaw(sendCtx, address, data)
	}

	// Build message and check actual serialized size before sending
	msg := getGossipMessage()
	msg.Type = GossipMessageType_MESSAGE_TYPE_CACHE_SYNC
	msg.Sender = gm.localNodeID
	msg.Hlc = gm.hlc.Now()
	msg.Payload = &GossipMessage_CacheSyncPayload{
		CacheSyncPayload: &SyncMessage{
			IncrementalSync: &IncrementalSyncPayload{
				Operations: ops,
			},
		},
	}
	gm.signMessageCanonical(msg)

	// Check actual serialized size (gossip messages convert to binary internally)
	binary := convertGossipMessageToBinary(msg)
	if binary != nil {
		data := binary.Marshal()
		PutBinaryMessage(binary)

		const maxMessageSize = 10 * 1024 * 1024 // 10MB max
		if len(data) > maxMessageSize {
			putGossipMessage(msg)
			// Split operations into smaller batches
			splitSize := len(ops) / 2
			if splitSize == 0 {
				splitSize = 1
			}
			var firstErr error
			for i := 0; i < len(ops); i += splitSize {
				end := i + splitSize
				if end > len(ops) {
					end = len(ops)
				}
				batch := ops[i:end]
				if err := gm.sendCacheSyncOps(ctx, address, batch); err != nil && firstErr == nil {
					firstErr = err
				}
			}
			return firstErr
		}
	}

	timeout := gm.replicationTimeout
	select {
	case <-ctx.Done():
		putGossipMessage(msg)
		return ctx.Err()
	default:
	}
	err := gm.network.SendWithTimeout(address, msg, timeout)
	putGossipMessage(msg)
	return err
}

func (gm *GossipManager) getNodeAddress(nodeID string) (string, bool) {
	gm.mu.RLock()
	defer gm.mu.RUnlock()
	if n, ok := gm.liveNodes[nodeID]; ok && n != nil && n.Address != "" {
		return n.Address, true
	}
	return "", false
}

func (gm *GossipManager) replicateSyncToTargets(ctx context.Context, key string, item *storage.StoredItem, targetIDs []string) int {
	if len(targetIDs) == 0 || item == nil {
		return 0
	}
	baseOp := gm.newCacheSyncOperation(key, item)
	if baseOp == nil {
		return 0
	}
	successes := 0
	for _, targetID := range targetIDs {
		if targetID == "" || targetID == gm.localNodeID {
			continue
		}
		select {
		case <-ctx.Done():
			return successes
		default:
		}
		addr, ok := gm.getNodeAddress(targetID)
		if !ok {
			continue
		}
		opClone := CloneCacheSyncOperation(baseOp)
		if err := gm.sendCacheSyncOps(ctx, addr, []*CacheSyncOperation{opClone}); err != nil {
			if logging.Log.IsDebugEnabled() {
				logging.Debug("direct replication failed", "key", key, "target", targetID, "err", err)
			}
			continue
		}
		successes++
		if logging.Log.IsDebugEnabled() {
			logging.Debug("shutdown replication delivered", "key", key, "target", targetID)
		}
	}
	return successes
}

//go:inline
func (gm *GossipManager) getGossipTargets(peerCount int) []string {
	gm.mu.RLock()
	defer gm.mu.RUnlock()

	var targets []string

	if peerCount <= 3 {
		for id := range gm.liveNodes {
			if id != gm.localNodeID {
				targets = append(targets, id)
			}
		}
	} else if peerCount <= 10 {
		count := 3
		if peerCount < 5 {
			count = 2
		}
		ids := make([]string, 0, len(gm.liveNodes))
		for id := range gm.liveNodes {
			if id != gm.localNodeID {
				ids = append(ids, id)
			}
		}
		for i := 0; i < count && i < len(ids); i++ {
			idx := rand.Intn(len(ids) - i)
			targets = append(targets, ids[idx])
			ids[idx], ids[len(ids)-1-i] = ids[len(ids)-1-i], ids[idx]
		}
	} else if peerCount <= 30 {
		count := 1
		if peerCount > 20 {
			count = 1
		}
		ids := make([]string, 0, len(gm.liveNodes))
		for id := range gm.liveNodes {
			if id != gm.localNodeID {
				ids = append(ids, id)
			}
		}
		for i := 0; i < count && i < len(ids); i++ {
			idx := rand.Intn(len(ids) - i)
			targets = append(targets, ids[idx])
			ids[idx], ids[len(ids)-1-i] = ids[len(ids)-1-i], ids[idx]
		}
	} else {
		target := gm.getRandomPeerID("")
		if target != "" {
			targets = append(targets, target)
		}
	}

	return targets
}

// Allow checks if a gossip message can be sent (rate limiting)
func (g *gossipRateLimiter) Allow() bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := time.Now()
	if now.Sub(g.lastSend) < g.minInterval {
		return false
	}
	g.lastSend = now
	return true
}

//go:inline
func (gm *GossipManager) gossipCachePeriodically(targetNodeID string) {
	// Add comprehensive nil checks to prevent panic
	if gm == nil || gm.store == nil || gm.network == nil {
		return
	}
	if targetNodeID == "" {
		return
	}

	// Rate limiting: skip this gossip cycle if rate limit exceeded
	if gm.gossipRateLimiter != nil && !gm.gossipRateLimiter.Allow() {
		return
	}

	items, err := gm.store.GetSyncBuffer()
	if err != nil {
		if logging.Log.IsDebugEnabled() {
			logging.Debug("get sync buffer failed", "err", err)
		}
		return
	}

	if len(items) == 0 {
		return
	}

	maxOpsPerBatch := gm.getAdaptiveBatchSize()

	for i := 0; i < len(items); i += maxOpsPerBatch {
		end := i + maxOpsPerBatch
		if end > len(items) {
			end = len(items)
		}
		batch := items[i:end]

		msg := &GossipMessage{
			Type:   CACHE_SYNC,
			Sender: gm.localNodeID,
			Payload: &GossipMessage_CacheSyncPayload{
				CacheSyncPayload: &SyncMessage{
					SyncType: &SyncMessage_IncrementalSync{
						IncrementalSync: &IncrementalSyncPayload{
							Operations: batch,
						},
					},
				},
			},
		}
		gm.signMessageCanonical(msg)

		if peer, ok := gm.getNode(targetNodeID); ok && peer != nil {
			// Increased timeout for cache sync to avoid I/O timeout errors under load
			gm.network.SendWithTimeout(peer.Address, msg, 8*time.Second)
			gm.msgRateCounter.Add(1)
		}
	}
}

//go:inline
func (gm *GossipManager) getAdaptiveBatchSize() int {
	now := time.Now().Unix()
	lastCheck := gm.lastRateCheck.Load()

	if now > lastCheck {
		msgCount := gm.msgRateCounter.Swap(0)
		gm.lastRateCheck.Store(now)

		var newBatchSize int32
		if msgCount > 10000 {
			newBatchSize = 200
		} else if msgCount > 1000 {
			newBatchSize = 100
		} else {
			newBatchSize = 50
		}

		gm.lastBatchSize.Store(newBatchSize)
		logging.Debug("Adaptive batch size updated", "rate", msgCount, "batchSize", newBatchSize)
	}

	return int(gm.lastBatchSize.Load())
}

func (gm *GossipManager) RequestFullSync(targetNodeID string) error {
	if targetNodeID == "" {
		targetNodeID = gm.getRandomPeerID("")
		if targetNodeID == "" {
			return errors.New("no peer for full sync")
		}
	}

	peer, ok := gm.getNode(targetNodeID)
	if !ok {
		return fmt.Errorf("peer %s not found", targetNodeID)
	}

	msg := &GossipMessage{
		Type:   GossipMessageType_MESSAGE_TYPE_FULL_SYNC_REQUEST,
		Sender: gm.localNodeID,
		Payload: &GossipMessage_FullSyncRequestPayload{
			FullSyncRequestPayload: &FullSyncRequestPayload{
				RequesterId: gm.localNodeID,
			},
		},
	}
	gm.signMessageCanonical(msg)
	logging.Info("SYNC request", "target", targetNodeID)
	return gm.network.SendWithTimeout(peer.Address, msg, gm.replicationTimeout)
}

func (gm *GossipManager) handleFullSyncRequest(requesterID string) {
	if gm.store == nil {
		logging.Warn("SYNC store nil")
		return
	}

	items, err := gm.store.GetFullSyncSnapshot()
	if err != nil {
		logging.Error(err, "get full snapshot failed")
		return
	}

	payload := &FullSyncResponsePayload{
		FullSync: &FullSyncPayload{
			Items:             items,
			SnapshotTimestamp: uint64(gm.localVersion),
		},
	}

	peer, ok := gm.getNode(requesterID)
	if !ok {
		logging.Warn("requester not found", "req", requesterID)
		return
	}

	resp := &GossipMessage{
		Type:   GossipMessageType_MESSAGE_TYPE_FULL_SYNC_RESPONSE,
		Sender: gm.localNodeID,
		Payload: &GossipMessage_FullSyncResponsePayload{
			FullSyncResponsePayload: payload,
		},
	}
	gm.signMessageCanonical(resp)
	gm.network.SendWithTimeout(peer.Address, resp, gm.replicationTimeout)
}

func (gm *GossipManager) handleFullSyncResponse(payload *FullSyncPayload) {
	if gm.store == nil {
		logging.Warn("SYNC apply store nil")
		return
	}

	if err := gm.store.ApplyFullSyncSnapshot(payload.GetItems(), time.Unix(int64(payload.GetSnapshotTimestamp()), 0)); err != nil {
		logging.Error(err, "apply full sync failed")
	}
	logging.Info("SYNC applied", "items", len(payload.Items))
}

// isCriticalMessage checks if a message type requires immediate processing.
//
//go:inline
func isCriticalMessage(msgType GossipMessageType) bool {
	switch msgType {
	case CLUSTER_SYNC, CONNECT, PROBE_REQUEST, PROBE_RESPONSE, READ_RESPONSE:
		// READ_RESPONSE must be processed immediately
		// to prevent timeout of pending reads and improve read success rate
		return true
	default:
		return false
	}
}

// SimulateReceive enqueues a message for processing.
// Critical messages are processed directly
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

	// Skip signature verification for batch read messages
	msgType := msg.Type
	skipVerification := msgType == GossipMessageType_MESSAGE_TYPE_BATCH_READ_REQUEST ||
		msgType == GossipMessageType_MESSAGE_TYPE_BATCH_READ_RESPONSE

	if !skipVerification {
		// Verify signature for other messages
		if !gm.verifyMessageCanonical(msg) {
			// Only log at debug level to reduce log spam during cluster formation
			// Invalid signatures are expected during bootstrap when pubkeys aren't exchanged yet
			if logging.Log.IsDebugEnabled() {
				logging.Debug("Dropped msg: invalid signature", "sender", msg.Sender, "type", msg.Type)
			}
			gm.messagesDropped.Add(1)
			return
		}
	}

	// Record received message metric
	if gm.metrics != nil {
		gm.metrics.IncrementGossipReceived()
	}

	// High-frequency messages processed directly
	// Direct processing eliminates queue overhead and reduces latency by 60-80%
	// Batch read messages are also high-frequency and should be processed directly
	if msg.Type == GossipMessageType_MESSAGE_TYPE_CACHE_SYNC ||
		msg.Type == GossipMessageType_MESSAGE_TYPE_READ_REQUEST ||
		msg.Type == GossipMessageType_MESSAGE_TYPE_READ_RESPONSE ||
		msg.Type == GossipMessageType_MESSAGE_TYPE_BATCH_READ_REQUEST ||
		msg.Type == GossipMessageType_MESSAGE_TYPE_BATCH_READ_RESPONSE {
		// Skip signature verification for batch read messages (already optimized in processLoop)
		// This reduces CPU overhead
		gm.processGossipMessage(msg)
		return
	}

	// Critical messages: direct processing
	if isCriticalMessage(msg.Type) {
		gm.processGossipMessage(msg)
		return
	}

	// Other messages: submit to pool or queue
	if gm.inboundPriorityPool != nil || gm.inboundPool != nil {
		if err := gm.submitInboundTaskWithPriority(func() {
			gm.processInboundMessage(msg)
		}, context.Background(), gm.messagePriority(msg), "inbound-fallback", msg.GetSender()); err == nil {
			return
		}
	}

	// Fallback: queue to inputCh
	select {
	case gm.inputCh <- msg:
		// Enqueued successfully
	default:
		logging.Warn("Dropped gossip message: input full", "type", msg.Type, "sender", msg.Sender)
		gm.messagesDropped.Add(1)
	}
}

func (gm *GossipManager) processInboundMessage(msg *GossipMessage) {
	// Recover from individual message processing panics
	defer func() {
		if r := recover(); r != nil {
			logging.Error(fmt.Errorf("panic processing message: %v", r),
				"Inbound message processing panic recovered",
				"sender", msg.GetSender(),
				"type", msg.GetType())
		}
	}()

	if ok := gm.verifyMessageCanonical(msg); !ok {
		// Only log at debug level to reduce log spam
		if logging.Log.IsDebugEnabled() {
			logging.Debug("Dropping msg: invalid signature", "sender", msg.Sender)
		}
		gm.messagesDropped.Add(1)
		return
	}
	gm.processGossipMessage(msg)
}

// MessageStats returns aggregated gossip message statistics.
// total: Total messages received
// dropped: Messages dropped due to queue saturation or validation failures.
func (gm *GossipManager) MessageStats() (total, dropped int64) {
	return gm.messagesTotal.Load(), gm.messagesDropped.Load()
}

// PipelineDrops returns the number of replication operations dropped due to
// saturated pipelines.
func (gm *GossipManager) PipelineDrops() int64 {
	return gm.pipelineDropCounter.Load()
}

func (gm *GossipManager) PendingReadsCount() int64 {
	return gm.pendingReadsCount.Load()
}

// InboundPoolSaturations returns the number of times the inbound pool was at capacity
func (gm *GossipManager) InboundPoolSaturations() int64 {
	return gm.inboundPoolSaturations.Load()
}

// InboundPoolSize returns the current inbound pool size
func (gm *GossipManager) InboundPoolSize() int {
	gm.mu.RLock()
	defer gm.mu.RUnlock()
	return gm.inboundPoolSize
}

// addPendingRead stores a pending read entry and increments the counter safely.
func (gm *GossipManager) addPendingRead(requestID string, entry *pendingReadEntry) {
	if requestID == "" || entry == nil {
		return
	}
	gm.pendingReads.Store(requestID, entry)
	gm.pendingReadsCount.Add(1)
}

// removePendingRead deletes a pending read entry and decrements the counter if present.
func (gm *GossipManager) removePendingRead(requestID string) {
	if requestID == "" {
		return
	}
	if _, loaded := gm.pendingReads.LoadAndDelete(requestID); loaded {
		gm.decrementPendingReads()
	}
}

func (gm *GossipManager) decrementPendingReads() {
	if gm.pendingReadsCount.Add(-1) < 0 {
		gm.pendingReadsCount.Store(0)
	}
}

// processLoop is the main event loop that processes messages and runs periodic tasks.
func (gm *GossipManager) processLoop() {
	defer gm.wg.Done()

	// Recover from panics and restart the loop
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

	for {
		select {
		case msg := <-gm.inputCh:
			// This allows multiple messages to be processed in parallel, improving throughput
			// and reducing latency for response handling
			if gm.inboundPriorityPool != nil || gm.inboundPool != nil {
				msgCopy := msg // Capture msg for goroutine
				err := gm.submitInboundTask(func() {
					// Recover from individual message processing panics
					defer func() {
						if r := recover(); r != nil {
							logging.Error(fmt.Errorf("panic processing message: %v", r),
								"Message processing panic recovered",
								"sender", msgCopy.GetSender(),
								"type", msgCopy.GetType())
						}
					}()

					// Skip signature verification for batch read requests/responses
					// Batch messages are already authenticated at the network layer
					msgType := msgCopy.GetType()
					skipVerification := msgType == GossipMessageType_MESSAGE_TYPE_BATCH_READ_REQUEST ||
						msgType == GossipMessageType_MESSAGE_TYPE_BATCH_READ_RESPONSE

					if !skipVerification {
						// Verify message signature for other message types
						if ok := gm.verifyMessageCanonical(msgCopy); !ok {
							// Only log at debug level to reduce log spam
							if logging.Log.IsDebugEnabled() {
								logging.Debug("Dropping msg: invalid signature", "sender", msgCopy.Sender)
							}
							return
						}
					}
					gm.processGossipMessage(msgCopy)
				}, context.Background(), "inbound-loop", msgCopy.GetSender())
				if err == nil {
					continue // Successfully submitted to pool
				}
				// Pool full: try emergency resize and retry once before fallback
				if gm.inboundPoolResizer != nil {
					gm.inboundPoolResizer.emergencyResize()
					// Retry immediately without sleep
					err = gm.submitInboundTask(func() {
						defer func() {
							if r := recover(); r != nil {
								logging.Error(fmt.Errorf("panic processing message: %v", r),
									"Message processing panic recovered",
									"sender", msgCopy.GetSender(),
									"type", msgCopy.GetType())
							}
						}()
						msgType := msgCopy.GetType()
						skipVerification := msgType == GossipMessageType_MESSAGE_TYPE_BATCH_READ_REQUEST ||
							msgType == GossipMessageType_MESSAGE_TYPE_BATCH_READ_RESPONSE
						if !skipVerification {
							if ok := gm.verifyMessageCanonical(msgCopy); !ok {
								if logging.Log.IsDebugEnabled() {
									logging.Debug("Dropping msg: invalid signature", "sender", msgCopy.Sender)
								}
								return
							}
						}
						gm.processGossipMessage(msgCopy)
					}, context.Background(), "inbound-loop", msgCopy.GetSender())
					if err == nil {
						continue // Successfully submitted after resize
					}
				}
				// Fallback to direct processing if pool still full after resize
			}

			// Fallback: direct processing if pool unavailable or full after retry
			// For batch read messages, process directly without verification
			msgType := msg.GetType()
			skipVerification := msgType == GossipMessageType_MESSAGE_TYPE_BATCH_READ_REQUEST ||
				msgType == GossipMessageType_MESSAGE_TYPE_BATCH_READ_RESPONSE

			// Recover from individual message processing panics
			func() {
				defer func() {
					if r := recover(); r != nil {
						logging.Error(fmt.Errorf("panic processing message: %v", r),
							"Message processing panic recovered",
							"sender", msg.GetSender(),
							"type", msg.GetType())
					}
				}()

				// Verify message signature (skip for batch read messages)
				if !skipVerification {
					if ok := gm.verifyMessageCanonical(msg); !ok {
						// Only log at debug level to reduce log spam
						if logging.Log.IsDebugEnabled() {
							logging.Debug("Dropping msg: invalid signature", "sender", msg.Sender)
						}
						return
					}
				}
				gm.processGossipMessage(msg)
			}()

		case <-gm.stopCh:
			return
		}
	}
}

// Helper methods

// getNode retrieves node info by ID (thread-safe, lock-free with fallback).
//
//go:inline
func (gm *GossipManager) getNode(id string) (*NodeInfo, bool) {
	// Fast path: try lock-free structure first (zero allocation, zero lock)
	if gm.liveNodesLF != nil {
		if n, ok := gm.liveNodesLF.Get(id); ok {
			return n, ok
		}
		// Fallback: node exists in liveNodes but not in liveNodesLF (sync issue)
		// This is rare, so we only check when liveNodesLF lookup fails
		gm.mu.RLock()
		n, ok := gm.liveNodes[id]
		gm.mu.RUnlock()
		return n, ok
	}
	gm.mu.RLock()
	defer gm.mu.RUnlock()
	n, ok := gm.liveNodes[id]
	return n, ok
}

// ForceRemoveNode marks a node as dead immediately. Intended for testing and administrative tooling.
func (gm *GossipManager) ForceRemoveNode(nodeID string) {
	gm.mu.RLock()
	node, ok := gm.liveNodes[nodeID]
	gm.mu.RUnlock()
	if !ok {
		return
	}

	gm.updateNode(nodeID, node.Address, NodeState_NODE_STATE_DEAD, gm.incrementLocalVersion())
}

// isNodeLocallyAlive checks if a node is alive (thread-safe, inlined)
//
//go:inline
func (gm *GossipManager) isNodeLocallyAlive(nodeID string) bool {
	n, ok := gm.getNode(nodeID)
	return ok && n.State == NodeState_NODE_STATE_ALIVE
}

// incrementLocalVersion atomically increments and returns the local version counter.
//
//go:inline
func (gm *GossipManager) incrementLocalVersion() int64 {
	return atomic.AddInt64(&gm.localVersion, 1)
}

// generateOpID creates a unique operation ID for request tracking.
//
//go:inline
func (gm *GossipManager) generateOpID() string {
	return gm.opidGen.Generate()
}

// hasMultipleNodes checks if cluster has multiple nodes.
func (gm *GossipManager) hasMultipleNodes() bool {
	gm.mu.RLock()
	defer gm.mu.RUnlock()
	return len(gm.liveNodes) > 1
}

// getReplicas returns replica nodes for a key using cache if available
func (gm *GossipManager) getReplicas(key string, n int) []string {
	if gm.hashRingCache != nil {
		return gm.hashRingCache.GetN(key, n)
	}
	if gm.hashRing != nil {
		if gm.hashRingCache != nil {
			return gm.hashRingCache.GetN(key, n)
		}
		return gm.hashRing.GetN(key, n)
	}
	return nil
}

// getRandomPeerID returns a random peer ID, excluding the specified ID.
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

func (gm *GossipManager) getBatchConfig(role BatchRole) (int, time.Duration) {
	if gm != nil && gm.batchManager != nil {
		return gm.batchManager.Get(role)
	}
	return pipelineBatchSize, pipelineFlushTick
}

func (gm *GossipManager) getPipelineFlushInterval() time.Duration {
	_, interval := gm.getBatchConfig(BatchRoleWrite)
	return interval
}

func (gm *GossipManager) updateBatchClusterSize(size int) {
	if gm == nil || gm.batchManager == nil {
		return
	}
	gm.batchManager.UpdateClusterSize(size)

	// Also update inbound pool size dynamically based on cluster size
	gm.updateInboundPoolSize(size)
}

// updateInboundPoolSize dynamically adjusts inbound pool size based on cluster size
// For large clusters (50+ nodes), scales beyond previous 6144 cap to handle message volume
func (gm *GossipManager) updateInboundPoolSize(clusterSize int) {
	if gm == nil {
		return
	}

	// Calculate optimal pool size based on cluster size
	baseSize := gm.maxReplicators * 64
	if baseSize < 512 {
		baseSize = 512
	}

	// For large clusters, scale more aggressively
	var newSize int
	switch {
	case clusterSize <= 10:
		// Small clusters: use base size
		newSize = baseSize
	case clusterSize <= 30:
		// Medium clusters: 1.5x base
		newSize = baseSize * 3 / 2
	case clusterSize <= 50:
		// Large clusters: 2x base
		newSize = baseSize * 2
	case clusterSize <= 100:
		// Very large clusters: 3x base (up to 12288 for typical configs)
		newSize = baseSize * 3
	default:
		// Huge clusters (>100 nodes): 4x base (up to 16384)
		newSize = baseSize * 4
	}

	// Absolute maximum cap to prevent excessive goroutine creation
	const maxPoolSize = 16384
	if newSize > maxPoolSize {
		newSize = maxPoolSize
	}

	// Only resize if the difference is significant (at least 20% increase needed)
	currentSize := gm.inboundPoolSize
	if gm.inboundPool == nil || currentSize == 0 || newSize == currentSize {
		return
	}

	resizeNeeded := false
	if newSize > currentSize {
		resizeNeeded = (newSize-currentSize)*100/currentSize >= 20
	} else {
		resizeNeeded = (currentSize-newSize)*100/currentSize >= 35
	}

	if !resizeNeeded {
		return
	}

	if err := gm.inboundPool.Resize(newSize); err != nil {
		logging.Warn("Failed to resize inbound pool", "newSize", newSize, "error", err)
		return
	}

	gm.inboundPoolSize = newSize

	// Update adaptive resizer limits based on cluster size
	if gm.inboundPoolResizer != nil {
		minSize := newSize / 4
		if minSize < 512 {
			minSize = 512
		}
		maxSize := newSize * 4
		if maxSize > maxPoolSize {
			maxSize = maxPoolSize
		}
		gm.inboundPoolResizer.UpdateLimits(minSize, maxSize)
	}

	if logging.Log.IsDebugEnabled() {
		logging.Debug("Inbound pool resized",
			"oldSize", currentSize,
			"newSize", newSize,
			"clusterSize", clusterSize)
	}
}

func (gm *GossipManager) initWorkerPoolMetrics() {
	if gm.metrics == nil {
		return
	}

	gm.replicationPoolMetrics = gm.metrics.RegisterWorkerPoolRecorder("gossip_replication")
	gm.inboundPoolMetrics = gm.metrics.RegisterWorkerPoolRecorder("gossip_inbound")

	gm.wg.Add(1)
	go func() {
		defer gm.wg.Done()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-gm.stopCh:
				return
			case <-ticker.C:
				gm.recordWorkerPoolMetrics()
			}
		}
	}()
}

func (gm *GossipManager) recordWorkerPoolMetrics() {
	if gm.replicationPool != nil && gm.replicationPoolMetrics != nil {
		gm.replicationPoolMetrics.RecordStats(gm.replicationPool.Stats())
	}

	if gm.inboundPool != nil && gm.inboundPoolMetrics != nil {
		gm.inboundPoolMetrics.RecordStats(gm.inboundPool.Stats())
	}

	// Check and perform adaptive resizing
	// Only resize if goroutine count is reasonable to avoid leaks
	currentGoroutines, _, _ := globalGoroutineMonitor.GetStats()
	maxGoroutines := 50000
	if currentGoroutines < maxGoroutines {
		if gm.replicationPoolResizer != nil {
			gm.replicationPoolResizer.checkAndResize()
		}
		if gm.inboundPoolResizer != nil {
			gm.inboundPoolResizer.checkAndResize()
		}
	}
}

func (gm *GossipManager) submitInboundTask(task func(), ctx context.Context, fallbackName string, hints ...string) error {
	return gm.submitInboundTaskWithPriority(task, ctx, workerpool.PriorityNormal, fallbackName, hints...)
}

func (gm *GossipManager) submitInboundTaskWithPriority(task func(), ctx context.Context, priority workerpool.TaskPriority, fallbackName string, hints ...string) error {
	if task == nil {
		return errors.New("nil inbound task")
	}

	var numaHint string
	if len(hints) > 0 {
		numaHint = hints[0]
	}

	taskCtx := ctx
	if taskCtx == nil {
		taskCtx = context.Background()
	}

	if gm.inboundPriorityPool != nil {
		numaNode := gm.selectNUMANode(numaHint)
		_, err := gm.inboundPriorityPool.SubmitTask(func(runCtx context.Context) {
			select {
			case <-runCtx.Done():
				return
			default:
				task()
			}
		}, workerpool.TaskOptions{
			Priority: priority,
			Context:  taskCtx,
			NUMANode: numaNode,
		})
		if err == nil {
			return nil
		}
		// Pool full - trigger resize and retry multiple times (no fallback)
		if gm.inboundPoolResizer != nil {
			for retry := 0; retry < 3; retry++ {
				gm.inboundPoolResizer.emergencyResize()
				time.Sleep(10 * time.Millisecond) // Allow resize to take effect
				_, err = gm.inboundPriorityPool.SubmitTask(func(runCtx context.Context) {
					select {
					case <-runCtx.Done():
						return
					default:
						task()
					}
				}, workerpool.TaskOptions{
					Priority: priority,
					Context:  taskCtx,
					NUMANode: numaNode,
				})
				if err == nil {
					return nil
				}
			}
		}
	} else if gm.inboundPool != nil {
		if err := gm.inboundPool.Submit(task); err == nil {
			return nil
		}
		// Pool full - trigger resize and retry multiple times (no fallback)
		if gm.inboundPoolResizer != nil {
			for retry := 0; retry < 3; retry++ {
				gm.inboundPoolResizer.emergencyResize()
				time.Sleep(10 * time.Millisecond) // Allow resize to take effect
				if err := gm.inboundPool.Submit(task); err == nil {
					return nil
				}
			}
		}
	} else {
		return ErrPoolExhausted
	}

	gm.inboundPoolSaturations.Add(1)
	// No fallback - return error if all retries failed
	// This forces proper error handling instead of creating goroutines
	return ErrPoolExhausted
}

func (gm *GossipManager) selectNUMANode(hint string) int {
	if gm.inboundNUMANodes <= 1 {
		return 0
	}
	if hint == "" {
		counter := gm.inboundTaskCounter.Add(1)
		return int(counter % uint64(gm.inboundNUMANodes))
	}
	// Simple hash function for NUMA node selection (djb2-like)
	hash := uint32(5381)
	for i := 0; i < len(hint); i++ {
		hash = hash*33 + uint32(hint[i])
	}
	return int(hash % uint32(gm.inboundNUMANodes))
}

func (gm *GossipManager) messagePriority(msg *GossipMessage) workerpool.TaskPriority {
	if msg == nil {
		return workerpool.PriorityNormal
	}

	switch msg.GetType() {
	case GossipMessageType_MESSAGE_TYPE_CONNECT,
		GossipMessageType_MESSAGE_TYPE_CLUSTER_SYNC,
		GossipMessageType_MESSAGE_TYPE_PROBE_REQUEST,
		GossipMessageType_MESSAGE_TYPE_PROBE_RESPONSE,
		GossipMessageType_MESSAGE_TYPE_FULL_SYNC_REQUEST,
		GossipMessageType_MESSAGE_TYPE_FULL_SYNC_RESPONSE:
		return workerpool.PriorityCritical
	case GossipMessageType_MESSAGE_TYPE_CACHE_SYNC,
		GossipMessageType_MESSAGE_TYPE_READ_REQUEST,
		GossipMessageType_MESSAGE_TYPE_READ_RESPONSE:
		return workerpool.PriorityHigh
	default:
		return workerpool.PriorityNormal
	}
}

const defaultClusterSyncChunkSize = 64

func chunkClusterSyncNodes(nodes []*NodeInfo, chunkSize int) [][]*NodeInfo {
	if chunkSize <= 0 {
		chunkSize = defaultClusterSyncChunkSize
	}
	if len(nodes) == 0 {
		return [][]*NodeInfo{}
	}

	chunks := make([][]*NodeInfo, 0, (len(nodes)+chunkSize-1)/chunkSize)
	for start := 0; start < len(nodes); start += chunkSize {
		end := start + chunkSize
		if end > len(nodes) {
			end = len(nodes)
		}
		chunks = append(chunks, nodes[start:end])
	}
	return chunks
}

func (gm *GossipManager) getClusterSyncChunkSize() int {
	if gm == nil {
		return defaultClusterSyncChunkSize
	}
	if gm.clusterSyncChunkSize > 0 {
		return gm.clusterSyncChunkSize
	}
	return defaultClusterSyncChunkSize
}

func (gm *GossipManager) buildClusterSyncMessage(nodes []*NodeInfo) *GossipMessage {
	msg := getGossipMessage()
	msg.Type = CLUSTER_SYNC
	msg.Sender = gm.localNodeID
	if gm.hlc != nil {
		msg.Hlc = gm.hlc.Now()
	}
	msg.Payload = &GossipMessage_ClusterSyncPayload{
		ClusterSyncPayload: &ClusterSyncPayload{
			Nodes: nodes,
		},
	}
	_ = gm.signMessageCanonical(msg)
	return msg
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// connectToSeedsWithBackoff initiates connections with staggered backoff to prevent connection storms
// in large clusters (50-100 nodes). This wraps connectToSeeds with per-seed staggered delays.
func (gm *GossipManager) connectToSeedsWithBackoff() {
	if len(gm.seedAddrs) == 0 {
		gm.connectToSeeds() // No seeds, use regular path
		return
	}

	// Stagger connections to prevent storms in large clusters
	// For large seed lists, add exponential backoff delays between connections
	numSeeds := len(gm.seedAddrs)
	var wg sync.WaitGroup

	for i, seedAddr := range gm.seedAddrs {
		// Check stopCh before each iteration
		select {
		case <-gm.stopCh:
			return
		default:
		}

		// Skip if seed address is our own address
		if seedAddr == gm.localAddress {
			continue
		}

		// Stagger connections: first few immediate, then exponential backoff
		// For large clusters (many seeds), spread connections over 0-2 seconds
		var delay time.Duration
		if numSeeds <= 5 {
			// Small cluster: all immediate
			delay = 0
		} else if numSeeds <= 20 {
			// Medium cluster: spread over 500ms
			delay = time.Duration(i*25) * time.Millisecond // 0-475ms
		} else {
			// Large cluster: spread over 2 seconds with exponential backoff
			// First 5 immediate, then exponential: 50ms, 100ms, 200ms, etc.
			if i < 5 {
				delay = 0
			} else {
				delay = time.Duration(50*(1<<uint(min(i-5, 6)))) * time.Millisecond
				if delay > 2*time.Second {
					delay = 2 * time.Second
				}
			}
		}

		if delay > 0 {
			select {
			case <-gm.stopCh:
				return
			case <-time.After(delay):
			}
		}

		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			// Check stopCh before connecting
			select {
			case <-gm.stopCh:
				return
			default:
			}
			// Now call regular connectToSeeds logic for this seed
			gm.connectToSingleSeed(addr)
		}(seedAddr)
	}

	wg.Wait()

	// After staggered initial connections, proceed with regular retry logic
	// This handles retries if initial connections failed
	// Check stopCh before retrying
	select {
	case <-gm.stopCh:
		return
	default:
		gm.connectToSeedsRetry()
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// connectToSingleSeed connects to a single seed node (helper for staggered backoff)
// Enhanced with exponential backoff retry logic
func (gm *GossipManager) connectToSingleSeed(seedAddr string) {
	if seedAddr == gm.localAddress {
		return
	}

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
				PublicKey: pubKey,
			},
		},
	}
	if err := gm.signMessageCanonical(connectMsg); err != nil && !gm.disableAuth {
		logging.Warn("Failed to sign CONNECT message to seed", "error", err)
	}

	// Exponential backoff retry for seed connection
	maxAttempts := 5
	baseDelay := 100 * time.Millisecond
	maxDelay := 2 * time.Second

	for attempt := 0; attempt < maxAttempts; attempt++ {
		// Check stopCh before each attempt
		select {
		case <-gm.stopCh:
			return
		default:
		}

		// Try to submit to pool
		err := gm.replicationPool.Submit(func() {
			// Increased timeout for seed connection stability
			if err := gm.network.SendWithTimeout(seedAddr, connectMsg, 3*time.Second); err != nil {
				if logging.Log.IsDebugEnabled() {
					logging.Debug("Failed to connect to seed", "seed", seedAddr, "error", err, "attempt", attempt+1)
				}
			} else {
				logging.Debug("Sent CONNECT to seed", "seed", seedAddr)
			}
		})

		if err == nil {
			// Successfully submitted, check if we got a response
			time.Sleep(200 * time.Millisecond) // Brief wait for response
			gm.mu.RLock()
			peerCount := len(gm.liveNodes) - 1
			gm.mu.RUnlock()
			if peerCount > 0 {
				return // Successfully connected
			}
		}

		// Pool full or connection failed - retry with backoff
		if attempt < maxAttempts-1 {
			// Exponential backoff with jitter
			delay := baseDelay * time.Duration(1<<uint(attempt))
			if delay > maxDelay {
				delay = maxDelay
			}
			// Add jitter: ±25%
			jitter := time.Duration(rand.Intn(int(delay / 4)))
			if rand.Intn(2) == 0 {
				delay += jitter
			} else {
				delay -= jitter
			}

			select {
			case <-gm.stopCh:
				return
			case <-time.After(delay):
			}

			// Try pool resize if available
			if gm.replicationPoolResizer != nil && attempt == 1 {
				gm.replicationPoolResizer.emergencyResize()
			}
		}
	}
}

// connectToSeeds initiates connections to all seed nodes on startup.
//
// This is required for cluster formation and node discovery.
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
	// signMessageCanonical will handle nil keypair gracefully
	if err := gm.signMessageCanonical(connectMsg); err != nil && !gm.disableAuth {
		logging.Warn("Failed to sign CONNECT message to seed", "error", err)
	}

	logging.Debug("Connecting to seed nodes", "seeds", len(gm.seedAddrs))

	// Send initial CONNECT to all seeds immediately
	// Use replication pool instead of creating goroutines directly
	var wg sync.WaitGroup
	for _, seedAddr := range gm.seedAddrs {
		// Skip if seed address is our own address
		if seedAddr == gm.localAddress {
			continue
		}

		addrCopy := seedAddr
		wg.Add(1)

		// Use replication pool for bounded concurrency
		if err := gm.replicationPool.Submit(func() {
			defer wg.Done()
			// Use shorter timeout for faster failure detection
			if err := gm.network.SendWithTimeout(addrCopy, connectMsg, 1*time.Second); err != nil {
				logging.Debug("Failed to connect to seed", "seed", addrCopy, "error", err)
			} else {
				logging.Debug("Sent CONNECT to seed", "seed", addrCopy)
			}
		}); err != nil {
			// Pool full - retry with resize or skip
			wg.Done()
			if gm.replicationPoolResizer != nil {
				gm.replicationPoolResizer.emergencyResize()
				time.Sleep(10 * time.Millisecond)
				if err := gm.replicationPool.Submit(func() {
					gm.network.SendWithTimeout(addrCopy, connectMsg, 1*time.Second)
				}); err != nil {
					if logging.Log.IsDebugEnabled() {
						logging.Debug("Skipped seed connection: pool exhausted after resize", "seed", addrCopy)
					}
				}
			} else {
				if logging.Log.IsDebugEnabled() {
					logging.Debug("Skipped seed connection: pool exhausted", "seed", addrCopy)
				}
			}
		}
	}
	wg.Wait()

	// Proceed with retry logic if initial connections failed
	gm.connectToSeedsRetry()
}

// connectToSeedsRetry handles retries for failed seed connections
func (gm *GossipManager) connectToSeedsRetry() {
	if len(gm.seedAddrs) == 0 {
		return
	}

	// Prepare CONNECT message for retries
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
				PublicKey: pubKey,
			},
		},
	}
	if err := gm.signMessageCanonical(connectMsg); err != nil && !gm.disableAuth {
		logging.Warn("Failed to sign CONNECT message for retry", "error", err)
	}

	maxRetries := 15
	retryDelay := 200 * time.Millisecond

	for attempt := 0; attempt < maxRetries; attempt++ {
		select {
		case <-gm.stopCh:
			return
		default:
		}

		// Send connect messages first
		for _, seedAddr := range gm.seedAddrs {
			select {
			case <-gm.stopCh:
				return
			default:
			}

			if seedAddr == gm.localAddress {
				continue
			}
			if err := gm.network.SendWithTimeout(seedAddr, connectMsg, 2*time.Second); err != nil {
				if logging.Log.IsDebugEnabled() {
					logging.Debug("Failed to send CONNECT", "seed", seedAddr, "error", err)
				}
			}
		}

		// Wait for response with progressive checks
		// Early attempts: check more frequently
		// Later attempts: check less frequently to reduce CPU
		checkInterval := 50 * time.Millisecond
		if attempt > 5 {
			checkInterval = 100 * time.Millisecond
		}
		if attempt > 10 {
			checkInterval = 200 * time.Millisecond
		}

		checks := int(retryDelay / checkInterval)
		if checks < 1 {
			checks = 1
		}
		if checks > 20 {
			checks = 20
		}

		for i := 0; i < checks; i++ {
			select {
			case <-gm.stopCh:
				return
			case <-time.After(checkInterval):
			}

			// Check if we've connected
			gm.mu.RLock()
			peerCount := len(gm.liveNodes) - 1
			gm.mu.RUnlock()

			if peerCount > 0 {
				logging.Debug("Successfully connected to cluster", "peers", peerCount, "attempt", attempt+1)
				return
			}
		}

		// Exponential backoff with cap
		retryDelay = retryDelay * 2
		if retryDelay > 1*time.Second {
			retryDelay = 1 * time.Second
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

	// System is ready if at least one node is available
	ready := healthyNodes > 0
	gm.mu.RUnlock()

	gm.clusterReady.Store(ready)
	gm.lastReadyCheck.Store(time.Now().UnixNano())

	// Check public key status (HasPublicKeysForPeers already locks internally)
	// Don't double-lock - HasPublicKeysForPeers manages its own locks
	pubkeyCount, peerCount, pubkeysReady := gm.HasPublicKeysForPeers()

	// Update cluster metrics (optimized - only update if metrics enabled)
	if gm.metrics != nil {
		gm.metrics.SetClusterNodesTotal(int64(clusterSize))
		gm.metrics.SetClusterNodesAlive(int64(healthyNodes))
		gm.metrics.SetGossipLocalHealthyNodes(int64(healthyNodes))
		gm.metrics.SetGossipLocalClusterSize(int64(clusterSize))

		// Count suspect and dead nodes
		suspectCount := int64(0)
		deadCount := int64(0)
		gm.mu.RLock()
		for _, node := range gm.liveNodes {
			switch node.State {
			case NodeState_NODE_STATE_SUSPECT:
				suspectCount++
			case NodeState_NODE_STATE_DEAD:
				deadCount++
			}
		}
		gm.mu.RUnlock()
		gm.metrics.SetClusterNodesSuspect(suspectCount)
		gm.metrics.SetClusterNodesDead(deadCount)

		// Update storage metrics (if store supports Stats)
		if gm.store != nil {
			stats := gm.store.Stats()
			gm.metrics.SetStorageKeys(int64(stats.KeyCount))
			gm.metrics.SetStorageBytes(stats.DBSize)
		}

		// Update pipeline metrics
		gm.pipelineMu.Lock()
		pipelineCount := int64(len(gm.pipelines))
		gm.pipelineMu.Unlock()
		gm.metrics.SetPipelineActiveCount(pipelineCount)
	}

	// Only log if debug is enabled
	if logging.Log.IsDebugEnabled() {
		ringMembers := gm.hashRing.Members()
		logging.Debug("Hash ring members", "count", len(ringMembers), "members", ringMembers)
	}

	return ReplicaStatus{
		Ready:         ready,
		ClusterSize:   clusterSize,
		HealthyNodes:  healthyNodes,
		ReplicaFactor: gm.replicaCount,
		LocalNodeID:   gm.localNodeID,
		PubkeysReady:  pubkeysReady,
		PubkeyCount:   pubkeyCount,
		PeerCount:     peerCount,
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
	Ready         bool
	ClusterSize   int
	HealthyNodes  int
	ReplicaFactor int
	LocalNodeID   string
	// New fields for public key status
	PubkeysReady bool // True if all peer public keys are obtained
	PubkeyCount  int  // Number of peer public keys obtained
	PeerCount    int  // Total number of peer nodes
}

// Helper functions and pools (merged from helpers.go)

var (
	gossipMessagePool = sync.Pool{
		New: func() interface{} {
			return &GossipMessage{}
		},
	}

	cacheSyncOpPool = sync.Pool{
		New: func() interface{} {
			return &CacheSyncOperation{}
		},
	}

	syncMessagePool = sync.Pool{
		New: func() interface{} {
			return &SyncMessage{}
		},
	}

	incrementalSyncPool = sync.Pool{
		New: func() interface{} {
			return &IncrementalSyncPayload{}
		},
	}

	protoCloneBufferPool = sync.Pool{
		New: func() interface{} {
			buf := make([]byte, 0, 16384)
			return &buf
		},
	}

	readResponseChannelPool = sync.Pool{
		New: func() interface{} {
			return make(chan *ReadResponsePayload, 1)
		},
	}

	replicationMessagePool = sync.Pool{
		New: func() interface{} {
			return &GossipMessage{}
		},
	}
)

//go:inline
func getGossipMessage() *GossipMessage {
	return gossipMessagePool.Get().(*GossipMessage)
}

//go:inline
func putGossipMessage(msg *GossipMessage) {
	if msg == nil {
		return
	}
	// Reset message fields for reuse
	*msg = GossipMessage{}
	gossipMessagePool.Put(msg)
}

//go:inline
func getReadResponseChannel() chan *ReadResponsePayload {
	return readResponseChannelPool.Get().(chan *ReadResponsePayload)
}

//go:inline
func putReadResponseChannel(ch chan *ReadResponsePayload) {
	select {
	case <-ch:
	default:
	}
	readResponseChannelPool.Put(ch)
}

//go:inline
func stringToBytes(s string) []byte {
	if s == "" {
		return nil
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

//go:inline
func bytesToString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(b), len(b))
}

//go:inline
func copyBytesIfNeeded(b []byte, needCopy bool) []byte {
	if !needCopy {
		return b
	}
	if len(b) == 0 {
		return nil
	}
	result := make([]byte, len(b))
	copy(result, b)
	return result
}

// copyBytes creates a copy of the byte slice.
// More efficient than append([]byte(nil), b...) as it avoids unnecessary allocations.
//
//go:inline
func copyBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	result := make([]byte, len(b))
	copy(result, b)
	return result
}

// copyStorageItem creates a deep copy of a storage item using object pool.
// Optimized for high-throughput reads - reuses allocated items from pool.
func copyStorageItem(item *storage.StoredItem) *storage.StoredItem {
	if item == nil {
		return nil
	}

	// Use object pool to reduce allocations
	copyItem := storage.GetStoredItem()
	copyItem.Version = item.Version
	copyItem.ExpireAt = item.ExpireAt

	if len(item.Value) > 0 {
		// Reuse existing capacity if available
		if cap(copyItem.Value) >= len(item.Value) {
			copyItem.Value = copyItem.Value[:len(item.Value)]
			copy(copyItem.Value, item.Value)
		} else {
			copyItem.Value = make([]byte, len(item.Value))
			copy(copyItem.Value, item.Value)
		}
	} else {
		copyItem.Value = nil
	}

	return copyItem
}

func fastBinarySearch(arr []uint32, target uint32) int {
	left, right := 0, len(arr)
	for left < right {
		mid := left + (right-left)/2
		if arr[mid] < target {
			left = mid + 1
		} else {
			right = mid
		}
	}
	return left
}

// fastKeyDedup removes duplicate keys keeping the highest version.
func fastKeyDedup(ops []*CacheSyncOperation) []*CacheSyncOperation {
	if len(ops) == 0 {
		return nil
	}

	keyMap := make(map[string]*CacheSyncOperation, len(ops))
	result := make([]*CacheSyncOperation, 0, len(ops))

	for _, op := range ops {
		if op.Key != "" {
			if existing, exists := keyMap[op.Key]; !exists || op.ClientVersion > existing.ClientVersion {
				keyMap[op.Key] = op
			}
		} else {
			result = append(result, op)
		}
	}

	for _, op := range keyMap {
		result = append(result, op)
	}

	return result
}

func fastProtoClone(msg interface{}) (interface{}, []byte, error) {
	// For binary protocol, we don't need protobuf marshaling
	// This function is kept for compatibility but should not be used with binary protocol
	if gm, ok := msg.(*GossipMessage); ok {
		clone := CloneGossipMessage(gm)
		// For binary protocol, we serialize directly
		binary := convertGossipMessageToBinary(clone)
		if binary != nil {
			data := binary.Marshal()
			PutBinaryMessage(binary)
			return clone, data, nil
		}
	}
	return nil, nil, fmt.Errorf("unsupported message type for cloning")
}

func fastProtoCloneForSign(msg *GossipMessage) (*GossipMessage, []byte, error) {
	clone := CloneGossipMessage(msg)
	clone.Signature = nil

	// Serialize using binary protocol
	binary := convertGossipMessageToBinary(clone)
	if binary == nil {
		return nil, nil, fmt.Errorf("failed to convert message to binary")
	}
	data := binary.Marshal()
	PutBinaryMessage(binary)

	dataCopy := make([]byte, len(data))
	copy(dataCopy, data)

	return clone, dataCopy, nil
}

//go:inline
func fastMin(a, b int) int {
	if a < b {
		return a
	}
	return b
}

//go:inline
func fastMax(a, b int) int {
	if a > b {
		return a
	}
	return b
}

//go:inline
func fastAbs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

//go:inline
func optimizedStorageItemToProto(item *storage.StoredItem) *StoredItem {
	if item == nil {
		return nil
	}
	var expire uint64
	if !item.ExpireAt.IsZero() {
		expire = uint64(item.ExpireAt.Unix())
	}

	valueCopy := make([]byte, len(item.Value))
	copy(valueCopy, item.Value)

	return &StoredItem{
		ExpireAt: expire,
		Value:    valueCopy,
	}
}

//go:inline
func optimizedProtoItemToStorage(item *StoredItem, version int64) *storage.StoredItem {
	if item == nil {
		return nil
	}
	var expire time.Time
	if item.ExpireAt != 0 {
		expire = time.Unix(int64(item.ExpireAt), 0)
	}

	valueCopy := make([]byte, len(item.Value))
	copy(valueCopy, item.Value)

	return &storage.StoredItem{
		ExpireAt: expire,
		Version:  version,
		Value:    valueCopy,
	}
}

// logPanicWithStack logs a panic with stack trace for debugging.
// Merged from panic_utils.go for file consolidation.
//
//go:inline
func logPanicWithStack(context string, r interface{}) {
	if r == nil {
		return
	}
	logging.Error(nil, context, "panic", r, "stack", string(debug.Stack()))
}
