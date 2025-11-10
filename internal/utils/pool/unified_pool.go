package pool

import (
	"sync"
	"sync/atomic"
)

// UnifiedPoolManager manages all object pools across GridKV for better observability and tuning.
// Week 8: Memory deep optimization with centralized pool management.

type PoolMetrics struct {
	Gets      int64
	Puts      int64
	Hits      int64 // Successful Get from pool (not from New)
	Misses    int64 // Had to call New
	InUse     int64 // Currently checked out
	PoolSize  int64 // Approximate pool size
}

type PoolStats struct {
	Name        string
	Metrics     PoolMetrics
	HitRate     float64
	Utilization float64
}

// TrackedPool wraps sync.Pool with metrics tracking.
type TrackedPool struct {
	name    string
	pool    sync.Pool
	metrics PoolMetrics
	newFunc func() interface{}
}

// NewTrackedPool creates a new pool with metrics.
func NewTrackedPool(name string, newFunc func() interface{}) *TrackedPool {
	tp := &TrackedPool{
		name:    name,
		newFunc: newFunc,
	}
	tp.pool.New = func() interface{} {
		atomic.AddInt64(&tp.metrics.Misses, 1)
		atomic.AddInt64(&tp.metrics.InUse, 1)
		return newFunc()
	}
	return tp
}

// Get retrieves an object from the pool.
func (tp *TrackedPool) Get() interface{} {
	atomic.AddInt64(&tp.metrics.Gets, 1)
	obj := tp.pool.Get()
	
	// Check if this was from pool (not New)
	// Note: This is approximate as sync.Pool may call New internally
	if atomic.LoadInt64(&tp.metrics.Puts) > 0 {
		atomic.AddInt64(&tp.metrics.Hits, 1)
	}
	
	return obj
}

// Put returns an object to the pool.
func (tp *TrackedPool) Put(obj interface{}) {
	if obj != nil {
		atomic.AddInt64(&tp.metrics.Puts, 1)
		atomic.AddInt64(&tp.metrics.InUse, -1)
		tp.pool.Put(obj)
	}
}

// GetStats returns current pool statistics.
func (tp *TrackedPool) GetStats() PoolStats {
	gets := atomic.LoadInt64(&tp.metrics.Gets)
	hits := atomic.LoadInt64(&tp.metrics.Hits)
	inUse := atomic.LoadInt64(&tp.metrics.InUse)
	
	var hitRate, utilization float64
	if gets > 0 {
		hitRate = float64(hits) / float64(gets)
	}
	
	// Approximate pool size (puts - gets + inUse)
	puts := atomic.LoadInt64(&tp.metrics.Puts)
	poolSize := puts - gets + inUse
	if poolSize < 0 {
		poolSize = 0
	}
	
	if poolSize > 0 {
		utilization = float64(inUse) / float64(poolSize)
	}
	
	return PoolStats{
		Name: tp.name,
		Metrics: PoolMetrics{
			Gets:     gets,
			Puts:     puts,
			Hits:     hits,
			Misses:   atomic.LoadInt64(&tp.metrics.Misses),
			InUse:    inUse,
			PoolSize: poolSize,
		},
		HitRate:     hitRate,
		Utilization: utilization,
	}
}

// Reset resets pool metrics (for testing).
func (tp *TrackedPool) Reset() {
	atomic.StoreInt64(&tp.metrics.Gets, 0)
	atomic.StoreInt64(&tp.metrics.Puts, 0)
	atomic.StoreInt64(&tp.metrics.Hits, 0)
	atomic.StoreInt64(&tp.metrics.Misses, 0)
	atomic.StoreInt64(&tp.metrics.InUse, 0)
}

// GlobalPoolRegistry tracks all pools in the system.
var (
	globalPools   []*TrackedPool
	globalPoolsMu sync.RWMutex
)

// RegisterPool registers a pool with the global registry.
func RegisterPool(pool *TrackedPool) {
	globalPoolsMu.Lock()
	defer globalPoolsMu.Unlock()
	globalPools = append(globalPools, pool)
}

// GetAllPoolStats returns statistics for all registered pools.
func GetAllPoolStats() []PoolStats {
	globalPoolsMu.RLock()
	defer globalPoolsMu.RUnlock()
	
	stats := make([]PoolStats, len(globalPools))
	for i, pool := range globalPools {
		stats[i] = pool.GetStats()
	}
	return stats
}

// ResetAllPools resets metrics for all registered pools.
func ResetAllPools() {
	globalPoolsMu.RLock()
	defer globalPoolsMu.RUnlock()
	
	for _, pool := range globalPools {
		pool.Reset()
	}
}

// GetPoolByName retrieves a pool by name.
func GetPoolByName(name string) *TrackedPool {
	globalPoolsMu.RLock()
	defer globalPoolsMu.RUnlock()
	
	for _, pool := range globalPools {
		if pool.name == name {
			return pool
		}
	}
	return nil
}

// PoolHealthReport generates a health report for all pools.
type PoolHealthReport struct {
	TotalPools      int
	HealthyPools    int
	LowHitRatePools []string // < 50% hit rate
	HighInUsePools  []string // > 80% in use
	IdlePools       []string // 0% in use
}

// GetPoolHealth analyzes pool health.
func GetPoolHealth() PoolHealthReport {
	stats := GetAllPoolStats()
	
	report := PoolHealthReport{
		TotalPools:      len(stats),
		LowHitRatePools: make([]string, 0),
		HighInUsePools:  make([]string, 0),
		IdlePools:       make([]string, 0),
	}
	
	for _, s := range stats {
		// Healthy if hit rate > 50%
		if s.HitRate > 0.5 || s.Metrics.Gets < 100 {
			report.HealthyPools++
		} else if s.HitRate < 0.5 && s.Metrics.Gets >= 100 {
			report.LowHitRatePools = append(report.LowHitRatePools, s.Name)
		}
		
		// High in-use warning
		if s.Utilization > 0.8 && s.Metrics.PoolSize > 10 {
			report.HighInUsePools = append(report.HighInUsePools, s.Name)
		}
		
		// Idle pool
		if s.Metrics.InUse == 0 && s.Metrics.Gets > 100 {
			report.IdlePools = append(report.IdlePools, s.Name)
		}
	}
	
	return report
}

