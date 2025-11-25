// Package transport provides pluggable network transport layer for GridKV.
//
// This package defines interfaces and implementations for network communication
// between GridKV nodes. Supports multiple transport protocols:
//   - TCP: Reliable transport (default, recommended)
//   - QUIC: High-performance UDP-based transport with reliability
//   - GNET: Event-driven transport (Linux/macOS only)
//   - UDP: High-performance UDP transport
//
// Features:
//   - Connection pooling for efficient resource usage
//   - Automatic connection health checking
//   - Configurable timeouts and retry logic
//   - Transport-agnostic interface for easy protocol switching
//
// Thread-safety: All implementations are safe for concurrent access.
package transport

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
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
	inUse     int // Number of connections currently in use
	closed    bool
	metrics   *ConnPoolMetrics
}

type ConnPoolMetrics struct {
	totalGets         atomic.Int64
	totalPuts         atomic.Int64
	totalInvalidate   atomic.Int64
	totalWaits        atomic.Int64
	totalDialed       atomic.Int64
	totalReused       atomic.Int64
	healthCheckFail   atomic.Int64
	totalExhausted    atomic.Int64 // Connection pool exhausted errors
	totalDialFailures atomic.Int64 // Dial failures (connection refused, etc.)
	totalWaitTimeouts atomic.Int64 // Waits that timed out
}

func (m *ConnPoolMetrics) GetMetrics() map[string]int64 {
	return map[string]int64{
		"total_gets":          m.totalGets.Load(),
		"total_puts":          m.totalPuts.Load(),
		"total_invalidate":    m.totalInvalidate.Load(),
		"total_waits":         m.totalWaits.Load(),
		"total_dialed":        m.totalDialed.Load(),
		"total_reused":        m.totalReused.Load(),
		"health_check_fail":   m.healthCheckFail.Load(),
		"total_exhausted":     m.totalExhausted.Load(),
		"total_dial_failures": m.totalDialFailures.Load(),
		"total_wait_timeouts": m.totalWaitTimeouts.Load(),
	}
}

func (m *ConnPoolMetrics) GetReuseRate() float64 {
	totalDialed := m.totalDialed.Load()
	totalReused := m.totalReused.Load()
	total := totalDialed + totalReused
	if total == 0 {
		return 0
	}
	return float64(totalReused) / float64(total)
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
		metrics:     &ConnPoolMetrics{}, // Initialize metrics
	}
	// Initialize the condition variable for waiting on available connections.
	pool.cond = sync.NewCond(&pool.mu)
	globalPoolCleaner.register(pool)
	return pool
}

// Prewarm creates initial connections to warm up the pool.
// This reduces connection establishment latency for the first requests.
// Enhanced: parallel prewarming for faster initialization
//
// Parameters:
//   - count: Number of connections to pre-create (capped at maxIdle)
//
// Returns number of successfully created connections
func (p *ConnPool) Prewarm(count int) int {
	if count > p.maxIdle {
		count = p.maxIdle
	}
	if count <= 0 {
		return 0
	}

	// Parallel prewarming for faster initialization (max 10 concurrent)
	maxConcurrent := 10
	if count < maxConcurrent {
		maxConcurrent = count
	}

	type result struct {
		conn TransportConn
		err  error
	}
	results := make(chan result, count)

	// Create connections in parallel
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrent)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}        // Acquire semaphore
			defer func() { <-sem }() // Release semaphore

			conn, err := p.transport.Dial(p.address)
			results <- result{conn: conn, err: err}
		}()
	}

	// Wait for all dials to complete
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect successful connections
	created := 0
	p.mu.Lock()
	defer p.mu.Unlock()

	for res := range results {
		if res.err != nil {
			continue
		}
		if len(p.idleConns) < p.maxIdle && !p.closed && p.total < p.maxConns {
			p.idleConns = append(p.idleConns, pooledTransportConn{
				conn:     res.conn,
				lastUsed: time.Now(),
			})
			p.total++
			created++
		} else {
			// Pool full or closed, close the connection
			res.conn.Close()
		}
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
	stats["in_use_connections"] = p.inUse
	stats["max_idle"] = p.maxIdle
	stats["max_conns"] = p.maxConns
	stats["idle_timeout"] = p.idleTimeout.String()

	// Calculate utilization rate
	utilizationRate := 0.0
	if p.maxConns > 0 {
		utilizationRate = float64(p.total) / float64(p.maxConns)
	}
	stats["utilization_rate"] = utilizationRate

	// Calculate availability rate
	availabilityRate := 0.0
	if p.maxConns > 0 {
		available := p.maxConns - p.total + len(p.idleConns)
		availabilityRate = float64(available) / float64(p.maxConns)
	}
	stats["availability_rate"] = availabilityRate

	if p.metrics != nil {
		stats["reuse_rate"] = p.metrics.GetReuseRate()
		// Calculate saturation rate: (total_waits / total_gets) indicates pool pressure
		metrics := p.metrics.GetMetrics()
		totalGets := metrics["total_gets"]
		totalWaits := metrics["total_waits"]
		if totalGets > 0 {
			stats["saturation_rate"] = float64(totalWaits) / float64(totalGets)
		} else {
			stats["saturation_rate"] = 0.0
		}
	}

	return stats
}

// AdjustPoolSize dynamically adjusts pool size based on usage
// Returns true if adjustment was made
func (p *ConnPool) AdjustPoolSize() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return false
	}

	stats := p.getStatsUnlocked()
	utilizationRate := stats["utilization_rate"].(float64)
	saturationRate := stats["saturation_rate"].(float64)

	// Increase pool size if utilization > 80% and saturation > 10%
	if utilizationRate > 0.8 && saturationRate > 0.1 && p.maxConns < 2000 {
		// Increase by 25% but cap at 2000
		newMaxConns := int(float64(p.maxConns) * 1.25)
		if newMaxConns > 2000 {
			newMaxConns = 2000
		}
		if newMaxConns > p.maxConns {
			p.maxConns = newMaxConns
			// Also increase maxIdle proportionally
			newMaxIdle := int(float64(p.maxIdle) * 1.25)
			if newMaxIdle > 500 {
				newMaxIdle = 500
			}
			if newMaxIdle > p.maxIdle {
				p.maxIdle = newMaxIdle
			}
			return true
		}
	}

	// Decrease pool size if utilization < 50% for extended period
	if utilizationRate < 0.5 && saturationRate < 0.01 {
		// Decrease by 10% but keep minimum
		minConns := 10
		newMaxConns := int(float64(p.maxConns) * 0.9)
		if newMaxConns < minConns {
			newMaxConns = minConns
		}
		if newMaxConns < p.maxConns && p.maxConns > minConns {
			p.maxConns = newMaxConns
			// Also decrease maxIdle proportionally
			newMaxIdle := int(float64(p.maxIdle) * 0.9)
			if newMaxIdle < 5 {
				newMaxIdle = 5
			}
			if newMaxIdle < p.maxIdle && p.maxIdle > 5 {
				p.maxIdle = newMaxIdle
			}
			return true
		}
	}

	return false
}

// getStatsUnlocked returns stats without locking (caller must hold lock)
func (p *ConnPool) getStatsUnlocked() map[string]interface{} {
	stats := make(map[string]interface{})

	utilizationRate := 0.0
	if p.maxConns > 0 {
		utilizationRate = float64(p.total) / float64(p.maxConns)
	}
	stats["utilization_rate"] = utilizationRate

	saturationRate := 0.0
	if p.metrics != nil {
		metrics := p.metrics.GetMetrics()
		totalGets := metrics["total_gets"]
		totalWaits := metrics["total_waits"]
		if totalGets > 0 {
			saturationRate = float64(totalWaits) / float64(totalGets)
		}
	}
	stats["saturation_rate"] = saturationRate

	return stats
}

func (p *ConnPool) cleanupExpired(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed || len(p.idleConns) == 0 {
		return
	}

	active := p.idleConns[:0]
	for _, pc := range p.idleConns {
		// Check if connection is too old
		if now.Sub(pc.lastUsed) > p.idleTimeout*2 {
			_ = pc.conn.Close()
			p.total--
			if p.metrics != nil {
				p.metrics.healthCheckFail.Add(1)
			}
			continue
		}

		if healthCheckable, ok := pc.conn.(HealthCheckable); ok {
			if err := healthCheckable.HealthCheck(); err != nil {
				_ = pc.conn.Close()
				p.total--
				if p.metrics != nil {
					p.metrics.healthCheckFail.Add(1)
				}
				continue
			}
		}

		// Keep connection if not expired
		if now.Sub(pc.lastUsed) < p.idleTimeout {
			active = append(active, pc)
		} else {
			_ = pc.conn.Close()
			p.total--
		}
	}
	p.idleConns = active
}

func (p *ConnPool) Get(ctx context.Context) (TransportConn, error) {
	if p.metrics != nil {
		p.metrics.totalGets.Add(1)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	var dialAttempts int
	const maxDialRetries = 3
	const baseBackoff = 50 * time.Millisecond

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
					_ = pc.conn.Close()
					p.total--
					if p.metrics != nil {
						p.metrics.healthCheckFail.Add(1)
					}
					continue
				}
			}

			if p.metrics != nil {
				p.metrics.totalReused.Add(1)
			}

			p.inUse++           // Track connection in use
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
				if p.metrics != nil {
					p.metrics.totalDialFailures.Add(1)
				}

				// Check if this is a connection refused error that might be transient
				errStr := err.Error()
				isConnectionRefused := strings.Contains(errStr, "connection refused") ||
					strings.Contains(errStr, "connect: connection refused")
				isTransient := isConnectionRefused && dialAttempts < maxDialRetries

				if isTransient {
					// Exponential backoff for transient connection refused errors
					dialAttempts++
					backoff := baseBackoff * time.Duration(1<<uint(dialAttempts-1)) // 50ms, 100ms, 200ms
					if backoff > 500*time.Millisecond {
						backoff = 500 * time.Millisecond // Cap at 500ms
					}

					// Check context deadline before waiting
					if deadline, ok := ctx.Deadline(); ok {
						remaining := time.Until(deadline)
						if remaining <= backoff {
							return nil, err
						}
					}

					p.mu.Unlock()
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(backoff):
					}
					p.mu.Lock()
					continue // Retry dial
				}

				// Permanent failure or max retries reached
				return nil, err
			}

			// Success - reset retry state
			dialAttempts = 0

			if p.metrics != nil {
				p.metrics.totalDialed.Add(1)
			}

			p.inUse++ // Track connection in use
			return conn, nil
		}

		if p.metrics != nil {
			p.metrics.totalWaits.Add(1)
		}

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		maxWait := 100 * time.Millisecond
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return nil, ctx.Err()
			}
			if remaining < maxWait {
				maxWait = remaining
			}
		}

		waitDeadline := time.Now().Add(maxWait)
		for p.total >= p.maxConns && !p.closed {
			remaining := time.Until(waitDeadline)
			if remaining <= 0 {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, errors.New("connection pool exhausted: max connections reached")
			}

			// Wait with timeout by periodically checking
			// We can't use cond.Wait with timeout directly, so we use a short wait interval
			waitInterval := 50 * time.Millisecond
			if remaining < waitInterval {
				waitInterval = remaining
			}

			// Unlock, wait, then re-lock
			p.mu.Unlock()
			time.Sleep(waitInterval)
			p.mu.Lock()

			// Check if we should continue waiting
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if time.Now().After(waitDeadline) {
				return nil, errors.New("connection pool exhausted: max connections reached")
			}
		}

		if p.closed {
			return nil, errors.New("connection pool closed")
		}
		continue
	}
}

func (p *ConnPool) Put(conn TransportConn) {
	if p.metrics != nil {
		p.metrics.totalPuts.Add(1)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Decrement in-use count
	if p.inUse > 0 {
		p.inUse--
	}

	// If the pool is closed or the idle list is full, close the connection and decrement the total count.
	if p.closed || len(p.idleConns) >= p.maxIdle {
		_ = conn.Close()
		p.total--
		p.cond.Signal() // Notify a waiter that total count has decreased (allowing new Dial attempts)
		return
	}

	// Check connection health before adding to pool
	if healthChecker, ok := conn.(HealthCheckable); ok {
		if err := healthChecker.HealthCheck(); err != nil {
			// Connection is unhealthy, close it and don't add to pool
			_ = conn.Close()
			p.total--
			p.cond.Signal()
			return
		}
	}

	// Add connection to the pool and update its last used time.
	p.idleConns = append(p.idleConns, pooledTransportConn{
		conn:     conn,
		lastUsed: time.Now(),
	})

	// Critical optimization: Notify a Goroutine waiting in Get() that a new connection is available.
	p.cond.Signal()
}

func (p *ConnPool) Invalidate(conn TransportConn) {
	if p.metrics != nil {
		p.metrics.totalInvalidate.Add(1)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	_ = conn.Close()
	// Decrement in-use count
	if p.inUse > 0 {
		p.inUse--
	}
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
	globalPoolCleaner.unregister(p)

	// Wake up all waiting Goroutines so they can see p.closed = true and return an error
	p.cond.Broadcast()

	// Close all current idle connections.
	for _, pc := range p.idleConns {
		_ = pc.conn.Close()
	}
	p.idleConns = nil
	p.mu.Unlock()

	// Wait briefly for in-use connections to be returned (short timeout to avoid blocking)
	p.mu.Lock()
	maxWaitTime := 500 * time.Millisecond // Increased for better cleanup
	deadline := time.Now().Add(maxWaitTime)
	iterations := 0
	maxIterations := 10 // Max 10 iterations (500ms total)
	for p.inUse > 0 && time.Now().Before(deadline) && iterations < maxIterations {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		p.mu.Unlock()
		time.Sleep(50 * time.Millisecond) // Poll every 50ms
		iterations++
		p.mu.Lock()
	}

	// Reset counters - remaining connections will be closed when Put/Invalidate is called
	if p.inUse > 0 {
		// Some connections still in use, but we've marked pool as closed
		// They will be cleaned up when Put/Invalidate is called
		p.inUse = 0
	}
	p.mu.Unlock()

	return nil
}
