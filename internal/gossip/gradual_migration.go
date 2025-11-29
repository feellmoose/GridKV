package gossip

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feellmoose/gridkv/internal/utils/logging"
)

// gradualMigrationState tracks the state of gradual data migration
type gradualMigrationState struct {
	mu                sync.RWMutex
	activeMigrations  map[string]*migrationTask // key: nodeID, value: migration task
	migrationProgress map[string]int64          // key: nodeID, value: migrated keys count
	lastMigrationTime map[string]time.Time      // key: nodeID, value: last migration time
}

type migrationTask struct {
	nodeID        string
	startTime     time.Time
	totalKeys     int64
	migratedKeys  atomic.Int64
	fetchedKeys   atomic.Int64
	status        string // "running", "paused", "completed"
	rateLimit     int    // keys per second
	lastBatchTime time.Time
}

// gradualMigrationManager manages gradual data migration to reduce impact of hashring changes
type gradualMigrationManager struct {
	gm              *GossipManager
	state           *gradualMigrationState
	migrationRate   int           // keys per second per migration
	batchSize       int           // keys per batch
	batchInterval   time.Duration // interval between batches
	maxConcurrent   int           // max concurrent migrations
	activeCount     atomic.Int32  // current active migration count
	stopCh          chan struct{} // stop signal for migrations
	stopOnce        sync.Once     // ensure stopCh is only closed once
	shutdownOnce    sync.Once
	shutdownPending atomic.Int64
}

const (
	shutdownReplicationMaxAttempts = 6
	shutdownReplicationBackoff     = 200 * time.Millisecond
)

// newGradualMigrationManager creates a new gradual migration manager
func newGradualMigrationManager(gm *GossipManager) *gradualMigrationManager {
	state := &gradualMigrationState{
		activeMigrations:  make(map[string]*migrationTask),
		migrationProgress: make(map[string]int64),
		lastMigrationTime: make(map[string]time.Time),
	}
	return &gradualMigrationManager{
		gm:            gm,
		state:         state,
		migrationRate: 100,                    // 100 keys/second default
		batchSize:     50,                     // 50 keys per batch
		batchInterval: 500 * time.Millisecond, // 500ms between batches
		maxConcurrent: 3,                      // max 3 concurrent migrations
		stopCh:        make(chan struct{}),
	}
}

// startGradualMigration starts a gradual migration for a node addition/removal
// This reduces the impact of hashring changes by spreading migration over time
func (gmm *gradualMigrationManager) startGradualMigration(nodeID string, isRemoval bool) {
	gmm.state.mu.Lock()
	defer gmm.state.mu.Unlock()

	// Check if migration already exists
	if _, exists := gmm.state.activeMigrations[nodeID]; exists {
		return
	}

	// Check concurrent limit
	if gmm.activeCount.Load() >= int32(gmm.maxConcurrent) {
		logging.Debug("Migration limit reached, queuing migration", "node", nodeID)
		// Queue for later (could implement a queue here)
		return
	}

	task := &migrationTask{
		nodeID:        nodeID,
		startTime:     time.Now(),
		status:        "running",
		rateLimit:     gmm.migrationRate,
		lastBatchTime: time.Now(),
	}

	gmm.state.activeMigrations[nodeID] = task
	gmm.state.migrationProgress[nodeID] = 0
	gmm.state.lastMigrationTime[nodeID] = time.Now()
	gmm.activeCount.Add(1)

	// Start gradual migration in background
	go gmm.runGradualMigration(nodeID, isRemoval, task)
}

// runGradualMigration performs gradual migration
func (gmm *gradualMigrationManager) runGradualMigration(nodeID string, isRemoval bool, task *migrationTask) {
	defer func() {
		gmm.activeCount.Add(-1)
		gmm.state.mu.Lock()
		delete(gmm.state.activeMigrations, nodeID)
		gmm.state.mu.Unlock()
	}()

	if gmm.gm.store == nil {
		return
	}

	// Get all keys
	allKeys := gmm.gm.store.Keys()
	if len(allKeys) == 0 {
		return
	}

	// Filter affected keys
	affectedKeys := gmm.filterAffectedKeysForMigration(allKeys, nodeID, isRemoval)
	if len(affectedKeys) == 0 {
		return
	}

	task.totalKeys = int64(len(affectedKeys))
	logging.Info("Starting gradual migration", "node", nodeID, "keys", len(affectedKeys), "isRemoval", isRemoval)

	// Process keys in small batches
	for i := 0; i < len(affectedKeys); i += gmm.batchSize {
		// Check for stop signal
		select {
		case <-gmm.stopCh:
			logging.Debug("Migration stopped", "node", nodeID)
			return
		default:
		}

		// Check if migration should pause (e.g., high load)
		if task.status != "running" {
			select {
			case <-gmm.stopCh:
				return
			case <-time.After(gmm.batchInterval * 2):
			}
			continue
		}

		// Wait if needed
		elapsed := time.Since(task.lastBatchTime)
		expectedInterval := time.Duration(gmm.batchSize) * time.Second / time.Duration(gmm.migrationRate)
		if elapsed < expectedInterval {
			select {
			case <-gmm.stopCh:
				return
			case <-time.After(expectedInterval - elapsed):
			}
		}

		end := i + gmm.batchSize
		if end > len(affectedKeys) {
			end = len(affectedKeys)
		}
		batch := affectedKeys[i:end]

		// Process batch
		migrated, fetched := gmm.migrateBatch(batch, nodeID, isRemoval)
		task.migratedKeys.Add(int64(migrated))
		task.fetchedKeys.Add(int64(fetched))

		// Update progress
		gmm.state.mu.Lock()
		gmm.state.migrationProgress[nodeID] = task.migratedKeys.Load()
		gmm.state.lastMigrationTime[nodeID] = time.Now()
		gmm.state.mu.Unlock()

		task.lastBatchTime = time.Now()

		// Log progress periodically
		if i%500 == 0 || i+gmm.batchSize >= len(affectedKeys) {
			progress := float64(task.migratedKeys.Load()) / float64(task.totalKeys) * 100
			logging.Info("Migration progress", "node", nodeID, "progress", progress, "migrated", task.migratedKeys.Load(), "total", task.totalKeys)
		}

		// Small delay between batches
		select {
		case <-gmm.stopCh:
			return
		case <-time.After(gmm.batchInterval):
		}
	}

	logging.Info("Gradual migration completed", "node", nodeID, "migrated", task.migratedKeys.Load(), "fetched", task.fetchedKeys.Load())
	task.status = "completed"
}

// filterAffectedKeysForMigration filters keys that need migration
func (gmm *gradualMigrationManager) filterAffectedKeysForMigration(allKeys []string, nodeID string, isRemoval bool) []string {
	if len(allKeys) == 0 {
		return nil
	}

	gmm.gm.mu.RLock()
	clusterSize := len(gmm.gm.liveNodes)
	gmm.gm.mu.RUnlock()

	if clusterSize <= 1 {
		return nil
	}

	affectedKeys := make([]string, 0, len(allKeys)/10) // Pre-allocate

	// For removal: keys that were on the removed node need to be migrated
	// For addition: keys that should now be on the new node need to be fetched
	for _, key := range allKeys {
		// Get current replica list (after hashring change)
		replicas := gmm.gm.getReplicas(key, gmm.gm.replicaCount)

		if isRemoval {
			// Check if this key needs migration (was on removed node, now on different node)
			// We check if local node is now responsible but wasn't before
			isLocalReplica := false
			for _, replicaID := range replicas {
				if replicaID == gmm.gm.localNodeID {
					isLocalReplica = true
					break
				}
			}

			if isLocalReplica {
				// Check if we have the data
				_, err := gmm.gm.store.Get(key)
				if err != nil {
					// We're responsible but don't have data - need to fetch
					affectedKeys = append(affectedKeys, key)
				}
			}
		} else {
			// For addition: check if new node should have this key
			// Handled by normal replication
			// But we can proactively fetch if needed
		}
	}

	return affectedKeys
}

// migrateBatch migrates a batch of keys
func (gmm *gradualMigrationManager) migrateBatch(keys []string, nodeID string, isRemoval bool) (migratedCount, fetchedCount int) {
	for _, key := range keys {
		m, f := gmm.gm.migrateSingleKey(key, nodeID)
		if m {
			migratedCount++
		}
		if f {
			fetchedCount++
		}
	}
	return migratedCount, fetchedCount
}

// pauseMigration pauses a migration (e.g., during high load)
func (gmm *gradualMigrationManager) pauseMigration(nodeID string) {
	gmm.state.mu.Lock()
	defer gmm.state.mu.Unlock()

	if task, exists := gmm.state.activeMigrations[nodeID]; exists {
		task.status = "paused"
	}
}

// resumeMigration resumes a paused migration
func (gmm *gradualMigrationManager) resumeMigration(nodeID string) {
	gmm.state.mu.Lock()
	defer gmm.state.mu.Unlock()

	if task, exists := gmm.state.activeMigrations[nodeID]; exists {
		task.status = "running"
	}
}

// getMigrationStatus returns the status of a migration
func (gmm *gradualMigrationManager) getMigrationStatus(nodeID string) (progress float64, status string) {
	gmm.state.mu.RLock()
	defer gmm.state.mu.RUnlock()

	if task, exists := gmm.state.activeMigrations[nodeID]; exists {
		if task.totalKeys > 0 {
			progress = float64(task.migratedKeys.Load()) / float64(task.totalKeys) * 100
		}
		status = task.status
		return progress, status
	}

	// Check if migration completed
	if _, exists := gmm.state.migrationProgress[nodeID]; exists {
		return 100, "completed"
	}

	return 0, "not found"
}

// stop stops all active migrations and waits for them to complete
// Stage 2 Sleep优化: 使用context超时替代固定sleep轮询
func (gmm *gradualMigrationManager) stop() {
	gmm.stopOnce.Do(func() {
		close(gmm.stopCh)
	})

	// Wait for active migrations to complete with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Poll with context cancellation instead of fixed sleep
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		if gmm.activeCount.Load() == 0 {
			return // All migrations completed
		}

		select {
		case <-ctx.Done():
			// Timeout reached
			if gmm.activeCount.Load() > 0 {
				logging.Warn("Migration stop timeout - some migrations may still be active")
			}
			return
		case <-ticker.C:
			// Continue polling
		}
	}
}

func (gmm *gradualMigrationManager) startShutdownMigration() {
	if gmm == nil {
		return
	}
	gmm.shutdownOnce.Do(func() {
		go gmm.runShutdownMigration()
	})
}

func (gmm *gradualMigrationManager) runShutdownMigration() {
	gm := gmm.gm
	if gm == nil || gm.store == nil {
		return
	}
	keys := gm.store.Keys()
	totalKeys := int64(len(keys))
	gmm.shutdownPending.Store(totalKeys)
	if len(keys) == 0 {
		return
	}
	defer gmm.shutdownPending.Store(0)
	logging.Info("Starting shutdown data sync", "node", gm.localNodeID, "keys", len(keys))

	batchSize := gm.maxReplicators
	if batchSize <= 0 {
		batchSize = 16
	}
	delay := gmm.batchInterval
	if delay <= 0 {
		delay = 50 * time.Millisecond
	}

	for i := 0; i < len(keys); i += batchSize {
		end := i + batchSize
		if end > len(keys) {
			end = len(keys)
		}

		var wg sync.WaitGroup
		for _, key := range keys[i:end] {
			key := key
			wg.Add(1)
			worker := func() {
				defer wg.Done()
				if !gmm.replicateKeyForShutdown(key) {
					logging.Warn("Shutdown replication exhausted retries", "node", gm.localNodeID, "key", key)
				}
				gmm.shutdownPending.Add(-1)
			}

			if gm.replicationPool != nil {
				if err := gm.replicationPool.Submit(worker); err != nil {
					worker()
				}
			} else {
				worker()
			}
		}
		wg.Wait()
		time.Sleep(delay)
	}

	logging.Info("Shutdown data sync completed", "node", gm.localNodeID)
}

func (gmm *gradualMigrationManager) replicateKeyForShutdown(key string) bool {
	gm := gmm.gm
	if gm == nil || gm.store == nil {
		return false
	}

	item, err := gm.store.Get(key)
	if err != nil || item == nil {
		return true
	}

	gm.mu.RLock()
	clusterSize := len(gm.liveNodes)
	gm.mu.RUnlock()
	if clusterSize <= 1 {
		return true
	}

	effectiveReplicaCount := gm.replicaCount
	if clusterSize < effectiveReplicaCount {
		effectiveReplicaCount = clusterSize
	}
	if effectiveReplicaCount == 0 {
		return true
	}

	attemptWindow := gm.replicationTimeout * 8
	if attemptWindow < 5*time.Second {
		attemptWindow = 5 * time.Second
	}
	deadline := time.Now().Add(attemptWindow)

	attempts := shutdownReplicationMaxAttempts
	if attempts <= 0 {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		if time.Now().After(deadline) {
			break
		}
		targets := gmm.shutdownTargetsForKey(key, effectiveReplicaCount)
		if len(targets) == 0 {
			break
		}
		ctx, cancel := context.WithTimeout(context.Background(), gm.replicationTimeout*2)
		successes := gm.replicateSyncToTargets(ctx, key, item, targets)
		cancel()
		if successes > 0 {
			return true
		}
		time.Sleep(shutdownReplicationBackoff)
	}

	return false
}

func (gmm *gradualMigrationManager) shutdownTargetsForKey(key string, replicaCount int) []string {
	gm := gmm.gm
	if gm == nil {
		return nil
	}

	base := gm.getReplicas(key, replicaCount)
	seen := make(map[string]struct{}, len(base)+4)
	targets := make([]string, 0, replicaCount*2+4)

	appendTarget := func(id string) {
		if id == "" || id == gm.localNodeID {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		targets = append(targets, id)
	}

	for _, id := range base {
		appendTarget(id)
	}

	gm.mu.RLock()
	alive := make([]string, 0, len(gm.liveNodes))
	suspect := make([]string, 0, len(gm.liveNodes))
	for id, node := range gm.liveNodes {
		if id == gm.localNodeID || node == nil || node.State == NodeState_NODE_STATE_DEAD {
			continue
		}
		if node.State == NodeState_NODE_STATE_ALIVE {
			alive = append(alive, id)
		} else {
			suspect = append(suspect, id)
		}
	}
	gm.mu.RUnlock()

	for _, id := range alive {
		appendTarget(id)
	}
	for _, id := range suspect {
		appendTarget(id)
	}

	return targets
}

func (gmm *gradualMigrationManager) pendingShutdownKeys() int64 {
	if gmm == nil {
		return 0
	}
	return gmm.shutdownPending.Load()
}
