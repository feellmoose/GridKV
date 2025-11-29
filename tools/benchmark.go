package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	gridkv "github.com/feellmoose/gridkv"
	"github.com/feellmoose/gridkv/internal/storage"
	"github.com/feellmoose/gridkv/internal/utils/network"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

type BenchmarkConfig struct {
	TestName      string
	NodeCount     int
	ReplicaCount  int
	BasePort      int
	NetworkType   string
	Backend       string
	Duration      time.Duration
	ConcurrentOps int
}

type BenchmarkResult struct {
	TestName          string                 `json:"test_name"`
	Timestamp         string                 `json:"timestamp"`
	Config            BenchmarkConfig        `json:"config"`
	WriteQPS          float64                `json:"write_qps"`
	ReadQPS           float64                `json:"read_qps"`
	DeleteQPS         float64                `json:"delete_qps"`
	TotalQPS          float64                `json:"total_qps"`
	WriteSuccessRate  float64                `json:"write_success_rate"`
	ReadSuccessRate   float64                `json:"read_success_rate"`
	DeleteSuccessRate float64                `json:"delete_success_rate"`
	P50LatencyMs      float64                `json:"p50_latency_ms"`
	P95LatencyMs      float64                `json:"p95_latency_ms"`
	P99LatencyMs      float64                `json:"p99_latency_ms"`
	ReadLatencyP50Ms  float64                `json:"read_latency_p50_ms"`
	ReadLatencyP95Ms  float64                `json:"read_latency_p95_ms"`
	ReadLatencyP99Ms  float64                `json:"read_latency_p99_ms"`
	PeakGoroutines    int                    `json:"peak_goroutines"`
	FinalGoroutines   int                    `json:"final_goroutines"`
	PeakMemoryMB      float64                `json:"peak_memory_mb"`
	FinalMemoryMB     float64                `json:"final_memory_mb"`
	Metrics           map[string]interface{} `json:"metrics,omitempty"`
}

type stats struct {
	writeSubmitted  atomic.Int64
	writeCompleted  atomic.Int64
	readSubmitted   atomic.Int64
	readCompleted   atomic.Int64
	deleteSubmitted atomic.Int64
	deleteCompleted atomic.Int64

	writeLatencies []time.Duration
	readLatencies  []time.Duration
	writeLatMu     sync.Mutex
	readLatMu      sync.Mutex

	duration time.Duration
}

func (s *stats) writeQPS() float64 {
	if s.duration == 0 {
		return 0
	}
	return float64(s.writeCompleted.Load()) / s.duration.Seconds()
}

func (s *stats) readQPS() float64 {
	if s.duration == 0 {
		return 0
	}
	return float64(s.readCompleted.Load()) / s.duration.Seconds()
}

func (s *stats) deleteQPS() float64 {
	if s.duration == 0 {
		return 0
	}
	return float64(s.deleteCompleted.Load()) / s.duration.Seconds()
}

func (s *stats) totalQPS() float64 {
	return s.writeQPS() + s.readQPS() + s.deleteQPS()
}

func (s *stats) writeSuccessRate() float64 {
	submitted := s.writeSubmitted.Load()
	if submitted == 0 {
		return 0
	}
	return float64(s.writeCompleted.Load()) / float64(submitted) * 100
}

func (s *stats) readSuccessRate() float64 {
	submitted := s.readSubmitted.Load()
	if submitted == 0 {
		return 0
	}
	return float64(s.readCompleted.Load()) / float64(submitted) * 100
}

func (s *stats) deleteSuccessRate() float64 {
	submitted := s.deleteSubmitted.Load()
	if submitted == 0 {
		return 0
	}
	return float64(s.deleteCompleted.Load()) / float64(submitted) * 100
}

func calculatePercentiles(latencies []time.Duration) (p50, p95, p99 time.Duration) {
	n := len(latencies)
	if n == 0 {
		return 0, 0, 0
	}

	sort.Slice(latencies, func(i, j int) bool {
		return latencies[i] < latencies[j]
	})

	p50 = latencies[n*50/100]
	p95 = latencies[n*95/100]
	p99 = latencies[n*99/100]
	return
}

func parseNetworkType(s string) gridkv.NetworkType {
	switch s {
	case "QUIC":
		return gridkv.QUIC
	case "UDP":
		return gridkv.UDP
	default:
		return gridkv.TCP
	}
}

func parseBackend(s string) gridkv.StorageBackendType {
	switch s {
	case "Memory":
		return gridkv.BackendMemory
	case "MemorySharded":
		return gridkv.BackendMemorySharded
	default:
		return gridkv.BackendMemorySharded
	}
}

func runBenchmark(config BenchmarkConfig) (*BenchmarkResult, error) {
	ctx := context.Background()

	// Create cluster
	latencyConfig := network.GetConfigForProfile(network.ProfileLAN, config.NodeCount)
	nodes := make([]*gridkv.GridKV, config.NodeCount)
	seedAddr := []string{}

	for i := 0; i < config.NodeCount; i++ {
		port := config.BasePort + i
		addr := fmt.Sprintf("localhost:%d", port)

		// First node is seed, others connect to it
		if i == 0 {
			seedAddr = []string{addr}
		}

		opts := &gridkv.GridKVOptions{
			LocalNodeID:        fmt.Sprintf("node-%d", i),
			LocalAddress:       addr,
			SeedAddrs:          seedAddr,
			FailureTimeout:     latencyConfig.FailureTimeout,
			SuspectTimeout:     latencyConfig.SuspectTimeout,
			GossipInterval:     latencyConfig.GossipInterval,
			ReplicationTimeout: latencyConfig.ReplicationTimeout,
			ReadTimeout:        latencyConfig.ReadTimeout,
			StartupGracePeriod: 1 * time.Second,
			DisableAuth:        true,
			ReplicaCount:       config.ReplicaCount,
			Network: &gridkv.NetworkOptions{
				Type:     parseNetworkType(config.NetworkType),
				BindAddr: addr,
				MaxConns: latencyConfig.MaxConnections,
				MaxIdle:  latencyConfig.MaxIdleConnections,
			},
			Storage: &gridkv.StorageOptions{
				Backend:     parseBackend(config.Backend),
				MaxMemoryMB: 1024,
				ShardCount:  128,
			},
		}

		node, err := gridkv.NewGridKV(opts)
		if err != nil {
			return nil, fmt.Errorf("failed to create node %d: %w", i, err)
		}
		nodes[i] = node
		defer node.Close()

		if i == 0 {
			time.Sleep(1 * time.Second)
		}
	}

	// Wait for cluster to stabilize
	time.Sleep(3 * time.Second)

	// Monitor goroutines and memory
	var peakGoroutines atomic.Int64
	var peakMemory atomic.Uint64
	_ = runtime.NumGoroutine() // baseline not used in result
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	_ = memStats.HeapInuse // baseline not used in result

	monitorStop := make(chan struct{})
	var monitorWg sync.WaitGroup
	monitorWg.Add(1)
	go func() {
		defer monitorWg.Done()
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-monitorStop:
				return
			case <-ticker.C:
				count := runtime.NumGoroutine()
				if int64(count) > peakGoroutines.Load() {
					peakGoroutines.Store(int64(count))
				}
				runtime.ReadMemStats(&memStats)
				if memStats.HeapInuse > peakMemory.Load() {
					peakMemory.Store(memStats.HeapInuse)
				}
			}
		}
	}()

	// Pre-populate data
	//
	// NOTE: 为避免每次读/删都 O(N) 扫描 map，这里使用一个简单的 key 列表，
	// 只在写成功时 append，一次随机读取即可，让基准的开销尽量接近 GridKV 本身。
	type keyList struct {
		mu   sync.RWMutex
		keys []string
	}
	addKey := func(kl *keyList, key string) {
		if key == "" {
			return
		}
		kl.mu.Lock()
		kl.keys = append(kl.keys, key)
		kl.mu.Unlock()
	}
	randomKey := func(kl *keyList) (string, bool) {
		kl.mu.RLock()
		defer kl.mu.RUnlock()
		if len(kl.keys) == 0 {
			return "", false
		}
		idx := rand.Intn(len(kl.keys))
		return kl.keys[idx], true
	}
	activeKeys := &keyList{}

	seedCount := 500
	for i := 0; i < seedCount; i++ {
		key := fmt.Sprintf("seed-%d", i)
		value := make([]byte, 128)
		_, _ = rand.Read(value)
		nodeIdx := i % len(nodes)
		if err := nodes[nodeIdx].Set(ctx, key, value); err == nil {
			addKey(activeKeys, key)
		}
	}
	time.Sleep(2 * time.Second)

	// Run workload
	stats := &stats{duration: config.Duration}
	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	// Workload mix: 混合读写删负载，便于观察整体 QPS 和各操作成功率。
	// 默认 roughly: 写 1/3，读 1/2，删 1/6，可通过并发总量整体放大。
	writers := max(1, config.ConcurrentOps/3)
	readers := max(1, config.ConcurrentOps/2)
	deleters := max(1, config.ConcurrentOps/6)

	// Writers
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			nodeIdx := workerID % len(nodes)
			for {
				select {
				case <-stopCh:
					return
				default:
					key := fmt.Sprintf("w-%d-%d", workerID, stats.writeSubmitted.Load())
					value := []byte(fmt.Sprintf("value-%d", time.Now().UnixNano()))
					stats.writeSubmitted.Add(1)

					start := time.Now()
					if err := nodes[nodeIdx].Set(ctx, key, value); err == nil {
						stats.writeCompleted.Add(1)
						latency := time.Since(start)
						stats.writeLatMu.Lock()
						stats.writeLatencies = append(stats.writeLatencies, latency)
						stats.writeLatMu.Unlock()

						addKey(activeKeys, key)
					}
				}
			}
		}(w)
	}

	time.Sleep(2 * time.Second)

	// Readers
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			nodeIdx := workerID % len(nodes)
			for {
				select {
				case <-stopCh:
					return
				default:
					key, ok := randomKey(activeKeys)
					if !ok {
						time.Sleep(2 * time.Millisecond)
						continue
					}
					stats.readSubmitted.Add(1)

					start := time.Now()
					if _, err := nodes[nodeIdx].Get(ctx, key); err == nil || errors.Is(err, storage.ErrItemNotFound) {
						latency := time.Since(start)
						stats.readCompleted.Add(1)
						stats.readLatMu.Lock()
						stats.readLatencies = append(stats.readLatencies, latency)
						stats.readLatMu.Unlock()
					}
				}
			}
		}(r)
	}

	// Deleters
	for d := 0; d < deleters; d++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			nodeIdx := workerID % len(nodes)
			for {
				select {
				case <-stopCh:
					return
				default:
					key, ok := randomKey(activeKeys)
					if !ok {
						time.Sleep(2 * time.Millisecond)
						continue
					}
					stats.deleteSubmitted.Add(1)
					if err := nodes[nodeIdx].Delete(ctx, key); err == nil {
						stats.deleteCompleted.Add(1)
					}
				}
			}
		}(d)
	}

	time.Sleep(config.Duration)
	close(stopCh)
	wg.Wait()

	close(monitorStop)
	monitorWg.Wait()

	// Cleanup wait
	time.Sleep(2 * time.Second)
	runtime.GC()
	time.Sleep(1 * time.Second)

	finalGoroutines := runtime.NumGoroutine()
	runtime.ReadMemStats(&memStats)
	finalMemory := memStats.HeapInuse

	// Calculate latencies
	stats.writeLatMu.Lock()
	writeLatencies := make([]time.Duration, len(stats.writeLatencies))
	copy(writeLatencies, stats.writeLatencies)
	stats.writeLatMu.Unlock()

	stats.readLatMu.Lock()
	readLatencies := make([]time.Duration, len(stats.readLatencies))
	copy(readLatencies, stats.readLatencies)
	stats.readLatMu.Unlock()

	readP50, readP95, readP99 := calculatePercentiles(readLatencies)

	// Combine latencies for overall P50/P95/P99
	allLatencies := append(writeLatencies, readLatencies...)
	overallP50, overallP95, overallP99 := calculatePercentiles(allLatencies)

	result := &BenchmarkResult{
		TestName:          config.TestName,
		Timestamp:         time.Now().Format(time.RFC3339),
		Config:            config,
		WriteQPS:          stats.writeQPS(),
		ReadQPS:           stats.readQPS(),
		DeleteQPS:         stats.deleteQPS(),
		TotalQPS:          stats.totalQPS(),
		WriteSuccessRate:  stats.writeSuccessRate(),
		ReadSuccessRate:   stats.readSuccessRate(),
		DeleteSuccessRate: stats.deleteSuccessRate(),
		P50LatencyMs:      overallP50.Seconds() * 1000,
		P95LatencyMs:      overallP95.Seconds() * 1000,
		P99LatencyMs:      overallP99.Seconds() * 1000,
		ReadLatencyP50Ms:  readP50.Seconds() * 1000,
		ReadLatencyP95Ms:  readP95.Seconds() * 1000,
		ReadLatencyP99Ms:  readP99.Seconds() * 1000,
		PeakGoroutines:    int(peakGoroutines.Load()),
		FinalGoroutines:   finalGoroutines,
		PeakMemoryMB:      float64(peakMemory.Load()) / 1024 / 1024,
		FinalMemoryMB:     float64(finalMemory) / 1024 / 1024,
	}

	return result, nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s <test_name> [json_output_file]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Environment variables:\n")
		fmt.Fprintf(os.Stderr, "  GRIDKV_BENCH_NODES=5 (default: 5)\n")
		fmt.Fprintf(os.Stderr, "  GRIDKV_BENCH_DURATION=30s (default: 30s)\n")
		fmt.Fprintf(os.Stderr, "  GRIDKV_BENCH_CONCURRENT=100 (default: 100)\n")
		fmt.Fprintf(os.Stderr, "  GRIDKV_BENCH_NETWORK=TCP (default: TCP, options: TCP/QUIC/UDP)\n")
		fmt.Fprintf(os.Stderr, "  GRIDKV_BENCH_BACKEND=MemorySharded (default: MemorySharded, options: Memory/MemorySharded)\n")
		os.Exit(1)
	}

	testName := os.Args[1]

	config := BenchmarkConfig{
		TestName:      testName,
		NodeCount:     5,
		ReplicaCount:  3,
		BasePort:      40000,
		NetworkType:   "TCP",
		Backend:       "MemorySharded",
		Duration:      30 * time.Second,
		ConcurrentOps: 100,
	}

	if nodes := os.Getenv("GRIDKV_BENCH_NODES"); nodes != "" {
		fmt.Sscanf(nodes, "%d", &config.NodeCount)
	}
	if dur := os.Getenv("GRIDKV_BENCH_DURATION"); dur != "" {
		if d, err := time.ParseDuration(dur); err == nil {
			config.Duration = d
		}
	}
	if concurrent := os.Getenv("GRIDKV_BENCH_CONCURRENT"); concurrent != "" {
		fmt.Sscanf(concurrent, "%d", &config.ConcurrentOps)
	}
	if network := os.Getenv("GRIDKV_BENCH_NETWORK"); network != "" {
		config.NetworkType = network
	}
	if backend := os.Getenv("GRIDKV_BENCH_BACKEND"); backend != "" {
		config.Backend = backend
	}

	fmt.Fprintf(os.Stderr, "Running benchmark: %s\n", testName)
	fmt.Fprintf(os.Stderr, "Config: %d nodes, %s backend, %s network, %d concurrent ops, %v duration\n",
		config.NodeCount, config.Backend, config.NetworkType, config.ConcurrentOps, config.Duration)

	result, err := runBenchmark(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Benchmark failed: %v\n", err)
		os.Exit(1)
	}

	// Print summary
	fmt.Fprintf(os.Stderr, "\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	fmt.Fprintf(os.Stderr, "   Benchmark Results: %s\n", testName)
	fmt.Fprintf(os.Stderr, "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	fmt.Fprintf(os.Stderr, "Total QPS:     %.2f (Write: %.2f, Read: %.2f, Delete: %.2f)\n",
		result.TotalQPS, result.WriteQPS, result.ReadQPS, result.DeleteQPS)
	fmt.Fprintf(os.Stderr, "Success Rate:  Write: %.1f%%, Read: %.1f%%, Delete: %.1f%%\n",
		result.WriteSuccessRate, result.ReadSuccessRate, result.DeleteSuccessRate)
	fmt.Fprintf(os.Stderr, "Latency (ms):  P50: %.2f, P95: %.2f, P99: %.2f\n",
		result.P50LatencyMs, result.P95LatencyMs, result.P99LatencyMs)
	fmt.Fprintf(os.Stderr, "Read Latency:  P50: %.2f, P95: %.2f, P99: %.2f\n",
		result.ReadLatencyP50Ms, result.ReadLatencyP95Ms, result.ReadLatencyP99Ms)
	fmt.Fprintf(os.Stderr, "Resources:     Peak Goroutines: %d, Final: %d, Peak Memory: %.1f MB\n",
		result.PeakGoroutines, result.FinalGoroutines, result.PeakMemoryMB)
	fmt.Fprintf(os.Stderr, "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

	// Output JSON
	jsonData, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to marshal JSON: %v\n", err)
		os.Exit(1)
	}

	if len(os.Args) >= 3 {
		if err := os.WriteFile(os.Args[2], jsonData, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to write JSON file: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Results saved to: %s\n", os.Args[2])
	} else {
		fmt.Println(string(jsonData))
	}
}
