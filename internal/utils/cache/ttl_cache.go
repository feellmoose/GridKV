package cache

// TTL cache with TinyLFU for smart eviction

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feellmoose/gridkv/internal/utils/lifecycle"
	"github.com/zeebo/xxh3"
)

const (
	DefaultShards = 256
	DefaultSize   = 10000
)

type Cache struct {
	shards    []*shard
	shardMask uint32
	closed    atomic.Bool

	tinyLFU *TinyLFU
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
	Shards int
	Size   int
}

// New creates a TTL cache
func New(opts Opts) *Cache {
	if opts.Shards <= 0 {
		opts.Shards = DefaultShards
	}
	if !isPow2(opts.Shards) {
		opts.Shards = nextPow2(opts.Shards)
	}

	if opts.Size <= 0 {
		// Enforce a finite capacity to prevent unbounded growth when used as SDK cache.
		opts.Size = DefaultSize
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
		shards:    shards,
		shardMask: uint32(opts.Shards - 1),
		tinyLFU:   NewTinyLFU(),
	}

	return c
}

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
		if c.tinyLFU != nil {
			c.tinyLFU.RecordAccess(key, false)
		}
		return nil, false
	}

	expireAt := e.expireAt
	if expireAt > 0 {
		now := time.Now().UnixNano()
		if now > expireAt {
			s.mu.RUnlock()
			// Synchronous deletion: TTL check on read path is sufficient
			s.mu.Lock()
			if e2, ok := s.items[key]; ok && e2 == e {
				delete(s.items, key)
				s.removeEntry(e)
				entryPool.Put(e)
			}
			s.mu.Unlock()
			return nil, false
		}
	}

	v := e.value
	needsMove := s.head != e
	s.mu.RUnlock()

	if needsMove {
		s.mu.Lock()
		if e2, ok := s.items[key]; ok && e2 == e && s.head != e {
			s.removeEntry(e)
			s.addToFront(e)
		}
		s.mu.Unlock()
	}

	if c.tinyLFU != nil {
		c.tinyLFU.RecordAccess(key, true)
	}

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
		oldest := s.tail
		if oldest != nil {
			victim := oldest
			if c.tinyLFU != nil {
				newFreq := c.tinyLFU.Estimate(key)
				oldFreq := c.tinyLFU.Estimate(oldest.key)
				if oldFreq > newFreq {
					current := oldest
					for current != nil {
						freq := c.tinyLFU.Estimate(current.key)
						if freq < newFreq {
							victim = current
							break
						}
						current = current.prev
						if current == s.head {
							break
						}
					}
				}
			}

			delete(s.items, victim.key)
			s.removeEntry(victim)
			entryPool.Put(victim)
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

	if c.tinyLFU != nil {
		c.tinyLFU.RecordAccess(key, true)
	}
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
	return nil
}

func (c *Cache) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	return nil
}

func (c *Cache) CloseNoContext() {
	_ = c.Close(context.Background())
}

var _ lifecycle.Component = (*Cache)(nil)

//go:inline
func (c *Cache) getShard(key string) *shard {
	if c == nil || len(c.shards) == 0 {
		return nil
	}
	fullHash := xxh3.HashString128(key)
	hash := (fullHash.Lo << 1) ^ fullHash.Hi
	idx := uint32(hash) & c.shardMask
	if idx >= uint32(len(c.shards)) {
		return nil
	}
	return c.shards[idx]
}

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
