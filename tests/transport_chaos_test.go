package tests

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feellmoose/gridkv/internal/transport"
)

// Chaos Testing: Inject failures and verify recovery
// Tests fault tolerance under adverse conditions

// TestTransport_ChaosInjection performs chaos testing with random failures
func TestTransport_ChaosInjection(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping chaos test in short mode")
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

			testChaosScenarios(t, trans, tt.name)
		})
	}
}

func testChaosScenarios(t *testing.T, trans transport.Transport, name string) {
	addr := fmt.Sprintf("localhost:%d", 63000+time.Now().UnixNano()%1000)

	// Start listener
	listener, err := trans.Listen(addr)
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}

	received := atomic.Int64{}
	handlerErrors := atomic.Int64{}

	// Handler that randomly fails
	listener.HandleMessage(func(msg []byte) error {
		received.Add(1)

		// 5% random failure to test error handling
		if rand.Float64() < 0.05 {
			handlerErrors.Add(1)
			return fmt.Errorf("simulated handler error")
		}

		return nil
	})

	if err := listener.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer listener.Stop()

	time.Sleep(200 * time.Millisecond)

	// Chaos scenarios
	concurrency := 50
	duration := 10 * time.Second
	message := make([]byte, 1024)

	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	sent := atomic.Int64{}
	dialErrors := atomic.Int64{}
	writeErrors := atomic.Int64{}

	start := time.Now()

	// Workers with random connection drops
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			for {
				select {
				case <-stopCh:
					return
				default:
				}

				// Create connection
				conn, err := trans.Dial(addr)
				if err != nil {
					dialErrors.Add(1)
					time.Sleep(10 * time.Millisecond)
					continue
				}

				// Random short-lived connections (chaos!)
				opsThisConn := 10 + rand.Intn(90) // 10-100 ops per conn
				ctx := context.Background()

				for j := 0; j < opsThisConn; j++ {
					select {
					case <-stopCh:
						conn.Close()
						return
					default:
					}

					if err := conn.WriteDataWithContext(ctx, message); err != nil {
						writeErrors.Add(1)
						break // Connection failed, create new one
					}
					sent.Add(1)

					// Random delay (0-10ms)
					if rand.Float64() < 0.1 {
						time.Sleep(time.Duration(rand.Intn(10)) * time.Millisecond)
					}
				}

				conn.Close()

				// Random delay between connections
				time.Sleep(time.Duration(rand.Intn(5)) * time.Millisecond)
			}
		}(i)
	}

	// Run for duration
	time.Sleep(duration)
	close(stopCh)
	wg.Wait()

	elapsed := time.Since(start)

	t.Logf("%s Chaos Test Results (%v):", name, duration)
	t.Logf("  Messages sent: %d", sent.Load())
	t.Logf("  Messages received: %d", received.Load())
	t.Logf("  Dial errors: %d", dialErrors.Load())
	t.Logf("  Write errors: %d", writeErrors.Load())
	t.Logf("  Handler errors: %d", handlerErrors.Load())
	t.Logf("  Throughput: %.2f K ops/sec", float64(sent.Load())/elapsed.Seconds()/1000)

	// Verify fault tolerance
	totalErrors := dialErrors.Load() + writeErrors.Load()
	errorRate := float64(totalErrors) / float64(sent.Load()+totalErrors)

	t.Logf("  Error rate: %.2f%%", errorRate*100)

	if sent.Load() == 0 {
		t.Errorf("%s: No messages sent during chaos test", name)
	}

	// Allow up to 10% errors in chaos scenario
	if errorRate > 0.10 {
		t.Errorf("%s: Error rate too high under chaos: %.2f%%", name, errorRate*100)
	}
}

// TestTransport_ConnectionChurn tests rapid connection create/destroy
func TestTransport_ConnectionChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping churn test in short mode")
	}

	transports := []struct {
		name    string
		factory func() (transport.Transport, error)
	}{
		{"TCP", func() (transport.Transport, error) { return transport.NewTCPTransport(), nil }},
	}

	for _, tt := range transports {
		t.Run(tt.name, func(t *testing.T) {
			trans, err := tt.factory()
			if err != nil {
				t.Skipf("%s not available: %v", tt.name, err)
				return
			}

			testConnectionChurn(t, trans, tt.name)
		})
	}
}

func testConnectionChurn(t *testing.T, trans transport.Transport, name string) {
	addr := fmt.Sprintf("localhost:%d", 64000+time.Now().UnixNano()%1000)

	// Start listener
	listener, err := trans.Listen(addr)
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}

	listener.HandleMessage(func(msg []byte) error {
		return nil
	})

	if err := listener.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer listener.Stop()

	time.Sleep(200 * time.Millisecond)

	// Rapid connection churn
	iterations := 1000
	concurrency := 10
	message := []byte("churn test")
	ctx := context.Background()

	var wg sync.WaitGroup
	errors := atomic.Int64{}

	start := time.Now()

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for j := 0; j < iterations/concurrency; j++ {
				// Create connection
				conn, err := trans.Dial(addr)
				if err != nil {
					errors.Add(1)
					continue
				}

				// Send one message
				if err := conn.WriteDataWithContext(ctx, message); err != nil {
					errors.Add(1)
				}

				// Immediately close (high churn!)
				conn.Close()
			}
		}()
	}

	wg.Wait()
	elapsed := time.Since(start)

	connsPerSec := float64(iterations) / elapsed.Seconds()

	t.Logf("%s Connection Churn Test:", name)
	t.Logf("  Connections: %d", iterations)
	t.Logf("  Duration: %v", elapsed)
	t.Logf("  Conn/sec: %.0f", connsPerSec)
	t.Logf("  Errors: %d", errors.Load())
	t.Logf("  Success rate: %.2f%%", float64(iterations-int(errors.Load()))/float64(iterations)*100)

	// Verify churn handling
	errorRate := float64(errors.Load()) / float64(iterations)
	if errorRate > 0.05 { // Allow 5% errors in high churn
		t.Errorf("%s: Too many errors during churn: %.2f%%", name, errorRate*100)
	}
}

// BenchmarkTransport_Production benchmarks all transports under realistic load
func BenchmarkTransport_Production(b *testing.B) {
	transports := []struct {
		name    string
		factory func() (transport.Transport, error)
	}{
		{"TCP", func() (transport.Transport, error) { return transport.NewTCPTransport(), nil }},
		{"gnet", func() (transport.Transport, error) { return transport.NewGnetTransport() }},
	}

	for _, tt := range transports {
		b.Run(tt.name, func(b *testing.B) {
			trans, err := tt.factory()
			if err != nil {
				b.Skipf("%s not available: %v", tt.name, err)
				return
			}

			benchmarkProductionLoad(b, trans, tt.name)
		})
	}
}

func benchmarkProductionLoad(b *testing.B, trans transport.Transport, name string) {
	addr := fmt.Sprintf("localhost:%d", 65000+b.N%1000)

	// Start listener
	listener, err := trans.Listen(addr)
	if err != nil {
		b.Fatalf("Listen failed: %v", err)
	}

	listener.HandleMessage(func(msg []byte) error {
		return nil
	})

	if err := listener.Start(); err != nil {
		b.Fatalf("Start failed: %v", err)
	}
	defer listener.Stop()

	time.Sleep(200 * time.Millisecond)

	message := make([]byte, 1024)
	ctx := context.Background()

	b.ResetTimer()
	b.SetBytes(1024)

	b.RunParallel(func(pb *testing.PB) {
		conn, err := trans.Dial(addr)
		if err != nil {
			b.Errorf("Dial failed: %v", err)
			return
		}
		defer conn.Close()

		for pb.Next() {
			if err := conn.WriteDataWithContext(ctx, message); err != nil {
				b.Errorf("Write failed: %v", err)
				return
			}
		}
	})

	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "ops/sec")
}
