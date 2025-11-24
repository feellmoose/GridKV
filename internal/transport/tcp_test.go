package transport

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTCPTransport_Basic(t *testing.T) {
	transport := NewTCPTransport()

	listener, err := transport.Listen("localhost:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}

	addr := listener.Addr().String()

	var received []byte
	var wg sync.WaitGroup
	wg.Add(1)

	err = listener.HandleMessage(func(msg []byte) error {
		received = msg
		wg.Done()
		return nil
	}).Start()
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer listener.Stop()

	conn, err := transport.Dial(addr)
	if err != nil {
		t.Fatalf("Failed to dial: %v", err)
	}
	defer conn.Close()

	testData := []byte("test message")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = conn.WriteDataWithContext(ctx, testData)
	if err != nil {
		t.Fatalf("Failed to write: %v", err)
	}

	wg.Wait()

	if string(received) != string(testData) {
		t.Errorf("Expected %q, got %q", string(testData), string(received))
	}
}

func TestTCPTransport_Timeout(t *testing.T) {
	transport := NewTCPTransport()

	listener, err := transport.Listen("localhost:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}

	err = listener.HandleMessage(func(msg []byte) error {
		time.Sleep(2 * time.Second)
		return nil
	}).Start()
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer listener.Stop()

	addr := listener.Addr().String()
	conn, err := transport.Dial(addr)
	if err != nil {
		t.Fatalf("Failed to dial: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err = conn.WriteDataWithContext(ctx, []byte("test"))
	if err == nil {
		t.Error("Expected timeout error, got nil")
	}

	readCtx, readCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer readCancel()

	_, err = conn.ReadDataWithContext(readCtx)
	if err == nil {
		t.Error("Expected timeout error on read, got nil")
	}
}

func TestTCPTransport_ConnectionClose(t *testing.T) {
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
	conn, err := transport.Dial(addr)
	if err != nil {
		t.Fatalf("Failed to dial: %v", err)
	}

	err = conn.Close()
	if err != nil {
		t.Errorf("Failed to close connection: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err = conn.WriteDataWithContext(ctx, []byte("test"))
	if err == nil {
		t.Error("Expected error after close, got nil")
	}
}

func TestTCPTransport_ConcurrentConnections(t *testing.T) {
	transport := NewTCPTransport()

	listener, err := transport.Listen("localhost:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}

	var count int64
	var mu sync.Mutex

	err = listener.HandleMessage(func(msg []byte) error {
		mu.Lock()
		count++
		mu.Unlock()
		return nil
	}).Start()
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer listener.Stop()

	addr := listener.Addr().String()

	const numConns = 50
	var wg sync.WaitGroup
	wg.Add(numConns)

	for i := 0; i < numConns; i++ {
		go func(id int) {
			defer wg.Done()
			conn, err := transport.Dial(addr)
			if err != nil {
				t.Errorf("Connection %d failed: %v", id, err)
				return
			}
			defer conn.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			data := []byte(fmt.Sprintf("message-%d", id))
			if err := conn.WriteDataWithContext(ctx, data); err != nil {
				t.Errorf("Write %d failed: %v", id, err)
			}
		}(i)
	}

	wg.Wait()
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	received := count
	mu.Unlock()

	if received != numConns {
		t.Errorf("Expected %d messages, got %d", numConns, received)
	}
}

func TestTCPTransport_HealthCheck(t *testing.T) {
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
	conn, err := transport.Dial(addr)
	if err != nil {
		t.Fatalf("Failed to dial: %v", err)
	}
	defer conn.Close()

	tcpConn, ok := conn.(*TCPTransportConn)
	if !ok {
		t.Fatal("Expected TCPTransportConn")
	}

	err = tcpConn.HealthCheck()
	if err != nil {
		t.Errorf("Health check failed: %v", err)
	}

	conn.Close()

	err = tcpConn.HealthCheck()
	if err == nil {
		t.Error("Expected health check to fail after close")
	}
}

func TestTCPTransport_ListenerStop(t *testing.T) {
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

	done := make(chan struct{})
	go func() {
		err := listener.Stop()
		if err != nil {
			t.Errorf("Stop failed: %v", err)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("Stop timeout")
	}
}

func TestTCPTransport_LargeMessage(t *testing.T) {
	transport := NewTCPTransport()

	listener, err := transport.Listen("localhost:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}

	var received []byte
	var wg sync.WaitGroup
	wg.Add(1)

	err = listener.HandleMessage(func(msg []byte) error {
		received = msg
		wg.Done()
		return nil
	}).Start()
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer listener.Stop()

	addr := listener.Addr().String()
	conn, err := transport.Dial(addr)
	if err != nil {
		t.Fatalf("Failed to dial: %v", err)
	}
	defer conn.Close()

	largeData := make([]byte, 1*1024*1024)
	for i := range largeData {
		largeData[i] = byte(i % 256)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = conn.WriteDataWithContext(ctx, largeData)
	if err != nil {
		t.Fatalf("Failed to write large message: %v", err)
	}

	wg.Wait()

	if len(received) != len(largeData) {
		t.Errorf("Expected %d bytes, got %d", len(largeData), len(received))
	}
}

func TestTCPTransport_MultipleMessages(t *testing.T) {
	transport := NewTCPTransport()

	listener, err := transport.Listen("localhost:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}

	var received []string
	var mu sync.Mutex
	var wg sync.WaitGroup

	err = listener.HandleMessage(func(msg []byte) error {
		mu.Lock()
		received = append(received, string(msg))
		mu.Unlock()
		wg.Done()
		return nil
	}).Start()
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer listener.Stop()

	addr := listener.Addr().String()
	conn, err := transport.Dial(addr)
	if err != nil {
		t.Fatalf("Failed to dial: %v", err)
	}
	defer conn.Close()

	const numMessages = 100
	wg.Add(numMessages)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for i := 0; i < numMessages; i++ {
		data := []byte(fmt.Sprintf("message-%d", i))
		if err := conn.WriteDataWithContext(ctx, data); err != nil {
			t.Fatalf("Failed to write message %d: %v", i, err)
		}
	}

	wg.Wait()

	mu.Lock()
	count := len(received)
	mu.Unlock()

	if count != numMessages {
		t.Errorf("Expected %d messages, got %d", numMessages, count)
	}
}

func TestTCPTransport_ServerDisconnect(t *testing.T) {
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

	addr := listener.Addr().String()
	conn, err := transport.Dial(addr)
	if err != nil {
		t.Fatalf("Failed to dial: %v", err)
	}

	listener.Stop()

	time.Sleep(100 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err = conn.WriteDataWithContext(ctx, []byte("test"))
	if err == nil {
		t.Error("Expected error after server disconnect, got nil")
	}

	conn.Close()
}

func TestTCPTransport_ClientDisconnect(t *testing.T) {
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
	conn, err := transport.Dial(addr)
	if err != nil {
		t.Fatalf("Failed to dial: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err = conn.WriteDataWithContext(ctx, []byte("test"))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	conn.Close()

	time.Sleep(100 * time.Millisecond)

	conn2, err := transport.Dial(addr)
	if err != nil {
		t.Fatalf("Failed to dial after disconnect: %v", err)
	}
	defer conn2.Close()

	err = conn2.WriteDataWithContext(ctx, []byte("test2"))
	if err != nil {
		t.Errorf("Write after reconnect failed: %v", err)
	}
}

func TestTCPTransport_ReadAfterClose(t *testing.T) {
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
	conn, err := transport.Dial(addr)
	if err != nil {
		t.Fatalf("Failed to dial: %v", err)
	}

	conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, err = conn.ReadDataWithContext(ctx)
	if err == nil {
		t.Error("Expected error when reading from closed connection, got nil")
	}
}

func TestTCPTransport_WriteDeadline(t *testing.T) {
	transport := NewTCPTransport()

	listener, err := transport.Listen("localhost:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}

	err = listener.HandleMessage(func(msg []byte) error {
		time.Sleep(2 * time.Second)
		return nil
	}).Start()
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer listener.Stop()

	addr := listener.Addr().String()
	conn, err := transport.Dial(addr)
	if err != nil {
		t.Fatalf("Failed to dial: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	data := make([]byte, 1024*1024)
	err = conn.WriteDataWithContext(ctx, data)
	if err == nil {
		t.Error("Expected timeout error for large write, got nil")
	}
}

func TestTCPTransport_Addr(t *testing.T) {
	transport := NewTCPTransport()

	listener, err := transport.Listen("localhost:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}
	defer listener.Stop()

	addr := listener.Addr()
	if addr == nil {
		t.Error("Expected listener address, got nil")
	}

	err = listener.HandleMessage(func(msg []byte) error {
		return nil
	}).Start()
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}

	conn, err := transport.Dial(addr.String())
	if err != nil {
		t.Fatalf("Failed to dial: %v", err)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr()
	if localAddr == nil {
		t.Error("Expected local address, got nil")
	}

	remoteAddr := conn.RemoteAddr()
	if remoteAddr == nil {
		t.Error("Expected remote address, got nil")
	}
}

func TestTCPTransport_ConcurrentReadWrite(t *testing.T) {
	transport := NewTCPTransport()

	listener, err := transport.Listen("localhost:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}

	var wg sync.WaitGroup
	var received int64

	err = listener.HandleMessage(func(msg []byte) error {
		atomic.AddInt64(&received, 1)
		wg.Done()
		return nil
	}).Start()
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer listener.Stop()

	addr := listener.Addr().String()
	conn, err := transport.Dial(addr)
	if err != nil {
		t.Fatalf("Failed to dial: %v", err)
	}
	defer conn.Close()

	const numOps = 50
	wg.Add(numOps)

	var writeWg sync.WaitGroup
	writeWg.Add(numOps)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for i := 0; i < numOps; i++ {
		go func(id int) {
			defer writeWg.Done()
			data := []byte(fmt.Sprintf("msg-%d", id))
			if err := conn.WriteDataWithContext(ctx, data); err != nil {
				t.Errorf("Write %d failed: %v", id, err)
			}
		}(i)
	}

	writeWg.Wait()
	wg.Wait()

	count := atomic.LoadInt64(&received)
	if count != numOps {
		t.Errorf("Expected %d messages, got %d", numOps, count)
	}
}

func TestTCPTransport_DialWithRTT(t *testing.T) {
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
	conn, err := transport.DialWithRTT(addr, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("Failed to dial with RTT: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = conn.WriteDataWithContext(ctx, []byte("test"))
	if err != nil {
		t.Errorf("Write failed: %v", err)
	}
}
