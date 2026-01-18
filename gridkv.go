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
const Version = "v0.3.6"

// Error constants exported for application error handling
var (
	// ErrShuttingDown indicates the node has begun graceful shutdown
	ErrShuttingDown = errors.New("gridkv shutting down")

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

func (g *GridKV) GetNetwork() network.Network {
	return g.network
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
		// Default NoCaller to true for production (avoid source paths in logs)
		// Only print source paths if explicitly requested (NoCaller=false)
		noCaller := v.NoCaller
		// If NoCaller is not explicitly set (false by default in struct), default to true for production safety
		// This means: if user doesn't set NoCaller, we default to not printing source paths
		logging.SetDefault(logging.New(logging.Opts{
			Level:      v.Level,
			Format:     v.Format,
			Output:     v.Output,
			TimeFormat: v.TimeFormat,
			NoCaller:   noCaller,
			NoTime:     v.NoTime,
		}))
	case logging.Opts:
		logging.SetDefault(logging.New(v))
	case *logging.Logger:
		logging.SetDefault(v)
	}

	hlcInstance := hlc.NewHLC(opts.LocalNodeID)

	logging.Info("gridkv starting", "version", Version, "node_id", opts.LocalNodeID, "address", opts.LocalAddress)

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
	// Note: store.Close() is now managed by lifecycle, but we keep CloseNoContext() for backward compatibility

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
		_ = store.Close(context.Background())
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
		BatchThreshold: opts.BatchThreshold,
		BatchWindow:    opts.BatchWindow,

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
			logging.Debug("failed to stop network during cleanup", "error", stopErr)
		}
		_ = store.Close(context.Background())
		return nil, fmt.Errorf("failed to create cluster: %w", err)
	}

	// Start cluster - lifecycle manager handles all component startup in dependency order
	// This includes: network, storage, executor, cache, and all cluster components
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		// Cleanup: lifecycle manager will handle component shutdown
		if stopErr := c.Stop(ctx); stopErr != nil {
			logging.Debug("failed to stop cluster during cleanup", "error", stopErr)
		}
		// Manual cleanup for components not yet in lifecycle (shouldn't happen after migration)
		if stopErr := net.Stop(ctx); stopErr != nil {
			logging.Debug("failed to stop network during cleanup", "error", stopErr)
		}
		_ = store.Close(ctx)
		return nil, fmt.Errorf("failed to start cluster: %w", err)
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

	logging.Info("gridkv initialized", "node_id", opts.LocalNodeID, "address", opts.LocalAddress)
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
			logging.Error(err, "panic recovered during set operation", "key", key)
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
	stats := g.Stats()
	if !stats.Cluster.Ready && stats.Cluster.HealthyNodes == 0 {
		return fmt.Errorf("cluster not ready: cannot Set key %s (nodes: %d, healthy: %d)",
			key, stats.Cluster.ClusterSize, stats.Cluster.HealthyNodes)
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
// Returns deep copy.
//
// If the key is not found, returns (nil, nil). Only returns an error for
// real failures (network errors, timeouts, etc.).
//
// Panic-safe, thread-safe.
func (g *GridKV) Get(ctx context.Context, key string) (value []byte, err error) {
	// SAFETY: Recover from panics
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in Get operation: %v", r)
			logging.Error(err, "panic recovered during get operation", "key", key)
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
		return nil, err
	}

	if item == nil {
		return nil, nil
	}

	if item.IsTombstone() {
		return nil, nil
	}

	if item.IsExpired() {
		return nil, nil
	}

	if len(item.Value) == 0 {
		return nil, nil
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
			logging.Error(err, "panic recovered during delete operation", "key", key)
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

// Stats returns complete GridKV statistics including cluster, network and storage stats. Thread-safe.
func (g *GridKV) Stats() Stats {
	if g.cluster == nil {
		return Stats{
			Cluster: ClusterStats{
				Ready:         false,
				ClusterSize:   0,
				HealthyNodes:  0,
				ReplicaFactor: 0,
				LocalNodeID:   "",
			},
			Version: Version,
		}
	}

	// Get cluster status
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

	clusterStats := ClusterStats{
		Ready:         healthyNodes > 0,
		ClusterSize:   len(members),
		HealthyNodes:  healthyNodes,
		ReplicaFactor: replicaCount,
		LocalNodeID:   localNodeID,
		PubkeysReady:  true, // v2 doesn't use pubkeys
		PubkeyCount:   0,
		PeerCount:     len(members) - 1,
	}

	// Collect network stats
	var networkStats network.NetworkSnapshot
	if g.network != nil {
		networkStats = network.GetStats().Snapshot()
	}

	// Collect storage stats
	var storageStats mem_storage.Stats
	if g.store != nil {
		storageStats = g.store.Stats()
	}

	return Stats{
		Cluster: clusterStats,
		Network: networkStats,
		Storage: storageStats,
		Version: Version,
	}
}

// ClusterStats returns only cluster statistics (backward compatibility).
// Thread-safe.
func (g *GridKV) ClusterStats() ClusterStats {
	return g.Stats().Cluster
}

// Status is deprecated, use Stats instead.
func (g *GridKV) Status() Stats {
	return g.Stats()
}

// ReplicaStatus is deprecated, use ClusterStats instead.
type ReplicaStatus = ClusterStats

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
		stats := g.Stats()
		cluster := stats.Cluster
		isReady := cluster.Ready && cluster.HealthyNodes > 0

		if isReady {
			if firstReadyTime == nil {
				now := time.Now()
				firstReadyTime = &now
				lastClusterSize = cluster.ClusterSize
				lastHealthyNodes = cluster.HealthyNodes
			} else {
				stableDuration := time.Since(*firstReadyTime)

				if cluster.ClusterSize != lastClusterSize ||
					cluster.HealthyNodes != lastHealthyNodes {
					now := time.Now()
					firstReadyTime = &now
					lastClusterSize = cluster.ClusterSize
					lastHealthyNodes = cluster.HealthyNodes
				} else if stableDuration >= stabilityGracePeriod {
					logging.Info("gridkv ready and stable",
						"nodes", cluster.HealthyNodes,
						"cluster_size", cluster.ClusterSize,
						"replica_factor", cluster.ReplicaFactor)
					return nil
				}
			}
		} else {
			firstReadyTime = nil
		}

		time.Sleep(checkInterval)
	}

	stats := g.Stats()
	cluster := stats.Cluster
	if !cluster.Ready {
		return fmt.Errorf("timeout waiting for cluster ready: nodes=%d, healthy=%d",
			cluster.ClusterSize, cluster.HealthyNodes)
	}
	return fmt.Errorf("timeout waiting for cluster stability")
}

// HealthCheck verifies GridKV is initialized and cluster has healthy nodes.
func (g *GridKV) HealthCheck() error {
	if g.cluster == nil {
		return errors.New("GridKV not initialized")
	}

	stats := g.Stats()
	cluster := stats.Cluster
	if !cluster.Ready {
		return fmt.Errorf("cluster not ready: nodes=%d, healthy=%d",
			cluster.ClusterSize, cluster.HealthyNodes)
	}

	if cluster.HealthyNodes == 0 {
		return errors.New("no healthy nodes available")
	}

	return nil
}

// Close shuts down GridKV: stops cluster (which manages all components via lifecycle).
// Uses 30s default timeout if none provided. Idempotent. Thread-safe.
// All resources (network, storage, executor, cache) are managed by cluster's lifecycle manager.
func (g *GridKV) Close(timeout ...time.Duration) error {
	defaultTimeout := 30 * time.Second
	if len(timeout) > 0 {
		defaultTimeout = timeout[0]
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	g.shutdownOnce.Do(func() {
		g.shuttingDown.Store(true)
	})

	// Stop cluster - lifecycle manager handles all component shutdown in dependency order
	// This includes: network, storage, executor, cache, and all cluster components
	if g.cluster != nil {
		if err := g.cluster.Stop(ctx); err != nil {
			return fmt.Errorf("cluster stop failed: %w", err)
		}
	}

	logging.Info("gridkv closed")
	return nil
}

// ClusterStats represents cluster health and readiness statistics.
type ClusterStats struct {
	Ready         bool
	ClusterSize   int
	HealthyNodes  int
	ReplicaFactor int
	LocalNodeID   string
	PubkeysReady  bool
	PubkeyCount   int
	PeerCount     int
}

// Stats represents complete GridKV statistics including cluster, network and storage stats.
type Stats struct {
	// Cluster stats
	Cluster ClusterStats

	// Network stats
	Network network.NetworkSnapshot

	// Storage stats
	Storage mem_storage.Stats

	// Version information
	Version string
}

func (g *GridKV) isShuttingDown() bool {
	return g != nil && g.shuttingDown.Load()
}
