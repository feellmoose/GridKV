package tests

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feellmoose/gridkv/internal/transport"
)

// TestConnectionStability_Gnet verifies gnet connection stability improvements
func TestConnectionStability_Gnet(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping connection stability test in short mode")
	}

	trans, err := transport.NewGnetTransport()
	if err != nil {
		t.Skipf("gnet not available: %v", err)
		return
	}

	testConnectionStability(t, trans, "gnet")
}

// TestConnectionStability_TCP verifies TCP connection stability improvements
func TestConnectionStability_TCP(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping connection stability test in short mode")
	}

	trans := transport.NewTCPTransport()
	testConnectionStability(t, trans, "tcp")
}

func testConnectionStability(t *testing.T, trans transport.Transport, name string) {
	addr := fmt.Sprintf("localhost:%d", 28000+time.Now().UnixNano()%1000)

	// Start listener
	listener, err := trans.Listen(addr)
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}

	received := atomic.Int64{}
	listener.HandleMessage(func(msg []byte) error {
		received.Add(1)
		return nil
	})

	if err := listener.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer listener.Stop()

	time.Sleep(200 * time.Millisecond)

	// Test: Many connections with keep-alive improvements
	// With increased keep-alive (60s for gnet, longer for TCP), connections should stay alive
	concurrency := 10
	duration := 10 * time.Second // Reduced for faster testing
	message := []byte("test-message")

	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	sent := atomic.Int64{}
	errors := atomic.Int64{}
	reconnects := atomic.Int64{}

	start := time.Now()

	// Workers that maintain connections
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			var conn transport.TransportConn
			var lastErr error

			for {
				select {
				case <-stopCh:
					if conn != nil {
						conn.Close()
					}
					return
				default:
					// Dial if no connection
					if conn == nil {
						var err error
						conn, err = trans.Dial(addr)
						if err != nil {
							errors.Add(1)
							lastErr = err
							time.Sleep(100 * time.Millisecond)
							continue
						}
						if lastErr != nil {
							reconnects.Add(1)
							lastErr = nil
						}
					}

					// Health check (should not fail with improved implementation)
					if healthChecker, ok := conn.(transport.HealthCheckable); ok {
						if err := healthChecker.HealthCheck(); err != nil {
							// Connection unhealthy, close and reconnect
							conn.Close()
							conn = nil
							reconnects.Add(1)
							continue
						}
					}

					// Send message
					ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
					err := conn.WriteDataWithContext(ctx, message)
					cancel()

					if err != nil {
						errors.Add(1)
						conn.Close()
						conn = nil
						time.Sleep(100 * time.Millisecond)
					} else {
						sent.Add(1)
					}

					time.Sleep(100 * time.Millisecond)
				}
			}
		}(i)
	}

	// Run for specified duration
	time.Sleep(duration)
	close(stopCh)
	wg.Wait()

	elapsed := time.Since(start)
	sentCount := sent.Load()
	errorCount := errors.Load()
	reconnectCount := reconnects.Load()
	receivedCount := received.Load()

	t.Logf("%s connection stability test:", name)
	t.Logf("  Duration: %v", elapsed)
	t.Logf("  Messages sent: %d", sentCount)
	t.Logf("  Messages received: %d", receivedCount)
	t.Logf("  Errors: %d", errorCount)
	t.Logf("  Reconnects: %d", reconnectCount)

	// With improved keep-alive and health checks, we should have:
	// - Low error rate (< 5%)
	// - Few reconnects (connections should stay alive)
	errorRate := float64(errorCount) / float64(sentCount) * 100
	if sentCount > 0 && errorRate > 5.0 {
		t.Errorf("Error rate too high: %.2f%% (expected < 5%%)", errorRate)
	}

	// Reconnect rate should be low (connections should stay alive longer)
	reconnectRate := float64(reconnectCount) / float64(sentCount) * 100
	if sentCount > 0 && reconnectRate > 10.0 {
		t.Errorf("Reconnect rate too high: %.2f%% (expected < 10%%)", reconnectRate)
	}
}

// TestIdleTimeout verifies increased idle timeout keeps connections alive
func TestIdleTimeout(t *testing.T) {
	ctx := context.Background()

	// Create cluster with default settings (30s idle timeout)
	nodes := setupTestCluster(t, 2, 29000)
	defer cleanupTestCluster(nodes)

	time.Sleep(1 * time.Second)

	// Send initial operation
	err := nodes[0].Set(ctx, "idle-test", []byte("value"))
	if err != nil {
		t.Fatalf("Initial Set failed: %v", err)
	}

	// Wait 15 seconds (should be less than 30s timeout)
	// Connection should still be alive
	time.Sleep(15 * time.Second)

	// Send another operation - should use existing connection
	err = nodes[0].Set(ctx, "idle-test-2", []byte("value-2"))
	if err != nil {
		t.Errorf("Set after idle period failed: %v (connection may have been closed)", err)
	}

	// Verify operation completed
	value, err := nodes[0].Get(ctx, "idle-test-2")
	if err != nil {
		t.Errorf("Get failed: %v", err)
	} else if string(value) != "value-2" {
		t.Errorf("Expected 'value-2', got '%s'", string(value))
	}

	t.Log("Idle timeout test passed: connection remained alive")
}

// TestHealthCheck verifies improved health check doesn't cause false negatives
func TestHealthCheck(t *testing.T) {
	trans := transport.NewTCPTransport()
	addr := fmt.Sprintf("localhost:%d", 30000+time.Now().UnixNano()%1000)

	// Start listener
	listener, err := trans.Listen(addr)
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}

	listener.HandleMessage(func(msg []byte) error {
		return nil
	})

	if err := listener.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer listener.Stop()

	time.Sleep(200 * time.Millisecond)

	// Create connection
	conn, err := trans.Dial(addr)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer conn.Close()

	// Health check should pass (no false negatives)
	// Old implementation used 1ms read deadline which could fail
	// New implementation uses connection state check
	healthChecker, ok := conn.(transport.HealthCheckable)
	if !ok {
		t.Skip("Connection doesn't support health check")
		return
	}

	// Perform multiple health checks
	// Should all pass (no false negatives from aggressive timeout)
	for i := 0; i < 10; i++ {
		err := healthChecker.HealthCheck()
		if err != nil {
			t.Errorf("Health check failed (false negative): %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Log("Health check test passed: no false negatives")
}

// TestKeepAliveSettings verifies keep-alive improvements
func TestKeepAliveSettings(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping keep-alive test in short mode")
	}

	ctx := context.Background()

	// Create cluster
	nodes := setupTestCluster(t, 2, 31000)
	defer cleanupTestCluster(nodes)

	time.Sleep(1 * time.Second)

	// Send operation to establish connection
	err := nodes[0].Set(ctx, "keepalive-test", []byte("value"))
	if err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// Wait 35 seconds (between old 30s and new 60s keep-alive)
	// With 60s keep-alive, connection should still be alive
	time.Sleep(35 * time.Second)

	// Send another operation
	// With improved keep-alive, connection should still be valid
	err = nodes[0].Set(ctx, "keepalive-test-2", []byte("value-2"))
	if err != nil {
		t.Errorf("Set after keep-alive period failed: %v", err)
	}

	// Verify operation completed
	value, err := nodes[0].Get(ctx, "keepalive-test-2")
	if err != nil {
		t.Errorf("Get failed: %v", err)
	} else if string(value) != "value-2" {
		t.Errorf("Expected 'value-2', got '%s'", string(value))
	}

	t.Log("Keep-alive test passed: connection remained alive with improved settings")
}

// BenchmarkConnectionReuse measures connection reuse with improved settings
func BenchmarkConnectionReuse(b *testing.B) {
	trans := transport.NewTCPTransport()
	addr := fmt.Sprintf("localhost:%d", 32000+time.Now().UnixNano()%1000)

	listener, err := trans.Listen(addr)
	if err != nil {
		b.Fatalf("Listen failed: %v", err)
	}

	listener.HandleMessage(func(msg []byte) error {
		return nil
	})

	if err := listener.Start(); err != nil {
		b.Fatalf("Start failed: %v", err)
	}
	defer listener.Stop()

	time.Sleep(200 * time.Millisecond)

	// Create connection pool
	pool := transport.NewConnPool(trans, addr, 10, 100, 30*time.Second)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			ctx := context.Background()
			conn, err := pool.Get(ctx)
			if err != nil {
				b.Errorf("Get connection failed: %v", err)
				continue
			}

			// Use connection
			_ = conn.WriteDataWithContext(ctx, []byte("test"))

			pool.Put(conn)
		}
	})
}
