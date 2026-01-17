package cluster

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"time"

	"github.com/feellmoose/gridkv/internal/mem_storage"
	"github.com/feellmoose/gridkv/internal/network"
	"github.com/feellmoose/gridkv/internal/utils/cache"
	"github.com/feellmoose/gridkv/internal/utils/executor"
	"github.com/feellmoose/gridkv/internal/utils/hlc"
	"github.com/feellmoose/gridkv/internal/utils/lifecycle"
	"github.com/feellmoose/gridkv/internal/utils/logging"
	"github.com/feellmoose/gridkv/internal/utils/zerocopy"
)

// Cluster is unified cluster component that manages all distributed operations
type Cluster struct {
	member  *memberMgr
	ring    *hashRing
	writer  *writer
	gossip  *gossip
	reader  *reader
	repair  *readRepair
	entropy *antiEntropy
	replay  *replay

	hlc       *hlc.HLC
	store     *mem_storage.MemStorage
	executor  *executor.Exec
	cache     *cache.Cache
	lifecycle *lifecycle.LifecycleManager
}

// Config configures Cluster
type Config struct {
	NodeID  string
	Address string
	Store   *mem_storage.MemStorage
	HLC     *hlc.HLC

	// Membership
	PingInterval   time.Duration
	FailureTimeout time.Duration
	SuspectTimeout time.Duration

	// Hash ring
	VirtualNodes int
	ReplicaCount int

	// Writer
	BatchThreshold int
	BatchWindow    time.Duration

	// Gossip
	GossipInterval time.Duration

	// Reader
	CacheTTL time.Duration

	// Anti-entropy
	EntropyInterval time.Duration

	// Network layer (optional, if nil will use placeholder functions)
	Network network.Network

	// Network functions (optional, used if Network is nil)
	SendFunc func(address string, msg interface{}) error
	GetFunc  func(nodeID string, key string) (*mem_storage.StoredItem, error)

	// Read repair
	ReadRepairRateLimitPerSec int64
}

// Executor configuration constants
const (
	workerMultiplier        = 4                    // Workers = CPU cores * multiplier
	minWorkerCount          = 16                    // Minimum workers for basic functionality
	maxWorkerCount          = 256                   // Maximum workers to prevent excessive goroutines
	requestsPerWorker       = 3000                 // Queue size per worker
	maxQueueSize            = 300000               // Maximum queue size to prevent memory issues
	cacheShards             = 256                  // Cache shard count
	cacheSize               = 10000                // Cache size
	cacheCleanupInterval    = 1 * time.Second     // Cache cleanup interval
	sendTimeout             = 5 * time.Second       // Network send timeout
	defaultCheckpointInterval = 1 * time.Minute     // Default checkpoint interval
)

// New creates a new Cluster
func New(cfg Config) (*Cluster, error) {
	// Dynamic worker count for massive concurrency support
	workerCount := runtime.NumCPU() * workerMultiplier
	if workerCount < minWorkerCount {
		workerCount = minWorkerCount
	}
	if workerCount > maxWorkerCount {
		workerCount = maxWorkerCount
	}

	// Massive queue size for handling bursts of concurrent requests
	// Further increased for high-load scenarios to reduce rejections
	queueSize := workerCount * requestsPerWorker
	if queueSize > maxQueueSize {
		queueSize = maxQueueSize
	}

	exec, err := executor.New(executor.Opts{
		Name:        "executor",
		Workers:     workerCount,
		QueueSize:   queueSize,
		NonBlocking: true, // Non-blocking for high concurrency, with proper error handling
	})
	if err != nil {
		return nil, err
	}

	// Cache for hot reads
	hotCache := cache.New(cache.Opts{
		Shards:        cacheShards,
		Size:          cacheSize,
		CleanupIntv:   cacheCleanupInterval,
		EnableCleanup: true,
	})

	// Create lifecycle manager
	lm := lifecycle.New()

	// Setup network adapter if provided
	var sendFunc func(address string, msg interface{}) error
	var getFunc func(nodeID string, key string) (*mem_storage.StoredItem, error)
	var net network.Network

	if cfg.Network != nil {
		net = cfg.Network
		// Wrap SendFunc to handle struct messages
		rawSendFunc := net.SendFunc()
		sendFunc = func(address string, msg interface{}) error {
			if b, ok := msg.([]byte); ok {
				return rawSendFunc(address, b)
			}
			// For struct messages, encode as Message and send
			msgData, err := encodeMemberMsg(msg)
			if err != nil {
				return err
			}
			netMsg := &network.Message{
				Type:      getMessageType(msg),
				ID:        uint64(time.Now().UnixNano()),
				Data:      msgData,
				Timestamp: time.Now().UnixNano(),
			}
			encoded, err := network.EncodeMessage(netMsg)
			if err != nil {
				return err
			}
			// Use timeout context for Send to avoid hanging
			const sendTimeout = 5 * time.Second
			ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
			defer cancel()
			return net.Send(ctx, address, encoded)
		}
		getFunc = nil
	} else {
		if cfg.SendFunc == nil {
			return nil, errors.New("Network is nil, SendFunc must be provided")
		}
		sendFunc = cfg.SendFunc
		getFunc = cfg.GetFunc
	}

	// Create hash ring first (needed for callback)
	ring := newHashRing(cfg.VirtualNodes)

	// Create member manager (callback will be set after creation)
	member, err := newMemberMgr(memberConfig{
		NodeID:         cfg.NodeID,
		Address:        cfg.Address,
		PingInterval:   cfg.PingInterval,
		FailureTimeout: cfg.FailureTimeout,
		SuspectTimeout: cfg.SuspectTimeout,
		SendFunc:       sendFunc,
	})
	if err != nil {
		return nil, err
	}

	// Helper function to update hash ring from member list
	updateRing := func() {
		members := member.Members()
		nodeIDs := make([]string, 0, len(members))
		for _, m := range members {
			if m.State == NodeStateAlive {
				nodeIDs = append(nodeIDs, m.NodeID)
			}
		}
		if len(nodeIDs) > 0 {
			ring.Update(time.Now().UnixNano(), nodeIDs)
		}
	}

	// Set callback after member is created
	member.onMembershipChange = updateRing

	// Initialize ring with local node (member already has self added)
	updateRing()

	// Create gossip (after member is created)
	var gossipSendFunc func(address string, data []byte) error
	if net != nil {
		gossipSendFunc = func(address string, data []byte) error {
			netMsg := &network.Message{
				Type:      network.ClusterMessageTypes.GossipPush,
				ID:        uint64(time.Now().UnixNano()),
				Data:      data,
				Timestamp: time.Now().UnixNano(),
			}
			encoded, err := network.EncodeMessage(netMsg)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return net.Send(ctx, address, encoded)
		}
	} else {
		// When Network is nil, use sendFunc to send gossip messages
		// sendFunc accepts interface{}, so []byte can be passed directly
		rawSendFunc := sendFunc
		gossipSendFunc = func(address string, data []byte) error {
			if rawSendFunc == nil {
				return errors.New("gossip sendFunc not available: Network is nil and SendFunc not provided")
			}
			return rawSendFunc(address, data)
		}
	}

	gossip, err := newGossip(gossipConfig{
		NodeID:       cfg.NodeID,
		Store:        cfg.Store,
		Ring:         ring,
		ReplicaCount: cfg.ReplicaCount,
		Executor:     exec,
		HLC:          cfg.HLC,
		Interval:     cfg.GossipInterval,
		Member:       member,
		SendFunc:     gossipSendFunc,
	})
	if err != nil {
		return nil, err
	}

	// Create read repair first (writer will be set after creation)
	repair := newReadRepair(readRepairConfig{
		Executor:        exec,
		RateLimitPerSec: cfg.ReadRepairRateLimitPerSec,
	})

	// Create writer
	writer, err := newWriter(writerConfig{
		NodeID:         cfg.NodeID,
		HLC:            cfg.HLC,
		Store:          cfg.Store,
		Ring:           ring,
		Gossip:         gossip,
		Cache:          hotCache,
		Executor:       exec,
		Member:         member,
		BatchThreshold: cfg.BatchThreshold,
		BatchWindow:    cfg.BatchWindow,
		ReplicaCount:   cfg.ReplicaCount,
	})
	if err != nil {
		return nil, err
	}

	repair.writer = writer

	// Setup GetFunc if network is provided
	if getFunc == nil && net != nil {
		getFunc = createGetFunc(net, member, cfg.Store)
	}

	// Create reader
	reader, err := newReader(readerConfig{
		NodeID:   cfg.NodeID,
		Store:    cfg.Store,
		Ring:     ring,
		Member:   member,
		Cache:    hotCache,
		Executor: exec,
		Repair:   repair,
		CacheTTL: cfg.CacheTTL,
		GetFunc:  getFunc,
	})
	if err != nil {
		return nil, err
	}

	// Create anti-entropy (after writer and gossip are created)
	entropy := newAntiEntropy(antiEntropyConfig{
		Store:    cfg.Store,
		Executor: exec,
		Interval: cfg.EntropyInterval,
		Member:   member,
		Gossip:   gossip,
		Writer:   writer,
	})

	// Create replay (after writer and gossip are created)
	replay := newReplay(replayConfig{
		Writer:         writer,
		Store:          cfg.Store,
		Executor:       exec,
		Gossip:         gossip,
		Member:         member,
		CheckpointIntv: defaultCheckpointInterval,
	})

	cluster := &Cluster{
		member:    member,
		ring:      ring,
		writer:    writer,
		gossip:    gossip,
		reader:    reader,
		repair:    repair,
		entropy:   entropy,
		replay:    replay,
		hlc:       cfg.HLC,
		store:     cfg.Store,
		executor:  exec,
		cache:     hotCache,
		lifecycle: lm,
	}

	// Register components with lifecycle
	// Dependency order: network -> storage -> executor -> cache -> member -> ring -> writer/gossip -> reader/repair -> entropy/replay
	// Register infrastructure components first (no dependencies) - they now implement lifecycle.Component directly
	if net != nil {
		lm.Register(net)
	}
	
	lm.Register(cfg.Store)
	
	lm.Register(exec, "storage")
	
	if hotCache != nil {
		lm.Register(hotCache, "storage")
	}
	
	// Register cluster components
	lm.Register(member, "executor")
	lm.Register(ring, "member-mgr")
	lm.Register(writer, "member-mgr")
	lm.Register(gossip, "member-mgr")
	lm.Register(reader, "gossip", "cache")
	lm.Register(repair, "gossip")
	lm.Register(entropy, "gossip")
	lm.Register(replay, "gossip")

	// Register network message handlers if network is provided
	if net != nil {
		logging.Info("Setting up network handlers", "node", cfg.NodeID)
		if err := setupNetworkHandlers(net, member, gossip, cfg.Store); err != nil {
			return nil, fmt.Errorf("failed to setup network handlers: %w", err)
		}
		logging.Info("Network handlers setup complete", "node", cfg.NodeID)
	} else {
		logging.Info("No network provided, skipping handler setup", "node", cfg.NodeID)
	}

	return cluster, nil
}

// createGetFunc creates GetFunc using network
// Returns function that deserializes complete StoredItem from remote node
func createGetFunc(net network.Network, member *memberMgr, store *mem_storage.MemStorage) func(nodeID string, key string) (*mem_storage.StoredItem, error) {
	return func(nodeID string, key string) (*mem_storage.StoredItem, error) {
		// Find node address
		members := member.Members()
		var address string
		for _, m := range members {
			if m.NodeID == nodeID {
				address = m.Address
				break
			}
		}
		if address == "" {
			return nil, fmt.Errorf("node %s not found in member list", nodeID)
		}

		// Send read request with proper timeout context - increased for distributed keys
		ctx, cancel := context.WithTimeout(context.Background(), 3000*time.Millisecond)
		defer cancel()

		respData, err := net.Request(ctx, address, []byte(key), 2*time.Second)
		if err != nil {
			return nil, fmt.Errorf("remote read failed for key %s on node %s: %w", key, nodeID, err)
		}
		if len(respData) == 0 {
			return nil, nil // Key not found
		}

		// Deserialize StoredItem from response (includes version from remote node)
		// Format: keyLen(2) + key + version(8) + opType(1) + expireAt(8) + valueLen(4) + value
		if len(respData) < 2 {
			return nil, fmt.Errorf("invalid response format from node %s", nodeID)
		}
		
		offset := 0
		
		// Key length (2 bytes)
		keyLen := int(binary.LittleEndian.Uint16(respData[offset:]))
		offset += 2
		
		if offset+keyLen+8+1+8+4 > len(respData) {
			// Fallback for old format (just value) - for backward compatibility
			return &mem_storage.StoredItem{
				Key:     key,
				Value:   respData,
				Version: time.Now().UnixNano(),
			}, nil
		}
		
		// Skip key (we already know it)
		offset += keyLen
		
		// Version (8 bytes) - this is the HLC version from remote node
		version := int64(binary.LittleEndian.Uint64(respData[offset:]))
		offset += 8
		
		// OpType (1 byte) - skip
		offset++
		
		// ExpireAt (8 bytes)
		expireAt := int64(binary.LittleEndian.Uint64(respData[offset:]))
		offset += 8
		
		// Value length (4 bytes)
		valueLen := int(binary.LittleEndian.Uint32(respData[offset:]))
		offset += 4
		
		if offset+valueLen > len(respData) {
			return nil, fmt.Errorf("invalid response format from node %s: value length mismatch", nodeID)
		}
		
		// Value
		var value []byte
		if valueLen > 0 {
			value = zerocopy.FastCloneBytes(respData[offset : offset+valueLen])
		}
		
		var expireTime time.Time
		if expireAt > 0 {
			expireTime = time.Unix(0, expireAt)
		}
		
		return &mem_storage.StoredItem{
			Key:      key,
			Value:    value,
			Version:  version, // Use HLC version from remote node
			ExpireAt: expireTime,
		}, nil
	}
}

// setupNetworkHandlers registers message handlers for member and gossip
func setupNetworkHandlers(net network.Network, member *memberMgr, gossip *gossip, store *mem_storage.MemStorage) error {
	// Member message handlers
	if err := net.RegisterMessageHandler(network.MessageTypePing, func(ctx context.Context, remoteAddr string, data []byte) ([]byte, error) {
		decoded := decodeMemberMsg(data, network.MessageTypePing)
		if decoded != nil {
			// Handle message (error ignored as this is async message handling)
			if err := member.HandleMessage(decoded); err != nil {
				logging.Debug("Failed to handle ping message", "remote", remoteAddr, "error", err)
			}
		}
		return nil, nil
	}); err != nil {
		return err
	}

	if err := net.RegisterMessageHandler(network.MessageTypeConnect, func(ctx context.Context, remoteAddr string, data []byte) ([]byte, error) {
		if len(data) < 2 {
			return nil, nil
		}
		decoded := decodeMemberMsg(data, network.MessageTypeConnect)
		if decoded == nil {
			return nil, nil
		}
		if err := member.HandleMessage(decoded); err != nil {
			logging.Debug("failed to handle CONNECT message", "remote", remoteAddr, "error", err)
		}
		return nil, nil
	}); err != nil {
		return err
	}

	if err := net.RegisterMessageHandler(network.MessageTypeLeave, func(ctx context.Context, remoteAddr string, data []byte) ([]byte, error) {
		decoded := decodeMemberMsg(data, network.MessageTypeLeave)
		if decoded != nil {
			// Handle message (error ignored as this is async message handling)
			if err := member.HandleMessage(decoded); err != nil {
				logging.Debug("Failed to handle leave message", "remote", remoteAddr, "error", err)
			}
		}
		return nil, nil
	}); err != nil {
		return err
	}

	// Gossip message handlers
	// Note: Gossip messages use prefix format ("PUSH:" or "PULL:nodeID:")
	// The network layer passes Message.Data directly to handlers
	if err := net.RegisterMessageHandler(network.MessageTypeGossipPush, func(ctx context.Context, remoteAddr string, data []byte) ([]byte, error) {
		// Handle gossip message (error ignored as this is async message handling)
		if err := gossip.HandleMessage(data); err != nil {
			logging.Debug("Failed to handle gossip push message", "remote", remoteAddr, "error", err, "dataLen", len(data))
		}
		return nil, nil
	}); err != nil {
		return err
	}

	if err := net.RegisterMessageHandler(network.MessageTypeGossipPull, func(ctx context.Context, remoteAddr string, data []byte) ([]byte, error) {
		// Handle gossip message (error ignored as this is async message handling)
		if err := gossip.HandleMessage(data); err != nil {
			logging.Debug("Failed to handle gossip pull message", "remote", remoteAddr, "error", err, "dataLen", len(data))
		}
		return nil, nil
	}); err != nil {
		return err
	}

	// Unified read request handler - handles network.MessageTypeRequest used by clients
	if err := net.RegisterMessageHandler(network.MessageTypeRequest, func(ctx context.Context, remoteAddr string, data []byte) ([]byte, error) {
		if len(data) == 0 {
			return nil, nil
		}
		// Use zerocopy to avoid allocation
		key := zerocopy.BytesToString(data)
		item, _ := store.Get(key)
		if item == nil {
			return nil, nil
		}
		return item.Value, nil
	}); err != nil {
		return err
	}

	// Also register MessageTypeReadRequest for cluster-internal communication
	// Returns serialized StoredItem with full metadata (version, expireAt, etc.)
	if err := net.RegisterMessageHandler(network.MessageTypeReadRequest, func(ctx context.Context, remoteAddr string, data []byte) ([]byte, error) {
		if len(data) == 0 {
			return nil, nil
		}
		// Use zerocopy to avoid allocation
		key := zerocopy.BytesToString(data)
		item, _ := store.Get(key)
		if item == nil {
			return nil, nil
		}
		// Serialize complete StoredItem (including version) using codec format
		// Format: keyLen(2) + key + version(8) + opType(1) + expireAt(8) + valueLen(4) + value
		keyBytes := zerocopy.StringToBytes(item.Key)
		keyLen := len(keyBytes)
		valueLen := 0
		if item.Value != nil {
			valueLen = len(item.Value)
		}
		expireAt := int64(0)
		if !item.ExpireAt.IsZero() {
			expireAt = item.ExpireAt.UnixNano()
		}
		
		// Estimate size: 2 + keyLen + 8 + 1 + 8 + 4 + valueLen
		totalSize := 2 + keyLen + 8 + 1 + 8 + 4 + valueLen
		buf := make([]byte, 0, totalSize)
		
		// Key length (2 bytes)
		buf = append(buf, byte(keyLen&0xFF), byte(keyLen>>8))
		buf = append(buf, keyBytes...)
		
		// Version (8 bytes)
		buf = append(buf, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.LittleEndian.PutUint64(buf[len(buf)-8:], uint64(item.Version))
		
		// OpType (1 byte: 0=Set)
		buf = append(buf, 0)
		
		// ExpireAt (8 bytes)
		buf = append(buf, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.LittleEndian.PutUint64(buf[len(buf)-8:], uint64(expireAt))
		
		// Value length (4 bytes) + value
		buf = append(buf, 0, 0, 0, 0)
		binary.LittleEndian.PutUint32(buf[len(buf)-4:], uint32(valueLen))
		if valueLen > 0 {
			buf = append(buf, item.Value...)
		}
		
		return buf, nil
	}); err != nil {
		return err
	}

	return nil
}

// encodeMemberMsg encodes member message to bytes (delegates to codec)
func encodeMemberMsg(msg interface{}) ([]byte, error) {
	return EncodeMemberMsg(msg)
}

// decodeMemberMsg decodes member message from bytes (delegates to codec)
// Converts network.MessageType to internal uint8 mapping for codec
func decodeMemberMsg(data []byte, msgType network.MessageType) interface{} {
	if len(data) == 0 {
		return nil
	}
	var msgTypeUint8 uint8
	switch msgType {
	case network.MessageTypePing:
		msgTypeUint8 = 1 // Ping, ACK, IndirectProbe all use type 1
	case network.MessageTypeConnect:
		msgTypeUint8 = 2 // Connect, ClusterSync both use type 2
	case network.MessageTypeLeave:
		msgTypeUint8 = 3
	case network.MessageTypeGossipPush, network.MessageTypeGossipPull:
		return data // gossip uses raw bytes
	default:
		return nil
	}
	return DecodeMemberMsg(data, msgTypeUint8)
}

// getMessageType returns MessageType for member message
func getMessageType(msg interface{}) network.MessageType {
	switch msg.(type) {
	case *pingMsg:
		return network.MessageTypePing
	case *ackMsg:
		return network.MessageTypePing // ack uses same type
	case *connectMsg:
		return network.MessageTypeConnect
	case *leaveMsg:
		return network.MessageTypeLeave
	case *indirectProbeMsg:
		return network.MessageTypePing
	case *clusterSyncMsg:
		return network.MessageTypeConnect
	}
	return network.MessageTypeUnknown
}

// Start starts the cluster
func (c *Cluster) Start(ctx context.Context) error {
	return c.lifecycle.Start(ctx)
}

// Stop stops the cluster
func (c *Cluster) Stop(ctx context.Context) error {
	// Update hash ring with current members before shutdown
	members := c.member.Members()
	nodeIDs := make([]string, 0, len(members))
	for _, m := range members {
		if m.State == NodeStateAlive {
			nodeIDs = append(nodeIDs, m.NodeID)
		}
	}
	if len(nodeIDs) > 0 {
		c.ring.Update(time.Now().UnixNano(), nodeIDs)
	}

	// Close all lifecycle-managed components (including infrastructure)
	// Lifecycle manager handles dependency order and cleanup
	if err := c.lifecycle.Close(ctx); err != nil {
		return err
	}

	return nil
}

// Join joins the cluster
func (c *Cluster) Join(seed []string) error {
	if err := c.member.Join(seed); err != nil {
		return err
	}

	// Update hash ring after joining (use retry with timeout instead of fixed sleep)
	members := c.member.Members()
	// Retry a few times if no members yet (membership may need time to propagate)
	for i := 0; i < 5 && len(members) == 0; i++ {
		time.Sleep(20 * time.Millisecond)
		members = c.member.Members()
	}
	nodeIDs := make([]string, 0, len(members))
	for _, m := range members {
		if m.State == NodeStateAlive {
			nodeIDs = append(nodeIDs, m.NodeID)
		}
	}
	if len(nodeIDs) > 0 {
		c.ring.Update(time.Now().UnixNano(), nodeIDs)
	}

	return nil
}

// Leave leaves the cluster
func (c *Cluster) Leave() error {
	return c.member.Leave()
}

// Set writes a key-value pair
func (c *Cluster) Set(ctx context.Context, key string, value []byte) error {
	item := &mem_storage.StoredItem{
		Value: value,
	}
	return c.writer.Set(ctx, key, item)
}

// Get reads a key-value pair
func (c *Cluster) Get(ctx context.Context, key string) ([]byte, error) {
	item, err := c.reader.Get(ctx, key)
	if err != nil {
		if errors.Is(err, mem_storage.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if item == nil {
		return nil, nil
	}
	return item.Value, nil
}

// Delete deletes a key
func (c *Cluster) Delete(ctx context.Context, key string) error {
	item, _ := c.store.Get(key)
	version := int64(0)
	if item != nil {
		version = item.Version
	}
	return c.writer.Delete(ctx, key, version)
}

// Members returns current cluster members
func (c *Cluster) Members() []NodeInfo {
	return c.member.Members()
}

// MemberMgr returns member manager
func (c *Cluster) MemberMgr() MemberMgr {
	return c.member
}

// HashRing returns hash ring
func (c *Cluster) HashRing() HashRing {
	return c.ring
}

// Writer returns writer
func (c *Cluster) Writer() Writer {
	return c.writer
}

// Reader returns reader
func (c *Cluster) Reader() Reader {
	return c.reader
}
