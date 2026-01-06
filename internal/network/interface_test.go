package network

import (
	"context"
	"testing"
	"time"
)

func TestNetwork_StartStop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	cfg := DefaultNetworkConfig("127.0.0.1:0")
	cfg.TransportConfig.Type = TransportTCP

	transport, err := NewTransport(cfg.TransportConfig)
	if err != nil {
		t.Fatalf("NewTransport() error = %v", err)
	}

	cfg.PoolConfig = DefaultPoolConfig(transport)
	cfg.ClientConfig = DefaultClientConfig(NewConnPool(cfg.PoolConfig))
	cfg.ServerConfig = DefaultServerConfig(transport)

	net, err := NewNetwork(cfg)
	if err != nil {
		t.Fatalf("NewNetwork() error = %v", err)
	}

	ctx := context.Background()
	if err := net.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if err := net.Stop(ctx); err != nil {
		t.Errorf("Stop() error = %v", err)
	}
}

func TestNetwork_SendReceive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	cfg := DefaultNetworkConfig("127.0.0.1:0")
	cfg.TransportConfig.Type = TransportTCP

	transport, err := NewTransport(cfg.TransportConfig)
	if err != nil {
		t.Fatalf("NewTransport() error = %v", err)
	}

	cfg.PoolConfig = DefaultPoolConfig(transport)
	cfg.ClientConfig = DefaultClientConfig(NewConnPool(cfg.PoolConfig))
	cfg.ServerConfig = DefaultServerConfig(transport)

	net, err := NewNetwork(cfg)
	if err != nil {
		t.Fatalf("NewNetwork() error = %v", err)
	}

	ctx := context.Background()
	if err := net.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() { _ = net.Stop(ctx) }()

	// register handler
	received := make(chan []byte, 1)
	handler := func(ctx context.Context, remoteAddr string, data []byte) ([]byte, error) {
		received <- data
		return append([]byte("echo: "), data...), nil
	}

	if err := net.RegisterHandler(MessageTypeRequest, handler); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	// get server address
	serverAddr := net.Server().Address()

	// send message using SendMessage
	msg := &Message{
		Type:      MessageTypeRequest,
		ID:        1,
		Data:      []byte("test message"),
		Timestamp: time.Now().UnixNano(),
	}
	if err := net.SendMessage(ctx, serverAddr, msg); err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}

	// wait for receive
	select {
	case recvData := <-received:
		if string(recvData) != string(msg.Data) {
			t.Errorf("Receive() = %v, want %v", string(recvData), string(msg.Data))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Receive() timeout")
	}
}

func TestNetwork_RequestResponse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	cfg := DefaultNetworkConfig("127.0.0.1:0")
	cfg.TransportConfig.Type = TransportTCP

	transport, err := NewTransport(cfg.TransportConfig)
	if err != nil {
		t.Fatalf("NewTransport() error = %v", err)
	}

	cfg.PoolConfig = DefaultPoolConfig(transport)
	cfg.ClientConfig = DefaultClientConfig(NewConnPool(cfg.PoolConfig))
	cfg.ServerConfig = DefaultServerConfig(transport)

	net, err := NewNetwork(cfg)
	if err != nil {
		t.Fatalf("NewNetwork() error = %v", err)
	}

	ctx := context.Background()
	if err := net.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() { _ = net.Stop(ctx) }()

	// register handler
	handler := func(ctx context.Context, remoteAddr string, data []byte) ([]byte, error) {
		return append([]byte("response: "), data...), nil
	}

	if err := net.RegisterHandler(MessageTypeRequest, handler); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	serverAddr := net.Server().Address()

	// send request using Request (which encodes message internally)
	request := []byte("test request")
	timeout := 5 * time.Second
	response, err := net.Request(ctx, serverAddr, request, timeout)
	if err != nil {
		t.Fatalf("Request() error = %v", err)
	}

	expected := "response: test request"
	if string(response) != expected {
		t.Errorf("Request() = %v, want %v", string(response), expected)
	}
}

func TestNetwork_SendMessage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	cfg := DefaultNetworkConfig("127.0.0.1:0")
	cfg.TransportConfig.Type = TransportTCP

	transport, err := NewTransport(cfg.TransportConfig)
	if err != nil {
		t.Fatalf("NewTransport() error = %v", err)
	}

	cfg.PoolConfig = DefaultPoolConfig(transport)
	cfg.ClientConfig = DefaultClientConfig(NewConnPool(cfg.PoolConfig))
	cfg.ServerConfig = DefaultServerConfig(transport)

	net, err := NewNetwork(cfg)
	if err != nil {
		t.Fatalf("NewNetwork() error = %v", err)
	}

	ctx := context.Background()
	if err := net.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() { _ = net.Stop(ctx) }()

	received := make(chan []byte, 1)
	handler := func(ctx context.Context, remoteAddr string, data []byte) ([]byte, error) {
		received <- data
		return nil, nil
	}

	if err := net.RegisterHandler(MessageTypeOneWay, handler); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	serverAddr := net.Server().Address()

	msg := &Message{
		Type:      MessageTypeOneWay,
		ID:        123,
		Data:      []byte("test"),
		Timestamp: time.Now().UnixNano(),
	}

	if err := net.SendMessage(ctx, serverAddr, msg); err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}

	// wait for receive with timeout
	select {
	case recvData := <-received:
		if string(recvData) != string(msg.Data) {
			t.Errorf("Receive() = %v, want %v", string(recvData), string(msg.Data))
		}
	case <-time.After(2 * time.Second):
		t.Error("Receive() timeout - message not received")
	}
}

func TestNetwork_ClusterMethods(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	cfg := DefaultNetworkConfig("127.0.0.1:0")
	cfg.TransportConfig.Type = TransportTCP

	transport, err := NewTransport(cfg.TransportConfig)
	if err != nil {
		t.Fatalf("NewTransport() error = %v", err)
	}

	cfg.PoolConfig = DefaultPoolConfig(transport)
	cfg.ClientConfig = DefaultClientConfig(NewConnPool(cfg.PoolConfig))
	cfg.ServerConfig = DefaultServerConfig(transport)

	net, err := NewNetwork(cfg)
	if err != nil {
		t.Fatalf("NewNetwork() error = %v", err)
	}

	ctx := context.Background()
	if err := net.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() { _ = net.Stop(ctx) }()

	// test SendFunc
	sendFunc := net.SendFunc()
	if sendFunc == nil {
		t.Fatal("SendFunc() returned nil")
	}

	// test SendBytesFunc
	sendBytesFunc := net.SendBytesFunc()
	if sendBytesFunc == nil {
		t.Fatal("SendBytesFunc() returned nil")
	}

	// test GetFunc
	getFunc := net.GetFunc()
	if getFunc == nil {
		t.Fatal("GetFunc() returned nil")
	}

	// test ReceiveFunc
	receiveFunc := net.ReceiveFunc()
	if receiveFunc == nil {
		t.Fatal("ReceiveFunc() returned nil")
	}
}
