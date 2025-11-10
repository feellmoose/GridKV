package pool

import (
	"fmt"
	"sort"
	"sync"
)

// PoolRegistry is a global registry for all object pools in GridKV.
// Week 8: Centralized pool management for better observability.

type PoolRegistry struct {
	pools map[string]*TrackedPool
	mu    sync.RWMutex
}

var globalRegistry = &PoolRegistry{
	pools: make(map[string]*TrackedPool),
}

// Register registers a pool with the global registry.
func Register(name string, pool *TrackedPool) error {
	globalRegistry.mu.Lock()
	defer globalRegistry.mu.Unlock()

	if _, exists := globalRegistry.pools[name]; exists {
		return fmt.Errorf("pool %s already registered", name)
	}

	globalRegistry.pools[name] = pool
	return nil
}

// Get retrieves a registered pool by name.
func Get(name string) (*TrackedPool, bool) {
	globalRegistry.mu.RLock()
	defer globalRegistry.mu.RUnlock()

	pool, ok := globalRegistry.pools[name]
	return pool, ok
}

// GetAllStats returns statistics for all registered pools.
func GetAllStats() []PoolStats {
	globalRegistry.mu.RLock()
	defer globalRegistry.mu.RUnlock()

	stats := make([]PoolStats, 0, len(globalRegistry.pools))
	for _, pool := range globalRegistry.pools {
		stats = append(stats, pool.GetStats())
	}

	// Sort by name for consistent output
	sort.Slice(stats, func(i, j int) bool {
		return stats[i].Name < stats[j].Name
	})

	return stats
}

// PrintStats prints formatted pool statistics.
func PrintStats() {
	stats := GetAllStats()
	
	fmt.Println("\n=== Object Pool Statistics ===")
	fmt.Printf("%-30s %10s %10s %10s %10s %8s %8s\n",
		"Pool", "Gets", "Puts", "Hits", "InUse", "HitRate", "Util%")
	fmt.Println(string(make([]byte, 100)))
	
	for _, s := range stats {
		fmt.Printf("%-30s %10d %10d %10d %10d %7.1f%% %7.1f%%\n",
			s.Name,
			s.Metrics.Gets,
			s.Metrics.Puts,
			s.Metrics.Hits,
			s.Metrics.InUse,
			s.HitRate*100,
			s.Utilization*100,
		)
	}
	
	// Summary
	totalGets := int64(0)
	totalHits := int64(0)
	for _, s := range stats {
		totalGets += s.Metrics.Gets
		totalHits += s.Metrics.Hits
	}
	
	var overallHitRate float64
	if totalGets > 0 {
		overallHitRate = float64(totalHits) / float64(totalGets)
	}
	
	fmt.Printf("\nTotal Pools: %d\n", len(stats))
	fmt.Printf("Overall Hit Rate: %.1f%%\n", overallHitRate*100)
}

// ResetAll resets all pool metrics.
func ResetAll() {
	globalRegistry.mu.RLock()
	defer globalRegistry.mu.RUnlock()

	for _, pool := range globalRegistry.pools {
		pool.Reset()
	}
}

// AnalyzePoolEfficiency identifies poorly performing pools.
func AnalyzePoolEfficiency() map[string]string {
	stats := GetAllStats()
	issues := make(map[string]string)

	for _, s := range stats {
		// Low hit rate (< 30%)
		if s.HitRate < 0.3 && s.Metrics.Gets > 1000 {
			issues[s.Name] = fmt.Sprintf("Low hit rate: %.1f%% (< 30%%)", s.HitRate*100)
		}

		// High contention (> 90% in use)
		if s.Utilization > 0.9 && s.Metrics.PoolSize > 10 {
			issues[s.Name] = fmt.Sprintf("High contention: %.1f%% in use", s.Utilization*100)
		}

		// Idle pool (no usage but taking memory)
		if s.Metrics.Gets > 0 && s.Metrics.InUse == 0 && s.Metrics.PoolSize > 100 {
			issues[s.Name] = fmt.Sprintf("Idle pool with %d objects", s.Metrics.PoolSize)
		}
	}

	return issues
}

