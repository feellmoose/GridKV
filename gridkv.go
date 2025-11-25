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
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feellmoose/gridkv/internal/gossip"
	"github.com/feellmoose/gridkv/internal/storage"
	"github.com/feellmoose/gridkv/internal/utils/crypto"
	"github.com/feellmoose/gridkv/internal/utils/logging"
)

// ErrShuttingDown indicates the node has begun graceful shutdown and no longer accepts new operations.
var ErrShuttingDown = errors.New("gridkv shutting down")

// GridKV is the distributed key-value cache instance.
//
// Components: gossip manager (membership/replication), storage backend, network transport,
// consistent hash ring. Thread-safe.
type GridKV struct {
	gm       *gossip.GossipManager
	store    gossip.KVStore
	network  gossip.Network
	hashRing *gossip.ConsistentHash
	ttl      time.Duration

	stopOnce     sync.Once
	shutdownOnce sync.Once
	shuttingDown atomic.Bool
}

func (g *GridKV) isShuttingDown() bool {
	return g != nil && g.shuttingDown.Load()
}

// NewGridKV initializes a GridKV instance.
//
// Required: LocalNodeID, LocalAddress. Optional: Network (default QUIC), Storage (default MemorySharded),
// SeedAddrs (empty for first node), ReplicaCount (default 3). Setup time: ~10-50ms.
func NewGridKV(opts *GridKVOptions) (*GridKV, error) {
	if opts == nil {
		return nil, errors.New("GridKVOptions cannot be nil")
	}

	// Validate required options
	if opts.LocalNodeID == "" {
		return nil, errors.New("LocalNodeID is required")
	}
	if opts.LocalAddress == "" {
		return nil, errors.New("LocalAddress is required")
	}

	// Apply all defaults (profile, network, storage)
	applyDefaults(opts)

	// Initialize logging (optional)
	if opts.Log != nil {
		logging.Log = logging.NewLogger(opts.Log)
	}

	// Convert public NetworkOptions to internal gossip.NetworkOptions
	networkType := gossip.TCP
	if opts.Network.Type == QUIC {
		networkType = gossip.QUIC
	} else if opts.Network.Type == UDP {
		networkType = gossip.UDP
	}

	// Set default network options if not specified
	internalNetworkOpts := &gossip.NetworkOptions{
		Type:         networkType,
		BindAddr:     opts.Network.BindAddr,
		MaxIdle:      opts.Network.MaxIdle,
		MaxConns:     opts.Network.MaxConns,
		Timeout:      opts.Network.Timeout,
		ReadTimeout:  opts.Network.ReadTimeout,
		WriteTimeout: opts.Network.WriteTimeout,
	}

	// Create network layer
	network, err := gossip.NewNetwork(internalNetworkOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to create network: %w", err)
	}

	// Convert public StorageOptions to internal storage.StorageOptions
	internalStorageOpts := &storage.StorageOptions{
		Backend:     storage.StorageBackendType(opts.Storage.Backend),
		MaxMemoryMB: opts.Storage.MaxMemoryMB,
		ShardCount:  opts.Storage.ShardCount,
	}

	// Create storage backend using the registry pattern (build-tag aware)
	// This allows conditional compilation - backends with external dependencies
	// can be excluded via build tags to reduce binary size and dependencies
	rawStore, err := storage.NewStorage(internalStorageOpts)
	if err != nil {
		network.Stop()
		return nil, fmt.Errorf("failed to create storage backend: %w", err)
	}

	// Wrap with gossip bridge for proto type conversion
	store := gossip.NewStorageBridge(rawStore)

	// Create consistent hash ring
	hashRing := gossip.NewConsistentHash(opts.VirtualNodes, nil)

	// Generate or load cryptographic key pair for message signing
	var keypair *crypto.KeyPair
	if opts.KeyPair != nil {
		keypair = opts.KeyPair
	} else {
		var err error
		keypair, err = crypto.GenerateKeyPair()
		if err != nil {
			network.Stop()
			store.Close()
			return nil, fmt.Errorf("failed to generate keypair: %w", err)
		}
	}

	// Initialize peer public keys map (for signature verification)
	// Pre-size to accommodate seed nodes + local node
	estimatedPeers := len(opts.SeedAddrs) + 1
	peerPubkeys := make(map[string]crypto.PublicKey, estimatedPeers)
	if opts.PeerPublicKeys != nil {
		peerPubkeys = opts.PeerPublicKeys
	}
	// Add own public key
	peerPubkeys[opts.LocalNodeID] = keypair.Pub

	// Create gossip options
	gossipOpts := &gossip.GossipOptions{
		LocalNodeID:        opts.LocalNodeID,
		LocalAddress:       opts.LocalAddress,
		SeedAddrs:          opts.SeedAddrs,
		FailureTimeout:     opts.FailureTimeout,
		SuspectTimeout:     opts.SuspectTimeout,
		GossipInterval:     opts.GossipInterval,
		ReplicaCount:       opts.ReplicaCount,
		MaxReplicators:     opts.MaxReplicators,
		ReplicationTimeout: opts.ReplicationTimeout,
		ReadTimeout:        opts.ReadTimeout,
		DisableAuth:        opts.DisableAuth,
		StartupGracePeriod: opts.StartupGracePeriod,
		// Optional tuning
		MigrateRateLimitPerSec:    opts.MigrateRateLimitPerSec,
		ReadRepairRateLimitPerSec: opts.ReadRepairRateLimitPerSec,
		HotReadCacheTTL:           opts.HotReadCacheTTL,
		Metrics:                   opts.Metrics,
	}

	// Create gossip manager
	manager, err := gossip.NewGossipManager(gossipOpts, hashRing, network, store, keypair, peerPubkeys)
	if err != nil {
		network.Stop()
		store.Close()
		return nil, fmt.Errorf("failed to create gossip manager: %w", err)
	}

	gridKV := &GridKV{
		gm:       manager,
		store:    store,
		network:  network,
		hashRing: hashRing,
		ttl:      opts.TTL,
	}

	// Start gossip manager
	manager.Start()

	// Setup network message handler
	if err := network.Listen(func(msg *gossip.GossipMessage) error {
		manager.SimulateReceive(msg)
		return nil
	}); err != nil {
		gridKV.Close()
		return nil, fmt.Errorf("failed to start network listener: %w", err)
	}

	logging.Info("GridKV initialized successfully", "nodeID", opts.LocalNodeID, "address", opts.LocalAddress)
	return gridKV, nil
}

// Set stores a key-value pair with eventual replication.
//
// Computes replica set via consistent hashing, writes locally, enqueues async replication.
// Returns immediately. TTL overrides default (0 = no expiration). Local: ~100ns.
// Panic-safe, thread-safe.
func (g *GridKV) Set(ctx context.Context, key string, value []byte, ttl ...time.Duration) (err error) {
	// SAFETY: Recover from panics to prevent application crash
	var item *storage.StoredItem
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in Set operation: %v", r)
			logging.Error(err, "Set panic recovered", "key", key, "stack", string(debug.Stack()))
			// Ensure object pool item is returned even on panic
			if item != nil {
				storage.PutStoredItem(item)
			}
		}
	}()

	if g.gm == nil {
		return errors.New("GridKV not initialized")
	}
	if g.isShuttingDown() {
		return ErrShuttingDown
	}
	if key == "" {
		return errors.New("key cannot be empty")
	}

	// Fast path readiness check using atomic variable (no lock)
	// Prevents data loss during startup when cluster is not fully stable
	if !g.gm.IsReady() {
		// Cluster not ready - do full check for detailed error message
		status := g.GetReplicaStatus()
		if !status.Ready {
			return fmt.Errorf("cluster not ready: cannot Set key %s (nodes: %d, healthy: %d)",
				key, status.ClusterSize, status.HealthyNodes)
		}
	}

	// Create StoredItem with TTL from object pool
	item = storage.GetStoredItem()
	item.Value = value
	item.Version = time.Now().UnixNano() // Use timestamp as version

	// Determine TTL: use provided TTL if available, otherwise use default
	// If ttl is provided (even if 0), it overrides the default TTL
	if len(ttl) > 0 {
		// TTL provided: use it (0 means no expiration, overriding default)
		if ttl[0] > 0 {
			item.ExpireAt = time.Now().Add(ttl[0])
		} else {
			item.ExpireAt = time.Time{} // No expiration (explicit override)
		}
	} else {
		// No TTL provided: use default TTL from options
		if g.ttl > 0 {
			item.ExpireAt = time.Now().Add(g.ttl)
		} else {
			item.ExpireAt = time.Time{} // No expiration (zero value)
		}
	}

	// Delegate to gossip manager for distributed write
	// Note: The gossip manager will copy the data, so we can reuse the item
	err = g.gm.Set(ctx, key, item)

	// Return item to pool after use
	storage.PutStoredItem(item)

	return err
}

// Get retrieves a value by key.
//
// Reads locally if available, otherwise forwards to coordinator with retries.
// Returns freshest value, triggers read-repair on version mismatch. Local: ~50ns, LAN <1ms, WAN ~50ms.
// Returns deep copy. Panic-safe, thread-safe.
func (g *GridKV) Get(ctx context.Context, key string) (value []byte, err error) {
	// SAFETY: Recover from panics to prevent application crash
	var item *storage.StoredItem
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in Get operation: %v", r)
			logging.Error(err, "Get panic recovered", "key", key, "stack", string(debug.Stack()))
			value = nil
			// Ensure object pool item is returned even on panic
			if item != nil {
				storage.PutStoredItem(item)
			}
		}
	}()

	if g.gm == nil {
		return nil, errors.New("gridkv not initialized")
	}
	if key == "" {
		return nil, errors.New("key cannot be empty")
	}
	if g.isShuttingDown() {
		return nil, ErrShuttingDown
	}

	// Get operation uses eventual-consistency path (no readiness check needed)

	// Delegate to gossip manager for distributed read
	item, err = g.gm.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	// Defensive: ensure non-nil item on success to avoid nil dereference
	if item == nil {
		return nil, storage.ErrItemNotFound
	}

	// Check expiration first (fastest check)
	now := time.Now()
	if !item.ExpireAt.IsZero() && now.After(item.ExpireAt) {
		storage.PutStoredItem(item)
		return nil, storage.ErrItemExpired
	}

	// Defensive: ensure Value is not nil to avoid panic on append
	if len(item.Value) == 0 {
		storage.PutStoredItem(item)
		return nil, storage.ErrItemNotFound
	}

	// SAFETY: Make a deep copy of Value to ensure user can safely modify it
	value = make([]byte, len(item.Value))
	copy(value, item.Value)

	// Return StoredItem to object pool
	storage.PutStoredItem(item)

	return value, nil
}

// ReadFuture represents an asynchronous read operation.
type ReadFuture interface {
	Get(ctx context.Context) ([]byte, error)
	GetWithTimeout(timeout time.Duration) ([]byte, error)
	Done() <-chan struct{}
	Cancel()
	IsDone() bool
}

type readFutureWrapper struct {
	inner gossip.ReadFuture
}

func (rf *readFutureWrapper) Get(ctx context.Context) ([]byte, error) {
	if rf.inner == nil {
		return nil, errors.New("read future is nil")
	}
	item, err := rf.inner.Get(ctx)
	if err != nil {
		return nil, err
	}
	if item == nil {
		return nil, storage.ErrItemNotFound
	}
	// Defensive: ensure Value is not nil
	if item.Value == nil {
		storage.PutStoredItem(item)
		return nil, storage.ErrItemNotFound
	}
	value := make([]byte, len(item.Value))
	copy(value, item.Value)
	storage.PutStoredItem(item)
	return value, nil
}

func (rf *readFutureWrapper) GetWithTimeout(timeout time.Duration) ([]byte, error) {
	item, err := rf.inner.GetWithTimeout(timeout)
	if err != nil {
		return nil, err
	}
	if item == nil {
		return nil, storage.ErrItemNotFound
	}
	value := make([]byte, len(item.Value))
	copy(value, item.Value)
	storage.PutStoredItem(item)
	return value, nil
}

func (rf *readFutureWrapper) Done() <-chan struct{} {
	return rf.inner.Done()
}

func (rf *readFutureWrapper) Cancel() {
	rf.inner.Cancel()
}

func (rf *readFutureWrapper) IsDone() bool {
	return rf.inner.IsDone()
}

// GetAsync performs an asynchronous read, returning a Future for concurrent batch operations.
func (g *GridKV) GetAsync(ctx context.Context, key string) (ReadFuture, error) {
	if g.gm == nil {
		return nil, errors.New("GridKV not initialized")
	}
	if key == "" {
		return nil, errors.New("key cannot be empty")
	}
	if g.isShuttingDown() {
		return nil, ErrShuttingDown
	}

	return &readFutureWrapper{inner: g.gm.GetAsync(ctx, key)}, nil
}

// BatchReadFuture represents multiple asynchronous read operations.
type BatchReadFuture interface {
	GetAll(ctx context.Context) (map[string][]byte, map[string]error)
	GetAllWithTimeout(timeout time.Duration) (map[string][]byte, map[string]error)
	GetAny(ctx context.Context) (string, []byte, error)
	Cancel()
	Done() <-chan struct{}
	Count() int
}

type batchReadFutureWrapper struct {
	inner gossip.BatchReadFuture
}

func (brf *batchReadFutureWrapper) GetAll(ctx context.Context) (map[string][]byte, map[string]error) {
	items, errs := brf.inner.GetAll(ctx)
	results := make(map[string][]byte, len(items))
	for key, item := range items {
		if item != nil {
			value := make([]byte, len(item.Value))
			copy(value, item.Value)
			results[key] = value
			storage.PutStoredItem(item)
		}
	}
	return results, errs
}

func (brf *batchReadFutureWrapper) GetAllWithTimeout(timeout time.Duration) (map[string][]byte, map[string]error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return brf.GetAll(ctx)
}

func (brf *batchReadFutureWrapper) GetAny(ctx context.Context) (string, []byte, error) {
	key, item, err := brf.inner.GetAny(ctx)
	if err != nil {
		return key, nil, err
	}
	if item == nil {
		return key, nil, storage.ErrItemNotFound
	}
	value := make([]byte, len(item.Value))
	copy(value, item.Value)
	storage.PutStoredItem(item)
	return key, value, nil
}

func (brf *batchReadFutureWrapper) Cancel() {
	brf.inner.Cancel()
}

func (brf *batchReadFutureWrapper) Done() <-chan struct{} {
	return brf.inner.Done()
}

func (brf *batchReadFutureWrapper) Count() int {
	return brf.inner.Count()
}

// GetBatchAsync performs asynchronous batch reads with automatic batching.
func (g *GridKV) GetBatchAsync(ctx context.Context, keys []string) (BatchReadFuture, error) {
	if g.gm == nil {
		return nil, errors.New("GridKV not initialized")
	}
	if g.isShuttingDown() {
		return nil, ErrShuttingDown
	}
	if len(keys) == 0 {
		return &batchReadFutureWrapper{inner: g.gm.GetBatchAsync(ctx, nil)}, nil
	}

	return &batchReadFutureWrapper{inner: g.gm.GetBatchAsync(ctx, keys)}, nil
}

// Delete removes a key-value pair.
//
// Writes tombstone locally, enqueues async replication. Idempotent (non-existent key returns nil).
// Local: ~100ns. Panic-safe, thread-safe.
func (g *GridKV) Delete(ctx context.Context, key string) (err error) {
	// SAFETY: Recover from panics to prevent application crash
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in Delete operation: %v", r)
			logging.Error(err, "Delete panic recovered", "key", key)
		}
	}()

	if g.gm == nil {
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
	if err != nil {
		if err == storage.ErrItemNotFound {
			return nil // Already deleted
		}
		return err
	}

	// Delegate to gossip manager for distributed delete
	return g.gm.Delete(ctx, key, item.Version)
}

// GetReplicaStatus returns cluster health and readiness state. Thread-safe.
func (g *GridKV) GetReplicaStatus() ReplicaStatus {
	if g.gm == nil {
		return ReplicaStatus{
			Ready:         false,
			ClusterSize:   0,
			HealthyNodes:  0,
			ReplicaFactor: 0,
			LocalNodeID:   "",
			PubkeysReady:  false,
			PubkeyCount:   0,
			PeerCount:     0,
		}
	}

	// Get status from gossip manager and convert to public type
	gmStatus := g.gm.GetReplicaStatus()
	return ReplicaStatus{
		Ready:         gmStatus.Ready,
		ClusterSize:   gmStatus.ClusterSize,
		HealthyNodes:  gmStatus.HealthyNodes,
		ReplicaFactor: gmStatus.ReplicaFactor,
		LocalNodeID:   gmStatus.LocalNodeID,
		PubkeysReady:  gmStatus.PubkeysReady,
		PubkeyCount:   gmStatus.PubkeyCount,
		PeerCount:     gmStatus.PeerCount,
	}
}

// WaitReady blocks until cluster is ready (nodes in ring, keys exchanged, stable grace period) or timeout.
func (g *GridKV) WaitReady(timeout time.Duration) error {
	if g.gm == nil {
		return errors.New("GridKV not initialized")
	}

	deadline := time.Now().Add(timeout)
	checkInterval := 100 * time.Millisecond
	stabilityGracePeriod := 500 * time.Millisecond // Stability check period
	var firstReadyTime *time.Time
	var lastClusterSize, lastHealthyNodes, lastPubkeyCount int

	for time.Now().Before(deadline) {
		status := g.GetReplicaStatus()

		// Check if system meets readiness criteria
		isReady := status.Ready && status.PubkeysReady && status.HealthyNodes > 0

		if isReady {
			// Stability check: ensure status doesn't change during grace period
			if firstReadyTime == nil {
				// First time we're ready - record timestamp
				now := time.Now()
				firstReadyTime = &now
				lastClusterSize = status.ClusterSize
				lastHealthyNodes = status.HealthyNodes
				lastPubkeyCount = status.PubkeyCount
			} else {
				// Check if status has been stable
				stableDuration := time.Since(*firstReadyTime)

				// If status changed, reset stability check
				if status.ClusterSize != lastClusterSize ||
					status.HealthyNodes != lastHealthyNodes ||
					status.PubkeyCount != lastPubkeyCount {
					now := time.Now()
					firstReadyTime = &now
					lastClusterSize = status.ClusterSize
					lastHealthyNodes = status.HealthyNodes
					lastPubkeyCount = status.PubkeyCount
				} else if stableDuration >= stabilityGracePeriod {
					// System has been stable for grace period - ready!
					logging.Info("GridKV fully ready and stable",
						"nodes", status.HealthyNodes,
						"clusterSize", status.ClusterSize,
						"replicaFactor", status.ReplicaFactor,
						"pubkeys", status.PubkeyCount,
						"peers", status.PeerCount)
					return nil
				}
			}
		} else {
			// Not ready yet - reset stability check
			firstReadyTime = nil

			// Provide detailed progress logs
			if status.Ready && !status.PubkeysReady {
				logging.Debug("Waiting for public key exchange",
					"pubkeys", status.PubkeyCount,
					"peers", status.PeerCount)
			} else if !status.Ready {
				logging.Debug("Waiting for cluster formation",
					"nodes", status.ClusterSize,
					"healthy", status.HealthyNodes)
			}
		}

		time.Sleep(checkInterval)
	}

	// Timeout reached - check final status
	status := g.GetReplicaStatus()

	// Provide specific error message based on what failed
	if !status.Ready {
		return fmt.Errorf("timeout waiting for replica system ready: nodes=%d, healthy=%d, clusterSize=%d",
			status.ClusterSize, status.HealthyNodes, status.ClusterSize)
	} else if !status.PubkeysReady {
		return fmt.Errorf("timeout waiting for public key exchange: got %d/%d keys",
			status.PubkeyCount, status.PeerCount)
	} else {
		return fmt.Errorf("timeout waiting for cluster stability: cluster may still be converging")
	}
}

// HealthCheck verifies GridKV is initialized and cluster has healthy nodes. Thread-safe.
func (g *GridKV) HealthCheck() error {
	if g.gm == nil {
		return errors.New("GridKV not initialized")
	}

	status := g.gm.GetReplicaStatus()
	if !status.Ready {
		return fmt.Errorf("replica system not ready: nodes=%d, healthy=%d",
			status.ClusterSize, status.HealthyNodes)
	}

	if status.HealthyNodes == 0 {
		return errors.New("no healthy nodes available")
	}

	return nil
}

// Close shuts down GridKV: stops gossip, closes network, flushes storage. ~10-100ms.
// Idempotent. Thread-safe.
func (g *GridKV) Close() error {
	return g.CloseWithTimeout(30 * time.Second)
}

// CloseWithTimeout shuts down with explicit timeout. Thread-safe.
func (g *GridKV) CloseWithTimeout(timeout time.Duration) error {
	var errs []error
	deadline := time.Now().Add(timeout)

	done := make(chan struct{})
	var closeErr error
	go func() {
		defer close(done)

		g.shutdownOnce.Do(func() {
			g.shuttingDown.Store(true)
		})

		if g.gm != nil {
			g.gm.BeginShutdown()
		}

		if g.gm != nil {
			ctx, cancel := context.WithDeadline(context.Background(), deadline)
			if err := g.gm.WaitForDrain(ctx); err != nil && !errors.Is(err, context.Canceled) {
				logging.Warn("wait for replication drain", "err", err)
			}
			cancel()
		}

		if g.gm != nil {
			g.stopOnce.Do(func() {
				g.gm.Stop()
			})
		}

		if g.network != nil {
			if err := g.network.Stop(); err != nil {
				errs = append(errs, fmt.Errorf("network stop failed: %w", err))
			}
		}

		if g.store != nil {
			if err := g.store.Close(); err != nil {
				errs = append(errs, fmt.Errorf("store close failed: %w", err))
			}
		}

		if len(errs) > 0 {
			closeErr = fmt.Errorf("errors during close: %v", errs)
		} else {
			logging.Info("GridKV closed successfully")
		}
	}()

	select {
	case <-done:
		return closeErr
	case <-time.After(time.Until(deadline)):
		return fmt.Errorf("close timeout after %v", timeout)
	}
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
