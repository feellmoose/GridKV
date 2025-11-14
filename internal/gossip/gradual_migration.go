package gossip

import (
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
	gm            *GossipManager
	state         *gradualMigrationState
	migrationRate int           // keys per second per migration
	batchSize     int           // keys per batch
	batchInterval time.Duration // interval between batches
	maxConcurrent int           // max concurrent migrations
	activeCount   atomic.Int32  // current active migration count
}

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

// runGradualMigration performs gradual migration with rate limiting
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

	// Process keys in small batches with rate limiting
	for i := 0; i < len(affectedKeys); i += gmm.batchSize {
		// Check if migration should pause (e.g., high load)
		if task.status != "running" {
			time.Sleep(gmm.batchInterval * 2) // Wait longer if paused
			continue
		}

		// Rate limiting: wait if needed
		elapsed := time.Since(task.lastBatchTime)
		expectedInterval := time.Duration(gmm.batchSize) * time.Second / time.Duration(gmm.migrationRate)
		if elapsed < expectedInterval {
			time.Sleep(expectedInterval - elapsed)
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
		time.Sleep(gmm.batchInterval)
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
		replicas := gmm.gm.hashRing.GetN(key, gmm.gm.replicaCount)

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
			// This is less critical, handled by normal replication
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
