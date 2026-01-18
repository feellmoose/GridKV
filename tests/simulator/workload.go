// Package simulator provides workload execution for GridKV testing.
// This file contains the workload executor implementation.
package simulator

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// WorkloadConfig defines configuration for workload execution
type WorkloadConfig struct {
	WorkerCount  int           // Number of concurrent workers
	Duration     time.Duration // How long to run the workload
	WriteRatio   float64       // Ratio of write operations (0.0-1.0)
	ReadRatio    float64       // Ratio of read operations (0.0-1.0)
	KeySpaceSize int           // Size of key space for operations
	ValueSize    int           // Size of values in bytes
}

// DefaultWorkloadConfig returns default workload configuration
func DefaultWorkloadConfig() *WorkloadConfig {
	return &WorkloadConfig{
		WorkerCount:  10,
		Duration:     30 * time.Second,
		WriteRatio:   0.8,
		ReadRatio:    0.2,
		KeySpaceSize: 1000,
		ValueSize:    256,
	}
}

// WorkloadExecutor manages concurrent workload execution against a cluster
type WorkloadExecutor struct {
	config    *WorkloadConfig
	simulator *Simulator
	ctx       context.Context
	cancel    context.CancelFunc
	startTime time.Time

	// Statistics
	opsCompleted  int64
	opsFailed     int64
	setFailed     int64
	getFailed     int64
	timeoutFailed int64
	contextFailed int64

	// Track written keys for consistency checking
	writtenKeys map[string]bool
	keysMu      sync.RWMutex
}

// NewWorkloadExecutor creates a new workload executor
func NewWorkloadExecutor(config *WorkloadConfig, simulator *Simulator) *WorkloadExecutor {
	if config == nil {
		config = DefaultWorkloadConfig()
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &WorkloadExecutor{
		config:      config,
		simulator:   simulator,
		ctx:         ctx,
		cancel:      cancel,
		startTime:   time.Now(),
		writtenKeys: make(map[string]bool),
	}
}

// ExecuteWorkload runs the workload for the configured duration
func (we *WorkloadExecutor) ExecuteWorkload() error {
	// Start workers
	var wg sync.WaitGroup
	for i := 0; i < we.config.WorkerCount; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			we.runWorker(workerID)
		}(i)
	}

	// Wait for duration or context cancellation
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	// Create a timer for the workload duration
	timer := time.NewTimer(we.config.Duration)
	defer timer.Stop()

	select {
	case <-timer.C:
		// Duration expired
	case <-we.ctx.Done():
		// Context already cancelled
	case <-done:
		// All workers finished early - this shouldn't happen normally
		// but handle it gracefully
		timer.Stop()
		return nil
	}

	// Cancel context to stop any remaining workers
	we.cancel()

	// Wait for all workers to finish with adaptive timeout
	// For large workloads, workers may need more time to clean up
	shutdownTimeout := 15 * time.Second
	if we.config.WorkerCount > 50 {
		shutdownTimeout = 25 * time.Second
	}
	if we.config.WorkerCount > 100 {
		shutdownTimeout = 35 * time.Second
	}

	// Use timer for better timeout handling
	shutdownTimer := time.NewTimer(shutdownTimeout)
	defer shutdownTimer.Stop()

	select {
	case <-done:
		// All workers finished
		if !shutdownTimer.Stop() {
			<-shutdownTimer.C
		}
		// Give a delay for any pending operations and goroutines to complete
		// Longer wait helps prevent resource leaks between tests
		time.Sleep(300 * time.Millisecond)
	case <-shutdownTimer.C:
		// Workers didn't finish in time, but continue anyway
		// This prevents the test from hanging indefinitely
		// Still wait a bit for cleanup even on timeout
		time.Sleep(200 * time.Millisecond)
	}

	return nil
}

// runWorker executes operations for a single worker
func (we *WorkloadExecutor) runWorker(workerID int) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)))

	// Batch operations to reduce overhead
	batchSize := 10
	if we.config.WorkerCount > 50 {
		batchSize = 5 // Smaller batch for high concurrency
	}

	// Use ticker for periodic context checks instead of every operation
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	opsSinceCheck := 0
	for {
		// Check context periodically and after each batch
		if opsSinceCheck >= batchSize {
			select {
			case <-we.ctx.Done():
				return
			case <-ticker.C:
				// Periodic check
			default:
				// Continue
			}
			opsSinceCheck = 0
		}

		we.executeOperation(workerID, rng)
		opsSinceCheck++
	}
}

// executeOperation performs a single read or write operation with retry
func (we *WorkloadExecutor) executeOperation(workerID int, rng *rand.Rand) {
	// Check context at the start
	if we.ctx.Err() != nil {
		return
	}

	nodes := we.simulator.GetNodes()
	if len(nodes) == 0 {
		atomic.AddInt64(&we.opsFailed, 1)
		return
	}

	// Choose random node
	node := nodes[rng.Intn(len(nodes))]
	if node == nil {
		atomic.AddInt64(&we.opsFailed, 1)
		return
	}

	// Generate key - optimize string formatting
	keyID := rng.Intn(we.config.KeySpaceSize)
	// Pre-allocate key buffer to reduce allocations
	var key string
	if we.config.KeySpaceSize < 100000 {
		// For smaller key spaces, cache formatted keys (simple approach)
		key = fmt.Sprintf("w-%d-k-%d", workerID, keyID)
	} else {
		// For large key spaces, use direct formatting
		key = fmt.Sprintf("worker-%d-key-%d", workerID, keyID)
	}

	// Decide operation type
	isWrite := rng.Float64() < we.config.WriteRatio

	// Retry logic for transient failures with minimal backoff to maintain high throughput
	maxRetries := 1
	var err error
	var lastErr error

	for retry := 0; retry <= maxRetries; retry++ {
		// Check context before each retry
		if we.ctx.Err() != nil {
			atomic.AddInt64(&we.contextFailed, 1)
			return
		}

		if retry > 0 {
			// Exponential backoff for retries - very short to maintain throughput
			select {
			case <-we.ctx.Done():
				atomic.AddInt64(&we.contextFailed, 1)
				return
			case <-time.After(time.Duration(retry) * 200 * time.Microsecond):
				// Continue with retry
			}
		}

		if isWrite {
			// Optimize value generation - reuse pattern for better performance
			value := make([]byte, we.config.ValueSize)
			// Use fast random fill for better performance
			seed := rng.Int63()
			for i := range value {
				// Simple PRNG for faster generation
				seed = seed*1103515245 + 12345
				value[i] = byte(seed & 0xFF)
			}
			err = node.Set(we.ctx, key, value)
			if err == nil {
				atomic.AddInt64(&we.opsCompleted, 1)
				we.keysMu.Lock()
				we.writtenKeys[key] = true
				we.keysMu.Unlock()
				return
			}
			lastErr = err
			atomic.AddInt64(&we.setFailed, 1)
		} else {
			_, err = node.Get(we.ctx, key)
			if err == nil {
				// err == nil means operation succeeded (whether key was found or not)
				// Return nil, nil for not found is treated as success (not an error)
				atomic.AddInt64(&we.opsCompleted, 1)
				return
			}
			// err != nil means real error (network, timeout, etc.)
			lastErr = err
			atomic.AddInt64(&we.getFailed, 1)
			break
		}

		// If context cancelled, don't retry
		if we.ctx.Err() != nil {
			break
		}
	}

	if we.ctx.Err() != nil {
		atomic.AddInt64(&we.contextFailed, 1)
	} else if lastErr != nil {
		errStr := lastErr.Error()
		if strings.Contains(errStr, "timeout") || strings.Contains(errStr, "deadline") {
			atomic.AddInt64(&we.timeoutFailed, 1)
		}
		atomic.AddInt64(&we.opsFailed, 1)
	}
}

// GetStats returns current execution statistics
func (we *WorkloadExecutor) GetStats() (completed, failed int64, duration time.Duration, qps float64) {
	completed = atomic.LoadInt64(&we.opsCompleted)
	failed = atomic.LoadInt64(&we.opsFailed)
	duration = time.Since(we.startTime)
	if duration.Seconds() > 0 {
		qps = float64(completed) / duration.Seconds()
	}
	return
}

func (we *WorkloadExecutor) GetFailureStats() (setFailed, getFailed, timeoutFailed, contextFailed int64) {
	return atomic.LoadInt64(&we.setFailed),
		atomic.LoadInt64(&we.getFailed),
		atomic.LoadInt64(&we.timeoutFailed),
		atomic.LoadInt64(&we.contextFailed)
}

// Stop cancels the workload execution
func (we *WorkloadExecutor) Stop() {
	we.cancel()
}

// GetWrittenKeys returns a copy of all keys that were successfully written
func (we *WorkloadExecutor) GetWrittenKeys() []string {
	we.keysMu.RLock()
	defer we.keysMu.RUnlock()

	keys := make([]string, 0, len(we.writtenKeys))
	for key := range we.writtenKeys {
		keys = append(keys, key)
	}
	return keys
}
