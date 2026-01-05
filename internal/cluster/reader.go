package cluster

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/feellmoose/gridkv/internal/mem_storage"
	"github.com/feellmoose/gridkv/internal/utils/cache"
	"github.com/feellmoose/gridkv/internal/utils/executor"
	"github.com/feellmoose/gridkv/internal/utils/logging"
)

// Simple object pools for basic reuse
var (
	aliveTargetsPool = sync.Pool{
		New: func() interface{} {
			return make([]string, 0, 16)
		},
	}

	versionMapPool = sync.Pool{
		New: func() interface{} {
			return make(map[int64]*mem_storage.StoredItem, 8)
		},
	}

	storedItemSlicePool = sync.Pool{
		New: func() interface{} {
			return make([]*mem_storage.StoredItem, 0, 8)
		},
	}
)

// ConsistencyLevel defines read consistency requirements
type ConsistencyLevel int

const (
	ConsistencyLevelOne    ConsistencyLevel = iota // R=1, eventual consistency (default)
	ConsistencyLevelQuorum                         // R=quorum, strong consistency
	ConsistencyLevelAll                            // R=all, linearizability
)

// Reader handles read operations with caching and speculative reads
type Reader interface {
	Get(ctx context.Context, key string) (*mem_storage.StoredItem, error)
	BatchGet(ctx context.Context, keys []string) (map[string]*mem_storage.StoredItem, error)
	GetSpeculative(ctx context.Context, key string, n int) (*mem_storage.StoredItem, error)
	GetWithConsistency(ctx context.Context, key string, level ConsistencyLevel) (*mem_storage.StoredItem, error)
}

type reader struct {
	nodeID   string
	store    *mem_storage.MemStorage
	ring     HashRing
	member   MemberMgr
	cache    *cache.Cache
	executor *executor.Exec
	repair   ReadRepair

	cacheTTL     time.Duration
	replicaCount int

	getFunc func(nodeID string, key string) (*mem_storage.StoredItem, error)
}

type readerConfig struct {
	NodeID       string
	Store        *mem_storage.MemStorage
	Ring         HashRing
	Member       MemberMgr
	Cache        *cache.Cache
	Executor     *executor.Exec
	Repair       ReadRepair
	CacheTTL     time.Duration
	ReplicaCount int
	GetFunc      func(nodeID string, key string) (*mem_storage.StoredItem, error)
}

var _ Reader = (*reader)(nil)

func newReader(cfg readerConfig) (*reader, error) {
	// Default TTL when cache is provided or not specified
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 15 * time.Millisecond
	}
	if cfg.ReplicaCount <= 0 {
		cfg.ReplicaCount = 3
	}

	return &reader{
		nodeID:       cfg.NodeID,
		store:        cfg.Store,
		ring:         cfg.Ring,
		member:       cfg.Member,
		cache:        cfg.Cache,
		executor:     cfg.Executor,
		repair:       cfg.Repair,
		cacheTTL:     cfg.CacheTTL,
		replicaCount: cfg.ReplicaCount,
		getFunc:      cfg.GetFunc,
	}, nil
}

func (r *reader) Get(ctx context.Context, key string) (*mem_storage.StoredItem, error) {
	// Check cache first
	if r.cache != nil {
		if val, ok := r.cache.Get(key); ok {
			if item, ok := val.(*mem_storage.StoredItem); ok {
				// Single DeepCopy: only when returning to user
				return item.DeepCopy(), nil
			}
		}
	}

	// Route to target node
	target := r.ring.Get(key)
	if target == "" {
		return nil, fmt.Errorf("no target node found for key %s", key)
	}

	// Get data from target with timeout control
	var item *mem_storage.StoredItem
	var err error

	if target == r.nodeID {
		// Local read
		item, err = r.store.Get(key)
		if err != nil {
			logging.Debug("Local store.Get failed", "key", key, "error", err)
		}
	} else if r.getFunc != nil {
		// Remote read with timeout control
		// Increased timeout for distributed keys with better hash distribution
		timeout := 5 * time.Second
		readCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		done := make(chan struct{})
		go func() {
			defer close(done)
			var localItem *mem_storage.StoredItem
			var localErr error
			localItem, localErr = r.getFunc(target, key)
			// Only assign if context not cancelled
			select {
			case <-readCtx.Done():
				return
			default:
				item = localItem
				err = localErr
			}
		}()

		// Wait for completion or timeout
		select {
		case <-done:
			// Remote read completed
		case <-readCtx.Done():
			logging.Warn("Remote read operation timed out", "key", key, "targetNode", target, "timeout", timeout)
			return nil, fmt.Errorf("remote read timeout for key %s on node %s after %v", key, target, timeout)
		}

		if err != nil {
			logging.Warn("Remote read failed due to network connectivity issue",
				"key", key, "targetNode", target, "error", err)
			return nil, fmt.Errorf("remote read failed for key %s on node %s: %w", key, target, err)
		}
	} else {
		return nil, fmt.Errorf("no remote read function available for key %s", key)
	}

	if err != nil || item == nil || item.IsTombstone() {
		return nil, err
	}

	// Cache the original item (zero-copy)
	if r.cache != nil {
		r.cache.Set(key, item, r.cacheTTL)
	}

	// Single DeepCopy for user safety
	return item.DeepCopy(), nil
}

func (r *reader) BatchGet(ctx context.Context, keys []string) (map[string]*mem_storage.StoredItem, error) {
	if len(keys) == 0 {
		return make(map[string]*mem_storage.StoredItem), nil
	}

	result := make(map[string]*mem_storage.StoredItem, len(keys))
	resultCh := make(chan struct {
		key  string
		item *mem_storage.StoredItem
	}, len(keys))

	var wg sync.WaitGroup

	// Process keys in parallel
	for _, key := range keys {
		key := key
		wg.Add(1)
		r.executor.Do(func() {
			defer wg.Done()
			if item, err := r.Get(ctx, key); err == nil && item != nil {
				resultCh <- struct {
					key  string
					item *mem_storage.StoredItem
				}{key, item}
			}
		})
	}

	// Close channel when all goroutines done
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	// Collect results
	for res := range resultCh {
		result[res.key] = res.item
	}

	return result, nil
}

func (r *reader) GetSpeculative(ctx context.Context, key string, n int) (*mem_storage.StoredItem, error) {
	if n <= 0 {
		n = 3
	}

	targets := r.ring.GetN(key, n)
	if len(targets) == 0 {
		return nil, nil
	}

	// Filter alive nodes
	aliveTargets := aliveTargetsPool.Get().([]string)[:0]
	defer func() {
		if cap(aliveTargets) <= 32 {
			aliveTargetsPool.Put(aliveTargets[:0])
		}
	}()

	for _, target := range targets {
		if r.member.State(target) == NodeStateAlive || target == r.nodeID {
			aliveTargets = append(aliveTargets, target)
		}
	}

	if len(aliveTargets) == 0 {
		return nil, nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan struct {
		item *mem_storage.StoredItem
		err  error
	}, len(aliveTargets))

	// Query all targets in parallel
	for _, target := range aliveTargets {
		target := target
		r.executor.Do(func() {
			var item *mem_storage.StoredItem
			var err error

			if target == r.nodeID {
				item, err = r.store.Get(key)
			} else if r.getFunc != nil {
				item, err = r.getFunc(target, key)
			}

			select {
			case results <- struct {
				item *mem_storage.StoredItem
				err  error
			}{item, err}:
			case <-ctx.Done():
			}
		})
	}

	var best *mem_storage.StoredItem
	items := storedItemSlicePool.Get().([]*mem_storage.StoredItem)[:0]
	defer func() {
		if cap(items) <= 32 {
			storedItemSlicePool.Put(items[:0])
		}
	}()

	// Wait for first successful response, then cancel remaining
	firstSuccess := false
	count := 0
	for count < len(aliveTargets) && !firstSuccess {
		select {
		case res := <-results:
			count++
			if res.err == nil && res.item != nil && !res.item.IsTombstone() {
				items = append(items, res.item)
				if best == nil || res.item.CompareVersion(best) > 0 {
					best = res.item
				}
				// First success: cancel remaining requests for fast path
				if !firstSuccess {
					firstSuccess = true
					cancel()
				}
			}
		case <-ctx.Done():
			break
		}
	}

	// Check for version conflicts and trigger repair
	if len(items) > 1 && r.repair != nil {
		versions := versionMapPool.Get().(map[int64]*mem_storage.StoredItem)
		for k := range versions {
			delete(versions, k)
		}
		defer func() {
			if len(versions) <= 32 {
				versionMapPool.Put(versions)
			}
		}()
		for _, item := range items {
			versions[item.Version] = item
		}

		if len(versions) > 1 {
			repairItems := make([]*mem_storage.StoredItem, len(items))
			copy(repairItems, items)
			_ = r.repair.Repair(key, repairItems)
		}
	}

	if best != nil {
		// DeepCopy is necessary for safety: user can modify returned value
		return best.DeepCopy(), nil
	}

	return nil, nil
}

// GetWithConsistency reads with specified consistency level
func (r *reader) GetWithConsistency(ctx context.Context, key string, level ConsistencyLevel) (*mem_storage.StoredItem, error) {
	// Check cache first (only for eventual consistency)
	if level == ConsistencyLevelOne && r.cache != nil {
		if val, ok := r.cache.Get(key); ok {
			if item, ok := val.(*mem_storage.StoredItem); ok {
				return item.DeepCopy(), nil
			}
		}
	}

	// Get replica nodes
	targets := r.ring.GetN(key, r.replicaCount)
	if len(targets) == 0 {
		return nil, nil
	}

	// Filter alive nodes (use pool to reduce allocations)
	aliveTargets := aliveTargetsPool.Get().([]string)
	aliveTargets = aliveTargets[:0] // Reset length, keep capacity
	defer func() {
		if cap(aliveTargets) <= 64 {
			aliveTargetsPool.Put(aliveTargets[:0])
		}
	}()
	for _, target := range targets {
		if r.member.State(target) == NodeStateAlive || target == r.nodeID {
			aliveTargets = append(aliveTargets, target)
		}
	}

	if len(aliveTargets) == 0 {
		return nil, nil
	}

	// Calculate required responses based on consistency level
	var requiredResponses int
	switch level {
	case ConsistencyLevelOne:
		requiredResponses = 1
	case ConsistencyLevelQuorum:
		requiredResponses = (len(aliveTargets) / 2) + 1
	case ConsistencyLevelAll:
		requiredResponses = len(aliveTargets)
	default:
		requiredResponses = 1
	}

	if requiredResponses > len(aliveTargets) {
		requiredResponses = len(aliveTargets)
	}

	// Parallel read from replicas
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan struct {
		item *mem_storage.StoredItem
		err  error
	}, len(aliveTargets))

	// Query all replicas
	for _, target := range aliveTargets {
		target := target
		r.executor.Do(func() {
			var item *mem_storage.StoredItem
			var err error

			if target == r.nodeID {
				item, err = r.store.Get(key)
			} else if r.getFunc != nil {
				item, err = r.getFunc(target, key)
			}

			select {
			case results <- struct {
				item *mem_storage.StoredItem
				err  error
			}{item, err}:
			case <-ctx.Done():
			}
		})
	}

	// Collect responses
	items := storedItemSlicePool.Get().([]*mem_storage.StoredItem)[:0]
	defer func() {
		if cap(items) <= 32 {
			storedItemSlicePool.Put(items[:0])
		}
	}()
	successCount := 0
	for i := 0; i < len(aliveTargets) && successCount < requiredResponses; i++ {
		select {
		case res := <-results:
			if res.err == nil && res.item != nil && !res.item.IsTombstone() {
				items = append(items, res.item)
				successCount++
			}
		case <-ctx.Done():
			break
		}
	}

	if successCount < requiredResponses {
		// Not enough responses for consistency requirement
		return nil, nil
	}

	// Find highest version
	var best *mem_storage.StoredItem
	for _, item := range items {
		if best == nil || item.CompareVersion(best) > 0 {
			best = item
		}
	}

	// Check for version conflicts and trigger repair
	if len(items) > 1 && r.repair != nil {
		versions := versionMapPool.Get().(map[int64]*mem_storage.StoredItem)
		for k := range versions {
			delete(versions, k)
		}
		defer func() {
			if len(versions) <= 32 {
				versionMapPool.Put(versions)
			}
		}()
		for _, item := range items {
			versions[item.Version] = item
		}

		if len(versions) > 1 {
			repairItems := make([]*mem_storage.StoredItem, len(items))
			copy(repairItems, items)
			_ = r.repair.Repair(key, repairItems)
		}
	}

	// Cache result (only for eventual consistency)
	if best != nil && level == ConsistencyLevelOne && r.cache != nil {
		r.cache.Set(key, best, r.cacheTTL)
	}

	if best != nil {
		// DeepCopy is necessary for safety: user can modify returned value
		return best.DeepCopy(), nil
	}

	return nil, nil
}

// ReadRepair handles asynchronous read repair
type ReadRepair interface {
	Repair(key string, versions []*mem_storage.StoredItem) error
}

type readRepair struct {
	writer   Writer
	executor *executor.Exec
	limiter  *rateLimiter
}

type readRepairConfig struct {
	Writer          Writer
	Executor        *executor.Exec
	RateLimitPerSec int64
}

func newReadRepair(cfg readRepairConfig) *readRepair {
	if cfg.RateLimitPerSec <= 0 {
		cfg.RateLimitPerSec = 100
	}

	return &readRepair{
		writer:   cfg.Writer,
		executor: cfg.Executor,
		limiter:  newRateLimiter(cfg.RateLimitPerSec),
	}
}

func (rr *readRepair) Repair(key string, versions []*mem_storage.StoredItem) error {
	if len(versions) == 0 {
		return nil
	}

	// Find highest version
	maxVersion := versions[0]
	for _, item := range versions[1:] {
		if item.CompareVersion(maxVersion) > 0 {
			maxVersion = item
		}
	}

	// Rate limit repair operations
	if !rr.limiter.Allow() {
		return nil
	}

	// Async repair
	rr.executor.Do(func() {
		ctx := context.Background()
		_ = rr.writer.Set(ctx, key, maxVersion)
	})

	return nil
}

// Simple rate limiter
type rateLimiter struct {
	rate   int64
	tokens int64
	last   int64
	mu     sync.Mutex
}

func newRateLimiter(ratePerSec int64) *rateLimiter {
	return &rateLimiter{
		rate:   ratePerSec,
		tokens: ratePerSec,
		last:   time.Now().UnixNano(),
	}
}

func (rl *rateLimiter) Allow() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now().UnixNano()
	elapsed := now - rl.last
	rl.last = now

	// Add tokens based on elapsed time
	rl.tokens += (elapsed * rl.rate) / int64(time.Second)
	if rl.tokens > rl.rate {
		rl.tokens = rl.rate
	}

	// Try to consume token
	if rl.tokens >= 1 {
		rl.tokens--
		return true
	}

	return false
}

// lifecycle.Component implementation
func (r *reader) Name() string                    { return "reader" }
func (r *reader) Start(ctx context.Context) error { return nil }
func (r *reader) Close(ctx context.Context) error { return nil }

// lifecycle.Component implementation for readRepair
func (rr *readRepair) Name() string                    { return "read-repair" }
func (rr *readRepair) Start(ctx context.Context) error { return nil }
func (rr *readRepair) Close(ctx context.Context) error { return nil }
