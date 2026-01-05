package network

import (
	"context"
	"testing"
)

func TestConnPool_GetPut(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)

	poolCfg := DefaultPoolConfig(transport)
	poolCfg.MaxIdle = 5
	poolCfg.MaxActive = 10

	pool := NewConnPool(poolCfg)
	defer pool.Close()

	ctx := context.Background()
	addr := "127.0.0.1:0"

	ln, err := transport.Listen(ctx, addr)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()

	serverAddr := ln.Address()

	// get connection
	conn1, err := pool.Get(ctx, serverAddr)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	stats := pool.Stats()
	if stats.Created != 1 {
		t.Errorf("Stats().Created = %d, want 1", stats.Created)
	}
	if stats.Active != 1 {
		t.Errorf("Stats().Active = %d, want 1", stats.Active)
	}

	// put back
	pool.Put(conn1)

	stats = pool.Stats()
	if stats.Idle != 1 {
		t.Errorf("Stats().Idle = %d, want 1", stats.Idle)
	}
	if stats.Active != 0 {
		t.Errorf("Stats().Active = %d, want 0", stats.Active)
	}

	// get again (should reuse)
	conn2, err := pool.Get(ctx, serverAddr)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	stats = pool.Stats()
	if stats.Created != 1 {
		t.Errorf("Stats().Created = %d, want 1 (reused)", stats.Created)
	}
	if stats.Active != 1 {
		t.Errorf("Stats().Active = %d, want 1", stats.Active)
	}
	if stats.Idle != 0 {
		t.Errorf("Stats().Idle = %d, want 0", stats.Idle)
	}

	pool.Put(conn2)
}

func TestConnPool_MaxIdle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)

	poolCfg := DefaultPoolConfig(transport)
	poolCfg.MaxIdle = 2
	poolCfg.MaxActive = 10

	pool := NewConnPool(poolCfg)
	defer pool.Close()

	ctx := context.Background()
	addr := "127.0.0.1:0"

	ln, err := transport.Listen(ctx, addr)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()

	serverAddr := ln.Address()

	// create more than MaxIdle
	conns := make([]Conn, 5)
	for i := range conns {
		conn, err := pool.Get(ctx, serverAddr)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		conns[i] = conn
	}

	// put all back
	for _, conn := range conns {
		pool.Put(conn)
	}

	stats := pool.Stats()
	if int(stats.Idle) > poolCfg.MaxIdle {
		t.Errorf("Stats().Idle = %d, want <= %d", stats.Idle, poolCfg.MaxIdle)
	}
	if stats.Closed < 3 {
		t.Errorf("Stats().Closed = %d, want >= 3", stats.Closed)
	}
}

func TestConnPool_MaxActive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)

	poolCfg := DefaultPoolConfig(transport)
	poolCfg.MaxIdle = 10
	poolCfg.MaxActive = 2

	pool := NewConnPool(poolCfg)
	defer pool.Close()

	ctx := context.Background()
	addr := "127.0.0.1:0"

	ln, err := transport.Listen(ctx, addr)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()

	serverAddr := ln.Address()

	// get up to MaxActive
	conn1, err := pool.Get(ctx, serverAddr)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	defer pool.Put(conn1)

	conn2, err := pool.Get(ctx, serverAddr)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	defer pool.Put(conn2)

	// should fail
	_, err = pool.Get(ctx, serverAddr)
	if err != ErrPoolExhausted {
		t.Errorf("Get() error = %v, want %v", err, ErrPoolExhausted)
	}
}

func TestConnPool_Remove(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)

	poolCfg := DefaultPoolConfig(transport)
	pool := NewConnPool(poolCfg)
	defer pool.Close()

	ctx := context.Background()
	addr := "127.0.0.1:0"

	ln, err := transport.Listen(ctx, addr)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()

	serverAddr := ln.Address()

	conn, err := pool.Get(ctx, serverAddr)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	pool.Remove(conn)

	stats := pool.Stats()
	if stats.Closed != 1 {
		t.Errorf("Stats().Closed = %d, want 1", stats.Closed)
	}
}

func TestConnPool_Close(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP
	transport := NewTCPTransport(cfg)

	poolCfg := DefaultPoolConfig(transport)
	pool := NewConnPool(poolCfg)

	ctx := context.Background()
	addr := "127.0.0.1:0"

	ln, err := transport.Listen(ctx, addr)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()

	serverAddr := ln.Address()

	conn, err := pool.Get(ctx, serverAddr)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	pool.Put(conn)

	if err := pool.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}

	stats := pool.Stats()
	if stats.Total != 0 || stats.Active != 0 || stats.Idle != 0 {
		t.Errorf("Stats() after Close() = %+v, want all zeros", stats)
	}
}

