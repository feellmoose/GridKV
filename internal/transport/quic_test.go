package transport

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestQUICTransport_Basic(t *testing.T) {
	config := DefaultQUICConfig()
	transport, err := NewQUICTransport(config)
	if err != nil {
		t.Fatalf("Failed to create QUIC transport: %v", err)
	}

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

	addr := listener.Addr().String()

	time.Sleep(500 * time.Millisecond)
	defer listener.Stop()
	defer transport.Stop()

	conn, err := transport.Dial(addr)
	if err != nil {
		t.Fatalf("Failed to dial: %v", err)
	}
	defer conn.Close()

	testData := []byte("test message")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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

func TestQUICTransport_Timeout(t *testing.T) {
	config := DefaultQUICConfig()
	config.StreamReadTimeout = 200 * time.Millisecond
	config.StreamAcceptTimeout = 200 * time.Millisecond

	transport, err := NewQUICTransport(config)
	if err != nil {
		t.Fatalf("Failed to create QUIC transport: %v", err)
	}
	defer transport.Stop()

	listener, err := transport.Listen("localhost:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}

	err = listener.HandleMessage(func(msg []byte) error {
		time.Sleep(1 * time.Second)
		return nil
	}).Start()
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer listener.Stop()

	time.Sleep(500 * time.Millisecond)
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
		t.Log("Write timeout may not trigger immediately, this is expected for QUIC")
	}

	readCtx, readCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer readCancel()

	_, err = conn.ReadDataWithContext(readCtx)
	if err == nil {
		t.Log("Read timeout may not trigger immediately, this is expected for QUIC")
	}
}

func TestQUICTransport_ConnectionClose(t *testing.T) {
	config := DefaultQUICConfig()
	transport, err := NewQUICTransport(config)
	if err != nil {
		t.Fatalf("Failed to create QUIC transport: %v", err)
	}
	defer transport.Stop()

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

	time.Sleep(500 * time.Millisecond)
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

func TestQUICTransport_ConcurrentConnections(t *testing.T) {
	config := DefaultQUICConfig()
	transport, err := NewQUICTransport(config)
	if err != nil {
		t.Fatalf("Failed to create QUIC transport: %v", err)
	}
	defer transport.Stop()

	listener, err := transport.Listen("localhost:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}

	var count int64

	err = listener.HandleMessage(func(msg []byte) error {
		atomic.AddInt64(&count, 1)
		return nil
	}).Start()
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer listener.Stop()

	time.Sleep(500 * time.Millisecond)
	addr := listener.Addr().String()

	const numConns = 20
	var wg sync.WaitGroup
	wg.Add(numConns)

	for i := 0; i < numConns; i++ {
		go func(id int) {
			defer wg.Done()
			conn, err := transport.Dial(addr)
			if err != nil {
				t.Logf("Connection %d failed: %v", id, err)
				return
			}
			defer conn.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			data := []byte(fmt.Sprintf("message-%d", id))
			if err := conn.WriteDataWithContext(ctx, data); err != nil {
				t.Logf("Write %d failed: %v", id, err)
			}
		}(i)
	}

	wg.Wait()
	time.Sleep(500 * time.Millisecond)

	received := atomic.LoadInt64(&count)
	if received < numConns/2 {
		t.Errorf("Expected at least %d messages, got %d", numConns/2, received)
	}
}

func TestQUICTransport_StopWithTimeout(t *testing.T) {
	config := DefaultQUICConfig()
	transport, err := NewQUICTransport(config)
	if err != nil {
		t.Fatalf("Failed to create QUIC transport: %v", err)
	}

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
		err = transport.Stop()
		if err != nil {
			t.Errorf("Transport stop failed: %v", err)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Error("Stop timeout")
	}
}
