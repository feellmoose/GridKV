package network

import (
	"context"
	"testing"
	"time"
)

func TestNetworkClient_Send(t *testing.T) {
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)

	poolCfg := DefaultPoolConfig(transport)
	pool := NewConnPool(poolCfg)

	clientCfg := DefaultClientConfig(pool)
	client := NewClient(clientCfg)
	defer client.Close()

	ctx := context.Background()
	addr := "127.0.0.1:0"

	ln, err := transport.Listen(ctx, addr)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()

	serverAddr := ln.Address()

	// accept in background
	recvCh := make(chan []byte, 1)
	go func() {
		conn, _ := ln.Accept(ctx)
		if conn != nil {
			data, _ := conn.Receive(ctx)
			recvCh <- data
			conn.Close()
		}
	}()

	// send
	sendData := []byte("test message")
	if err := client.Send(ctx, serverAddr, sendData); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	select {
	case recvData := <-recvCh:
		if string(recvData) != string(sendData) {
			t.Errorf("Receive() = %v, want %v", string(recvData), string(sendData))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Receive() timeout")
	}
}

func TestNetworkClient_SendWithTimeout(t *testing.T) {
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)

	poolCfg := DefaultPoolConfig(transport)
	pool := NewConnPool(poolCfg)

	clientCfg := DefaultClientConfig(pool)
	client := NewClient(clientCfg)
	defer client.Close()

	ctx := context.Background()
	addr := "127.0.0.1:0"

	ln, err := transport.Listen(ctx, addr)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()

	serverAddr := ln.Address()

	go func() {
		conn, _ := ln.Accept(ctx)
		if conn != nil {
			conn.Receive(ctx)
			conn.Close()
		}
	}()

	data := []byte("test")
	timeout := 2 * time.Second
	if err := client.SendWithTimeout(ctx, serverAddr, data, timeout); err != nil {
		t.Fatalf("SendWithTimeout() error = %v", err)
	}
}

func TestNetworkClient_Request(t *testing.T) {
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)

	poolCfg := DefaultPoolConfig(transport)
	pool := NewConnPool(poolCfg)

	clientCfg := DefaultClientConfig(pool)
	client := NewClient(clientCfg)
	defer client.Close()

	ctx := context.Background()
	addr := "127.0.0.1:0"

	ln, err := transport.Listen(ctx, addr)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()

	serverAddr := ln.Address()

	// echo server
	go func() {
		conn, _ := ln.Accept(ctx)
		if conn != nil {
			req, _ := conn.Receive(ctx)
			conn.Send(ctx, append([]byte("echo: "), req...))
			conn.Close()
		}
	}()

	request := []byte("test request")
	timeout := 5 * time.Second
	response, err := client.Request(ctx, serverAddr, request, timeout)
	if err != nil {
		t.Fatalf("Request() error = %v", err)
	}

	expected := "echo: test request"
	if string(response) != expected {
		t.Errorf("Request() = %v, want %v", string(response), expected)
	}
}

func TestNetworkClient_Broadcast(t *testing.T) {
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)

	poolCfg := DefaultPoolConfig(transport)
	pool := NewConnPool(poolCfg)

	clientCfg := DefaultClientConfig(pool)
	client := NewClient(clientCfg)
	defer client.Close()

	ctx := context.Background()

	// create multiple servers
	servers := make([]Listener, 3)
	addresses := make([]string, 3)
	for i := range servers {
		ln, err := transport.Listen(ctx, "127.0.0.1:0")
		if err != nil {
			t.Fatalf("Listen() error = %v", err)
		}
		servers[i] = ln
		addresses[i] = ln.Address()
		defer ln.Close()
	}

	// accept on all servers
	recvCh := make(chan []byte, len(servers))
	for _, ln := range servers {
		go func(l Listener) {
			conn, _ := l.Accept(ctx)
			if conn != nil {
				data, _ := conn.Receive(ctx)
				recvCh <- data
				conn.Close()
			}
		}(ln)
	}

	// broadcast
	data := []byte("broadcast message")
	if err := client.Broadcast(ctx, addresses, data); err != nil {
		t.Fatalf("Broadcast() error = %v", err)
	}

	// wait for all receives
	received := 0
	timeout := time.After(5 * time.Second)
	for received < len(servers) {
		select {
		case recvData := <-recvCh:
			if string(recvData) != string(data) {
				t.Errorf("Receive() = %v, want %v", string(recvData), string(data))
			}
			received++
		case <-timeout:
			t.Fatalf("Broadcast() only received %d/%d messages", received, len(servers))
		}
	}
}

func TestNetworkClient_RequestTimeout(t *testing.T) {
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)

	poolCfg := DefaultPoolConfig(transport)
	pool := NewConnPool(poolCfg)

	clientCfg := DefaultClientConfig(pool)
	client := NewClient(clientCfg)
	defer client.Close()

	ctx := context.Background()
	addr := "127.0.0.1:0"

	ln, err := transport.Listen(ctx, addr)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()

	serverAddr := ln.Address()

	// server that doesn't respond
	go func() {
		conn, _ := ln.Accept(ctx)
		if conn != nil {
			conn.Receive(ctx)
			// don't send response
			time.Sleep(2 * time.Second)
			conn.Close()
		}
	}()

	request := []byte("test")
	timeout := 100 * time.Millisecond
	_, err = client.Request(ctx, serverAddr, request, timeout)
	if err == nil {
		t.Error("Request() expected timeout error")
	}
}

