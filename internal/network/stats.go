package network

// Package network provides unified, low-overhead stats collection system.
// All stats use atomic operations for lock-free, high-performance updates.
//
// Features:
//   - Zero allocation for counter/gauge updates
//   - Lock-free atomic operations
//   - Optional stats (can be disabled for zero overhead)
//   - Snapshot support for safe stats reading

import (
	"sync/atomic"
)

// Counter is a thread-safe counter using atomic operations
type Counter struct {
	value atomic.Uint64
}

// Inc increments the counter by 1
func (c *Counter) Inc() {
	c.Add(1)
}

// Add adds delta to the counter
func (c *Counter) Add(delta uint64) {
	if c != nil {
		c.value.Add(delta)
	}
}

// Load returns the current counter value
func (c *Counter) Load() uint64 {
	if c == nil {
		return 0
	}
	return c.value.Load()
}

// Store sets the counter value
func (c *Counter) Store(value uint64) {
	if c != nil {
		c.value.Store(value)
	}
}

// Gauge is a thread-safe gauge using atomic operations
type Gauge struct {
	value atomic.Int64
}

// Inc increments the gauge by 1
func (g *Gauge) Inc() {
	g.Add(1)
}

// Dec decrements the gauge by 1
func (g *Gauge) Dec() {
	g.Add(-1)
}

// Add adds delta to the gauge
func (g *Gauge) Add(delta int64) {
	if g != nil {
		g.value.Add(delta)
	}
}

// Load returns the current gauge value
func (g *Gauge) Load() int64 {
	if g == nil {
		return 0
	}
	return g.value.Load()
}

// Store sets the gauge value
func (g *Gauge) Store(value int64) {
	if g != nil {
		g.value.Store(value)
	}
}

// Stats provides a collection of common stats
type Stats struct {
	// Counters
	Requests Counter
	Success  Counter
	Errors   Counter
	Bytes    Counter
	Messages Counter

	// Gauges
	Active    Gauge
	Idle      Gauge
	Waiters   Gauge
	QueueSize Gauge
}

// StatsSnapshot represents a point-in-time snapshot of stats
type StatsSnapshot struct {
	Requests  uint64
	Success   uint64
	Errors    uint64
	Bytes     uint64
	Messages  uint64
	Active    int64
	Idle      int64
	Waiters   int64
	QueueSize int64
}

// Snapshot returns a snapshot of current stats
func (s *Stats) Snapshot() StatsSnapshot {
	if s == nil {
		return StatsSnapshot{}
	}
	return StatsSnapshot{
		Requests:  s.Requests.Load(),
		Success:   s.Success.Load(),
		Errors:    s.Errors.Load(),
		Bytes:     s.Bytes.Load(),
		Messages:  s.Messages.Load(),
		Active:    s.Active.Load(),
		Idle:      s.Idle.Load(),
		Waiters:   s.Waiters.Load(),
		QueueSize: s.QueueSize.Load(),
	}
}

// Reset resets all stats to zero (for testing)
func (s *Stats) Reset() {
	if s == nil {
		return
	}
	s.Requests.Store(0)
	s.Success.Store(0)
	s.Errors.Store(0)
	s.Bytes.Store(0)
	s.Messages.Store(0)
	s.Active.Store(0)
	s.Idle.Store(0)
	s.Waiters.Store(0)
	s.QueueSize.Store(0)
}

// NetworkStats aggregates stats from Server, Pool, and Client components
type NetworkStats struct {
	Server Stats
	Pool   Stats
	Client Stats
}

var globalStats = &NetworkStats{
	Server: Stats{},
	Pool:   Stats{},
	Client: Stats{},
}

// GetStats returns global network stats
func GetStats() *NetworkStats {
	return globalStats
}

// ResetStats resets all stats (for testing)
func ResetStats() {
	globalStats.Server.Reset()
	globalStats.Pool.Reset()
	globalStats.Client.Reset()
}

// NetworkSnapshot represents aggregated network stats snapshot
type NetworkSnapshot struct {
	ServerConnections uint64
	ServerMessages    uint64
	ServerBytes       uint64
	ServerErrors      uint64
	ServerActiveConns int64
	PoolTotal         int64
	PoolActive        int64
	PoolIdle          int64
	PoolWaiters       int64
	PoolCreated       uint64
	PoolClosed        uint64
	PoolErrors        uint64
	ClientRequests    uint64
	ClientResponses   uint64
	ClientErrors      uint64
	ClientBytes       uint64
}

// Snapshot returns a snapshot of current network stats
func (ns *NetworkStats) Snapshot() NetworkSnapshot {
	if ns == nil {
		return NetworkSnapshot{}
	}
	server := ns.Server.Snapshot()
	pool := ns.Pool.Snapshot()
	client := ns.Client.Snapshot()

	return NetworkSnapshot{
		ServerConnections: server.Messages,
		ServerMessages:    server.Messages,
		ServerBytes:       server.Bytes,
		ServerErrors:      server.Errors,
		ServerActiveConns: server.Active,
		PoolTotal:         pool.Active + pool.Idle,
		PoolActive:        pool.Active,
		PoolIdle:          pool.Idle,
		PoolWaiters:       pool.Waiters,
		PoolCreated:       pool.Success,
		PoolClosed:        pool.Errors,
		PoolErrors:        pool.Errors,
		ClientRequests:    client.Requests,
		ClientResponses:   client.Success,
		ClientErrors:      client.Errors,
		ClientBytes:       client.Bytes,
	}
}

// Deprecated: Use GetStats instead
func GetMetrics() *NetworkStats {
	return GetStats()
}

// Deprecated: Use NetworkStats instead
type Metrics = NetworkStats

// Deprecated: Use NetworkSnapshot instead
type NetworkStatsSnapshot = NetworkSnapshot
