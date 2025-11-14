package transport

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

// TransportConn defines the basic operations for pluggable network connections.
type TransportConn interface {
	WriteDataWithContext(ctx context.Context, data []byte) error
	ReadDataWithContext(ctx context.Context) ([]byte, error)
	Close() error
	LocalAddr() net.Addr
	RemoteAddr() net.Addr
}

// HealthCheckable is an optional interface for connections that support health checking
type HealthCheckable interface {
	HealthCheck() error
}

// Transport defines the functionality required for a pluggable network layer.
type Transport interface {
	Dial(address string) (TransportConn, error)
	Listen(address string) (TransportListener, error)
}

// TransportListener defines the basic operations for a listener.
type TransportListener interface {
	Start() error
	HandleMessage(handler func(message []byte) error) TransportListener
	Stop() error
	Addr() net.Addr
}

// pooledConn wraps a TransportConn with last used timestamp.
type pooledTransportConn struct {
	conn     TransportConn
	lastUsed time.Time
}

// ConnPool manages a pool of TransportConn connections.
type ConnPool struct {
	transport   Transport
	address     string
	maxIdle     int
	maxConns    int
	idleTimeout time.Duration

	mu        sync.Mutex
	cond      *sync.Cond // Used to wake up Goroutines waiting for a connection in Get()
	idleConns []pooledTransportConn
	total     int // Total number of connections, both idle and in-use
	closed    bool
	stopCh    chan struct{} // Signal channel to stop cleanupLoop
	stopOnce  sync.Once     // Ensure stopCh is only closed once

	metrics *ConnPoolMetrics
}

// ConnPoolMetrics tracks connection pool statistics for monitoring
type ConnPoolMetrics struct {
	totalGets       int64 // Total Get() calls
	totalPuts       int64 // Total Put() calls
	totalInvalidate int64 // Total Invalidate() calls
	totalWaits      int64 // Number of times Get() had to wait
	totalDialed     int64 // Total new connections created
	totalReused     int64 // Total connections reused from pool
	healthCheckFail int64 // Total health check failures
	mu              sync.RWMutex
}

// GetMetrics returns a snapshot of pool metrics
func (m *ConnPoolMetrics) GetMetrics() map[string]int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return map[string]int64{
		"total_gets":        m.totalGets,
		"total_puts":        m.totalPuts,
		"total_invalidate":  m.totalInvalidate,
		"total_waits":       m.totalWaits,
		"total_dialed":      m.totalDialed,
		"total_reused":      m.totalReused,
		"health_check_fail": m.healthCheckFail,
	}
}

// GetReuseRate returns the connection reuse rate (0-1)
func (m *ConnPoolMetrics) GetReuseRate() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	total := m.totalDialed + m.totalReused
	if total == 0 {
		return 0
	}
	return float64(m.totalReused) / float64(total)
}

// NewConnPool creates a new connection pool.
func NewConnPool(transport Transport, address string, maxIdle, maxConns int, idleTimeout time.Duration) *ConnPool {
	pool := &ConnPool{
		transport:   transport,
		address:     address,
		maxIdle:     maxIdle,
		maxConns:    maxConns,
		idleTimeout: idleTimeout,
		idleConns:   make([]pooledTransportConn, 0, maxIdle),
		stopCh:      make(chan struct{}),
		metrics:     &ConnPoolMetrics{}, // Initialize metrics
	}
	// Initialize the condition variable for waiting on available connections.
	pool.cond = sync.NewCond(&pool.mu)

	// Start the background cleanup Goroutine.
	go pool.cleanupLoop()
	return pool
}

// Prewarm creates initial connections to warm up the pool.
// This reduces connection establishment latency for the first requests.
//
// Parameters:
//   - count: Number of connections to pre-create (capped at maxIdle)
//
// Returns number of successfully created connections
func (p *ConnPool) Prewarm(count int) int {
	if count > p.maxIdle {
		count = p.maxIdle
	}

	created := 0
	for i := 0; i < count; i++ {
		conn, err := p.transport.Dial(p.address)
		if err != nil {
			break
		}

		p.mu.Lock()
		if len(p.idleConns) < p.maxIdle && !p.closed {
			p.idleConns = append(p.idleConns, pooledTransportConn{
				conn:     conn,
				lastUsed: time.Now(),
			})
			p.total++
			created++
		} else {
			// Pool full or closed, close the connection
			conn.Close()
		}
		p.mu.Unlock()
	}

	return created
}

// GetMetrics returns current pool metrics
func (p *ConnPool) GetMetrics() map[string]int64 {
	if p.metrics == nil {
		return make(map[string]int64)
	}
	return p.metrics.GetMetrics()
}

// GetStats returns current pool state
func (p *ConnPool) GetStats() map[string]interface{} {
	p.mu.Lock()
	defer p.mu.Unlock()

	stats := make(map[string]interface{})
	stats["address"] = p.address
	stats["total_connections"] = p.total
	stats["idle_connections"] = len(p.idleConns)
	stats["max_idle"] = p.maxIdle
	stats["max_conns"] = p.maxConns
	stats["idle_timeout"] = p.idleTimeout.String()

	if p.metrics != nil {
		stats["reuse_rate"] = p.metrics.GetReuseRate()
	}

	return stats
}

// cleanupLoop periodically cleans up expired idle connections.
func (p *ConnPool) cleanupLoop() {
	ticker := time.NewTicker(p.idleTimeout / 2)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				return
			}
			now := time.Now()
			// Use slice trick for efficient in-place cleanup
			active := p.idleConns[:0]
			for _, pc := range p.idleConns {
				if now.Sub(pc.lastUsed) < p.idleTimeout {
					active = append(active, pc)
				} else {
					// Close the expired connection and decrement the total count.
					_ = pc.conn.Close()
					p.total--
				}
			}
			p.idleConns = active
			p.mu.Unlock()
		case _, ok := <-p.stopCh:
			// Stop signal received (channel closed), exit immediately
			if !ok {
				return
			}
		}
	}
}

// Get returns an active TransportConn. If the pool is exhausted, it waits until one is available.
// This method is Context-aware, supporting cancellation/timeout while waiting.
func (p *ConnPool) Get(ctx context.Context) (TransportConn, error) {
	// METRICS: Record Get call
	if p.metrics != nil {
		p.metrics.mu.Lock()
		p.metrics.totalGets++
		p.metrics.mu.Unlock()
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	for {
		// 1. Check if the connection pool is closed
		if p.closed {
			return nil, errors.New("connection pool closed")
		}

		now := time.Now()
		// 2. Try to get a connection from the idle list
		for len(p.idleConns) > 0 {
			// Pop connection from the end
			pc := p.idleConns[len(p.idleConns)-1]
			p.idleConns = p.idleConns[:len(p.idleConns)-1]

			// Check for expiration
			if now.Sub(pc.lastUsed) > p.idleTimeout {
				_ = pc.conn.Close()
				p.total--
				continue // Try the next one
			}

			if healthChecker, ok := pc.conn.(HealthCheckable); ok {
				if err := healthChecker.HealthCheck(); err != nil {
					// Connection is broken, close and try next
					_ = pc.conn.Close()
					p.total--

					// METRICS: Record health check failure
					if p.metrics != nil {
						p.metrics.mu.Lock()
						p.metrics.healthCheckFail++
						p.metrics.mu.Unlock()
					}
					continue
				}
			}

			// METRICS: Record connection reuse
			if p.metrics != nil {
				p.metrics.mu.Lock()
				p.metrics.totalReused++
				p.metrics.mu.Unlock()
			}

			return pc.conn, nil // Found a healthy connection
		}

		// 3. Try to create a new connection (if max connections not reached)
		if p.total < p.maxConns {
			p.total++ // Pre-increment total to reserve a spot
			p.mu.Unlock()

			// Critical optimization: Dialing is done outside the lock to prevent blocking
			conn, err := p.transport.Dial(p.address)

			p.mu.Lock() // Re-acquire the lock
			if err != nil {
				// Dial failed, revert the total count
				p.total--
				return nil, err
			}

			// METRICS: Record new connection
			if p.metrics != nil {
				p.metrics.mu.Lock()
				p.metrics.totalDialed++
				p.metrics.mu.Unlock()
			}

			return conn, nil
		}

		// 4. Pool exhausted (total == maxConns and no idle connections). Wait.
		// METRICS: Record wait event
		if p.metrics != nil {
			p.metrics.mu.Lock()
			p.metrics.totalWaits++
			p.metrics.mu.Unlock()
		}

		// Check context for expiration before waiting
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// CRITICAL FIX: Use a goroutine to wake up on context cancellation
		// without this, the goroutine would wait forever even if context is cancelled
		waitDone := make(chan struct{})
		go func() {
			<-ctx.Done()
			select {
			case <-waitDone:
				// Wait finished normally, do nothing
			default:
				p.mu.Lock()
				p.cond.Broadcast() // Wake up all waiters
				p.mu.Unlock()
			}
		}()

		// Block the current Goroutine using Cond.Wait() until a Signal or Broadcast is received
		p.cond.Wait()

		// Signal the context watcher to exit
		select {
		case <-waitDone:
			// Already closed
		default:
			close(waitDone)
		}

		// Check context again after waking up
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// The loop restarts after being signaled
	}
}

// Put returns a connection back to the pool.
func (p *ConnPool) Put(conn TransportConn) {
	// METRICS: Record Put call
	if p.metrics != nil {
		p.metrics.mu.Lock()
		p.metrics.totalPuts++
		p.metrics.mu.Unlock()
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// If the pool is closed or the idle list is full, close the connection and decrement the total count.
	if p.closed || len(p.idleConns) >= p.maxIdle {
		_ = conn.Close()
		p.total--
		p.cond.Signal() // Notify a waiter that total count has decreased (allowing new Dial attempts)
		return
	}

	// Add connection to the pool and update its last used time.
	p.idleConns = append(p.idleConns, pooledTransportConn{
		conn:     conn,
		lastUsed: time.Now(),
	})

	// Critical optimization: Notify a Goroutine waiting in Get() that a new connection is available.
	p.cond.Signal()
}

// Invalidate closes a connection and decrements the total count.
// This should be called instead of Put() when a retrieved connection is found to be broken or unusable.
func (p *ConnPool) Invalidate(conn TransportConn) {
	// METRICS: Record Invalidate call
	if p.metrics != nil {
		p.metrics.mu.Lock()
		p.metrics.totalInvalidate++
		p.metrics.mu.Unlock()
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	_ = conn.Close()
	// Decrement total count to free up space for a new connection.
	if p.total > 0 {
		p.total--
		// Critical optimization: Notify a waiter that the total count has decreased, allowing a new connection to be created.
		p.cond.Signal()
	}
}

// Close closes all connections in the pool and stops the cleanup routine.
func (p *ConnPool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true // Signal cleanupLoop to exit

	// Wake up all waiting Goroutines so they can see p.closed = true and return an error
	p.cond.Broadcast()

	// Close all current idle connections.
	for _, pc := range p.idleConns {
		_ = pc.conn.Close()
	}
	p.idleConns = nil
	p.total = 0
	p.mu.Unlock()

	// Signal cleanupLoop to exit immediately by closing the channel
	// This ensures all waiting goroutines receive the signal
	// Use sync.Once to prevent closing the channel multiple times
	p.stopOnce.Do(func() {
		close(p.stopCh)
	})
	return nil
}
