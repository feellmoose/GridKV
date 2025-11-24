package transport

import (
	"sync"
	"time"
)

type poolCleaner struct {
	mu    sync.RWMutex
	pools map[*ConnPool]struct{}
	once  sync.Once
}

var globalPoolCleaner = newPoolCleaner()

func newPoolCleaner() *poolCleaner {
	pc := &poolCleaner{
		pools: make(map[*ConnPool]struct{}),
	}
	go pc.run()
	return pc
}

func (pc *poolCleaner) register(pool *ConnPool) {
	pc.mu.Lock()
	pc.pools[pool] = struct{}{}
	pc.mu.Unlock()
}

func (pc *poolCleaner) unregister(pool *ConnPool) {
	pc.mu.Lock()
	delete(pc.pools, pool)
	pc.mu.Unlock()
}

func (pc *poolCleaner) run() {
	// Increased frequency for better connection health monitoring
	// 100ms interval provides better responsiveness while not being too aggressive
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		pc.clean()
	}
}

func (pc *poolCleaner) clean() {
	pc.mu.RLock()
	if len(pc.pools) == 0 {
		pc.mu.RUnlock()
		return
	}
	pools := make([]*ConnPool, 0, len(pc.pools))
	for p := range pc.pools {
		pools = append(pools, p)
	}
	pc.mu.RUnlock()

	now := time.Now()
	for _, pool := range pools {
		pool.cleanupExpired(now)
	}
}
