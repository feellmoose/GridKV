package network

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// TestServerGoroutineLeak tests that server properly cleans up goroutines
func TestServerGoroutineLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping leak test in short mode")
	}

	// Get initial goroutine count
	runtime.GC()
	initialGoroutines := runtime.NumGoroutine()

	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)
	defer transport.Close()

	serverCfg := DefaultServerConfig(transport)
	server := NewServer(serverCfg)

	ctx := context.Background()
	addr := "127.0.0.1:0"

	handler := func(ctx context.Context, remoteAddr string, data []byte) ([]byte, error) {
		return data, nil
	}

	// Start and stop server multiple times
	for i := 0; i < 5; i++ {
		if err := server.Start(ctx, addr, handler); err != nil {
			t.Fatalf("Start() error = %v", err)
		}

		// Give server time to start goroutines
		time.Sleep(50 * time.Millisecond)

		if err := server.Stop(ctx); err != nil {
			t.Fatalf("Stop() error = %v", err)
		}

		// Give goroutines time to exit
		time.Sleep(100 * time.Millisecond)
		runtime.GC()
	}

	// Wait a bit more for all goroutines to exit
	time.Sleep(500 * time.Millisecond)
	runtime.GC()

	finalGoroutines := runtime.NumGoroutine()
	goroutineIncrease := finalGoroutines - initialGoroutines

	// Allow small increase (test framework may create goroutines)
	if goroutineIncrease > 5 {
		t.Errorf("Goroutine leak detected: initial=%d, final=%d, increase=%d",
			initialGoroutines, finalGoroutines, goroutineIncrease)
	}
}

// TestPoolGoroutineLeak tests that connection pool properly cleans up goroutines
func TestPoolGoroutineLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping leak test in short mode")
	}

	// Get initial goroutine count
	runtime.GC()
	initialGoroutines := runtime.NumGoroutine()

	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)
	defer transport.Close()

	// Create and close pool multiple times
	for i := 0; i < 5; i++ {
		poolCfg := DefaultPoolConfig(transport)
		poolCfg.CleanupInterval = 100 * time.Millisecond
		pool := NewConnPool(poolCfg)

		// Give cleanup goroutine time to start
		time.Sleep(50 * time.Millisecond)

		if err := pool.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}

		// Give goroutines time to exit
		time.Sleep(100 * time.Millisecond)
		runtime.GC()
	}

	// Wait a bit more for all goroutines to exit
	time.Sleep(500 * time.Millisecond)
	runtime.GC()

	finalGoroutines := runtime.NumGoroutine()
	goroutineIncrease := finalGoroutines - initialGoroutines

	// Allow small increase (test framework may create goroutines)
	if goroutineIncrease > 5 {
		t.Errorf("Goroutine leak detected: initial=%d, final=%d, increase=%d",
			initialGoroutines, finalGoroutines, goroutineIncrease)
	}
}

// TestConnectionLeak tests that connections are properly closed
func TestConnectionLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping leak test in short mode")
	}

	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)
	defer transport.Close()

	poolCfg := DefaultPoolConfig(transport)
	poolCfg.MaxIdle = 10
	poolCfg.MaxActive = 20
	pool := NewConnPool(poolCfg)
	defer pool.Close()

	ctx := context.Background()
	addr := "127.0.0.1:0"

	// Create listener for testing
	ln, err := transport.Listen(ctx, addr)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()

	serverAddr := ln.Address()

	// Start echo server
	go func() {
		for {
			conn, err := ln.Accept(ctx)
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
					_ = c.Send(ctx, data)
				}
			}(conn)
		}
	}()

	// Get and put connections multiple times
	for i := 0; i < 100; i++ {
		conn, err := pool.Get(ctx, serverAddr)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}

		stats := pool.Stats()
		if stats.Total > int64(poolCfg.MaxActive) {
			t.Errorf("Connection leak: Total=%d, MaxActive=%d",
				stats.Total, poolCfg.MaxActive)
		}

		pool.Put(conn)
	}

	// Wait for cleanup
	time.Sleep(2 * time.Second)

	finalStats := pool.Stats()
	// After cleanup, idle connections should be within MaxIdle
	if finalStats.Idle > int64(poolCfg.MaxIdle) {
		t.Errorf("Idle connection leak: Idle=%d, MaxIdle=%d",
			finalStats.Idle, poolCfg.MaxIdle)
	}
}
