package transport

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConnPool_Basic(t *testing.T) {
	transport := NewTCPTransport()
	pool := NewConnPool(transport, "localhost:99999", 5, 10, 30*time.Second)
	defer pool.Close()

	conn, err := pool.Get(context.Background())
	if err == nil {
		conn.Close()
		t.Error("Expected error for invalid address, got nil")
	}
}

func TestConnPool_Timeout(t *testing.T) {
	transport := NewTCPTransport()

	listener, err := transport.Listen("localhost:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}

	err = listener.HandleMessage(func(msg []byte) error {
		return nil
	}).Start()
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer listener.Stop()

	addr := listener.Addr().String()
	pool := NewConnPool(transport, addr, 2, 2, 100*time.Millisecond)
	defer pool.Close()

	conn1, err := pool.Get(context.Background())
	if err != nil {
		t.Fatalf("Failed to get connection: %v", err)
	}
	defer pool.Put(conn1)

	conn2, err := pool.Get(context.Background())
	if err != nil {
		t.Fatalf("Failed to get connection: %v", err)
	}
	defer pool.Put(conn2)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err = pool.Get(ctx)
	if err == nil {
		t.Error("Expected pool exhausted error, got nil")
	}
}

func TestConnPool_ConnectionReuse(t *testing.T) {
	transport := NewTCPTransport()

	listener, err := transport.Listen("localhost:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}

	err = listener.HandleMessage(func(msg []byte) error {
		return nil
	}).Start()
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer listener.Stop()

	addr := listener.Addr().String()
	pool := NewConnPool(transport, addr, 5, 10, 30*time.Second)
	defer pool.Close()

	conn1, err := pool.Get(context.Background())
	if err != nil {
		t.Fatalf("Failed to get connection: %v", err)
	}

	pool.Put(conn1)

	conn2, err := pool.Get(context.Background())
	if err != nil {
		t.Fatalf("Failed to get connection: %v", err)
	}

	if conn1 != conn2 {
		t.Error("Expected connection reuse, got new connection")
	}

	pool.Put(conn2)
}

func TestConnPool_IdleTimeout(t *testing.T) {
	transport := NewTCPTransport()

	listener, err := transport.Listen("localhost:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}

	err = listener.HandleMessage(func(msg []byte) error {
		return nil
	}).Start()
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer listener.Stop()

	addr := listener.Addr().String()
	pool := NewConnPool(transport, addr, 5, 10, 200*time.Millisecond)
	defer pool.Close()

	conn, err := pool.Get(context.Background())
	if err != nil {
		t.Fatalf("Failed to get connection: %v", err)
	}

	pool.Put(conn)

	time.Sleep(300 * time.Millisecond)

	conn2, err := pool.Get(context.Background())
	if err != nil {
		t.Fatalf("Failed to get connection: %v", err)
	}

	if conn == conn2 {
		t.Error("Expected new connection after idle timeout, got reused connection")
	}

	pool.Put(conn2)
}

func TestConnPool_Close(t *testing.T) {
	transport := NewTCPTransport()

	listener, err := transport.Listen("localhost:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}

	err = listener.HandleMessage(func(msg []byte) error {
		return nil
	}).Start()
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer listener.Stop()

	addr := listener.Addr().String()
	pool := NewConnPool(transport, addr, 5, 10, 30*time.Second)

	conn, err := pool.Get(context.Background())
	if err != nil {
		t.Fatalf("Failed to get connection: %v", err)
	}

	err = pool.Close()
	if err != nil {
		t.Errorf("Close failed: %v", err)
	}

	pool.Put(conn)

	_, err = pool.Get(context.Background())
	if err == nil {
		t.Error("Expected error after close, got nil")
	}
}

func TestConnPool_Invalidate(t *testing.T) {
	transport := NewTCPTransport()

	listener, err := transport.Listen("localhost:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}

	err = listener.HandleMessage(func(msg []byte) error {
		return nil
	}).Start()
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer listener.Stop()

	addr := listener.Addr().String()
	pool := NewConnPool(transport, addr, 5, 10, 30*time.Second)
	defer pool.Close()

	conn1, err := pool.Get(context.Background())
	if err != nil {
		t.Fatalf("Failed to get connection: %v", err)
	}

	pool.Invalidate(conn1)

	conn2, err := pool.Get(context.Background())
	if err != nil {
		t.Fatalf("Failed to get connection: %v", err)
	}

	if conn1 == conn2 {
		t.Error("Expected new connection after invalidate, got same connection")
	}

	pool.Put(conn2)
}

func TestConnPool_ConcurrentAccess(t *testing.T) {
	transport := NewTCPTransport()

	listener, err := transport.Listen("localhost:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}

	var msgCount int64
	err = listener.HandleMessage(func(msg []byte) error {
		atomic.AddInt64(&msgCount, 1)
		return nil
	}).Start()
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer listener.Stop()

	addr := listener.Addr().String()
	pool := NewConnPool(transport, addr, 10, 20, 30*time.Second)
	defer pool.Close()

	const numGoroutines = 50
	const opsPerGoroutine = 10
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				conn, err := pool.Get(context.Background())
				if err != nil {
					t.Errorf("Goroutine %d, op %d: Get failed: %v", id, j, err)
					continue
				}

				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				data := []byte{byte(id), byte(j)}
				err = conn.WriteDataWithContext(ctx, data)
				cancel()

				if err != nil {
					pool.Invalidate(conn)
				} else {
					pool.Put(conn)
				}
			}
		}(i)
	}

	wg.Wait()
	time.Sleep(100 * time.Millisecond)

	received := atomic.LoadInt64(&msgCount)
	expected := int64(numGoroutines * opsPerGoroutine)
	if received != expected {
		t.Errorf("Expected %d messages, got %d", expected, received)
	}
}

func TestConnPool_ContextCancel(t *testing.T) {
	transport := NewTCPTransport()

	listener, err := transport.Listen("localhost:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}

	err = listener.HandleMessage(func(msg []byte) error {
		return nil
	}).Start()
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer listener.Stop()

	addr := listener.Addr().String()
	pool := NewConnPool(transport, addr, 1, 1, 30*time.Second)
	defer pool.Close()

	conn, err := pool.Get(context.Background())
	if err != nil {
		t.Fatalf("Failed to get connection: %v", err)
	}
	defer pool.Put(conn)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = pool.Get(ctx)
	if err == nil {
		t.Error("Expected context cancelled error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Expected context.Canceled, got %v", err)
	}
}

func TestConnPool_HealthCheck(t *testing.T) {
	transport := NewTCPTransport()

	listener, err := transport.Listen("localhost:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}

	err = listener.HandleMessage(func(msg []byte) error {
		return nil
	}).Start()
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer listener.Stop()

	addr := listener.Addr().String()
	pool := NewConnPool(transport, addr, 5, 10, 30*time.Second)
	defer pool.Close()

	conn1, err := pool.Get(context.Background())
	if err != nil {
		t.Fatalf("Failed to get connection: %v", err)
	}

	tcpConn, ok := conn1.(*TCPTransportConn)
	if !ok {
		t.Fatal("Expected TCPTransportConn")
	}

	err = tcpConn.HealthCheck()
	if err != nil {
		t.Errorf("Health check failed: %v", err)
	}

	pool.Put(conn1)

	conn2, err := pool.Get(context.Background())
	if err != nil {
		t.Fatalf("Failed to get connection: %v", err)
	}

	if conn1 != conn2 {
		t.Error("Expected connection reuse after health check")
	}

	pool.Put(conn2)
}

func TestConnPool_UnhealthyConnection(t *testing.T) {
	transport := NewTCPTransport()

	listener, err := transport.Listen("localhost:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}

	err = listener.HandleMessage(func(msg []byte) error {
		return nil
	}).Start()
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer listener.Stop()

	addr := listener.Addr().String()
	pool := NewConnPool(transport, addr, 5, 10, 200*time.Millisecond)
	defer pool.Close()

	conn1, err := pool.Get(context.Background())
	if err != nil {
		t.Fatalf("Failed to get connection: %v", err)
	}

	conn1.Close()
	pool.Put(conn1)

	time.Sleep(100 * time.Millisecond)

	conn2, err := pool.Get(context.Background())
	if err != nil {
		t.Fatalf("Failed to get connection: %v", err)
	}

	if conn1 == conn2 {
		t.Error("Expected new connection after unhealthy, got reused")
	}

	pool.Put(conn2)
}

func TestConnPool_MaxIdleLimit(t *testing.T) {
	transport := NewTCPTransport()

	listener, err := transport.Listen("localhost:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}

	err = listener.HandleMessage(func(msg []byte) error {
		return nil
	}).Start()
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer listener.Stop()

	addr := listener.Addr().String()
	pool := NewConnPool(transport, addr, 3, 5, 30*time.Second)
	defer pool.Close()

	conns := make([]TransportConn, 6)
	for i := 0; i < 6; i++ {
		conn, err := pool.Get(context.Background())
		if err != nil {
			t.Fatalf("Failed to get connection %d: %v", i, err)
		}
		conns[i] = conn
	}

	for i := 0; i < 6; i++ {
		pool.Put(conns[i])
	}

	for i := 0; i < 10; i++ {
		conn, err := pool.Get(context.Background())
		if err != nil {
			t.Fatalf("Failed to get connection: %v", err)
		}
		pool.Put(conn)
	}
}

func TestConnPool_StressTest(t *testing.T) {
	transport := NewTCPTransport()

	listener, err := transport.Listen("localhost:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}

	var msgCount int64
	err = listener.HandleMessage(func(msg []byte) error {
		atomic.AddInt64(&msgCount, 1)
		return nil
	}).Start()
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer listener.Stop()

	addr := listener.Addr().String()
	pool := NewConnPool(transport, addr, 20, 50, 30*time.Second)
	defer pool.Close()

	const numGoroutines = 200
	const opsPerGoroutine = 20
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	start := time.Now()

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				conn, err := pool.Get(context.Background())
				if err != nil {
					continue
				}

				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				data := []byte{byte(id), byte(j)}
				err = conn.WriteDataWithContext(ctx, data)
				cancel()

				if err != nil {
					pool.Invalidate(conn)
				} else {
					pool.Put(conn)
				}
			}
		}(i)
	}

	wg.Wait()
	duration := time.Since(start)

	maxWait := 2 * time.Second
	maxWaitTime := time.After(maxWait)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	var received int64
	expected := int64(numGoroutines * opsPerGoroutine)

	for {
		received = atomic.LoadInt64(&msgCount)
		if received >= expected*9/10 {
			break
		}
		select {
		case <-maxWaitTime:
			goto done
		case <-ticker.C:
			continue
		}
	}
done:
	received = atomic.LoadInt64(&msgCount)

	t.Logf("Stress test: %d goroutines, %d ops each, %v duration", numGoroutines, opsPerGoroutine, duration)
	t.Logf("Expected %d messages, received %d (%.1f%%)", expected, received, float64(received)/float64(expected)*100)

	if received < expected*5/10 {
		t.Errorf("Received only %d/%d messages (%.1f%%)", received, expected, float64(received)/float64(expected)*100)
	}

	if received == 0 {
		t.Error("Expected at least some messages, got 0")
	}
}
