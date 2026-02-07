package network

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testTransportDeps holds common test dependencies for network tests
type testTransportDeps struct {
	transport  Transport
	pool       *connPool
	listener   Listener
	serverAddr string
}

// setupTransportDeps creates common test dependencies
func setupTransportDeps(t *testing.T, transportType TransportType) *testTransportDeps {
	t.Helper()

	cfg := DefaultTransportConfig()
	cfg.Type = transportType
	cfg.ReadTimeout = 1 * time.Second
	cfg.WriteTimeout = 1 * time.Second
	cfg.MaxMessageSize = 64 * 1024 // 64KB for high-throughput tests

	transport, err := NewTransport(cfg)
	if err != nil {
		t.Fatalf("NewTransport() error = %v", err)
	}

	poolCfg := DefaultPoolConfig(transport)
	poolCfg.MaxIdle = 50    // Higher for concurrent tests
	poolCfg.MaxActive = 200 // Higher for concurrent tests
	poolCfg.IdleTimeout = 30 * time.Second
	pool := NewConnPool(poolCfg)

	ctx := context.Background()
	addr := "127.0.0.1:0"

	ln, err := transport.Listen(ctx, addr)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}

	serverAddr := ln.Address()

	return &testTransportDeps{
		transport:  transport,
		pool:       pool.(*connPool),
		listener:   ln,
		serverAddr: serverAddr,
	}
}

// cleanupTransportDeps cleans up test dependencies
func cleanupTransportDeps(t *testing.T, deps *testTransportDeps) {
	t.Helper()
	deps.pool.Close()
	deps.listener.Close()
	deps.transport.Close()
}

func TestConnPool_GetPut(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}

	deps := setupTransportDeps(t, TransportTCP)
	defer cleanupTransportDeps(t, deps)
	pool := deps.pool
	ctx := context.Background()
	serverAddr := deps.serverAddr

	// Start echo server
	go func() {
		for {
			conn, err := deps.listener.Accept(ctx)
			if err != nil {
				return
			}
			go func(c Conn) {
				defer c.Close()
				for {
					data, err := c.Receive(ctx)
					if err != nil {
						return
					}
					_ = c.Send(ctx, data) // echo
				}
			}(conn)
		}
	}()

	// get connection
	conn1, err := pool.Get(ctx, serverAddr)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	stats := pool.Stats()
	if stats.Created != 1 {
		t.Errorf("Stats().Created = %d, want 1", stats.Created)
	}
	if stats.Active != 1 {
		t.Errorf("Stats().Active = %d, want 1", stats.Active)
	}

	// put back
	pool.Put(conn1)

	stats = pool.Stats()
	if stats.Idle != 1 {
		t.Errorf("Stats().Idle = %d, want 1", stats.Idle)
	}
	if stats.Active != 0 {
		t.Errorf("Stats().Active = %d, want 0", stats.Active)
	}

	// get again (should reuse)
	conn2, err := pool.Get(ctx, serverAddr)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	stats = pool.Stats()
	if stats.Created != 1 {
		t.Errorf("Stats().Created = %d, want 1 (reused)", stats.Created)
	}
	if stats.Active != 1 {
		t.Errorf("Stats().Active = %d, want 1", stats.Active)
	}
	if stats.Idle != 0 {
		t.Errorf("Stats().Idle = %d, want 0", stats.Idle)
	}

	pool.Put(conn2)
}

func TestConnPool_MaxIdle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)

	poolCfg := DefaultPoolConfig(transport)
	poolCfg.MaxIdle = 2
	poolCfg.MaxActive = 10

	pool := NewConnPool(poolCfg)
	defer pool.Close()

	ctx := context.Background()
	addr := "127.0.0.1:0"

	ln, err := transport.Listen(ctx, addr)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()

	serverAddr := ln.Address()

	// create more than MaxIdle
	conns := make([]Conn, 5)
	for i := range conns {
		conn, err := pool.Get(ctx, serverAddr)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		conns[i] = conn
	}

	// put all back
	for _, conn := range conns {
		pool.Put(conn)
	}

	stats := pool.Stats()
	if int(stats.Idle) > poolCfg.MaxIdle {
		t.Errorf("Stats().Idle = %d, want <= %d", stats.Idle, poolCfg.MaxIdle)
	}
	if stats.Closed < 3 {
		t.Errorf("Stats().Closed = %d, want >= 3", stats.Closed)
	}
}

func TestConnPool_MaxActive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)

	poolCfg := DefaultPoolConfig(transport)
	poolCfg.MaxIdle = 10
	poolCfg.MaxActive = 2

	pool := NewConnPool(poolCfg)
	defer pool.Close()

	ctx := context.Background()
	addr := "127.0.0.1:0"

	ln, err := transport.Listen(ctx, addr)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()

	serverAddr := ln.Address()

	// get up to MaxActive
	conn1, err := pool.Get(ctx, serverAddr)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	defer pool.Put(conn1)

	conn2, err := pool.Get(ctx, serverAddr)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	defer pool.Put(conn2)

	// should fail
	_, err = pool.Get(ctx, serverAddr)
	if err != ErrPoolExhausted {
		t.Errorf("Get() error = %v, want %v", err, ErrPoolExhausted)
	}
}

func TestConnPool_Remove(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)

	poolCfg := DefaultPoolConfig(transport)
	pool := NewConnPool(poolCfg)
	defer pool.Close()

	ctx := context.Background()
	addr := "127.0.0.1:0"

	ln, err := transport.Listen(ctx, addr)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()

	serverAddr := ln.Address()

	conn, err := pool.Get(ctx, serverAddr)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	pool.Remove(conn)

	stats := pool.Stats()
	if stats.Closed != 1 {
		t.Errorf("Stats().Closed = %d, want 1", stats.Closed)
	}
}

func TestConnPool_Close(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)

	poolCfg := DefaultPoolConfig(transport)
	pool := NewConnPool(poolCfg)

	ctx := context.Background()
	addr := "127.0.0.1:0"

	ln, err := transport.Listen(ctx, addr)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()

	serverAddr := ln.Address()

	conn, err := pool.Get(ctx, serverAddr)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	pool.Put(conn)

	if err := pool.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}

	stats := pool.Stats()
	if stats.Total != 0 || stats.Active != 0 || stats.Idle != 0 {
		t.Errorf("Stats() after Close() = %+v, want all zeros", stats)
	}
}

// TestConnectionPool_HighConcurrency tests connection pool under high concurrency
func TestConnectionPool_HighConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping high concurrency pool test in short mode")
	}

	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	cfg.ReadTimeout = 30 * time.Second
	cfg.WriteTimeout = 30 * time.Second

	transport := NewTCPTransport(cfg)
	defer transport.Close()

	// Setup connection pool for high concurrency
	poolCfg := DefaultPoolConfig(transport)
	poolCfg.MaxIdle = 128
	poolCfg.MaxActive = 512
	poolCfg.IdleTimeout = 30 * time.Second
	poolCfg.WaitTimeout = 2 * time.Second

	pool := NewConnPool(poolCfg)
	defer pool.Close()

	ctx := context.Background()

	// Start echo server
	ln, err := transport.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()

	serverAddr := ln.Address()

	go func() {
		for {
			conn, err := ln.Accept(ctx)
			if err != nil {
				return
			}

			go func(conn Conn) {
				defer conn.Close()
				for {
					data, err := conn.Receive(ctx)
					if err != nil {
						return
					}
					if err := conn.Send(ctx, data); err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	time.Sleep(100 * time.Millisecond)

	const (
		numGoroutines   = 200
		opsPerGoroutine = 500
	)

	var (
		totalGets    int64
		totalPuts    int64
		totalErrors  int64
		totalLatency int64
	)

	start := time.Now()

	var wg sync.WaitGroup
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for j := 0; j < opsPerGoroutine; j++ {
				opStart := time.Now()

				conn, err := pool.Get(ctx, serverAddr)
				if err != nil {
					atomic.AddInt64(&totalErrors, 1)
					continue
				}
				atomic.AddInt64(&totalGets, 1)

				// Simple ping-pong
				testData := []byte("ping")
				if err := conn.Send(ctx, testData); err != nil {
					atomic.AddInt64(&totalErrors, 1)
					pool.Remove(conn) // Remove broken connection
					continue
				}

				_, err = conn.Receive(ctx)
				if err != nil {
					atomic.AddInt64(&totalErrors, 1)
					pool.Remove(conn)
					continue
				}

				pool.Put(conn)
				atomic.AddInt64(&totalPuts, 1)

				latency := time.Since(opStart).Nanoseconds()
				atomic.AddInt64(&totalLatency, latency)
			}
		}()
	}

	wg.Wait()
	elapsed := time.Since(start)

	// Calculate metrics
	gets := atomic.LoadInt64(&totalGets)
	puts := atomic.LoadInt64(&totalPuts)
	errors := atomic.LoadInt64(&totalErrors)
	totalLatNs := atomic.LoadInt64(&totalLatency)

	opsPerSec := float64(gets) / elapsed.Seconds()
	avgLatencyMs := float64(totalLatNs) / float64(gets) * 1e-6
	errorRate := float64(errors) / float64(gets+errors) * 100

	t.Logf("High Concurrency Connection Pool Test:")
	t.Logf("  Gets: %d, Puts: %d, Errors: %d", gets, puts, errors)
	t.Logf("  Duration: %v", elapsed)
	t.Logf("  Throughput: %.0f ops/sec", opsPerSec)
	t.Logf("  Average Latency: %.2f ms", avgLatencyMs)
	t.Logf("  Error Rate: %.2f%%", errorRate)

	// Performance assertions
	if opsPerSec < 10000 { // Expect high throughput
		t.Errorf("Throughput too low: %.0f ops/sec (expected > 10000)", opsPerSec)
	}

	if avgLatencyMs > 20 { // Expect low latency
		t.Errorf("Latency too high: %.2f ms (expected < 20)", avgLatencyMs)
	}

	if errorRate > 1.0 { // Expect low error rate
		t.Errorf("Error rate too high: %.2f%% (expected < 1.0%%)", errorRate)
	}

	// Pool efficiency check
	stats := pool.Stats()
	t.Logf("Pool Stats: Total=%d, Active=%d, Idle=%d, Created=%d",
		stats.Total, stats.Active, stats.Idle, stats.Created)

	// Verify connection reuse efficiency
	reuseRate := float64(puts) / float64(stats.Created) * 100
	t.Logf("Connection Reuse Rate: %.1f%%", reuseRate)

	if reuseRate < 50 { // Expect good reuse
		t.Logf("Low connection reuse rate: %.1f%% (may indicate pool configuration issues)", reuseRate)
	}
}

// TestConnectionPool_MemoryEfficiency tests memory efficiency of connection pool
func TestConnectionPool_MemoryEfficiency(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping memory efficiency pool test in short mode")
	}

	// Force GC before measurement
	runtime.GC()
	runtime.GC()
	memBefore := runtime.MemStats{}
	runtime.ReadMemStats(&memBefore)

	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)
	defer transport.Close()

	// Minimal pool configuration for memory efficiency
	poolCfg := PoolConfig{
		MaxIdle:         4,
		MaxActive:       8,
		IdleTimeout:     5 * time.Second,
		MaxLifetime:     30 * time.Second,
		WaitTimeout:     1 * time.Second,
		CleanupInterval: 2 * time.Second,
		Transport:       transport,
	}

	pool := NewConnPool(poolCfg)
	defer pool.Close()

	ctx := context.Background()

	// Start echo server
	ln, err := transport.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()

	serverAddr := ln.Address()

	go func() {
		for {
			conn, err := ln.Accept(ctx)
			if err != nil {
				return
			}

			go func(conn Conn) {
				defer conn.Close()
				for {
					data, err := conn.Receive(ctx)
					if err != nil {
						return
					}
					if err := conn.Send(ctx, data); err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	time.Sleep(100 * time.Millisecond)

	// Simulate realistic usage pattern
	const numCycles = 10
	const opsPerCycle = 50

	for cycle := 0; cycle < numCycles; cycle++ {
		var wg sync.WaitGroup

		// Burst of operations
		for i := 0; i < opsPerCycle; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()

				conn, err := pool.Get(ctx, serverAddr)
				if err != nil {
					return
				}

				data := []byte("test")
				if err := conn.Send(ctx, data); err != nil {
					pool.Remove(conn)
					return
				}

				_, err = conn.Receive(ctx)
				if err != nil {
					pool.Remove(conn)
					return
				}

				pool.Put(conn)
			}()
		}

		wg.Wait()

		// Simulate idle period
		time.Sleep(500 * time.Millisecond)
	}

	// Force GC and measure memory
	runtime.GC()
	runtime.GC()
	memAfter := runtime.MemStats{}
	runtime.ReadMemStats(&memAfter)

	memIncreaseMB := float64(memAfter.Alloc-memBefore.Alloc) / 1024 / 1024

	t.Logf("Connection Pool Memory Efficiency:")
	t.Logf("  Memory Before: %.2f MB", float64(memBefore.Alloc)/1024/1024)
	t.Logf("  Memory After: %.2f MB", float64(memAfter.Alloc)/1024/1024)
	t.Logf("  Memory Increase: %.2f MB", memIncreaseMB)
	t.Logf("  GC Cycles: %d", memAfter.NumGC-memBefore.NumGC)

	// Assert memory efficiency
	if memIncreaseMB > 5.0 { // Expect < 5MB for connection pool
		t.Errorf("Memory increase too high: %.2f MB (expected < 5.0 MB)", memIncreaseMB)
	}

	stats := pool.Stats()
	t.Logf("Memory Efficiency Stats: Total=%d, Active=%d, Idle=%d, Created=%d",
		stats.Total, stats.Active, stats.Idle, stats.Created)

	// Verify cleanup effectiveness
	if stats.Idle > int64(poolCfg.MaxIdle) {
		t.Errorf("Too many idle connections: %d (max allowed: %d)", stats.Idle, poolCfg.MaxIdle)
	}
}

// TestConnectionPool_AdaptiveScaling tests pool behavior under varying loads
func TestConnectionPool_AdaptiveScaling(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping adaptive scaling pool test in short mode")
	}

	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)
	defer transport.Close()

	// Adaptive pool configuration
	poolCfg := DefaultPoolConfig(transport)
	poolCfg.MaxIdle = 16
	poolCfg.MaxActive = 64
	poolCfg.IdleTimeout = 10 * time.Second

	pool := NewConnPool(poolCfg)
	defer pool.Close()

	ctx := context.Background()

	// Start echo server
	ln, err := transport.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()

	serverAddr := ln.Address()

	go func() {
		for {
			conn, err := ln.Accept(ctx)
			if err != nil {
				return
			}

			go func(conn Conn) {
				defer conn.Close()
				for {
					data, err := conn.Receive(ctx)
					if err != nil {
						return
					}
					if err := conn.Send(ctx, data); err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	time.Sleep(100 * time.Millisecond)

	// Test load patterns
	loadPatterns := []struct {
		name         string
		numClients   int
		opsPerClient int
		description  string
	}{
		{"Low Load", 5, 20, "Simulate low-frequency operations"},
		{"Medium Load", 20, 50, "Simulate moderate cluster traffic"},
		{"High Load", 50, 100, "Simulate peak cluster activity"},
		{"Burst Load", 100, 10, "Simulate sudden traffic spikes"},
	}

	for _, pattern := range loadPatterns {
		t.Run(pattern.name, func(t *testing.T) {
			var wg sync.WaitGroup
			start := time.Now()

			// Launch clients
			for i := 0; i < pattern.numClients; i++ {
				wg.Add(1)
				go func(clientID int) {
					defer wg.Done()

					for j := 0; j < pattern.opsPerClient; j++ {
						conn, err := pool.Get(ctx, serverAddr)
						if err != nil {
							continue
						}

						data := []byte(fmt.Sprintf("client-%d-op-%d", clientID, j))
						if err := conn.Send(ctx, data); err != nil {
							pool.Remove(conn)
							continue
						}

						_, err = conn.Receive(ctx)
						if err != nil {
							pool.Remove(conn)
							continue
						}

						pool.Put(conn)
					}
				}(i)
			}

			wg.Wait()
			elapsed := time.Since(start)

			stats := pool.Stats()
			opsPerSec := float64(pattern.numClients*pattern.opsPerClient) / elapsed.Seconds()

			t.Logf("%s Results:", pattern.description)
			t.Logf("  Operations: %d", pattern.numClients*pattern.opsPerClient)
			t.Logf("  Duration: %v", elapsed)
			t.Logf("  Throughput: %.0f ops/sec", opsPerSec)
			t.Logf("  Pool Stats: Active=%d, Idle=%d, Created=%d",
				stats.Active, stats.Idle, stats.Created)

			// Basic performance checks
			if opsPerSec < 100 { // Minimum acceptable throughput
				t.Errorf("Throughput too low for %s: %.0f ops/sec", pattern.name, opsPerSec)
			}
		})

		// Allow pool to stabilize between tests
		time.Sleep(1 * time.Second)
	}
}

// BenchmarkConnectionPool_GetPut benchmarks connection pool get/put operations
func BenchmarkConnectionPool_GetPut(b *testing.B) {
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)
	defer transport.Close()

	poolCfg := DefaultPoolConfig(transport)
	poolCfg.MaxIdle = 64
	poolCfg.MaxActive = 256
	pool := NewConnPool(poolCfg)
	defer pool.Close()

	ctx := context.Background()

	// Setup server
	ln, err := transport.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		b.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()

	serverAddr := ln.Address()

	go func() {
		for {
			conn, err := ln.Accept(ctx)
			if err != nil {
				return
			}

			go func(conn Conn) {
				defer conn.Close()
				for {
					data, err := conn.Receive(ctx)
					if err != nil {
						return
					}
					if err := conn.Send(ctx, data); err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	time.Sleep(100 * time.Millisecond)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			conn, err := pool.Get(ctx, serverAddr)
			if err != nil {
				b.Errorf("Get() error = %v", err)
				continue
			}

			// Simple operation
			data := []byte("bench")
			if err := conn.Send(ctx, data); err != nil {
				b.Errorf("Send() error = %v", err)
				pool.Remove(conn)
				continue
			}

			_, err = conn.Receive(ctx)
			if err != nil {
				b.Errorf("Receive() error = %v", err)
				pool.Remove(conn)
				continue
			}

			pool.Put(conn)
		}
	})
}

// BenchmarkConnectionPool_Contention benchmarks pool under high contention
func BenchmarkConnectionPool_Contention(b *testing.B) {
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)
	defer transport.Close()

	// Small pool to create contention
	poolCfg := PoolConfig{
		MaxIdle:         4,
		MaxActive:       8,
		IdleTimeout:     30 * time.Second,
		MaxLifetime:     5 * time.Minute,
		WaitTimeout:     100 * time.Millisecond,
		CleanupInterval: 1 * time.Minute,
		Transport:       transport,
	}

	pool := NewConnPool(poolCfg)
	defer pool.Close()

	ctx := context.Background()

	// Setup server
	ln, err := transport.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		b.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()

	serverAddr := ln.Address()

	go func() {
		for {
			conn, err := ln.Accept(ctx)
			if err != nil {
				return
			}

			go func(conn Conn) {
				defer conn.Close()
				for {
					data, err := conn.Receive(ctx)
					if err != nil {
						return
					}
					if err := conn.Send(ctx, data); err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	time.Sleep(100 * time.Millisecond)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			conn, err := pool.Get(ctx, serverAddr)
			if err != nil {
				b.Errorf("Get() error = %v", err)
				continue
			}

			data := []byte("contention-test")
			if err := conn.Send(ctx, data); err != nil {
				pool.Remove(conn)
				continue
			}

			_, err = conn.Receive(ctx)
			if err != nil {
				pool.Remove(conn)
				continue
			}

			pool.Put(conn)
		}
	})
}
