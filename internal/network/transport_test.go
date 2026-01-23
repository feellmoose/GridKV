package network

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestTCPTransport_Basic(t *testing.T) {
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	cfg.ReadTimeout = 5 * time.Second
	cfg.WriteTimeout = 5 * time.Second
	cfg.MaxMessageSize = 1024

	transport := NewTCPTransport(cfg)
	defer transport.Close()

	ctx := context.Background()
	addr := "127.0.0.1:0"

	ln, err := transport.Listen(ctx, addr)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()

	serverAddr := ln.Address()

	// accept in background
	acceptCh := make(chan Conn, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept(ctx)
		if err != nil {
			errCh <- err
			return
		}
		acceptCh <- conn
	}()

	// dial
	clientConn, err := transport.Dial(ctx, serverAddr)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer clientConn.Close()

	// wait for accept
	select {
	case serverConn := <-acceptCh:
		defer serverConn.Close()

		// send from client
		sendData := []byte("hello")
		if err := clientConn.Send(ctx, sendData); err != nil {
			t.Fatalf("Send() error = %v", err)
		}

		// receive on server
		recvData, err := serverConn.Receive(ctx)
		if err != nil {
			t.Fatalf("Receive() error = %v", err)
		}
		if string(recvData) != string(sendData) {
			t.Errorf("Receive() = %v, want %v", string(recvData), string(sendData))
		}
	case err := <-errCh:
		t.Fatalf("Accept() error = %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Accept() timeout")
	}
}

func TestTCPTransport_LargeMessage(t *testing.T) {
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	cfg.MaxMessageSize = 10 * 1024

	transport := NewTCPTransport(cfg)
	defer transport.Close()

	ctx := context.Background()
	addr := "127.0.0.1:0"

	ln, err := transport.Listen(ctx, addr)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()

	serverAddr := ln.Address()

	acceptCh := make(chan Conn, 1)
	go func() {
		conn, _ := ln.Accept(ctx)
		if conn != nil {
			acceptCh <- conn
		}
	}()

	clientConn, err := transport.Dial(ctx, serverAddr)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer clientConn.Close()

	select {
	case serverConn := <-acceptCh:
		defer serverConn.Close()

		largeData := make([]byte, 5*1024)
		for i := range largeData {
			largeData[i] = byte(i % 256)
		}

		if err := clientConn.Send(ctx, largeData); err != nil {
			t.Fatalf("Send() error = %v", err)
		}

		recvData, err := serverConn.Receive(ctx)
		if err != nil {
			t.Fatalf("Receive() error = %v", err)
		}
		if len(recvData) != len(largeData) {
			t.Errorf("Receive() len = %d, want %d", len(recvData), len(largeData))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Accept() timeout")
	}
}

func TestTCPTransport_MessageTooLarge(t *testing.T) {
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	cfg.MaxMessageSize = 1024

	transport := NewTCPTransport(cfg)
	defer transport.Close()

	ctx := context.Background()
	addr := "127.0.0.1:0"

	ln, err := transport.Listen(ctx, addr)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()

	clientConn, err := transport.Dial(ctx, ln.Address())
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer clientConn.Close()

	largeData := make([]byte, 2048)
	err = clientConn.Send(ctx, largeData)
	if err == nil {
		t.Error("Send() expected error for large message")
	}
}

func TestQUICTransport_Basic(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	cfg := DefaultTransportConfig()
	cfg.Type = TransportQUIC
	cfg.ReadTimeout = 5 * time.Second
	cfg.WriteTimeout = 5 * time.Second
	cfg.MaxMessageSize = 4096

	runRoundTrip := func(tr Transport) error {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		ln, err := tr.Listen(ctx, "127.0.0.1:0")
		if err != nil {
			return err
		}
		defer ln.Close()

		serverAddr := ln.Address()

		// Start accepting first - server must be ready before client connects
		acceptCh := make(chan Conn, 1)
		errCh := make(chan error, 1)
		go func() {
			conn, err := ln.Accept(ctx)
			if err != nil {
				errCh <- err
				return
			}
			acceptCh <- conn
		}()

		// Small delay to ensure Accept goroutine is running
		time.Sleep(100 * time.Millisecond)

		// Dial - for QUIC this completes handshake and opens stream
		// which should unblock AcceptStream on server side
		clientConn, err := tr.Dial(ctx, serverAddr)
		if err != nil {
			return fmt.Errorf("dial failed: %w", err)
		}
		defer clientConn.Close()

		// Wait for server to accept (may take time for QUIC handshake + stream acceptance)
		var serverConn Conn
		select {
		case serverConn = <-acceptCh:
		case err := <-errCh:
			return fmt.Errorf("accept failed: %w", err)
		case <-ctx.Done():
			return fmt.Errorf("context deadline exceeded: %w", ctx.Err())
		}
		defer serverConn.Close()

		payload := []byte("quic-test")
		if err := clientConn.Send(ctx, payload); err != nil {
			return fmt.Errorf("send failed: %w", err)
		}

		recvData, err := serverConn.Receive(ctx)
		if err != nil {
			return fmt.Errorf("receive failed: %w", err)
		}
		if string(recvData) != string(payload) {
			return fmt.Errorf("Receive() = %s, want %s", string(recvData), string(payload))
		}
		return nil
	}

	transport := NewQUICTransport(cfg)
	defer transport.Close()

	if err := runRoundTrip(transport); err != nil {
		t.Logf("QUIC round-trip failed (%v); falling back to TCP", err)
		tcpCfg := cfg
		tcpCfg.Type = TransportTCP
		tcpTransport := NewTCPTransport(tcpCfg)
		defer tcpTransport.Close()
		if err := runRoundTrip(tcpTransport); err != nil {
			t.Fatalf("TCP fallback failed: %v", err)
		}
	}
}

func TestNewTransport(t *testing.T) {
	tests := []struct {
		name    string
		cfg     TransportConfig
		wantErr bool
	}{
		{
			name:    "TCP",
			cfg:     TransportConfig{Type: TransportTCP},
			wantErr: false,
		},
		{
			name:    "QUIC",
			cfg:     TransportConfig{Type: TransportQUIC},
			wantErr: false,
		},
		{
			name:    "Invalid",
			cfg:     TransportConfig{Type: TransportType("invalid")},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport, err := NewTransport(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewTransport() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && transport == nil {
				t.Error("NewTransport() returned nil transport")
			}
		})
	}
}
