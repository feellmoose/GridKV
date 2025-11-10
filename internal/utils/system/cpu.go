package system

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/feellmoose/gridkv/internal/utils/logging"
)

// CPUInfo 包含 CPU 相关信息
type CPUInfo struct {
	CoreCount       int     // CPU 核心数
	CurrentFreqMHz  float64 // 当前频率 (MHz)
	MaxFreqMHz      float64 // 最大频率 (MHz)
	ScalingGovernor string  // 频率调节策略
	FreqRatio       float64 // 当前频率占最大频率的比例
}

// GetCPUInfo 获取 CPU 信息
func GetCPUInfo() (*CPUInfo, error) {
	info := &CPUInfo{
		CoreCount: runtime.NumCPU(),
	}

	if runtime.GOOS != "linux" {
		return info, nil // 非 Linux 系统仅返回核心数
	}

	// 读取当前频率
	currentFreq, err := getCurrentCPUFreq()
	if err == nil {
		info.CurrentFreqMHz = currentFreq
	}

	// 读取最大频率
	maxFreq, err := getMaxCPUFreq()
	if err == nil {
		info.MaxFreqMHz = maxFreq
	}

	// 读取调节策略
	governor, err := getCPUGovernor()
	if err == nil {
		info.ScalingGovernor = governor
	}

	// 计算频率比例
	if info.MaxFreqMHz > 0 && info.CurrentFreqMHz > 0 {
		info.FreqRatio = info.CurrentFreqMHz / info.MaxFreqMHz
	}

	return info, nil
}

// getCurrentCPUFreq 获取当前 CPU 频率（MHz）
func getCurrentCPUFreq() (float64, error) {
	// 从 /proc/cpuinfo 读取
	file, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return 0, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "cpu MHz") {
			parts := strings.Split(line, ":")
			if len(parts) == 2 {
				freq, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
				if err == nil {
					return freq, nil
				}
			}
		}
	}

	return 0, fmt.Errorf("CPU frequency not found in /proc/cpuinfo")
}

// getMaxCPUFreq 获取最大 CPU 频率（MHz）
func getMaxCPUFreq() (float64, error) {
	data, err := os.ReadFile("/sys/devices/system/cpu/cpu0/cpufreq/cpuinfo_max_freq")
	if err != nil {
		return 0, err
	}

	// 文件中的值是 kHz，需要转换为 MHz
	freqKHz, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
	if err != nil {
		return 0, err
	}

	return freqKHz / 1000, nil
}

// getCPUGovernor 获取 CPU 频率调节策略
func getCPUGovernor() (string, error) {
	data, err := os.ReadFile("/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor")
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(data)), nil
}

// CheckCPUConfig 检查 CPU 配置并给出警告
func CheckCPUConfig() {
	info, err := GetCPUInfo()
	if err != nil {
		logging.Debug("Failed to get CPU info", "error", err)
		return
	}

	logging.Info(fmt.Sprintf("CPU Info: %d cores, %.0f MHz (max: %.0f MHz, %.1f%%)",
		info.CoreCount, info.CurrentFreqMHz, info.MaxFreqMHz, info.FreqRatio*100))

	// 检查频率是否过低
	if info.FreqRatio > 0 && info.FreqRatio < 0.90 {
		logging.Warn(fmt.Sprintf("⚠️  CPU 频率过低 (%.1f%% of max)", info.FreqRatio*100))
		logging.Warn("建议: sudo cpupower frequency-set -g performance")
		logging.Warn("或选择计算优化型实例")
	}

	// 检查调节策略
	if info.ScalingGovernor != "" && info.ScalingGovernor != "performance" {
		logging.Warn(fmt.Sprintf("⚠️  CPU 频率策略: %s (建议: performance)", info.ScalingGovernor))
		logging.Warn("设置方法: sudo cpupower frequency-set -g performance")
	}
}

// MemoryInfo 包含内存相关信息
type MemoryInfo struct {
	TotalMB     int64 // 总内存 (MB)
	AvailableMB int64 // 可用内存 (MB)
	UsedPercent float64
	SwapTotalMB int64 // Swap 总量 (MB)
	SwapUsedMB  int64 // Swap 使用 (MB)
}

// GetMemoryInfo 获取内存信息
func GetMemoryInfo() (*MemoryInfo, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("memory info only available on Linux")
	}

	info := &MemoryInfo{}

	// 读取 /proc/meminfo
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		key := strings.TrimSuffix(fields[0], ":")
		value, _ := strconv.ParseInt(fields[1], 10, 64)

		switch key {
		case "MemTotal":
			info.TotalMB = value / 1024
		case "MemAvailable":
			info.AvailableMB = value / 1024
		case "SwapTotal":
			info.SwapTotalMB = value / 1024
		case "SwapFree":
			swapFree := value / 1024
			info.SwapUsedMB = info.SwapTotalMB - swapFree
		}
	}

	if info.TotalMB > 0 {
		info.UsedPercent = float64(info.TotalMB-info.AvailableMB) / float64(info.TotalMB) * 100
	}

	return info, nil
}

// CheckMemoryConfig 检查内存配置并给出警告
func CheckMemoryConfig() {
	info, err := GetMemoryInfo()
	if err != nil {
		logging.Debug("Failed to get memory info", "error", err)
		return
	}

	logging.Info(fmt.Sprintf("Memory: Total=%d MB, Available=%d MB, Used=%.1f%%",
		info.TotalMB, info.AvailableMB, info.UsedPercent))

	// 检查 Swap 使用
	if info.SwapUsedMB > 0 {
		logging.Warn(fmt.Sprintf("⚠️  正在使用 Swap: %d MB", info.SwapUsedMB))
		logging.Warn("建议: sudo swapoff -a")
	}

	// 检查内存使用率
	if info.UsedPercent > 80 {
		logging.Warn(fmt.Sprintf("⚠️  内存使用率较高: %.1f%%", info.UsedPercent))
		logging.Warn("建议: 确保为 GridKV 预留足够内存")
	}

	// 推荐配置
	recommendedMaxMem := info.TotalMB * 70 / 100
	logging.Info(fmt.Sprintf("推荐 MaxMemoryMB 配置: %d MB (总内存的 70%%)", recommendedMaxMem))
}

// CheckSystemLimits 检查系统限制（文件描述符等）
func CheckSystemLimits() {
	if runtime.GOOS != "linux" {
		return
	}

	// 读取当前进程的文件描述符限制
	data, err := os.ReadFile("/proc/self/limits")
	if err != nil {
		logging.Debug("Failed to read process limits", "error", err)
		return
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if strings.Contains(line, "Max open files") {
			fields := strings.Fields(line)
			if len(fields) >= 4 {
				soft := fields[3]
				logging.Info(fmt.Sprintf("File descriptor limit: %s", soft))
				
				limit, _ := strconv.Atoi(soft)
				if limit < 65536 {
					logging.Warn(fmt.Sprintf("⚠️  文件描述符限制过低: %d", limit))
					logging.Warn("建议: ulimit -n 65536")
				}
			}
		}
	}
}

// CheckAllSystemConfig 执行所有系统配置检查
func CheckAllSystemConfig() {
	logging.Info("=== System Configuration Check ===")
	CheckCPUConfig()
	CheckMemoryConfig()
	CheckSystemLimits()
	logging.Info("=== Configuration Check Complete ===")
}

