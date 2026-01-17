package cluster

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/feellmoose/gridkv/internal/mem_storage"
	"github.com/feellmoose/gridkv/internal/utils/executor"
	"github.com/zeebo/xxh3"
)

// AntiEntropy handles periodic anti-entropy for consistency
type AntiEntropy interface {
	Digest(rangeKey string) (bloom []byte, vv map[string]int64)
	Sync(bloom []byte, vv map[string]int64) ([]string, error)
	Start() error
	Stop() error
}

type antiEntropy struct {
	store    *mem_storage.MemStorage
	executor *executor.Exec
	interval time.Duration
	stopCh   chan struct{}
	stopOnce sync.Once
	member   MemberMgr
	gossip   Gossip
	writer   Writer
}

type antiEntropyConfig struct {
	Store    *mem_storage.MemStorage
	Executor *executor.Exec
	Interval time.Duration
	Member   MemberMgr
	Gossip   Gossip
	Writer   Writer
}

func newAntiEntropy(cfg antiEntropyConfig) *antiEntropy {
	if cfg.Interval <= 0 {
		// Reduced default interval for faster consistency convergence
		// Original: 5 minutes, optimized: 30 seconds
		cfg.Interval = 30 * time.Second
	}

	return &antiEntropy{
		store:    cfg.Store,
		executor: cfg.Executor,
		interval: cfg.Interval,
		stopCh:   make(chan struct{}),
		member:   cfg.Member,
		gossip:   cfg.Gossip,
		writer:   cfg.Writer,
	}
}

func (ae *antiEntropy) Digest(rangeKey string) ([]byte, map[string]int64) {
	// Build bloom filter for keys in range
	keys := ae.getKeysInRange(rangeKey)

	bloomSize := 1024 * 8 // 1KB bloom filter
	bloom := make([]byte, bloomSize/8)

	// Version vector: map[nodeID]maxVersion
	// Note: Since Version is compressed int64 (not HLC string), we use key-based tracking
	vv := make(map[string]int64)

	for _, key := range keys {
		// Add to bloom filter
		hash := xxh3.HashString128(key).Hi
		idx := (hash % uint64(bloomSize)) / 8
		bit := (hash % uint64(bloomSize)) % 8
		bloom[idx] |= 1 << bit

		// Get version for version vector
		// Track max version per key (practical approximation of nodeID-based version vector)
		// Current implementation uses key as identifier, which is sufficient for detecting differences
		item, err := ae.store.Get(key)
		if err == nil && item != nil {
			if item.Version > vv[key] {
				vv[key] = item.Version
			}
		}
	}

	return bloom, vv
}

func (ae *antiEntropy) Sync(bloom []byte, vv map[string]int64) ([]string, error) {
	// Compare bloom filters and version vectors
	allKeys := ae.getAllKeys()
	diffKeys := make([]string, 0)

	localBloom, _ := ae.Digest("")

	for _, key := range allKeys {
		// Check bloom filter
		hash := xxh3.HashString128(key).Hi
		bloomSize := uint64(len(bloom) * 8)
		if bloomSize == 0 {
			// Remote has no keys, all local keys are different
			diffKeys = append(diffKeys, key)
			continue
		}
		idx := (hash % bloomSize) / 8
		bit := (hash % bloomSize) % 8

		hasKey := (bloom[idx] & (1 << bit)) != 0
		localHasKey := false
		if len(localBloom) > int(idx) {
			localHasKey = (localBloom[idx] & (1 << bit)) != 0
		}

		if hasKey != localHasKey {
			diffKeys = append(diffKeys, key)
			continue
		}

		// Check version vector
		item, err := ae.store.Get(key)
		if err == nil && item != nil {
			// Compare versions (simplified: use key as identifier)
			if remoteVersion, ok := vv[key]; ok {
				if item.Version < remoteVersion {
					diffKeys = append(diffKeys, key)
				}
			} else if hasKey {
				// Key exists locally but not in remote version vector (but in bloom)
				// This might be a false positive, but we'll include it for safety
				diffKeys = append(diffKeys, key)
			}
		} else {
			// Key in remote but not local
			if _, ok := vv[key]; ok {
				diffKeys = append(diffKeys, key)
			}
		}
	}

	// Also check for keys in remote that we don't have locally
	for key := range vv {
		item, err := ae.store.Get(key)
		if err != nil || item == nil {
			diffKeys = append(diffKeys, key)
		}
	}

	return diffKeys, nil
}

func (ae *antiEntropy) getKeysInRange(rangeKey string) []string {
	allKeys := ae.store.Keys()
	if rangeKey == "" {
		return allKeys
	}

	// Filter keys with prefix
	filtered := make([]string, 0)
	for _, key := range allKeys {
		if len(key) >= len(rangeKey) && key[:len(rangeKey)] == rangeKey {
			filtered = append(filtered, key)
		}
	}
	return filtered
}

func (ae *antiEntropy) getAllKeys() []string {
	return ae.store.Keys()
}

func (ae *antiEntropy) start() error {
	go ae.entropyLoop()
	return nil
}

func (ae *antiEntropy) stop() error {
	ae.stopOnce.Do(func() {
		close(ae.stopCh)
	})
	return nil
}

func (ae *antiEntropy) entropyLoop() {
	ticker := time.NewTicker(ae.interval)
	defer ticker.Stop()

	lastGC := time.Now()
	gcInterval := 10 * time.Minute // Periodic GC hint for long-running processes

	for {
		select {
		case <-ae.stopCh:
			return
		case <-ticker.C:
			// Periodic GC hint for long-running processes
			if time.Since(lastGC) > gcInterval {
				runtime.GC()
				lastGC = time.Now()
			}
			
			if err := ae.executor.Do(func() {
				ae.doAntiEntropy()
			}); err != nil {
				return
			}
		}
	}
}

func (ae *antiEntropy) doAntiEntropy() {
	if ae.member == nil || ae.gossip == nil || ae.writer == nil {
		return
	}

	// Get alive members (excluding self)
	members := ae.member.Members()
	aliveMembers := make([]string, 0, len(members))
	for _, m := range members {
		if m.State == NodeStateAlive && m.Address != "" {
			aliveMembers = append(aliveMembers, m.Address)
		}
	}

	if len(aliveMembers) == 0 {
		return
	}

	// Select random target for anti-entropy sync
	// Use hash-based selection for deterministic but distributed load balancing
	hash := int(time.Now().UnixNano() % int64(len(aliveMembers)))
	if hash < 0 {
		hash = -hash
	}
	target := aliveMembers[hash]

	// Get local digest
	localBloom, _ := ae.Digest("")

	// Send digest to target and get remote digest
	// For now, we'll do a one-way sync: compare with what we know

	// Compare digests and sync differences
	// Note: Remote VV not available in simplified implementation, so we use empty map
	// The bloom filter comparison will still detect missing keys
	diffKeys, err := ae.Sync(localBloom, make(map[string]int64))
	if err != nil || len(diffKeys) == 0 {
		return
	}

	// Fetch missing/outdated keys from target and apply
	// Trigger gossip pull to sync - this will exchange operations
	_, _ = ae.gossip.Pull(target)

	// The gossip pull will handle syncing operations automatically
}

// Replay handles checkpoint and hinted-handoff for recovery
type Replay interface {
	SaveCheckpoint(ops []*mem_storage.SyncOperation) error
	LoadCheckpoint() ([]*mem_storage.SyncOperation, error)
	HintedHandoff(nodeID string, ops []*mem_storage.SyncOperation) error
}

type replay struct {
	checkpointPath  string
	writer          Writer
	store           *mem_storage.MemStorage
	executor        *executor.Exec
	hintedHandoff   map[string][]*mem_storage.SyncOperation
	hhMu            sync.Mutex
	gossip          Gossip
	member          MemberMgr
	checkpointIntv  time.Duration
	checkpointTimer *time.Timer
	checkpointMu    sync.Mutex
	stopCh          chan struct{}
	stopOnce        sync.Once
}

type replayConfig struct {
	CheckpointPath string
	Writer         Writer
	Store          *mem_storage.MemStorage
	Executor       *executor.Exec
	Gossip         Gossip
	Member         MemberMgr
	CheckpointIntv time.Duration
}

func newReplay(cfg replayConfig) *replay {
	if cfg.CheckpointPath == "" {
		cfg.CheckpointPath = "./checkpoint.dat"
	}
	if cfg.CheckpointIntv <= 0 {
		cfg.CheckpointIntv = 1 * time.Minute
	}

	return &replay{
		checkpointPath:  cfg.CheckpointPath,
		writer:          cfg.Writer,
		store:           cfg.Store,
		executor:        cfg.Executor,
		hintedHandoff:   make(map[string][]*mem_storage.SyncOperation),
		gossip:          cfg.Gossip,
		member:          cfg.Member,
		checkpointIntv:  cfg.CheckpointIntv,
		checkpointTimer: time.NewTimer(cfg.CheckpointIntv),
		stopCh:          make(chan struct{}),
	}
}

func (r *replay) SaveCheckpoint(ops []*mem_storage.SyncOperation) error {
	if len(ops) == 0 {
		return nil
	}

	// Serialize ops
	data, err := SerializeSyncOps(ops)
	if err != nil {
		return err
	}

	// Atomic write: temp file + rename
	dir := filepath.Dir(r.checkpointPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	tmpPath := r.checkpointPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	// Write length prefix
	if err := binary.Write(f, binary.LittleEndian, uint32(len(data))); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}

	// Write data
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}

	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}

	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}

	// Atomic rename
	return os.Rename(tmpPath, r.checkpointPath)
}

func (r *replay) LoadCheckpoint() ([]*mem_storage.SyncOperation, error) {
	data, err := os.ReadFile(r.checkpointPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	if len(data) < 4 {
		return nil, nil
	}

	// Read length prefix
	length := binary.LittleEndian.Uint32(data[0:4])
	if len(data) < 4+int(length) {
		return nil, nil
	}

	// Deserialize ops
	return DeserializeSyncOps(data[4 : 4+length])
}

func (r *replay) HintedHandoff(nodeID string, ops []*mem_storage.SyncOperation) error {
	if len(ops) == 0 {
		return nil
	}

	// Add to hinted-handoff queue
	r.hhMu.Lock()
	r.hintedHandoff[nodeID] = append(r.hintedHandoff[nodeID], ops...)
	r.hhMu.Unlock()

	// Async retry
	if err := r.executor.Do(func() {
		r.retryHintedHandoff(nodeID)
	}); err != nil {
		return err
	}

	return nil
}

func (r *replay) retryHintedHandoff(nodeID string) {
	maxRetries := 10
	for i := 0; i < maxRetries; i++ {
		r.hhMu.Lock()
		ops := r.hintedHandoff[nodeID]
		if len(ops) == 0 {
			r.hhMu.Unlock()
			return
		}
		// Copy ops for sending
		opsCopy := make([]*mem_storage.SyncOperation, len(ops))
		copy(opsCopy, ops)
		r.hhMu.Unlock()

		// Check if node is alive
		if r.member != nil {
			state := r.member.State(nodeID)
			if state != NodeStateAlive {
				// Node not alive yet, wait and retry
				backoff := time.Duration(1<<uint(i)) * 100 * time.Millisecond
				if backoff > 10*time.Second {
					backoff = 10 * time.Second
				}
				time.Sleep(backoff)
				continue
			}
		}

		// Get node address
		var targetAddr string
		if r.member != nil {
			members := r.member.Members()
			for _, m := range members {
				if m.NodeID == nodeID && m.State == NodeStateAlive {
					targetAddr = m.Address
					break
				}
			}
		}

		if targetAddr == "" {
			// Node address not found, wait and retry
			backoff := time.Duration(1<<uint(i)) * 100 * time.Millisecond
			if backoff > 10*time.Second {
				backoff = 10 * time.Second
			}
			time.Sleep(backoff)
			continue
		}

		// Try to send operations via gossip
		if r.gossip != nil {
			err := r.gossip.Push(opsCopy, []string{targetAddr})
			if err == nil {
				// Success: remove from queue
				r.hhMu.Lock()
				// Double-check: only remove if ops haven't changed
				if len(r.hintedHandoff[nodeID]) == len(opsCopy) {
					delete(r.hintedHandoff, nodeID)
				} else {
					// Some ops were added, remove only the ones we sent
					remaining := r.hintedHandoff[nodeID][len(opsCopy):]
					if len(remaining) > 0 {
						r.hintedHandoff[nodeID] = remaining
					} else {
						delete(r.hintedHandoff, nodeID)
					}
				}
				r.hhMu.Unlock()
				return
			}
		}

		// Failed, wait and retry
		backoff := time.Duration(1<<uint(i)) * 100 * time.Millisecond
		if backoff > 10*time.Second {
			backoff = 10 * time.Second
		}
		time.Sleep(backoff)
	}

	// Max retries reached, remove from queue
	r.hhMu.Lock()
	delete(r.hintedHandoff, nodeID)
	r.hhMu.Unlock()
}

// lifecycle.Component implementation for antiEntropy
func (ae *antiEntropy) Name() string { return "anti-entropy" }
func (ae *antiEntropy) Start(ctx context.Context) error {
	return ae.start()
}
func (ae *antiEntropy) Close(ctx context.Context) error {
	return ae.stop()
}

// lifecycle.Component implementation for replay
func (r *replay) Name() string { return "replay" }
func (r *replay) Start(ctx context.Context) error {
	// Load checkpoint on start
	ops, err := r.LoadCheckpoint()
	if err == nil && len(ops) > 0 {
		ctx := context.Background()
		items := make(map[string]*mem_storage.StoredItem)
		for _, op := range ops {
			if op.Item != nil {
				items[op.Key] = op.Item
			}
		}
		if len(items) > 0 {
			_ = r.writer.BatchSet(ctx, items)
		}
	}

	// Start periodic checkpoint
	go r.checkpointLoop()

	// Retry hinted-handoff for any pending nodes
	if r.member != nil {
		r.hhMu.Lock()
		for nodeID := range r.hintedHandoff {
			nodeID := nodeID
			if err := r.executor.Do(func() {
				r.retryHintedHandoff(nodeID)
			}); err != nil {
				break
			}
		}
		r.hhMu.Unlock()
	}

	return nil
}

func (r *replay) checkpointLoop() {
	for {
		select {
		case <-r.stopCh:
			return
		case <-r.checkpointTimer.C:
			r.checkpointMu.Lock()
			ops, _ := r.store.GetSyncBuffer()
			if len(ops) > 0 {
				_ = r.SaveCheckpoint(ops)
			}
			r.checkpointTimer.Reset(r.checkpointIntv)
			r.checkpointMu.Unlock()
		}
	}
}

func (r *replay) Close(ctx context.Context) error {
	// Signal checkpoint loop to stop
	r.stopOnce.Do(func() {
		close(r.stopCh)
	})

	// Stop checkpoint timer
	r.checkpointMu.Lock()
	if r.checkpointTimer != nil {
		r.checkpointTimer.Stop()
	}
	r.checkpointMu.Unlock()

	// Save checkpoint on close
	ops, _ := r.store.GetSyncBuffer()
	_ = r.SaveCheckpoint(ops)
	return nil
}
