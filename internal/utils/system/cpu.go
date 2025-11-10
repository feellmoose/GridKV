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

type CPUInfo struct {
	CoreCount       int
	CurrentFreqMHz  float64
	MaxFreqMHz      float64
	ScalingGovernor string
	FreqRatio       float64
}

func GetCPUInfo() (*CPUInfo, error) {
	info := &CPUInfo{
		CoreCount: runtime.NumCPU(),
	}

	if runtime.GOOS != "linux" {
		return info, nil
	}

	currentFreq, err := getCurrentCPUFreq()
	if err == nil {
		info.CurrentFreqMHz = currentFreq
	}

	maxFreq, err := getMaxCPUFreq()
	if err == nil {
		info.MaxFreqMHz = maxFreq
	}

	governor, err := getCPUGovernor()
	if err == nil {
		info.ScalingGovernor = governor
	}

	if info.MaxFreqMHz > 0 && info.CurrentFreqMHz > 0 {
		info.FreqRatio = info.CurrentFreqMHz / info.MaxFreqMHz
	}

	return info, nil
}

func getCurrentCPUFreq() (float64, error) {

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

func getMaxCPUFreq() (float64, error) {
	data, err := os.ReadFile("/sys/devices/system/cpu/cpu0/cpufreq/cpuinfo_max_freq")
	if err != nil {
		return 0, err
	}

	freqKHz, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
	if err != nil {
		return 0, err
	}

	return freqKHz / 1000, nil
}

func getCPUGovernor() (string, error) {
	data, err := os.ReadFile("/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor")
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(data)), nil
}

func CheckCPUConfig() {
	info, err := GetCPUInfo()
	if err != nil {
		logging.Debug("Failed to get CPU info", "error", err)
		return
	}

	logging.Info(fmt.Sprintf("CPU Info: %d cores, %.0f MHz (max: %.0f MHz, %.1f%%)",
		info.CoreCount, info.CurrentFreqMHz, info.MaxFreqMHz, info.FreqRatio*100))

	// Check if frequency is too low
	if info.FreqRatio > 0 && info.FreqRatio < 0.90 {
		logging.Warn(fmt.Sprintf("⚠️  CPU frequency too low (%.1f%% of max)", info.FreqRatio*100))
		logging.Warn("Suggestion: sudo cpupower frequency-set -g performance")
		logging.Warn("Or choose compute-optimized instances")
	}

	// Check scaling governor
	if info.ScalingGovernor != "" && info.ScalingGovernor != "performance" {
		logging.Warn(fmt.Sprintf("⚠️  CPU scaling governor: %s (recommended: performance)", info.ScalingGovernor))
		logging.Warn("How to set: sudo cpupower frequency-set -g performance")
	}
}

// MemoryInfo contains memory-related information
type MemoryInfo struct {
	TotalMB     int64 // Total memory (MB)
	AvailableMB int64 // Available memory (MB)
	UsedPercent float64
	SwapTotalMB int64 // Swap total (MB)
	SwapUsedMB  int64 // Swap used (MB)
}

// GetMemoryInfo retrieves memory information
func GetMemoryInfo() (*MemoryInfo, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("memory info only available on Linux")
	}

	info := &MemoryInfo{}

	// Read /proc/meminfo
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

// CheckMemoryConfig checks memory configuration and provides warnings
func CheckMemoryConfig() {
	info, err := GetMemoryInfo()
	if err != nil {
		logging.Debug("Failed to get memory info", "error", err)
		return
	}

	logging.Info(fmt.Sprintf("Memory: Total=%d MB, Available=%d MB, Used=%.1f%%",
		info.TotalMB, info.AvailableMB, info.UsedPercent))

	// Check swap usage
	if info.SwapUsedMB > 0 {
		logging.Warn(fmt.Sprintf("⚠️  Using swap: %d MB", info.SwapUsedMB))
		logging.Warn("Suggestion: sudo swapoff -a")
	}

	// Check memory usage
	if info.UsedPercent > 80 {
		logging.Warn(fmt.Sprintf("⚠️  High memory usage: %.1f%%", info.UsedPercent))
		logging.Warn("Suggestion: Ensure sufficient memory is reserved for GridKV")
	}

	// Recommended configuration
	recommendedMaxMem := info.TotalMB * 70 / 100
	logging.Info(fmt.Sprintf("Recommended MaxMemoryMB: %d MB (70%% of total memory)", recommendedMaxMem))
}

// CheckSystemLimits checks system limits (file descriptors, etc.)
func CheckSystemLimits() {
	if runtime.GOOS != "linux" {
		return
	}

	// Read file descriptor limit of current process
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
					logging.Warn(fmt.Sprintf("⚠️  File descriptor limit too low: %d", limit))
					logging.Warn("Suggestion: ulimit -n 65536")
				}
			}
		}
	}
}

// CheckAllSystemConfig performs all system configuration checks
func CheckAllSystemConfig() {
	logging.Info("=== System Configuration Check ===")
	CheckCPUConfig()
	CheckMemoryConfig()
	CheckSystemLimits()
	logging.Info("=== Configuration Check Complete ===")
}
