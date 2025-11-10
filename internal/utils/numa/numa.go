package numa

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/feellmoose/gridkv/internal/utils/logging"
)

// CheckNUMAConfig 检查 NUMA 配置并给出建议
// 这个函数应该在 GridKV 启动时调用
func CheckNUMAConfig() {
	if runtime.GOOS != "linux" {
		return // 仅在 Linux 上检测
	}

	// 检查 NUMA 节点数
	nodes, err := getNumaNodeCount()
	if err != nil || nodes <= 1 {
		logging.Debug("Single NUMA node or detection failed, no optimization needed")
		return // 单节点或检测失败，无需优化
	}

	// 检查是否已配置 NUMA 策略
	policy, _ := getCurrentNumaPolicy()
	if policy == "" || policy == "default" {
		// 打印警告
		logging.Warn(fmt.Sprintf("检测到多 NUMA 节点环境 (%d 节点)", nodes))
		logging.Warn("建议使用: numactl --interleave=all ./gridkv")
		logging.Warn("或设置: numactl --cpunodebind=0 --membind=0 ./gridkv")
	} else {
		logging.Info(fmt.Sprintf("NUMA policy configured: %s", policy))
	}
}

// getNumaNodeCount 返回系统的 NUMA 节点数
func getNumaNodeCount() (int, error) {
	data, err := os.ReadFile("/sys/devices/system/node/online")
	if err != nil {
		return 0, err
	}
	
	// 解析格式如 "0-3" 或 "0"
	content := strings.TrimSpace(string(data))
	
	if strings.Contains(content, "-") {
		parts := strings.Split(content, "-")
		if len(parts) == 2 {
			max, err := strconv.Atoi(parts[1])
			if err != nil {
				return 0, err
			}
			return max + 1, nil
		}
	}
	
	// 单节点情况
	return 1, nil
}

// getCurrentNumaPolicy 返回当前进程的 NUMA 策略
func getCurrentNumaPolicy() (string, error) {
	data, err := os.ReadFile("/proc/self/numa_maps")
	if err != nil {
		return "", err
	}
	
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 {
		firstLine := lines[0]
		// 检查策略类型
		if strings.Contains(firstLine, "interleave") {
			return "interleave", nil
		}
		if strings.Contains(firstLine, "bind") {
			return "bind", nil
		}
		if strings.Contains(firstLine, "preferred") {
			return "preferred", nil
		}
	}
	
	return "default", nil
}

// GetNumaStats 返回 NUMA 统计信息（用于监控）
func GetNumaStats() (*NumaStats, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("NUMA stats only available on Linux")
	}

	nodes, err := getNumaNodeCount()
	if err != nil {
		return nil, err
	}

	policy, _ := getCurrentNumaPolicy()

	stats := &NumaStats{
		NodeCount:   nodes,
		Policy:      policy,
		IsOptimized: nodes <= 1 || (policy != "" && policy != "default"),
	}

	return stats, nil
}

// NumaStats 包含 NUMA 相关统计信息
type NumaStats struct {
	NodeCount   int    // NUMA 节点数
	Policy      string // 当前 NUMA 策略
	IsOptimized bool   // 是否已优化
}

// String 返回格式化的统计信息
func (s *NumaStats) String() string {
	if s.NodeCount <= 1 {
		return "Single NUMA node (no optimization needed)"
	}
	return fmt.Sprintf("NUMA nodes: %d, Policy: %s, Optimized: %v",
		s.NodeCount, s.Policy, s.IsOptimized)
}

