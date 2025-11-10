package main

import (
	"fmt"
	"log"
	"runtime"
	"time"

	gridkv "github.com/feellmoose/gridkv"
	"github.com/feellmoose/gridkv/internal/storage"
	"github.com/feellmoose/gridkv/internal/utils/numa"
	"github.com/feellmoose/gridkv/internal/utils/system"
)

// This example demonstrates how to detect and avoid common production pitfalls
// that can cause 10-25% performance degradation.

func main() {
	fmt.Println("==============================================")
	fmt.Println("GridKV Production Pitfalls Detection Example")
	fmt.Println("==============================================")
	fmt.Println()

	// Step 1: Check CPU Configuration
	fmt.Println("📊 Step 1: CPU Configuration Check")
	fmt.Println("──────────────────────────────────────────")
	cpuInfo, err := system.GetCPUInfo()
	if err != nil {
		fmt.Printf("⚠️  Could not detect CPU info: %v\n", err)
	} else {
		fmt.Printf("  CPU Cores: %d\n", cpuInfo.CoreCount)
		if cpuInfo.CurrentFreqMHz > 0 {
			fmt.Printf("  Current Frequency: %.0f MHz\n", cpuInfo.CurrentFreqMHz)
			fmt.Printf("  Max Frequency: %.0f MHz\n", cpuInfo.MaxFreqMHz)
			fmt.Printf("  Frequency Ratio: %.1f%%\n", cpuInfo.FreqRatio*100)

			if cpuInfo.FreqRatio < 0.90 {
				fmt.Println("  ⚠️  WARNING: CPU frequency is low!")
				fmt.Println("     Fix: sudo cpupower frequency-set -g performance")
			} else {
				fmt.Println("  ✅ CPU frequency is good")
			}
		}

		if cpuInfo.ScalingGovernor != "" {
			fmt.Printf("  Scaling Governor: %s\n", cpuInfo.ScalingGovernor)
			if cpuInfo.ScalingGovernor != "performance" {
				fmt.Println("  ⚠️  WARNING: Not using 'performance' governor")
			} else {
				fmt.Println("  ✅ Governor is optimal")
			}
		}
	}
	fmt.Println()

	// Step 2: Check Memory Configuration
	fmt.Println("💾 Step 2: Memory Configuration Check")
	fmt.Println("──────────────────────────────────────────")
	memInfo, err := system.GetMemoryInfo()
	if err != nil {
		fmt.Printf("⚠️  Could not detect memory info: %v\n", err)
	} else {
		fmt.Printf("  Total Memory: %d MB\n", memInfo.TotalMB)
		fmt.Printf("  Available Memory: %d MB\n", memInfo.AvailableMB)
		fmt.Printf("  Used: %.1f%%\n", memInfo.UsedPercent)
		fmt.Printf("  Swap Total: %d MB\n", memInfo.SwapTotalMB)
		fmt.Printf("  Swap Used: %d MB\n", memInfo.SwapUsedMB)

		if memInfo.SwapUsedMB > 0 {
			fmt.Println("  ⚠️  WARNING: Swap is being used!")
			fmt.Println("     Fix: sudo swapoff -a")
		} else if memInfo.SwapTotalMB > 0 {
			fmt.Println("  ⚠️  Swap is enabled but not in use")
			fmt.Println("     Consider: sudo swapoff -a")
		} else {
			fmt.Println("  ✅ Swap is disabled")
		}

		recommendedMem := memInfo.TotalMB * 70 / 100
		fmt.Printf("\n  Recommended MaxMemoryMB: %d MB (70%% of total)\n", recommendedMem)
	}
	fmt.Println()

	// Step 3: Check NUMA Configuration
	fmt.Println("🔗 Step 3: NUMA Configuration Check")
	fmt.Println("──────────────────────────────────────────")
	numaStats, err := numa.GetNumaStats()
	if err != nil {
		fmt.Printf("⚠️  Could not detect NUMA info: %v\n", err)
	} else {
		fmt.Printf("  NUMA Nodes: %d\n", numaStats.NodeCount)
		fmt.Printf("  NUMA Policy: %s\n", numaStats.Policy)
		fmt.Printf("  Optimized: %v\n", numaStats.IsOptimized)

		if numaStats.NodeCount > 1 && !numaStats.IsOptimized {
			fmt.Println("  ⚠️  WARNING: Multi-NUMA without optimization!")
			fmt.Println("     Fix: numactl --interleave=all ./gridkv")
		} else if numaStats.NodeCount > 1 {
			fmt.Println("  ✅ NUMA optimization applied")
		} else {
			fmt.Println("  ✅ Single NUMA node (no optimization needed)")
		}
	}
	fmt.Println()

	// Step 4: Check Go Version
	fmt.Println("🔧 Step 4: Go Runtime Check")
	fmt.Println("──────────────────────────────────────────")
	goVersion := runtime.Version()
	fmt.Printf("  Go Version: %s\n", goVersion)

	// Go 1.20+ has improved sync.Pool
	if goVersion >= "go1.20" {
		fmt.Println("  ✅ Go version supports optimized sync.Pool")
	} else {
		fmt.Println("  ⚠️  WARNING: Upgrade to Go 1.20+ recommended")
	}
	fmt.Println()

	// Step 5: Display Recommended Configuration
	fmt.Println("⚙️  Step 5: Recommended GridKV Configuration")
	fmt.Println("──────────────────────────────────────────")

	config := storage.GetRecommendedConfig()
	fmt.Println("  Based on your system, recommended settings:")
	fmt.Printf("    MaxMemoryMB:    %d MB\n", config.MaxMemoryMB)
	fmt.Printf("    ShardCount:     %d (CPU × 4)\n", config.ShardCount)
	fmt.Printf("    MaxConnections: %d (CPU × 2)\n", config.MaxConnections)
	fmt.Printf("    IdleTimeout:    %d seconds\n", config.IdleTimeout)
	fmt.Printf("    GOGC:           %d%%\n", config.GOGCPercent)
	fmt.Println()

	// Step 6: Create an optimized GridKV instance with pitfall avoidance
	fmt.Println("🚀 Step 6: Creating Optimized GridKV Instance")
	fmt.Println("──────────────────────────────────────────")

	// Apply GC optimization
	storage.ApplyGCOptimizations()
	fmt.Println("  ✓ Applied GC optimizations (GOGC=200)")

	opts := &gridkv.GridKVOptions{
		LocalNodeID:  "production-node-1",
		LocalAddress: "localhost:9001",
		Network: &gridkv.NetworkOptions{
			Type:         gridkv.TCP, // 使用 TCP（已移除 UDP）
			BindAddr:     "localhost:9001",
			MaxConns:     config.MaxConnections,
			MaxIdle:      config.MaxConnections / 10,
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 5 * time.Second,
		},
		Storage: &gridkv.StorageOptions{
			Backend:     gridkv.BackendMemorySharded,
			MaxMemoryMB: config.MaxMemoryMB,
		},
		ReplicaCount:   1,
		WriteQuorum:    1,
		ReadQuorum:     1,
		VirtualNodes:   150,
		MaxReplicators: 8,

		// 避免 Gossip 协议风暴：延长周期到 3s（大集群推荐）
		GossipInterval: 3 * time.Second,  // 默认 1s，大集群改为 3s
		FailureTimeout: 10 * time.Second, // 放宽故障检测
		SuspectTimeout: 20 * time.Second,

		ReplicationTimeout: 2 * time.Second,
		ReadTimeout:        2 * time.Second,
	}

	fmt.Println("  ✓ Configuration prepared with anti-pitfall settings")
	fmt.Println("    - Using TCP transport (reliable, no UDP storm)")
	fmt.Println("    - Gossip interval: 3s (avoid protocol storm)")
	fmt.Println("    - Memory-only storage (no disk I/O)")
	fmt.Printf("    - Shard count: %d (CPU-adaptive)\n", config.ShardCount)
	fmt.Println()

	// Create the instance (with automatic system checks)
	kv, err := gridkv.NewGridKV(opts)
	if err != nil {
		log.Fatalf("Failed to create GridKV: %v", err)
	}
	defer kv.Close()

	fmt.Println("  ✓ GridKV instance created successfully")
	fmt.Println()

	// Step 7: Performance Tips
	fmt.Println("💡 Step 7: Additional Performance Tips")
	fmt.Println("──────────────────────────────────────────")
	fmt.Println("  1. CPU Frequency:")
	fmt.Println("     sudo cpupower frequency-set -g performance")
	fmt.Println()
	fmt.Println("  2. Disable Swap:")
	fmt.Println("     sudo swapoff -a")
	fmt.Println()
	fmt.Println("  3. NUMA Optimization:")
	fmt.Println("     numactl --interleave=all ./gridkv")
	fmt.Println()
	fmt.Println("  4. File Descriptor Limit:")
	fmt.Println("     ulimit -n 65536")
	fmt.Println()
	fmt.Println("  5. Network Tuning:")
	fmt.Println("     sudo sysctl -w net.ipv4.tcp_tw_reuse=1")
	fmt.Println()

	fmt.Println("==============================================")
	fmt.Println("✅ All pitfall checks completed!")
	fmt.Println("==============================================")
	fmt.Println()
	fmt.Println("For production deployment, ensure all warnings are addressed.")
	fmt.Println("Expected performance gain after fixing all pitfalls: +10% to +25%")
	fmt.Println()
	fmt.Println("See docs/PRODUCTION_PITFALLS_CN.md for detailed guidance.")
}
