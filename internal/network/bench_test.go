package network

import (
	"context"
	"testing"
	"time"
)

// BenchmarkConnPool_GetPut benchmarks connection pool Get/Put operations
func BenchmarkConnPool_GetPut(b *testing.B) {
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)
	defer transport.Close()

	poolCfg := DefaultPoolConfig(transport)
	poolCfg.MaxIdle = 100
	poolCfg.MaxActive = 200
	pool := NewConnPool(poolCfg)
	defer pool.Close()

	ctx := context.Background()
	addr := "127.0.0.1:0"

	// Create listener for testing
	ln, err := transport.Listen(ctx, addr)
	if err != nil {
		b.Fatalf("Listen() error = %v", err)
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

	// Warm up
	for i := 0; i < 10; i++ {
		conn, _ := pool.Get(ctx, serverAddr)
		if conn != nil {
			pool.Put(conn)
		}
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			conn, err := pool.Get(ctx, serverAddr)
			if err != nil {
				b.Fatal(err)
			}
			pool.Put(conn)
		}
	})
}

// BenchmarkServer_HandleMessage benchmarks server message handling
func BenchmarkServer_HandleMessage(b *testing.B) {
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)
	defer transport.Close()

	serverCfg := DefaultServerConfig(transport)
	serverCfg.WorkerPoolSize = 100
	server := NewServer(serverCfg)

	ctx := context.Background()
	addr := "127.0.0.1:0"

	handler := func(ctx context.Context, remoteAddr string, data []byte) ([]byte, error) {
		return data, nil
	}

	if err := server.Start(ctx, addr, handler); err != nil {
		b.Fatalf("Start() error = %v", err)
	}
	defer server.Stop(ctx)

	serverAddr := server.Address()

	// Create client connection
	clientConn, err := transport.Dial(ctx, serverAddr)
	if err != nil {
		b.Fatalf("Dial() error = %v", err)
	}
	defer clientConn.Close()

	testData := []byte("test message")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := clientConn.Send(ctx, testData); err != nil {
			b.Fatal(err)
		}
		_, err := clientConn.Receive(ctx)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkClient_Send benchmarks client send operations
func BenchmarkClient_Send(b *testing.B) {
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)
	defer transport.Close()

	poolCfg := DefaultPoolConfig(transport)
	pool := NewConnPool(poolCfg)
	defer pool.Close()

	clientCfg := DefaultClientConfig(pool)
	client := NewClient(clientCfg)
	defer client.Close()

	ctx := context.Background()
	addr := "127.0.0.1:0"

	// Create listener and echo server
	ln, err := transport.Listen(ctx, addr)
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

	// Warm up
	time.Sleep(100 * time.Millisecond)

	testData := []byte("benchmark data")

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = client.Send(ctx, serverAddr, testData)
		}
	})
}

// BenchmarkBackpressure_AcquireRelease benchmarks backpressure operations
func BenchmarkBackpressure_AcquireRelease(b *testing.B) {
	cfg := DefaultBackpressureConfig()
	bp := NewBackpressure(cfg)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = bp.Acquire()
			bp.Release()
		}
	})
}
