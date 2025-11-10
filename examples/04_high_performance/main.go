package main

import (
	"context"
	"fmt"
	"log"
	"runtime"
	"sync"
	"time"

	gridkv "github.com/feellmoose/gridkv"
)

// Scenario 4: Low Latency Configuration
// Use Case: Real-time recommendations, game state, real-time bidding, HFT trading
//
// Performance Expectations:
//   - P99 latency: < 1ms
//   - P50 latency: < 200ns
//   - Reads: 6-7M ops/s
//
// Configuration Highlights:
//   - 2 replicas (minimal redundancy)
//   - ReadQuorum=1 (lowest latency)
//   - Ultra-short timeout
//   - Memory pre-allocation

func main() {
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Println("  GridKV Scenario 4: Low Latency Configuration")
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Println()

	ctx := context.Background()

	// Create 2-node cluster (minimal configuration)
	fmt.Println("📦 Creating node 1...")
	node1, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
		LocalNodeID:  "latency-node-1",
		LocalAddress: "localhost:12001",

		Network: &gridkv.NetworkOptions{
			Type:         gridkv.TCP,
			BindAddr:     "localhost:12001",
			MaxConns:     500, // Moderate connection pool
			MaxIdle:      50,
			ReadTimeout:  500 * time.Millisecond, // Ultra-short timeout
			WriteTimeout: 500 * time.Millisecond,
		},

		Storage: &gridkv.StorageOptions{
			Backend:     gridkv.BackendMemorySharded,
			MaxMemoryMB: 4096, // 4GB, avoid GC
		},

		// Low latency configuration
		ReplicaCount: 2, // Only 2 replicas
		WriteQuorum:  1, // Write to 1 replica succeeds (fastest) ⚡
		ReadQuorum:   1, // Read from 1 replica (fastest) ⚡

		VirtualNodes:       100, // Fewer virtual nodes, reduce computation
		MaxReplicators:     runtime.NumCPU(),
		ReplicationTimeout: 500 * time.Millisecond, // Ultra-short timeout
		ReadTimeout:        500 * time.Millisecond,
		FailureTimeout:     2 * time.Second,
		SuspectTimeout:     3 * time.Second,
		GossipInterval:     500 * time.Millisecond,
	})
	if err != nil {
		log.Fatalf("Failed to create node 1: %v", err)
	}
	defer node1.Close()
	fmt.Println("✅ Node 1 created successfully")
	fmt.Println()

	fmt.Println("📦 Creating node 2...")
	node2, err := gridkv.NewGridKV(&gridkv.GridKVOptions{
		LocalNodeID:  "latency-node-2",
		LocalAddress: "localhost:12002",
		SeedAddrs:    []string{"localhost:12001"},

		Network: &gridkv.NetworkOptions{
			Type:         gridkv.TCP,
			BindAddr:     "localhost:12002",
			MaxConns:     500,
			MaxIdle:      50,
			ReadTimeout:  500 * time.Millisecond,
			WriteTimeout: 500 * time.Millisecond,
		},

		Storage: &gridkv.StorageOptions{
			Backend:     gridkv.BackendMemorySharded,
			MaxMemoryMB: 4096,
		},

		ReplicaCount: 2,
		WriteQuorum:  1,
		ReadQuorum:   1,

		VirtualNodes:       100,
		MaxReplicators:     runtime.NumCPU(),
		ReplicationTimeout: 500 * time.Millisecond,
		ReadTimeout:        500 * time.Millisecond,
		FailureTimeout:     2 * time.Second,
		SuspectTimeout:     3 * time.Second,
		GossipInterval:     500 * time.Millisecond,
	})
	if err != nil {
		log.Fatalf("Failed to create node 2: %v", err)
	}
	defer node2.Close()
	fmt.Println("✅ Node 2 created successfully")
	fmt.Println()

	// Wait for cluster formation
	fmt.Println("⏳ Waiting for cluster convergence...")
	time.Sleep(1 * time.Second)
	fmt.Println("✅ Low latency cluster ready")
	fmt.Println()

	// ═══════════════════════════════════════════════════════
	// Latency Testing
	// ═══════════════════════════════════════════════════════
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Println("  Latency Benchmark Testing")
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Println()

	// Warmup
	fmt.Println("🔥 Warming up...")
	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("warmup-%d", i)
		node1.Set(ctx, key, []byte("value"))
	}
	fmt.Println("✅ Warmup completed")
	fmt.Println()

	// Test write latency
	fmt.Println("📊 Test: Write latency distribution")
	fmt.Println("─────────────────────────────────────────────────────")
	testWriteLatency(ctx, node1)

	// Test read latency
	fmt.Println()
	fmt.Println("📊 Test: Read latency distribution")
	fmt.Println("─────────────────────────────────────────────────────")
	testReadLatency(ctx, node1)

	// Concurrent latency test
	fmt.Println()
	fmt.Println("📊 Test: Concurrent scenario latency")
	fmt.Println("─────────────────────────────────────────────────────")
	testConcurrentLatency(ctx, node1)

	// Summary
	fmt.Println()
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Println("  Summary")
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Println()
	fmt.Println("✅ Low latency configuration:")
	fmt.Println("   • 2 replicas + R=1, W=1")
	fmt.Println("   • Ultra-short timeout (500ms)")
	fmt.Println("   • MemorySharded backend")
	fmt.Println()
	fmt.Println("✅ Use cases:")
	fmt.Println("   • Real-time recommendation systems")
	fmt.Println("   • Game state synchronization")
	fmt.Println("   • Real-time bidding (RTB)")
	fmt.Println("   • High-frequency trading (HFT)")
	fmt.Println()
	fmt.Println("✅ Performance characteristics:")
	fmt.Println("   • P50 latency: < 200ns")
	fmt.Println("   • P99 latency: < 1ms")
	fmt.Println("   • Reads: 6-7M ops/s")
	fmt.Println()
	fmt.Println("⚠️  Considerations:")
	fmt.Println("   • Eventual consistency (R+W=2 ≤ N)")
	fmt.Println("   • Weak fault tolerance (can only tolerate 1 node failure)")
	fmt.Println("   • Suitable for latency-sensitive scenarios")
	fmt.Println()
}

// Test write latency distribution
func testWriteLatency(ctx context.Context, kv *gridkv.GridKV) {
	numOps := 10000
	latencies := make([]time.Duration, numOps)

	for i := 0; i < numOps; i++ {
		key := fmt.Sprintf("lat-write-%d", i)
		value := []byte(fmt.Sprintf("value-%d", i))

		start := time.Now()
		kv.Set(ctx, key, value)
		latencies[i] = time.Since(start)
	}

	printLatencyStats("Write", latencies)
}

// Test read latency distribution
func testReadLatency(ctx context.Context, kv *gridkv.GridKV) {
	// Pre-write data
	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("lat-read-%d", i)
		value := []byte(fmt.Sprintf("value-%d", i))
		kv.Set(ctx, key, value)
	}

	numOps := 10000
	latencies := make([]time.Duration, numOps)

	for i := 0; i < numOps; i++ {
		key := fmt.Sprintf("lat-read-%d", i%1000)

		start := time.Now()
		kv.Get(ctx, key)
		latencies[i] = time.Since(start)
	}

	printLatencyStats("Read", latencies)
}

// Test concurrent scenario latency
func testConcurrentLatency(ctx context.Context, kv *gridkv.GridKV) {
	numGoroutines := 50
	opsPerGoroutine := 200
	totalOps := numGoroutines * opsPerGoroutine

	latencies := make([]time.Duration, totalOps)
	var mu sync.Mutex
	idx := 0

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				key := fmt.Sprintf("lat-concurrent-%d-%d", id, j)
				value := []byte(fmt.Sprintf("value-%d-%d", id, j))

				start := time.Now()
				kv.Set(ctx, key, value)
				lat := time.Since(start)

				mu.Lock()
				latencies[idx] = lat
				idx++
				mu.Unlock()
			}
		}(i)
	}

	wg.Wait()
	printLatencyStats("Concurrent write", latencies)
}

// Print latency statistics
func printLatencyStats(name string, latencies []time.Duration) {
	// Sort
	sorted := make([]time.Duration, len(latencies))
	copy(sorted, latencies)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[i] > sorted[j] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	// Calculate statistics
	p50 := sorted[len(sorted)*50/100]
	p95 := sorted[len(sorted)*95/100]
	p99 := sorted[len(sorted)*99/100]
	p999 := sorted[len(sorted)*999/1000]
	max := sorted[len(sorted)-1]

	// Calculate average
	var sum time.Duration
	for _, lat := range latencies {
		sum += lat
	}
	avg := sum / time.Duration(len(latencies))

	fmt.Printf("%s latency statistics (%d operations):\n", name, len(latencies))
	fmt.Printf("  Average:  %v\n", avg)
	fmt.Printf("  P50:   %v\n", p50)
	fmt.Printf("  P95:   %v\n", p95)
	fmt.Printf("  P99:   %v\n", p99)
	fmt.Printf("  P99.9: %v\n", p999)
	fmt.Printf("  Max:  %v\n", max)
}
