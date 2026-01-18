package network

import (
	"context"
	"testing"
	"time"
)

// BenchmarkPoolMetrics_Baseline collects baseline metrics for connection pool
func BenchmarkPoolMetrics_Baseline(b *testing.B) {
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP

	transport, err := NewTransport(cfg)
	if err != nil {
		b.Fatalf("NewTransport: %v", err)
	}
	defer transport.Close()

	poolCfg := DefaultPoolConfig(transport)
	poolCfg.MaxIdle = 100
	poolCfg.MaxActive = 500
	pool := NewConnPool(poolCfg).(*connPool)
	defer pool.Close()

	ctx := context.Background()
	addr := "127.0.0.1:0"

	// Start a simple server
	ln, err := transport.Listen(ctx, addr)
	if err != nil {
		b.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	serverAddr := ln.Address()

	b.ResetTimer()
	b.ReportAllocs()

	b.Run("GetPut", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				conn, err := pool.Get(ctx, serverAddr)
				if err != nil {
					b.Fatalf("Get: %v", err)
				}
				time.Sleep(10 * time.Microsecond) // Simulate work
				pool.Put(conn)
			}
		})
	})

	// Report baseline metrics
	stats := pool.Stats()
	metrics := pool.Metrics()

	b.Logf("=== BASELINE METRICS ===")
	b.Logf("Pool Stats: Total=%d, Active=%d, Idle=%d, Waiters=%d, Created=%d, Closed=%d, Errors=%d",
		stats.Total, stats.Active, stats.Idle, stats.Waiters, stats.Created, stats.Closed, stats.Errors)
	b.Logf("Wait Metrics: AvgWait=%v, MaxWait=%v, Samples=%d",
		metrics.AvgWaitTime, metrics.MaxWaitTime, metrics.WaitSamples)
	b.Logf("Hold Metrics: AvgHold=%v, Samples=%d",
		metrics.AvgHoldTime, metrics.HoldSamples)
	b.Logf("Request Rate: %.2f req/s", metrics.RequestRate)
}

// BenchmarkPoolMetrics_HighLoad tests pool under high load
func BenchmarkPoolMetrics_HighLoad(b *testing.B) {
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP

	transport, err := NewTransport(cfg)
	if err != nil {
		b.Fatalf("NewTransport: %v", err)
	}
	defer transport.Close()

	poolCfg := DefaultPoolConfig(transport)
	poolCfg.MaxIdle = 50
	poolCfg.MaxActive = 200 // Smaller pool to create contention
	pool := NewConnPool(poolCfg).(*connPool)
	defer pool.Close()

	ctx := context.Background()
	addr := "127.0.0.1:0"

	ln, err := transport.Listen(ctx, addr)
	if err != nil {
		b.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	serverAddr := ln.Address()

	b.ResetTimer()
	b.ReportAllocs()

	b.Run("HighLoad", func(b *testing.B) {
		b.SetParallelism(100) // High parallelism
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				conn, err := pool.Get(ctx, serverAddr)
				if err != nil {
					// Accept some errors under high load
					continue
				}
				time.Sleep(100 * time.Microsecond) // Simulate work
				pool.Put(conn)
			}
		})
	})

	stats := pool.Stats()
	metrics := pool.Metrics()

	b.Logf("=== HIGH LOAD METRICS ===")
	b.Logf("Pool Stats: Total=%d, Active=%d, Idle=%d, Waiters=%d, Errors=%d",
		stats.Total, stats.Active, stats.Idle, stats.Waiters, stats.Errors)
	b.Logf("Wait Metrics: AvgWait=%v, MaxWait=%v, Samples=%d",
		metrics.AvgWaitTime, metrics.MaxWaitTime, metrics.WaitSamples)
	b.Logf("Request Rate: %.2f req/s", metrics.RequestRate)
	b.Logf("Utilization: %.2f%%", float64(stats.Active)/float64(poolCfg.MaxActive)*100)
}

// BenchmarkPoolMetrics_Adaptive tests adaptive sizing
func BenchmarkPoolMetrics_Adaptive(b *testing.B) {
	cfg := DefaultTransportConfig()
	cfg.Type = TransportTCP

	transport, err := NewTransport(cfg)
	if err != nil {
		b.Fatalf("NewTransport: %v", err)
	}
	defer transport.Close()

	poolCfg := DefaultPoolConfig(transport)
	poolCfg.MaxIdle = 50
	poolCfg.MaxActive = 200
	pool := NewConnPool(poolCfg).(*connPool)
	defer pool.Close()

	adaptiveCfg := DefaultAdaptive()
	adaptiveCfg.MinSize = 100
	adaptiveCfg.MaxSize = 1000
	adaptiveCfg.InitialSize = 200
	pool.EnableAdaptive(adaptiveCfg)

	ctx := context.Background()
	addr := "127.0.0.1:0"

	ln, err := transport.Listen(ctx, addr)
	if err != nil {
		b.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	serverAddr := ln.Address()

	b.ResetTimer()
	b.ReportAllocs()

	b.Run("Adaptive", func(b *testing.B) {
		b.SetParallelism(50)
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				conn, err := pool.Get(ctx, serverAddr)
				if err != nil {
					continue
				}
				time.Sleep(50 * time.Microsecond)
				pool.Put(conn)
			}
		})
	})

	// Wait for adjustment
	time.Sleep(6 * time.Second)

	stats := pool.Stats()
	metrics := pool.Metrics()
	currentSize := pool.adaptive.size()

	b.Logf("=== ADAPTIVE METRICS ===")
	b.Logf("Pool Stats: Total=%d, Active=%d, Idle=%d, Waiters=%d",
		stats.Total, stats.Active, stats.Idle, stats.Waiters)
	b.Logf("Adaptive Size: Initial=%d, Current=%d, Min=%d, Max=%d",
		adaptiveCfg.InitialSize, currentSize, adaptiveCfg.MinSize, adaptiveCfg.MaxSize)
	b.Logf("Wait Metrics: AvgWait=%v, MaxWait=%v, Samples=%d",
		metrics.AvgWaitTime, metrics.MaxWaitTime, metrics.WaitSamples)
	b.Logf("Request Rate: %.2f req/s", metrics.RequestRate)
	b.Logf("Utilization: %.2f%%", float64(stats.Active)/float64(currentSize)*100)
}
