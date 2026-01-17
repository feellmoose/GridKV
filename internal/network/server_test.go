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

func TestNetworkServer_StartStop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)

	serverCfg := DefaultServerConfig(transport)
	server := NewServer(serverCfg)

	ctx := context.Background()
	addr := "127.0.0.1:0"

	handler := func(ctx context.Context, remoteAddr string, data []byte) ([]byte, error) {
		return append(data, []byte("_echo")...), nil
	}

	if err := server.Start(ctx, addr, handler); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	serverAddr := server.Address()
	if serverAddr == "" {
		t.Error("Address() returned empty string")
	}

	stats := server.Stats()
	if stats.Connections != 0 {
		t.Errorf("Stats().Connections = %d, want 0", stats.Connections)
	}

	if err := server.Stop(ctx); err != nil {
		t.Errorf("Stop() error = %v", err)
	}
}

func TestNetworkServer_HandleMessage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)

	serverCfg := DefaultServerConfig(transport)
	server := NewServer(serverCfg)

	ctx := context.Background()
	addr := "127.0.0.1:0"

	handler := func(ctx context.Context, remoteAddr string, data []byte) ([]byte, error) {
		return append([]byte("echo: "), data...), nil
	}

	if err := server.Start(ctx, addr, handler); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() { _ = server.Stop(ctx) }()

	serverAddr := server.Address()

	// connect and send
	clientConn, err := transport.Dial(ctx, serverAddr)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer clientConn.Close()

	sendData := []byte("test message")
	if err := clientConn.Send(ctx, sendData); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	// wait for processing
	time.Sleep(100 * time.Millisecond)

	stats := server.Stats()
	if stats.Messages == 0 {
		t.Error("Stats().Messages = 0, want > 0")
	}
}

func TestNetworkServer_RequestResponse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)

	serverCfg := DefaultServerConfig(transport)
	serverCfg.EnableRequestResponse = true
	server := NewServer(serverCfg)

	ctx := context.Background()
	addr := "127.0.0.1:0"

	reqHandler := func(ctx context.Context, remoteAddr string, request []byte) ([]byte, error) {
		return append([]byte("response: "), request...), nil
	}

	if err := server.StartRequestResponse(ctx, addr, reqHandler); err != nil {
		t.Fatalf("StartRequestResponse() error = %v", err)
	}
	defer func() { _ = server.Stop(ctx) }()

	serverAddr := server.Address()

	// connect and send request
	clientConn, err := transport.Dial(ctx, serverAddr)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer clientConn.Close()

	request := []byte("test request")
	if err := clientConn.Send(ctx, request); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	// receive response
	response, err := clientConn.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive() error = %v", err)
	}

	expected := "response: test request"
	if string(response) != expected {
		t.Errorf("Receive() = %v, want %v", string(response), expected)
	}
}

func TestNetworkServer_Stats(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)

	serverCfg := DefaultServerConfig(transport)
	server := NewServer(serverCfg)

	ctx := context.Background()
	addr := "127.0.0.1:0"

	handler := func(ctx context.Context, remoteAddr string, data []byte) ([]byte, error) {
		return data, nil
	}

	if err := server.Start(ctx, addr, handler); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() { _ = server.Stop(ctx) }()

	serverAddr := server.Address()

	clientConn, err := transport.Dial(ctx, serverAddr)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer clientConn.Close()

	data := []byte("test")
	_ = clientConn.Send(ctx, data)

	time.Sleep(100 * time.Millisecond)

	stats := server.Stats()
	if stats.Connections == 0 {
		t.Error("Stats().Connections = 0, want > 0")
	}
	if stats.Messages == 0 {
		t.Error("Stats().Messages = 0, want > 0")
	}
	if stats.Bytes == 0 {
		t.Error("Stats().Bytes = 0, want > 0")
	}
}

// TestNetworkServer_HighConcurrency tests server handling of high concurrent load
// This validates that the server can efficiently handle thousands of concurrent connections
// with minimal resource overhead, essential for high-performance distributed systems.
func TestNetworkServer_HighConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping high concurrency server test in short mode.")
	}

	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	cfg.ReadTimeout = 5 * time.Second
	cfg.WriteTimeout = 5 * time.Second
	cfg.MaxMessageSize = 16 * 1024 // 16KB for performance

	transport, err := NewTransport(cfg)
	if err != nil {
		t.Fatalf("NewTransport() error = %v", err)
	}
	defer transport.Close()

	serverCfg := DefaultServerConfig(transport)
	// Note: MaxConnections field removed from ServerConfig
	server := NewServer(serverCfg)
	defer server.Stop(context.Background())

	ctx := context.Background()
	addr := "127.0.0.1:0"

	// Echo handler
	handler := func(ctx context.Context, remoteAddr string, data []byte) ([]byte, error) {
		return data, nil // Simple echo
	}

	if err := server.Start(ctx, addr, handler); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	serverAddr := server.Address()

	// Test with high concurrent clients
	const numClients = 200
	const messagesPerClient = 50
	var totalMessages int64
	var totalErrors int64

	start := time.Now()
	var wg sync.WaitGroup

	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func(clientID int) {
			defer wg.Done()

			// Create connection to server
			conn, err := transport.Dial(ctx, serverAddr)
			if err != nil {
				atomic.AddInt64(&totalErrors, 1)
				return
			}
			defer conn.Close()

			// Send multiple messages
			for j := 0; j < messagesPerClient; j++ {
				testData := []byte(fmt.Sprintf("client-%d-msg-%d", clientID, j))

				if err := conn.Send(ctx, testData); err != nil {
					atomic.AddInt64(&totalErrors, 1)
					return
				}

				resp, err := conn.Receive(ctx)
				if err != nil {
					atomic.AddInt64(&totalErrors, 1)
					return
				}

				if string(resp) != string(testData) {
					atomic.AddInt64(&totalErrors, 1)
					return
				}

				atomic.AddInt64(&totalMessages, 1)
			}
		}(i)
	}

	wg.Wait()
	elapsed := time.Since(start)

	// Check server stats
	stats := server.Stats()
	messagesPerSec := float64(totalMessages) / elapsed.Seconds()
	errorRate := float64(totalErrors) / float64(totalMessages+totalErrors) * 100

	t.Logf("High concurrency server test:")
	t.Logf("  Clients: %d", numClients)
	t.Logf("  Messages per client: %d", messagesPerClient)
	t.Logf("  Total messages: %d", totalMessages)
	t.Logf("  Total errors: %d", totalErrors)
	t.Logf("  Error rate: %.2f%%", errorRate)
	t.Logf("  Duration: %v", elapsed)
	t.Logf("  Messages/sec: %.0f", messagesPerSec)
	t.Logf("  Server connections: %d", stats.Connections)
	t.Logf("  Server messages: %d", stats.Messages)
	t.Logf("  Server bytes: %d", stats.Bytes)

	// Verify high-concurrency performance
	if messagesPerSec < 5000 {
		t.Errorf("Server throughput too low: %.0f msg/sec (want >= 5000)", messagesPerSec)
	}

	if errorRate > 0.5 {
		t.Errorf("Error rate too high: %.2f%% (want <= 0.5%%)", errorRate)
	}

	if stats.Connections < numClients/2 {
		t.Errorf("Server connections too low: %d (want >= %d)", stats.Connections, numClients/2)
	}

	// Verify server resource efficiency
	// Server counts both request and response messages, so it's 2x the client count
	expectedServerMessages := uint64(totalMessages * 2)
	if uint64(stats.Messages) != expectedServerMessages {
		t.Errorf("Server message count mismatch: got %d, want %d (requests: %d, responses: %d)", 
			stats.Messages, expectedServerMessages, totalMessages, totalMessages)
	}
}

// TestNetworkServer_ResourceEfficiency tests server resource usage under load
// This validates that the server maintains efficient resource usage
// during sustained high-concurrency operation.
func TestNetworkServer_ResourceEfficiency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping resource efficiency test in short mode.")
	}

	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport, err := NewTransport(cfg)
	if err != nil {
		t.Fatalf("NewTransport() error = %v", err)
	}
	defer transport.Close()

	serverCfg := DefaultServerConfig(transport)
	server := NewServer(serverCfg)
	defer server.Stop(context.Background())

	ctx := context.Background()
	addr := "127.0.0.1:0"

	// Counter handler
	var messageCount int64
	handler := func(ctx context.Context, remoteAddr string, data []byte) ([]byte, error) {
		atomic.AddInt64(&messageCount, 1)
		return data, nil
	}

	if err := server.Start(ctx, addr, handler); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	serverAddr := server.Address()

	// Record initial resource usage
	initialGoroutines := runtime.NumGoroutine()
	runtime.GC()
	var initialMem runtime.MemStats
	runtime.ReadMemStats(&initialMem)

	// Run sustained load test
	const testDuration = 3 * time.Second
	const numWorkers = 50

	start := time.Now()
	var wg sync.WaitGroup

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			for time.Since(start) < testDuration {
				conn, err := transport.Dial(ctx, serverAddr)
				if err != nil {
					continue
				}

				testData := []byte(fmt.Sprintf("resource-test-%d-%d", workerID, time.Now().UnixNano()))
				conn.Send(ctx, testData)
				conn.Receive(ctx)
				conn.Close()
			}
		}(i)
	}

	wg.Wait()
	elapsed := time.Since(start)

	// Record final resource usage
	finalGoroutines := runtime.NumGoroutine()
	runtime.GC()
	var finalMem runtime.MemStats
	runtime.ReadMemStats(&finalMem)

	// Calculate resource usage
	goroutineIncrease := finalGoroutines - initialGoroutines
	// Compare HeapAlloc with HeapAlloc (not TotalAlloc which is cumulative)
	memIncrease := int64(finalMem.HeapAlloc) - int64(initialMem.HeapAlloc)
	messagesProcessed := atomic.LoadInt64(&messageCount)

	messagesPerSec := float64(messagesProcessed) / elapsed.Seconds()

	t.Logf("Server resource efficiency test:")
	t.Logf("  Test duration: %v", elapsed)
	t.Logf("  Messages processed: %d", messagesProcessed)
	t.Logf("  Messages/sec: %.0f", messagesPerSec)
	t.Logf("  Initial goroutines: %d", initialGoroutines)
	t.Logf("  Final goroutines: %d", finalGoroutines)
	t.Logf("  Goroutine increase: %d", goroutineIncrease)
	t.Logf("  Memory increase: %d bytes", memIncrease)
	t.Logf("  Final heap: %d MB", finalMem.HeapAlloc/1024/1024)

	// Verify resource efficiency
	if goroutineIncrease > numWorkers+20 {
		t.Errorf("Goroutine leak: increase %d (want <= %d+20)", goroutineIncrease, numWorkers)
	}

	if memIncrease > 10*1024*1024 { // 10MB
		t.Errorf("Memory increase too high: %d MB (want <= 10MB)", memIncrease/1024/1024)
	}

	if messagesPerSec < 1000 {
		t.Errorf("Server throughput too low: %.0f msg/sec (want >= 1000)", messagesPerSec)
	}
}

// BenchmarkNetworkServer_Throughput benchmarks server message throughput
func BenchmarkNetworkServer_Throughput(b *testing.B) {
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport, err := NewTransport(cfg)
	if err != nil {
		b.Fatalf("NewTransport() error = %v", err)
	}
	defer transport.Close()

	serverCfg := DefaultServerConfig(transport)
	server := NewServer(serverCfg)
	defer server.Stop(context.Background())

	ctx := context.Background()
	addr := "127.0.0.1:0"

	handler := func(ctx context.Context, remoteAddr string, data []byte) ([]byte, error) {
		return data, nil // Echo
	}

	if err := server.Start(ctx, addr, handler); err != nil {
		b.Fatalf("Start() error = %v", err)
	}

	serverAddr := server.Address()

	// Give server time to start
	time.Sleep(100 * time.Millisecond)

	testData := []byte("benchmark-server-throughput-test-message-payload")

	b.ResetTimer()
	b.SetBytes(int64(len(testData)))

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			conn, err := transport.Dial(ctx, serverAddr)
			if err != nil {
				b.Fatal(err)
			}

			if err := conn.Send(ctx, testData); err != nil {
				conn.Close()
				b.Fatal(err)
			}

			if _, err := conn.Receive(ctx); err != nil {
				conn.Close()
				b.Fatal(err)
			}

			conn.Close()
		}
	})
}
