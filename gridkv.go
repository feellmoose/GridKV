// Package gridkv provides a distributed key-value cache with eventual consistency.
//
// Features: consistent hashing, batched replication, SWIM failure detection,
// adaptive LAN/WAN networking, Prometheus/OTLP metrics.
//
// Thread-safe: all public methods are safe for concurrent access.
package gridkv

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feellmoose/gridkv/internal/cluster"
	"github.com/feellmoose/gridkv/internal/mem_storage"
	"github.com/feellmoose/gridkv/internal/network"
	"github.com/feellmoose/gridkv/internal/utils/hlc"
	"github.com/feellmoose/gridkv/internal/utils/logging"
)

// Version represents the current version of GridKV
const Version = "v0.3.2"

// Error constants exported for application error handling
var (
	// ErrShuttingDown indicates the node has begun graceful shutdown
	ErrShuttingDown = errors.New("gridkv shutting down")

	// ErrItemNotFound indicates the requested key was not found
	ErrItemNotFound = mem_storage.ErrNotFound

	// ErrItemExpired indicates the requested item has expired
	ErrItemExpired = mem_storage.ErrExpired

	// ErrVersionMismatch indicates a version conflict
	ErrVersionMismatch = errors.New("version mismatch")

	// ErrMemoryLimitExceeded indicates memory limit reached
	ErrMemoryLimitExceeded = errors.New("memory limit exceeded")
)

// GridKV is the distributed key-value cache instance.
//
// Components: cluster (membership/replication), storage backend, network transport.
// Thread-safe.
type GridKV struct {
	cluster      *cluster.Cluster
	store        *mem_storage.MemStorage
	network      network.Network
	ttl          time.Duration
	replicaCount int

	shutdownOnce sync.Once
	shuttingDown atomic.Bool
}

// NewGridKV initializes a GridKV instance with the provided options.
//
// Required fields: LocalNodeID, LocalAddress
// Optional fields: Use Profile for automatic configuration
//
// Example:
//
//	opts := &gridkv.GridKVOptions{
//	    LocalNodeID:  "node-1",
//	    LocalAddress: "localhost:8080",
//	    SeedAddrs:    []string{"localhost:8081"},
//	}
//	kv, err := gridkv.NewGridKV(opts)
func NewGridKV(opts *GridKVOptions) (*GridKV, error) {
	if opts == nil {
		return nil, errors.New("GridKVOptions cannot be nil")
	}

	if opts.LocalNodeID == "" {
		return nil, errors.New("LocalNodeID is required")
	}
	if opts.LocalAddress == "" {
		return nil, errors.New("LocalAddress is required")
	}

	applyDefaults(opts)

	switch v := opts.Log.(type) {
	case LoggerOptions:
		logging.SetDefault(logging.New(logging.Opts{
			Level:      v.Level,
			Format:     v.Format,
			Output:     v.Output,
			TimeFormat: v.TimeFormat,
			NoCaller:   v.NoCaller,
			NoTime:     v.NoTime,
		}))
	case logging.Opts:
		logging.SetDefault(logging.New(v))
	case *logging.Logger:
		logging.SetDefault(v)
	}

	hlcInstance := hlc.NewHLC(opts.LocalNodeID)

	logging.Info("GridKV starting", "version", Version, "node_id", opts.LocalNodeID, "address", opts.LocalAddress)

	storeConfig := mem_storage.Config{
		MaxMemoryMB:          int64(opts.Storage.MaxMemoryMB),
		ShardCount:           opts.Storage.ShardCount,
		CompressionEnabled:   true,
		CompressionThreshold: 64,
		EvictThreshold:       90,
		EvictTarget:          80,
	}
	store, err := mem_storage.New(storeConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create storage: %w", err)
	}

	// Use BindAddr if provided, otherwise use LocalAddress
	bindAddr := opts.LocalAddress
	if opts.Network != nil && opts.Network.BindAddr != "" {
		bindAddr = opts.Network.BindAddr
	}
	netConfig := network.DefaultNetworkConfig(bindAddr)

	switch opts.Network.Type {
	case QUIC:
		netConfig.TransportType = network.TransportQUIC
	default:
		netConfig.TransportType = network.TransportTCP
	}

	netConfig.TransportConfig = network.DefaultTransportConfig()
	netConfig.TransportConfig.Type = netConfig.TransportType
	if opts.Network.Timeout > 0 {
		netConfig.TransportConfig.Timeout = opts.Network.Timeout
	}
	if opts.Network.ReadTimeout > 0 {
		netConfig.TransportConfig.ReadTimeout = opts.Network.ReadTimeout
	}
	if opts.Network.WriteTimeout > 0 {
		netConfig.TransportConfig.WriteTimeout = opts.Network.WriteTimeout
	}

	if opts.Network.MaxIdle > 0 {
		netConfig.PoolConfig.MaxIdle = opts.Network.MaxIdle
	}
	if opts.Network.MaxConns > 0 {
		netConfig.PoolConfig.MaxActive = opts.Network.MaxConns
	}
	if netConfig.PoolConfig.IdleTimeout == 0 {
		netConfig.PoolConfig.IdleTimeout = 30 * time.Second
	}
	if opts.Network.Timeout > 0 {
		netConfig.ClientConfig.DefaultTimeout = opts.Network.Timeout
	}

	net, err := network.NewNetwork(netConfig)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("failed to create network: %w", err)
	}

	// Use bindAddr for cluster config to ensure consistency
	clusterConfig := cluster.Config{
		NodeID:  opts.LocalNodeID,
		Address: bindAddr,
		Store:   store,
		HLC:     hlcInstance,
		Network: net,

		// Membership
		PingInterval:   opts.FailureTimeout / 3,
		FailureTimeout: opts.FailureTimeout,
		SuspectTimeout: opts.SuspectTimeout,

		// Hash ring
		VirtualNodes: opts.VirtualNodes,
		ReplicaCount: opts.ReplicaCount,

		// Writer
		BatchThreshold: 15,
		BatchWindow:    50 * time.Millisecond,

		// Gossip
		GossipInterval: opts.GossipInterval,

		// Reader
		CacheTTL: opts.HotReadCacheTTL,

		// Anti-entropy
		EntropyInterval: 5 * time.Minute,

		// Read repair
		ReadRepairRateLimitPerSec: opts.ReadRepairRateLimitPerSec,
	}

	c, err := cluster.New(clusterConfig)
	if err != nil {
		if stopErr := net.Stop(context.Background()); stopErr != nil {
			logging.Warn("failed to stop network during cleanup", "error", stopErr)
		}
		store.Close()
		return nil, fmt.Errorf("failed to create cluster: %w", err)
	}

	// Start cluster
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		if stopErr := c.Stop(ctx); stopErr != nil {
			logging.Warn("failed to stop cluster during cleanup", "error", stopErr)
		}
		if stopErr := net.Stop(ctx); stopErr != nil {
			logging.Warn("failed to stop network during cleanup", "error", stopErr)
		}
		store.Close()
		return nil, fmt.Errorf("failed to start cluster: %w", err)
	}

	// Start network
	if err := net.Start(ctx); err != nil {
		if stopErr := c.Stop(ctx); stopErr != nil {
			logging.Warn("failed to stop cluster during cleanup", "error", stopErr)
		}
		if stopErr := net.Stop(ctx); stopErr != nil {
			logging.Warn("failed to stop network during cleanup", "error", stopErr)
		}
		store.Close()
		return nil, fmt.Errorf("failed to start network on %s: %w", bindAddr, err)
	}

	// Start Join asynchronously if seed addresses provided
	// No delay needed - network.Start() is synchronous and server is ready immediately
	if len(opts.SeedAddrs) > 0 {
		go func() {
			maxRetries := 5
			retryDelay := 200 * time.Millisecond
			for i := 0; i < maxRetries; i++ {
				if err := c.Join(opts.SeedAddrs); err != nil {
					if i < maxRetries-1 {
						time.Sleep(retryDelay)
						retryDelay = time.Duration(float64(retryDelay) * 1.5)
						continue
					}
				} else {
					break
				}
			}
		}()
	}

	gridKV := &GridKV{
		cluster:      c,
		store:        store,
		network:      net,
		ttl:          opts.TTL,
		replicaCount: opts.ReplicaCount,
	}

	logging.Info("GridKV initialized successfully",
		"nodeID", opts.LocalNodeID, "address", opts.LocalAddress)
	return gridKV, nil
}

// Set stores a key-value pair with eventual replication.
//
// Computes replica set via consistent hashing, writes locally, enqueues async replication.
// Returns immediately. TTL overrides default (0 = no expiration).
// Panic-safe, thread-safe.
func (g *GridKV) Set(ctx context.Context, key string, value []byte, ttl ...time.Duration) (err error) {
	// SAFETY: Recover from panics
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in Set operation: %v", r)
			logging.Error(err, "Panic recovered during Set operation", "key", key)
		}
	}()

	if g.cluster == nil {
		return errors.New("GridKV not initialized")
	}
	if g.isShuttingDown() {
		return ErrShuttingDown
	}
	if key == "" {
		return errors.New("key cannot be empty")
	}

	// Determine TTL
	var expireAt time.Time
	if len(ttl) > 0 {
		if ttl[0] > 0 {
			expireAt = time.Now().Add(ttl[0])
		}
	} else if g.ttl > 0 {
		expireAt = time.Now().Add(g.ttl)
	}

	// Check cluster readiness for Set operations
	status := g.Status()
	if !status.Ready && status.HealthyNodes == 0 {
		return fmt.Errorf("cluster not ready: cannot Set key %s (nodes: %d, healthy: %d)",
			key, status.ClusterSize, status.HealthyNodes)
	}

	// Use Writer directly to support TTL
	writer := g.cluster.Writer()

	// Create item with TTL
	item := &mem_storage.StoredItem{
		Value:    value,
		ExpireAt: expireAt,
		Key:      key,
	}

	return writer.Set(ctx, key, item)
}

// Get retrieves a value by key.
//
// Reads locally if available, otherwise forwards to coordinator with retries.
// Returns freshest value, triggers read-repair on version mismatch.
// Returns deep copy. Panic-safe, thread-safe.
func (g *GridKV) Get(ctx context.Context, key string) (value []byte, err error) {
	// SAFETY: Recover from panics
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in Get operation: %v", r)
			logging.Error(err, "Panic recovered during Get operation", "key", key)
			value = nil
		}
	}()

	if g.cluster == nil {
		return nil, errors.New("GridKV not initialized")
	}
	if key == "" {
		return nil, errors.New("key cannot be empty")
	}
	if g.isShuttingDown() {
		return nil, ErrShuttingDown
	}

	// Use Reader to get value
	reader := g.cluster.Reader()

	item, err := reader.Get(ctx, key)
	if err != nil {
		// mem_storage may return ErrNotFound or ErrExpired
		if err == mem_storage.ErrNotFound || err == mem_storage.ErrExpired {
			return nil, ErrItemNotFound
		}
		return nil, err
	}
	if item == nil {
		return nil, ErrItemNotFound
	}

	// Check if tombstone FIRST (before expiration check)
	// Tombstone has Version > 0 but empty Value
	if item.IsTombstone() {
		return nil, ErrItemNotFound
	}

	// Check expiration
	if item.IsExpired() {
		return nil, ErrItemExpired
	}

	// Return deep copy - ensure Value is not empty
	if len(item.Value) == 0 {
		return nil, ErrItemNotFound
	}

	value = make([]byte, len(item.Value))
	copy(value, item.Value)
	return value, nil
}

// Delete removes a key-value pair.
//
// Writes tombstone locally, enqueues async replication. Idempotent.
// Panic-safe, thread-safe.
func (g *GridKV) Delete(ctx context.Context, key string) (err error) {
	// SAFETY: Recover from panics
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in Delete operation: %v", r)
			logging.Error(err, "Panic recovered during Delete operation", "key", key)
		}
	}()

	if g.cluster == nil {
		return errors.New("GridKV not initialized")
	}
	if key == "" {
		return errors.New("key cannot be empty")
	}
	if g.isShuttingDown() {
		return ErrShuttingDown
	}

	// Get current version for optimistic locking
	item, err := g.store.Get(key)
	version := int64(0)
	if err == nil && item != nil {
		version = item.Version
	}

	// Use Writer to delete
	writer := g.cluster.Writer()

	return writer.Delete(ctx, key, version)
}

// Status returns cluster health and readiness state. Thread-safe.
func (g *GridKV) Status() ReplicaStatus {
	if g.cluster == nil {
		return ReplicaStatus{
			Ready:         false,
			ClusterSize:   0,
			HealthyNodes:  0,
			ReplicaFactor: 0,
			LocalNodeID:   "",
		}
	}

	members := g.cluster.Members()
	healthyNodes := 0
	localNodeID := ""

	for _, m := range members {
		if m.State == cluster.NodeStateAlive {
			healthyNodes++
		}
		if localNodeID == "" {
			localNodeID = m.NodeID
		}
	}

	// Get replica count from stored config
	replicaCount := g.replicaCount
	if replicaCount <= 0 {
		replicaCount = 3 // fallback default
	}

	return ReplicaStatus{
		Ready:         healthyNodes > 0,
		ClusterSize:   len(members),
		HealthyNodes:  healthyNodes,
		ReplicaFactor: replicaCount,
		LocalNodeID:   localNodeID,
		PubkeysReady:  true, // v2 doesn't use pubkeys
		PubkeyCount:   0,
		PeerCount:     len(members) - 1,
	}
}

// WaitReady blocks until cluster is ready or timeout.
func (g *GridKV) WaitReady(timeout time.Duration) error {
	if g.cluster == nil {
		return errors.New("GridKV not initialized")
	}

	deadline := time.Now().Add(timeout)
	checkInterval := 100 * time.Millisecond
	stabilityGracePeriod := 500 * time.Millisecond
	var firstReadyTime *time.Time
	var lastClusterSize, lastHealthyNodes int

	for time.Now().Before(deadline) {
		status := g.Status()
		isReady := status.Ready && status.HealthyNodes > 0

		if isReady {
			if firstReadyTime == nil {
				now := time.Now()
				firstReadyTime = &now
				lastClusterSize = status.ClusterSize
				lastHealthyNodes = status.HealthyNodes
			} else {
				stableDuration := time.Since(*firstReadyTime)

				if status.ClusterSize != lastClusterSize ||
					status.HealthyNodes != lastHealthyNodes {
					now := time.Now()
					firstReadyTime = &now
					lastClusterSize = status.ClusterSize
					lastHealthyNodes = status.HealthyNodes
				} else if stableDuration >= stabilityGracePeriod {
					logging.Info("GridKV fully ready and stable",
						"nodes", status.HealthyNodes,
						"clusterSize", status.ClusterSize,
						"replicaFactor", status.ReplicaFactor)
					return nil
				}
			}
		} else {
			firstReadyTime = nil
		}

		time.Sleep(checkInterval)
	}

	status := g.Status()
	if !status.Ready {
		return fmt.Errorf("timeout waiting for cluster ready: nodes=%d, healthy=%d",
			status.ClusterSize, status.HealthyNodes)
	}
	return fmt.Errorf("timeout waiting for cluster stability")
}

// HealthCheck verifies GridKV is initialized and cluster has healthy nodes.
func (g *GridKV) HealthCheck() error {
	if g.cluster == nil {
		return errors.New("GridKV not initialized")
	}

	status := g.Status()
	if !status.Ready {
		return fmt.Errorf("cluster not ready: nodes=%d, healthy=%d",
			status.ClusterSize, status.HealthyNodes)
	}

	if status.HealthyNodes == 0 {
		return errors.New("no healthy nodes available")
	}

	return nil
}

// Close shuts down GridKV: stops cluster, closes network, flushes storage.
// Uses 30s default timeout if none provided. Idempotent. Thread-safe.
func (g *GridKV) Close(timeout ...time.Duration) error {
	defaultTimeout := 30 * time.Second
	if len(timeout) > 0 {
		defaultTimeout = timeout[0]
	}

	var errs []error
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	g.shutdownOnce.Do(func() {
		g.shuttingDown.Store(true)
	})

	// Stop cluster
	if g.cluster != nil {
		if err := g.cluster.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("cluster stop failed: %w", err))
		}
	}

	// Stop network
	if g.network != nil {
		if err := g.network.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("network stop failed: %w", err))
		}
	}

	// Close storage
	if g.store != nil {
		if err := g.store.Close(); err != nil {
			errs = append(errs, fmt.Errorf("store close failed: %w", err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("errors during close: %v", errs)
	}

	logging.Info("GridKV closed successfully")
	return nil
}

// ReplicaStatus represents cluster health and readiness state.
type ReplicaStatus struct {
	Ready         bool
	ClusterSize   int
	HealthyNodes  int
	ReplicaFactor int
	LocalNodeID   string
	PubkeysReady  bool
	PubkeyCount   int
	PeerCount     int
}

func (g *GridKV) isShuttingDown() bool {
	return g != nil && g.shuttingDown.Load()
}
