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
			buf := make([]string, 0, 16)
			return &buf
		},
	}

	versionMapPool = sync.Pool{
		New: func() interface{} {
			return make(map[int64]*mem_storage.StoredItem, 8)
		},
	}

	storedItemSlicePool = sync.Pool{
		New: func() interface{} {
			buf := make([]*mem_storage.StoredItem, 0, 8)
			return &buf
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
	// Validate required dependencies
	if cfg.Store == nil {
		return nil, fmt.Errorf("store cannot be nil")
	}
	if cfg.Executor == nil {
		return nil, fmt.Errorf("executor cannot be nil")
	}
	if cfg.Ring == nil {
		return nil, fmt.Errorf("ring cannot be nil")
	}

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
	if r.cache != nil {
		if val, ok := r.cache.Get(key); ok {
			if item, ok := val.(*mem_storage.StoredItem); ok {
				return item.DeepCopy(), nil
			}
		}
	}

	item, err := r.store.Get(key)
	if err == nil && item != nil {
		return item, nil
	}

	if err != nil && err != mem_storage.ErrNotFound && err != mem_storage.ErrExpired {
		return nil, err
	}

	targets := r.ring.GetN(key, r.replicaCount)
	if len(targets) == 0 {
		return nil, nil
	}

	aliveTargetsPtr := aliveTargetsPool.Get().(*[]string)
	aliveTargets := (*aliveTargetsPtr)[:0]
	defer func() {
		if cap(aliveTargets) <= 32 {
			*aliveTargetsPtr = (*aliveTargetsPtr)[:0]
			aliveTargetsPool.Put(aliveTargetsPtr)
		}
	}()

	for _, target := range targets {
		if target == r.nodeID {
			continue
		}
		if r.member != nil && r.member.State(target) == NodeStateAlive {
			*aliveTargetsPtr = append(*aliveTargetsPtr, target)
		} else if r.member == nil {
			*aliveTargetsPtr = append(*aliveTargetsPtr, target)
		}
	}

	if len(*aliveTargetsPtr) == 0 || r.getFunc == nil {
		return nil, nil
	}

	timeout := 5 * time.Second
	// For a multi-replica read batch, create at most one timeout context and
	// reuse it across all replica attempts to avoid per-target timers.
	readCtx := ctx
	var cancel context.CancelFunc
	if ctx == nil || ctx == context.Background() {
		readCtx, cancel = context.WithTimeout(context.Background(), timeout)
	} else if ctxDeadline, hasDeadline := ctx.Deadline(); !hasDeadline || time.Until(ctxDeadline) > timeout {
		readCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	if cancel != nil {
		defer cancel()
	}

	hasSuccessfulResponse := false
	var lastNetworkErr error
	for _, target := range *aliveTargetsPtr {
		done := make(chan struct{})
		go func(t string) {
			defer close(done)
			// Check context before starting expensive operation
			select {
			case <-readCtx.Done():
				return
			default:
			}

			var localItem *mem_storage.StoredItem
			var localErr error
			localItem, localErr = r.getFunc(t, key)

			// Check context again after operation completes
			select {
			case <-readCtx.Done():
				// Context cancelled, discard result
				return
			default:
				// Context still valid, use result
				item = localItem
				err = localErr
			}
		}(target)

		select {
		case <-done:
			// Remote read completed
			if err == nil && item != nil && !item.IsTombstone() {
				// If value is empty, treat as key not found
				if len(item.Value) == 0 {
					hasSuccessfulResponse = true
				} else {
					// Success, return immediately
					goto success
				}
			} else if err == nil || err == mem_storage.ErrNotFound || err == mem_storage.ErrExpired {
				hasSuccessfulResponse = true
			} else {
				if lastNetworkErr == nil {
					lastNetworkErr = err
				}
			}
		case <-readCtx.Done():
			if lastNetworkErr == nil {
				lastNetworkErr = fmt.Errorf("remote read timeout for key %s on node %s", key, target)
			}
			continue
		}
	}

	if hasSuccessfulResponse || lastNetworkErr == nil {
		return nil, nil
	}
	return nil, nil

success:
	if r.cache != nil {
		r.cache.Set(key, item, r.cacheTTL)
	}
	return item.DeepCopy(), nil
}

func (r *reader) BatchGet(ctx context.Context, keys []string) (map[string]*mem_storage.StoredItem, error) {
	if len(keys) == 0 {
		return make(map[string]*mem_storage.StoredItem), nil
	}

	// Pre-allocate result map with known size
	result := make(map[string]*mem_storage.StoredItem, len(keys))
	resultCh := make(chan struct {
		key  string
		item *mem_storage.StoredItem
	}, len(keys))

	var wg sync.WaitGroup

	for _, key := range keys {
		key := key
		wg.Add(1)
		if err := r.executor.Do(func() {
			defer wg.Done()
			if item, err := r.Get(ctx, key); err == nil && item != nil {
				resultCh <- struct {
					key  string
					item *mem_storage.StoredItem
				}{key, item}
			}
		}); err != nil {
			wg.Done()
			break
		}
	}

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	for res := range resultCh {
		result[res.key] = res.item
	}

	return result, nil
}

func (r *reader) GetSpeculative(ctx context.Context, key string, n int) (*mem_storage.StoredItem, error) {
	if key == "" {
		_, err := r.store.Get(key)
		return nil, err
	}

	if n <= 0 {
		n = 3
	}

	targets := r.ring.GetN(key, n)
	if len(targets) == 0 {
		return nil, nil
	}

	// Filter alive nodes
	aliveTargetsPtr := aliveTargetsPool.Get().(*[]string)
	aliveTargets := (*aliveTargetsPtr)[:0]
	defer func() {
		if cap(aliveTargets) <= 32 {
			*aliveTargetsPtr = (*aliveTargetsPtr)[:0]
			aliveTargetsPool.Put(aliveTargetsPtr)
		}
	}()

	for _, target := range targets {
		if r.member.State(target) == NodeStateAlive || target == r.nodeID {
			*aliveTargetsPtr = append(*aliveTargetsPtr, target)
		}
	}

	if len(*aliveTargetsPtr) == 0 {
		return nil, nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan struct {
		item *mem_storage.StoredItem
		err  error
	}, len(*aliveTargetsPtr))

	for _, target := range *aliveTargetsPtr {
		target := target
		if err := r.executor.Do(func() {
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
		}); err != nil {
			break
		}
	}

	var best *mem_storage.StoredItem
	itemsPtr := storedItemSlicePool.Get().(*[]*mem_storage.StoredItem)
	items := (*itemsPtr)[:0]
	defer func() {
		if cap(items) <= 32 {
			*itemsPtr = (*itemsPtr)[:0]
			storedItemSlicePool.Put(itemsPtr)
		}
	}()

	// Wait for all responses to get the highest version
	count := 0
loop1:
	for count < len(*aliveTargetsPtr) {
		select {
		case res := <-results:
			count++
			if res.err == nil && res.item != nil && !res.item.IsTombstone() && len(res.item.Value) > 0 {
				*itemsPtr = append(*itemsPtr, res.item)
				if best == nil || res.item.CompareVersion(best) > 0 {
					best = res.item
				}
			}
		case <-ctx.Done():
			break loop1
		}
	}

	if len(*itemsPtr) > 1 && r.repair != nil {
		versions := versionMapPool.Get().(map[int64]*mem_storage.StoredItem)
		for k := range versions {
			delete(versions, k)
		}
		defer func() {
			if len(versions) <= 32 {
				versionMapPool.Put(versions)
			}
		}()
		for _, item := range *itemsPtr {
			versions[item.Version] = item
		}

		if len(versions) > 1 {
			repairItemsPtr := storedItemSlicePool.Get().(*[]*mem_storage.StoredItem)
			repairItems := (*repairItemsPtr)[:0]
			usedPool := true
			if cap(repairItems) < len(*itemsPtr) {
				repairItems = make([]*mem_storage.StoredItem, 0, len(*itemsPtr))
				usedPool = false
			}
			repairItems = append(repairItems, *itemsPtr...)
			if err := r.repair.Repair(key, repairItems); err != nil {
				logging.Debug("Read repair failed", "key", key, "error", err)
			}
			if usedPool && cap(repairItems) <= 64 {
				*repairItemsPtr = (*repairItemsPtr)[:0]
				storedItemSlicePool.Put(repairItemsPtr)
			}
		}
	}

	if best != nil {
		return best.DeepCopy(), nil
	}

	return nil, nil
}

func (r *reader) GetWithConsistency(ctx context.Context, key string, level ConsistencyLevel) (*mem_storage.StoredItem, error) {
	if key == "" {
		_, err := r.store.Get(key)
		return nil, err
	}

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
	aliveTargetsPtr := aliveTargetsPool.Get().(*[]string)
	aliveTargets := (*aliveTargetsPtr)[:0]
	defer func() {
		if cap(aliveTargets) <= 64 {
			*aliveTargetsPtr = (*aliveTargetsPtr)[:0]
			aliveTargetsPool.Put(aliveTargetsPtr)
		}
	}()
	for _, target := range targets {
		if r.member.State(target) == NodeStateAlive || target == r.nodeID {
			*aliveTargetsPtr = append(*aliveTargetsPtr, target)
		}
	}

	if len(*aliveTargetsPtr) == 0 {
		return nil, nil
	}

	// Calculate required responses based on consistency level
	var requiredResponses int
	switch level {
	case ConsistencyLevelOne:
		requiredResponses = 1
	case ConsistencyLevelQuorum:
		requiredResponses = (len(*aliveTargetsPtr) / 2) + 1
	case ConsistencyLevelAll:
		requiredResponses = len(aliveTargets)
	default:
		requiredResponses = 1
	}

	if requiredResponses > len(*aliveTargetsPtr) {
		requiredResponses = len(*aliveTargetsPtr)
	}

	// Parallel read from replicas
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		item *mem_storage.StoredItem
		err  error
	}
	results := make(chan result, len(*aliveTargetsPtr))

	for _, target := range *aliveTargetsPtr {
		target := target
		if err := r.executor.Do(func() {
			var item *mem_storage.StoredItem
			var err error
			if target == r.nodeID {
				item, err = r.store.Get(key)
			} else if r.getFunc != nil {
				item, err = r.getFunc(target, key)
			}
			select {
			case results <- result{item, err}:
			case <-ctx.Done():
			}
		}); err != nil {
			break
		}
	}

	itemsPtr := storedItemSlicePool.Get().(*[]*mem_storage.StoredItem)
	items := (*itemsPtr)[:0]
	defer func() {
		if cap(items) <= 32 {
			*itemsPtr = (*itemsPtr)[:0]
			storedItemSlicePool.Put(itemsPtr)
		}
	}()
	successCount := 0
loop2:
	for i := 0; i < len(aliveTargets) && successCount < requiredResponses; i++ {
		select {
		case res := <-results:
			if res.err == nil && res.item != nil && !res.item.IsTombstone() && len(res.item.Value) > 0 {
				items = append(items, res.item)
				successCount++
			}
		case <-ctx.Done():
			break loop2
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

	if len(*itemsPtr) > 1 && r.repair != nil {
		versions := versionMapPool.Get().(map[int64]*mem_storage.StoredItem)
		for k := range versions {
			delete(versions, k)
		}
		defer func() {
			if len(versions) <= 32 {
				versionMapPool.Put(versions)
			}
		}()
		for _, item := range *itemsPtr {
			versions[item.Version] = item
		}

		if len(versions) > 1 {
			// Use object pool for repair items
			repairItemsPtr := storedItemSlicePool.Get().(*[]*mem_storage.StoredItem)
			repairItems := (*repairItemsPtr)[:0]
			usedPool := true
			// Only allocate new slice if pool buffer is too small
			if cap(repairItems) < len(*itemsPtr) {
				// Pool buffer too small, allocate new slice
				repairItems = make([]*mem_storage.StoredItem, 0, len(*itemsPtr))
				usedPool = false
			}
			repairItems = append(repairItems, *itemsPtr...)
			// Trigger read repair (error ignored as this is async repair path)
			_ = r.repair.Repair(key, repairItems)
			// Return to pool only if we used the pool buffer and capacity is reasonable
			if usedPool && cap(repairItems) <= 64 {
				*repairItemsPtr = (*repairItemsPtr)[:0]
				storedItemSlicePool.Put(repairItemsPtr)
			}
		}
	}

	if best == nil {
		return nil, nil
	}

	if level == ConsistencyLevelOne && r.cache != nil {
		r.cache.Set(key, best, r.cacheTTL)
	}
	return best.DeepCopy(), nil
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

	// Async repair - only if writer is available
	if rr.writer != nil {
		if err := rr.executor.Do(func() {
			ctx := context.Background()
			_ = rr.writer.Set(ctx, key, maxVersion)
		}); err != nil {
			return nil
		}
	}

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
