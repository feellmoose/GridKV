package network

import (
	"context"
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
	defer server.Stop(ctx)

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
	defer server.Stop(ctx)

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
	defer server.Stop(ctx)

	serverAddr := server.Address()

	clientConn, err := transport.Dial(ctx, serverAddr)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer clientConn.Close()

	data := []byte("test")
	clientConn.Send(ctx, data)

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

