package memory

import (
	"runtime"
	"runtime/debug"
	"time"
)

// GCConfig contains GC tuning parameters.
type GCConfig struct {
	GOGCPercent      int           // GOGC percentage (default: 100)
	GOMemLimitMB     int64         // Soft memory limit in MB (Go 1.19+)
	MaxHeapMB        int64         // Target max heap size
	GCInterval       time.Duration // Forced GC interval (0 = disabled)
	EnableGCTuning   bool          // Enable automatic GC tuning
}

// DefaultGCConfig returns recommended GC configuration.
func DefaultGCConfig() *GCConfig {
	return &GCConfig{
		GOGCPercent:    100,
		GOMemLimitMB:   0,    // Auto-detect
		MaxHeapMB:      0,    // Auto-detect
		GCInterval:     0,    // Disabled
		EnableGCTuning: false,
	}
}

// AggressiveGCConfig returns configuration for memory-constrained environments.
func AggressiveGCConfig(maxMemoryMB int64) *GCConfig {
	return &GCConfig{
		GOGCPercent:    50,              // More aggressive GC
		GOMemLimitMB:   maxMemoryMB,
		MaxHeapMB:      maxMemoryMB * 80 / 100, // 80% of limit
		GCInterval:     30 * time.Second,        // Force GC every 30s
		EnableGCTuning: true,
	}
}

// HighPerformanceGCConfig returns configuration for latency-sensitive workloads.
func HighPerformanceGCConfig() *GCConfig {
	return &GCConfig{
		GOGCPercent:    200,  // Less frequent GC
		GOMemLimitMB:   0,
		MaxHeapMB:      0,
		GCInterval:     0,    // Never force
		EnableGCTuning: false,
	}
}

// ApplyGCConfig applies GC configuration.
func ApplyGCConfig(cfg *GCConfig) {
	if cfg == nil {
		return
	}

	// Set GOGC
	if cfg.GOGCPercent > 0 {
		debug.SetGCPercent(cfg.GOGCPercent)
	}

	// Set memory limit (Go 1.19+)
	if cfg.GOMemLimitMB > 0 {
		debug.SetMemoryLimit(cfg.GOMemLimitMB * 1024 * 1024)
	}

	// Start periodic GC if configured
	if cfg.GCInterval > 0 {
		go periodicGC(cfg.GCInterval)
	}
}

// periodicGC forces GC at regular intervals.
func periodicGC(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		runtime.GC()
	}
}

// GetMemoryStats returns current memory statistics.
type MemoryStats struct {
	Alloc         uint64  // Bytes allocated and in use
	TotalAlloc    uint64  // Cumulative bytes allocated
	Sys           uint64  // Bytes obtained from system
	NumGC         uint32  // Number of GC runs
	PauseNs       uint64  // Last GC pause (nanoseconds)
	HeapAlloc     uint64  // Heap bytes allocated
	HeapSys       uint64  // Heap bytes from system
	HeapIdle      uint64  // Heap bytes idle
	HeapInuse     uint64  // Heap bytes in use
	HeapReleased  uint64  // Heap bytes released to OS
	GCCPUFraction float64 // Fraction of CPU time in GC
}

// GetMemoryStats retrieves current memory statistics.
func GetMemoryStats() *MemoryStats {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return &MemoryStats{
		Alloc:         m.Alloc,
		TotalAlloc:    m.TotalAlloc,
		Sys:           m.Sys,
		NumGC:         m.NumGC,
		PauseNs:       m.PauseNs[(m.NumGC+255)%256],
		HeapAlloc:     m.HeapAlloc,
		HeapSys:       m.HeapSys,
		HeapIdle:      m.HeapIdle,
		HeapInuse:     m.HeapInuse,
		HeapReleased:  m.HeapReleased,
		GCCPUFraction: m.GCCPUFraction,
	}
}

// SuggestGCConfig analyzes current memory usage and suggests GC config.
func SuggestGCConfig() *GCConfig {
	stats := GetMemoryStats()
	heapMB := int64(stats.HeapInuse / 1024 / 1024)

	// High memory pressure
	if stats.GCCPUFraction > 0.05 || heapMB > 1024 {
		return AggressiveGCConfig(heapMB * 2)
	}

	// Low memory usage, optimize for latency
	if heapMB < 256 {
		return HighPerformanceGCConfig()
	}

	// Default balanced config
	return DefaultGCConfig()
}

// OptimizeForWorkload applies GC configuration based on workload type.
func OptimizeForWorkload(workload string) {
	var cfg *GCConfig

	switch workload {
	case "high-throughput":
		cfg = HighPerformanceGCConfig()
	case "memory-constrained":
		cfg = AggressiveGCConfig(512) // 512MB limit
	case "latency-sensitive":
		cfg = HighPerformanceGCConfig()
	default:
		cfg = DefaultGCConfig()
	}

	ApplyGCConfig(cfg)
}

// ForceGC forces an immediate garbage collection.
func ForceGC() {
	runtime.GC()
}

// FreeOSMemory forces release of unused memory to OS.
func FreeOSMemory() {
	debug.FreeOSMemory()
}

