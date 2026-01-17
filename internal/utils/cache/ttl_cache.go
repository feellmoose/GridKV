package cache

// TTL cache
// Features:
//   - O(1) LRU eviction
//   - Object pooling for reduced GC pressure
//   - Parallel cleanup across shards
//   - XXH3 hash for fast sharding
//   - Leak prevention: all goroutines tracked and cleaned up
//   - Lifecycle.Component integration

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feellmoose/gridkv/internal/utils/lifecycle"
	"github.com/zeebo/xxh3"
)

const (
	DefaultShards      = 256
	DefaultCleanupIntv = 1 * time.Second
	MinCleanupIntv     = 100 * time.Millisecond
)

// Cache is a sharded TTL cache
type Cache struct {
	shards      []*shard
	shardMask   uint32
	cleanupTick *time.Ticker
	stopCleanup chan struct{}
	cleanupDone chan struct{}
	closed      atomic.Bool
	cleanupWG   sync.WaitGroup
}

type shard struct {
	mu      sync.RWMutex
	items   map[string]*entry
	head    *entry
	tail    *entry
	maxSize int
}

type entry struct {
	key      string
	value    interface{}
	expireAt int64
	next     *entry
	prev     *entry
}

var entryPool = sync.Pool{
	New: func() interface{} {
		return &entry{}
	},
}

// Opts configures cache behavior
type Opts struct {
	Shards        int
	Size          int
	CleanupIntv   time.Duration
	EnableCleanup bool
}

// New creates a TTL cache
func New(opts Opts) *Cache {
	if opts.Shards <= 0 {
		opts.Shards = DefaultShards
	}
	if !isPow2(opts.Shards) {
		opts.Shards = nextPow2(opts.Shards)
	}
	if opts.CleanupIntv <= 0 {
		opts.CleanupIntv = DefaultCleanupIntv
	}
	if opts.CleanupIntv < MinCleanupIntv {
		opts.CleanupIntv = MinCleanupIntv
	}

	shardSize := opts.Size / opts.Shards
	if shardSize < 1 && opts.Size > 0 {
		shardSize = 1
	}

	shards := make([]*shard, opts.Shards)
	for i := 0; i < opts.Shards; i++ {
		shards[i] = &shard{
			items:   make(map[string]*entry, shardSize),
			maxSize: shardSize,
		}
	}

	c := &Cache{
		shards:      shards,
		shardMask:   uint32(opts.Shards - 1),
		stopCleanup: make(chan struct{}),
		cleanupDone: make(chan struct{}),
	}

	if opts.EnableCleanup {
		c.startCleanup(opts.CleanupIntv)
	}

	return c
}

// Get retrieves value
//
//go:inline
func (c *Cache) Get(key string) (interface{}, bool) {
	if c == nil {
		return nil, false
	}
	if c.closed.Load() {
		return nil, false
	}
	if key == "" {
		return nil, false
	}

	s := c.getShard(key)
	if s == nil {
		return nil, false
	}

	s.mu.RLock()
	e, ok := s.items[key]
	if !ok || e == nil {
		s.mu.RUnlock()
		return nil, false
	}

	expireAt := e.expireAt
	if expireAt > 0 {
		// Fast path: use cached time if available (reduces time.Now() calls)
		// Only call time.Now() if expiration check is needed
		now := time.Now().UnixNano()
		if now > expireAt {
			s.mu.RUnlock()
			// Async cleanup to avoid blocking read path
			// Use non-blocking send to avoid goroutine leak if cleanupWG is full
			select {
			case <-c.stopCleanup:
				// Cache is closing, skip async cleanup
				return nil, false
			default:
				c.cleanupWG.Add(1)
				go func() {
					defer c.cleanupWG.Done()
					c.deleteExpired(key, s)
				}()
			}
			return nil, false
		}
	}

	v := e.value
	s.moveToFront(e)
	s.mu.RUnlock()
	return v, true
}

// Delete removes a key if present.
func (c *Cache) Delete(key string) bool {
	if c == nil || c.closed.Load() || key == "" {
		return false
	}
	sh := c.getShard(key)
	if sh == nil {
		return false
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if e, ok := sh.items[key]; ok {
		delete(sh.items, key)
		sh.removeEntry(e)
		return true
	}
	return false
}

// Set stores value with TTL
func (c *Cache) Set(key string, value interface{}, ttl time.Duration) {
	if c == nil {
		return
	}
	if c.closed.Load() {
		return
	}
	if key == "" {
		return
	}

	s := c.getShard(key)
	if s == nil {
		return
	}

	s.mu.Lock()

	var expireAt int64
	if ttl > 0 {
		expireAt = time.Now().Add(ttl).UnixNano()
	}

	if existing := s.items[key]; existing != nil {
		existing.value = value
		existing.expireAt = expireAt
		s.moveToFront(existing)
		s.mu.Unlock()
		return
	}

	if s.maxSize > 0 && len(s.items) >= s.maxSize {
		if oldest := s.tail; oldest != nil {
			delete(s.items, oldest.key)
			s.removeEntry(oldest)
			entryPool.Put(oldest)
		}
	}

	e := entryPool.Get().(*entry)
	if e == nil {
		s.mu.Unlock()
		return
	}
	e.key = key
	e.value = value
	e.expireAt = expireAt
	e.next = nil
	e.prev = nil

	s.items[key] = e
	s.addToFront(e)
	s.mu.Unlock()
}

// Del removes key
func (c *Cache) Del(key string) {
	if c == nil {
		return
	}
	if c.closed.Load() {
		return
	}
	if key == "" {
		return
	}

	s := c.getShard(key)
	if s == nil {
		return
	}

	s.mu.Lock()
	if e := s.items[key]; e != nil {
		delete(s.items, key)
		s.removeEntry(e)
		entryPool.Put(e)
	}
	s.mu.Unlock()
}

// Clear removes all entries
func (c *Cache) Clear() {
	if c == nil {
		return
	}

	for _, s := range c.shards {
		if s == nil {
			continue
		}
		s.mu.Lock()
		for _, e := range s.items {
			if e != nil {
				entryPool.Put(e)
			}
		}
		s.items = make(map[string]*entry, s.maxSize)
		s.head = nil
		s.tail = nil
		s.mu.Unlock()
	}
}

// Len returns total item count
func (c *Cache) Len() int {
	if c == nil {
		return 0
	}

	total := 0
	for _, s := range c.shards {
		if s == nil {
			continue
		}
		s.mu.RLock()
		total += len(s.items)
		s.mu.RUnlock()
	}
	return total
}

// lifecycle.Component implementation
func (c *Cache) Name() string {
	return "cache"
}

func (c *Cache) Start(ctx context.Context) error {
	// Cache starts cleanup in New() if enabled, no additional start needed
	return nil
}

func (c *Cache) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}

	close(c.stopCleanup)
	if c.cleanupTick != nil {
		c.cleanupTick.Stop()
		select {
		case <-c.cleanupDone:
		case <-time.After(5 * time.Second):
		}
	}

	done := make(chan struct{})
	go func() {
		c.cleanupWG.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// CloseNoContext stops background cleanup (public method for backward compatibility).
// Deprecated: Use Close(ctx) instead for lifecycle management.
func (c *Cache) CloseNoContext() {
	_ = c.Close(context.Background())
}

// Ensure Cache implements lifecycle.Component
var _ lifecycle.Component = (*Cache)(nil)

func (c *Cache) deleteExpired(key string, s *shard) {
	if c == nil || s == nil || key == "" {
		return
	}

	s.mu.Lock()
	if e := s.items[key]; e != nil {
		delete(s.items, key)
		s.removeEntry(e)
		entryPool.Put(e)
	}
	s.mu.Unlock()
}

//go:inline
func (c *Cache) getShard(key string) *shard {
	if c == nil || len(c.shards) == 0 {
		return nil
	}
	// Use 128-bit hash with bit rotation for layered load balancing
	// Different from MemStorage (.Lo) and HashRing (.Hi) but still from same hash function
	fullHash := xxh3.HashString128(key)
	hash := (fullHash.Lo << 1) ^ fullHash.Hi // Bit rotation combination for load balancing
	idx := uint32(hash) & c.shardMask
	if idx >= uint32(len(c.shards)) {
		return nil
	}
	return c.shards[idx]
}

func (c *Cache) startCleanup(interval time.Duration) {
	if c == nil {
		return
	}

	c.cleanupTick = time.NewTicker(interval)
	c.cleanupWG.Add(1)
	go func() {
		defer c.cleanupWG.Done()
		defer close(c.cleanupDone)

		lastGC := time.Now()
		gcInterval := 5 * time.Minute // Periodic GC hint for long-running processes

		for {
			select {
			case <-c.stopCleanup:
				return
			case <-c.cleanupTick.C:
				c.parallelCleanup()
				
				// Periodic GC hint for long-running processes
				if time.Since(lastGC) > gcInterval {
					runtime.GC()
					lastGC = time.Now()
				}
			}
		}
	}()
}

func (c *Cache) parallelCleanup() {
	if c == nil {
		return
	}

	now := time.Now().UnixNano()
	var wg sync.WaitGroup

	batchSize := 32
	shardCount := len(c.shards)
	for i := 0; i < shardCount; i += batchSize {
		end := i + batchSize
		if end > shardCount {
			end = shardCount
		}

		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			for j := start; j < end; j++ {
				if j < len(c.shards) && c.shards[j] != nil {
					c.cleanupShard(c.shards[j], now)
				}
			}
		}(i, end)
	}
	wg.Wait()
}

func (c *Cache) cleanupShard(s *shard, now int64) {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var expired []*entry
	for _, e := range s.items {
		if e != nil && e.expireAt > 0 && now > e.expireAt {
			expired = append(expired, e)
		}
	}

	for _, e := range expired {
		if e != nil {
			delete(s.items, e.key)
			s.removeEntry(e)
			entryPool.Put(e)
		}
	}
}

//go:inline
func (s *shard) addToFront(e *entry) {
	if s == nil || e == nil {
		return
	}
	e.next = s.head
	e.prev = nil
	if s.head != nil {
		s.head.prev = e
	}
	s.head = e
	if s.tail == nil {
		s.tail = e
	}
}

//go:inline
func (s *shard) removeEntry(e *entry) {
	if s == nil || e == nil {
		return
	}
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		s.head = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else {
		s.tail = e.prev
	}
}

//go:inline
func (s *shard) moveToFront(e *entry) {
	if s == nil || e == nil {
		return
	}
	if s.head == e {
		return
	}
	s.removeEntry(e)
	s.addToFront(e)
}

//go:inline
func isPow2(n int) bool {
	return n > 0 && (n&(n-1)) == 0
}

func nextPow2(n int) int {
	if n <= 1 {
		return 1
	}
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	n |= n >> 32
	n++
	return n
}
