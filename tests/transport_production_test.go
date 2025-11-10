package tests

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feellmoose/gridkv/internal/transport"
)

// Production-Grade Transport Tests
// 1. Race Detector Clean
// 2. Load Testing
// 3. Stability Testing

// TestTransport_RaceDetector tests all transports with race detector
// Run with: go test -race -run=TestTransport_RaceDetector
func TestTransport_RaceDetector(t *testing.T) {
	transports := []struct {
		name    string
		factory func() (transport.Transport, error)
	}{
		{"TCP", func() (transport.Transport, error) { return transport.NewTCPTransport(), nil }},
		{"gnet", func() (transport.Transport, error) { return transport.NewGnetTransport() }},
	}

	for _, tt := range transports {
		t.Run(tt.name, func(t *testing.T) {
			trans, err := tt.factory()
			if err != nil {
				t.Skipf("%s not available: %v", tt.name, err)
				return
			}

			testRaceConditions(t, trans, tt.name)
		})
	}
}

func testRaceConditions(t *testing.T, trans transport.Transport, name string) {
	addr := fmt.Sprintf("localhost:%d", 60000+time.Now().UnixNano()%1000)

	// Start listener
	listener, err := trans.Listen(addr)
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}

	received := atomic.Int64{}
	listener.HandleMessage(func(msg []byte) error {
		received.Add(1)
		// Simulate concurrent access
		_ = len(msg)
		return nil
	})

	if err := listener.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer listener.Stop()

	time.Sleep(200 * time.Millisecond)

	// Concurrent writers (race detector will catch issues)
	concurrency := 100
	opsPerGoroutine := 100
	message := []byte("test message for race detection")

	var wg sync.WaitGroup
	ctx := context.Background()

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			conn, err := trans.Dial(addr)
			if err != nil {
				t.Errorf("Dial failed: %v", err)
				return
			}
			defer conn.Close()

			for j := 0; j < opsPerGoroutine; j++ {
				if err := conn.WriteDataWithContext(ctx, message); err != nil {
					t.Errorf("Write failed: %v", err)
					return
				}
			}
		}()
	}

	wg.Wait()
	t.Logf("%s: Race test passed, received %d messages", name, received.Load())
}

// TestTransport_LoadTesting performs load testing on all transports
func TestTransport_LoadTesting(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping load test in short mode")
	}

	transports := []struct {
		name    string
		factory func() (transport.Transport, error)
	}{
		{"TCP", func() (transport.Transport, error) { return transport.NewTCPTransport(), nil }},
		{"gnet", func() (transport.Transport, error) { return transport.NewGnetTransport() }},
	}

	for _, tt := range transports {
		t.Run(tt.name, func(t *testing.T) {
			trans, err := tt.factory()
			if err != nil {
				t.Skipf("%s not available: %v", tt.name, err)
				return
			}

			testLoadCapacity(t, trans, tt.name)
		})
	}
}

func testLoadCapacity(t *testing.T, trans transport.Transport, name string) {
	addr := fmt.Sprintf("localhost:%d", 61000+time.Now().UnixNano()%1000)

	// Start listener
	listener, err := trans.Listen(addr)
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}

	received := atomic.Int64{}
	errors := atomic.Int64{}

	listener.HandleMessage(func(msg []byte) error {
		received.Add(1)
		return nil
	})

	if err := listener.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer listener.Stop()

	time.Sleep(200 * time.Millisecond)

	// Load parameters
	concurrency := runtime.NumCPU() * 4 // High concurrency
	opsPerGoroutine := 10000
	message := make([]byte, 1024)

	var wg sync.WaitGroup
	ctx := context.Background()

	start := time.Now()

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			conn, err := trans.Dial(addr)
			if err != nil {
				errors.Add(1)
				return
			}
			defer conn.Close()

			for j := 0; j < opsPerGoroutine; j++ {
				if err := conn.WriteDataWithContext(ctx, message); err != nil {
					errors.Add(1)
					return
				}
			}
		}()
	}

	wg.Wait()
	elapsed := time.Since(start)

	totalOps := int64(concurrency * opsPerGoroutine)
	opsPerSec := float64(totalOps) / elapsed.Seconds()

	t.Logf("%s Load Test Results:", name)
	t.Logf("  Concurrency: %d goroutines", concurrency)
	t.Logf("  Total ops: %d", totalOps)
	t.Logf("  Duration: %v", elapsed)
	t.Logf("  Throughput: %.2f M ops/sec", opsPerSec/1000000)
	t.Logf("  Errors: %d", errors.Load())
	t.Logf("  Success rate: %.2f%%", float64(totalOps-errors.Load())/float64(totalOps)*100)

	// Verify reasonable performance
	if opsPerSec < 100000 {
		t.Errorf("%s: Performance too low: %.0f ops/sec", name, opsPerSec)
	}

	errorRate := float64(errors.Load()) / float64(totalOps)
	if errorRate > 0.01 {
		t.Errorf("%s: Error rate too high: %.2f%%", name, errorRate*100)
	}
}

// TestTransport_Stability performs short stability test
func TestTransport_Stability(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stability test in short mode")
	}

	transports := []struct {
		name    string
		factory func() (transport.Transport, error)
	}{
		{"TCP", func() (transport.Transport, error) { return transport.NewTCPTransport(), nil }},
		{"gnet", func() (transport.Transport, error) { return transport.NewGnetTransport() }},
	}

	for _, tt := range transports {
		t.Run(tt.name, func(t *testing.T) {
			trans, err := tt.factory()
			if err != nil {
				t.Skipf("%s not available: %v", tt.name, err)
				return
			}

			testStability(t, trans, tt.name, 30*time.Second) // 30-second test
		})
	}
}

func testStability(t *testing.T, trans transport.Transport, name string, duration time.Duration) {
	addr := fmt.Sprintf("localhost:%d", 62000+time.Now().UnixNano()%1000)

	// Start listener
	listener, err := trans.Listen(addr)
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}

	received := atomic.Int64{}
	listener.HandleMessage(func(msg []byte) error {
		received.Add(1)
		return nil
	})

	if err := listener.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer listener.Stop()

	time.Sleep(200 * time.Millisecond)

	// Run continuous load for duration
	concurrency := runtime.NumCPU()
	message := make([]byte, 1024)
	ctx := context.Background()

	stopCh := make(chan struct{})
	var wg sync.WaitGroup
	totalSent := atomic.Int64{}
	totalErrors := atomic.Int64{}

	start := time.Now()

	// Workers
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			conn, err := trans.Dial(addr)
			if err != nil {
				totalErrors.Add(1)
				return
			}
			defer conn.Close()

			for {
				select {
				case <-stopCh:
					return
				default:
					if err := conn.WriteDataWithContext(ctx, message); err != nil {
						totalErrors.Add(1)
						return
					}
					totalSent.Add(1)
				}
			}
		}()
	}

	// Run for duration
	time.Sleep(duration)
	close(stopCh)
	wg.Wait()

	elapsed := time.Since(start)
	sent := totalSent.Load()
	errors := totalErrors.Load()
	opsPerSec := float64(sent) / elapsed.Seconds()

	t.Logf("%s Stability Test (%v):", name, duration)
	t.Logf("  Total sent: %d", sent)
	t.Logf("  Errors: %d", errors)
	t.Logf("  Throughput: %.2f K ops/sec", opsPerSec/1000)
	t.Logf("  Error rate: %.4f%%", float64(errors)/float64(sent+errors)*100)

	// Verify stability
	errorRate := float64(errors) / float64(sent+errors)
	if errorRate > 0.001 { // < 0.1% error rate
		t.Errorf("%s: Error rate too high for stability: %.4f%%", name, errorRate*100)
	}

	if sent == 0 {
		t.Errorf("%s: No messages sent during stability test", name)
	}
}
