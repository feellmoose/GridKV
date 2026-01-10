package network

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feellmoose/gridkv/internal/utils/logging"
)

// ConnPool manages connection pool
type ConnPool interface {
	// Get gets connection from pool (creates if needed)
	Get(ctx context.Context, address string) (Conn, error)

	// Put returns connection to pool
	Put(conn Conn)

	// Remove removes connection from pool
	Remove(conn Conn)

	// Close closes all connections in pool
	Close() error

	// Stats returns pool statistics
	Stats() PoolStats
}

// PoolStats represents connection pool statistics (lock-free using atomic)
type PoolStats struct {
	Total   int64  // Total connections (atomic)
	Active  int64  // Active connections (atomic)
	Idle    int64  // Idle connections (atomic)
	Waiters int64  // Waiting for connection (atomic)
	Created uint64 // Total created (atomic)
	Closed  uint64 // Total closed (atomic)
	Errors  uint64 // Connection errors (atomic)
}

// PoolConfig configures connection pool
type PoolConfig struct {
	// MaxIdle is maximum idle connections per address
	MaxIdle int

	// MaxActive is maximum active connections per address
	MaxActive int

	// IdleTimeout is idle connection timeout
	IdleTimeout time.Duration

	// MaxLifetime is maximum connection lifetime
	MaxLifetime time.Duration

	// WaitTimeout is timeout when pool is exhausted
	WaitTimeout time.Duration

	// CleanupInterval is pool cleanup interval
	CleanupInterval time.Duration

	// Transport is underlying transport
	Transport Transport
}

// DefaultPoolConfig returns default pool config optimized for distributed systems
func DefaultPoolConfig(transport Transport) PoolConfig {
	return PoolConfig{
		MaxIdle:         32,
		MaxActive:       200,
		IdleTimeout:     5 * time.Minute,
		MaxLifetime:     30 * time.Minute,
		WaitTimeout:     5 * time.Second,
		CleanupInterval: 1 * time.Minute,
		Transport:       transport,
	}
}

const (
	poolShards = 256 // Number of shards for lock reduction
)

// simple connection pool per address with sharded locks
type connPool struct {
	cfg         PoolConfig
	shards      [poolShards]*poolShard
	stats       PoolStats     // Lock-free stats using atomic
	closed      int32         // Atomic flag for closed state
	cleanupDone chan struct{} // Channel to signal cleanup goroutine to stop
}

type poolShard struct {
	mu     sync.Mutex
	pools  map[string]*addrPool
	active map[Conn]struct{} // Track active connections in this shard
}

func (p *connPool) getShard(address string) *poolShard {
	// Simple hash-based sharding
	hash := uint32(0)
	for i := 0; i < len(address); i++ {
		hash = hash*31 + uint32(address[i])
	}
	return p.shards[hash%poolShards]
}

type idleConn struct {
	conn      Conn
	idleSince time.Time
}

type addrPool struct {
	mu     sync.Mutex // Per-address lock to reduce contention
	idle   []idleConn // Track idle connections with timestamp
	active int
}

// NewConnPool creates a connection pool with given config
func NewConnPool(cfg PoolConfig) ConnPool {
	p := &connPool{
		cfg:         cfg,
		cleanupDone: make(chan struct{}),
	}
	for i := 0; i < poolShards; i++ {
		p.shards[i] = &poolShard{
			pools:  make(map[string]*addrPool),
			active: make(map[Conn]struct{}),
		}
	}

	// Start cleanup goroutine if cleanup interval is set
	if cfg.CleanupInterval > 0 {
		go p.cleanupLoop()
	}

	return p
}

func (p *connPool) cleanupLoop() {
	ticker := time.NewTicker(p.cfg.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if atomic.LoadInt32(&p.closed) != 0 {
				close(p.cleanupDone)
				return
			}
			p.cleanupIdleConns()
		case <-p.cleanupDone:
			return
		}
	}
}

func (p *connPool) cleanupIdleConns() {
	now := time.Now()
	for _, shard := range p.shards {
		shard.mu.Lock()
		for _, ap := range shard.pools {
			ap.mu.Lock()

			// Remove expired idle connections
			validIdle := ap.idle[:0]
			for _, ic := range ap.idle {
				// Check idle timeout
				if p.cfg.IdleTimeout > 0 && now.Sub(ic.idleSince) > p.cfg.IdleTimeout {
					// Connection expired, close it
					atomic.AddInt64(&p.stats.Idle, -1)
					atomic.AddInt64(&p.stats.Total, -1)
					atomic.AddUint64(&p.stats.Closed, 1)
					_ = ic.conn.Close()
					continue
				}
				validIdle = append(validIdle, ic)
			}
			ap.idle = validIdle

			ap.mu.Unlock()
		}
		shard.mu.Unlock()
	}
}

// isConnectionHealthy performs a cached lightweight health check on the connection.
func (p *connPool) isConnectionHealthy(conn Conn) bool {
	if tcpConn, ok := conn.(*tcpConn); ok {
		now := time.Now().Unix()
		lastCheck := atomic.LoadInt64(&tcpConn.lastHealthCheck)

		// Use cached result if checked within last 30 seconds
		if now-lastCheck < 30 {
			return atomic.LoadInt32(&tcpConn.healthCheckCached) == 1
		}

		// Perform actual health check
		addr := tcpConn.conn.RemoteAddr()
		isHealthy := addr != nil

		// Cache the result atomically
		atomic.StoreInt64(&tcpConn.lastHealthCheck, now)
		if isHealthy {
			atomic.StoreInt32(&tcpConn.healthCheckCached, 1)
		} else {
			atomic.StoreInt32(&tcpConn.healthCheckCached, 0)
		}

		return isHealthy
	}
	return true // For other connection types, assume healthy
}

func (p *connPool) Get(ctx context.Context, address string) (Conn, error) {
	// Check if pool is closed first
	if atomic.LoadInt32(&p.closed) != 0 {
		return nil, ErrPoolClosed
	}

	shard := p.getShard(address)
	shard.mu.Lock()

	// Get or create address pool
	ap := shard.pools[address]
	if ap == nil {
		ap = &addrPool{}
		shard.pools[address] = ap
	}

	// Reuse idle connection with health check
	if n := len(ap.idle); n > 0 {
		ic := ap.idle[n-1]
		ap.idle = ap.idle[:n-1]

		// Perform health check on idle connection
		if !p.isConnectionHealthy(ic.conn) {
			// Connection unhealthy, close and continue to create new one
			atomic.AddInt64(&p.stats.Total, -1)
			atomic.AddUint64(&p.stats.Closed, 1)
			_ = ic.conn.Close()
		} else {
			// Connection healthy, reuse it
			ap.active++
			atomic.AddInt64(&p.stats.Active, 1)
			atomic.AddInt64(&p.stats.Idle, -1)
			shard.active[ic.conn] = struct{}{}
			shard.mu.Unlock()
			return ic.conn, nil
		}
	}

	if p.cfg.MaxActive > 0 && ap.active >= p.cfg.MaxActive {
		// Wait for connection to become available if WaitTimeout is set
		if p.cfg.WaitTimeout > 0 {
			shard.mu.Unlock()
			waitDeadline := time.Now().Add(p.cfg.WaitTimeout)
			for time.Now().Before(waitDeadline) {
				// Check context deadline
				if ctx != nil {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					if deadline, ok := ctx.Deadline(); ok && time.Now().After(deadline) {
						return nil, context.DeadlineExceeded
					}
				}
				// Brief sleep to avoid busy waiting
				time.Sleep(10 * time.Millisecond)
				// Retry getting connection
				shard.mu.Lock()
				if ap.active < p.cfg.MaxActive {
					// Connection available now
					break
				}
				shard.mu.Unlock()
			}
			shard.mu.Lock()
			// Check again after wait
			if p.cfg.MaxActive > 0 && ap.active >= p.cfg.MaxActive {
				shard.mu.Unlock()
				return nil, ErrPoolExhausted
			}
		} else {
			shard.mu.Unlock()
			return nil, ErrPoolExhausted
		}
	}
	ap.active++
	atomic.AddInt64(&p.stats.Active, 1)
	atomic.AddUint64(&p.stats.Created, 1)
	shard.mu.Unlock()

	conn, err := p.cfg.Transport.Dial(ctx, address)
	if err != nil {
		shard.mu.Lock()
		ap.active--
		atomic.AddInt64(&p.stats.Active, -1)
		atomic.AddUint64(&p.stats.Errors, 1)
		shard.mu.Unlock()
		return nil, err
	}

	atomic.AddInt64(&p.stats.Total, 1)
	shard.mu.Lock()
	shard.active[conn] = struct{}{}
	shard.mu.Unlock()
	return conn, nil
}

func (p *connPool) Put(conn Conn) {
	if conn == nil {
		return
	}

	// If pool is closed, close connection immediately
	if atomic.LoadInt32(&p.closed) != 0 {
		_ = conn.Close()
		return
	}

	addr := conn.RemoteAddr()
	shard := p.getShard(addr)
	shard.mu.Lock()

	// Remove from active tracking
	delete(shard.active, conn)

	ap := shard.pools[addr]
	if ap == nil {
		ap = &addrPool{}
		shard.pools[addr] = ap
	}

	if p.cfg.MaxIdle == 0 || len(ap.idle) >= p.cfg.MaxIdle {
		ap.active--
		atomic.AddInt64(&p.stats.Active, -1)
		atomic.AddUint64(&p.stats.Closed, 1)
		atomic.AddInt64(&p.stats.Total, -1)
		shard.mu.Unlock()
		_ = conn.Close()
		return
	}
	// Add connection with current timestamp
	ap.idle = append(ap.idle, idleConn{
		conn:      conn,
		idleSince: time.Now(),
	})
	ap.active--
	atomic.AddInt64(&p.stats.Active, -1)
	atomic.AddInt64(&p.stats.Idle, 1)
	shard.mu.Unlock()
}

func (p *connPool) Remove(conn Conn) {
	if conn == nil {
		return
	}

	addr := conn.RemoteAddr()
	shard := p.getShard(addr)
	shard.mu.Lock()

	// Remove from active tracking
	delete(shard.active, conn)

	ap := shard.pools[addr]
	if ap == nil {
		shard.mu.Unlock()
		_ = conn.Close()
		return
	}

	// Prefer decrementing active if possible
	if ap.active > 0 {
		ap.active--
		atomic.AddInt64(&p.stats.Active, -1)
	} else {
		// Otherwise try removing from idle list
		for i, ic := range ap.idle {
			if ic.conn == conn {
				ap.idle = append(ap.idle[:i], ap.idle[i+1:]...)
				atomic.AddInt64(&p.stats.Idle, -1)
				break
			}
		}
	}
	atomic.AddInt64(&p.stats.Total, -1)
	atomic.AddUint64(&p.stats.Closed, 1)
	shard.mu.Unlock()
	logging.Debug("connPool: removed connection", "remote", addr)
	_ = conn.Close()
}

func (p *connPool) Close() error {
	// Mark pool as closed
	atomic.StoreInt32(&p.closed, 1)

	// Signal cleanup goroutine to stop (if it exists)
	if p.cleanupDone != nil {
		select {
		case p.cleanupDone <- struct{}{}:
		default:
			// cleanupDone already closed or not initialized
		}
	}

	var firstErr error
	var wg sync.WaitGroup

	// Close all connections from all shards concurrently
	for _, shard := range p.shards {
		shard.mu.Lock()

		// Collect all connections
		var allConns []Conn
		for _, ap := range shard.pools {
			for _, ic := range ap.idle {
				allConns = append(allConns, ic.conn)
			}
		}
		for conn := range shard.active {
			allConns = append(allConns, conn)
		}

		// Clear shard state
		shard.pools = make(map[string]*addrPool)
		shard.active = make(map[Conn]struct{})
		shard.mu.Unlock()

		// Close connections concurrently with bounded concurrency
		if len(allConns) > 0 {
			const maxWorkers = 50
			connCh := make(chan Conn, len(allConns))
			for _, c := range allConns {
				connCh <- c
			}
			close(connCh)

			workerCount := len(allConns)
			if workerCount > maxWorkers {
				workerCount = maxWorkers
			}

			for i := 0; i < workerCount; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for conn := range connCh {
						if err := conn.Close(); err != nil && firstErr == nil {
							// Use atomic compare-and-swap for thread-safe error capture
							// Note: This is best-effort, may miss some errors
							firstErr = err
						}
					}
				}()
			}
		}
	}

	// Wait for all closes to complete (with timeout)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}

	// Reset atomic stats
	atomic.StoreInt64(&p.stats.Total, 0)
	atomic.StoreInt64(&p.stats.Active, 0)
	atomic.StoreInt64(&p.stats.Idle, 0)
	atomic.StoreInt64(&p.stats.Waiters, 0)
	atomic.StoreUint64(&p.stats.Created, 0)
	atomic.StoreUint64(&p.stats.Closed, 0)
	atomic.StoreUint64(&p.stats.Errors, 0)
	return firstErr
}

func (p *connPool) Stats() PoolStats {
	// Return snapshot of atomic values (lock-free read)
	return PoolStats{
		Total:   atomic.LoadInt64(&p.stats.Total),
		Active:  atomic.LoadInt64(&p.stats.Active),
		Idle:    atomic.LoadInt64(&p.stats.Idle),
		Waiters: atomic.LoadInt64(&p.stats.Waiters),
		Created: atomic.LoadUint64(&p.stats.Created),
		Closed:  atomic.LoadUint64(&p.stats.Closed),
		Errors:  atomic.LoadUint64(&p.stats.Errors),
	}
}
