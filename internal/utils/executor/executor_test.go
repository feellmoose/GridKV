package executor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestExec_Basic(t *testing.T) {
	exec, err := New(Opts{
		Name:      "test",
		Workers:   4,
		QueueSize: 10,
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}
	defer func() { _ = exec.Stop(5 * time.Second) }()

	var wg sync.WaitGroup
	wg.Add(10)

	for i := 0; i < 10; i++ {
		err := exec.Do(func() {
			wg.Done()
		})
		if err != nil {
			t.Fatalf("Failed to submit: %v", err)
		}
	}

	wg.Wait()
}

func TestExec_NilTask(t *testing.T) {
	exec, err := New(Opts{
		Name:    "test",
		Workers: 4,
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}
	defer func() { _ = exec.Stop(5 * time.Second) }()

	err = exec.Do(nil)
	if err == nil {
		t.Fatal("Expected error for nil task")
	}
}

func TestExec_Closed(t *testing.T) {
	exec, err := New(Opts{
		Name:    "test",
		Workers: 4,
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}

	_ = exec.Stop(5 * time.Second)

	err = exec.Do(func() {})
	if err != ErrClosed {
		t.Fatalf("Expected ErrClosed, got %v", err)
	}
}

func TestExec_NonBlocking(t *testing.T) {
	exec, err := New(Opts{
		Name:        "test",
		Workers:     2,
		QueueSize:   2,
		NonBlocking: true,
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}
	defer func() { _ = exec.Stop(5 * time.Second) }()

	// Fill queue
	for i := 0; i < 4; i++ {
		_ = exec.Do(func() {
			time.Sleep(100 * time.Millisecond)
		})
	}

	// Next should fail
	err = exec.Do(func() {})
	if err != ErrFull {
		t.Fatalf("Expected ErrFull, got %v", err)
	}
}

func TestExec_Resize(t *testing.T) {
	exec, err := New(Opts{
		Name:    "test",
		Workers: 4,
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}
	defer func() { _ = exec.Stop(5 * time.Second) }()

	stats := exec.Stats()
	if stats.Cap != 4 {
		t.Fatalf("Expected cap 4, got %d", stats.Cap)
	}

	// Resize up
	err = exec.Resize(8)
	if err != nil {
		t.Fatalf("Failed to resize: %v", err)
	}

	stats = exec.Stats()
	if stats.Cap != 8 {
		t.Fatalf("Expected cap 8, got %d", stats.Cap)
	}

	// Resize down
	err = exec.Resize(2)
	if err != nil {
		t.Fatalf("Failed to resize: %v", err)
	}

	stats = exec.Stats()
	if stats.Cap != 2 {
		t.Fatalf("Expected cap 2, got %d", stats.Cap)
	}
}

func TestExec_ResizeInvalid(t *testing.T) {
	exec, err := New(Opts{
		Name:    "test",
		Workers: 4,
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}
	defer func() { _ = exec.Stop(5 * time.Second) }()

	err = exec.Resize(0)
	if err == nil {
		t.Fatal("Expected error for invalid resize")
	}

	err = exec.Resize(-1)
	if err == nil {
		t.Fatal("Expected error for invalid resize")
	}
}

func TestExec_ResizeClosed(t *testing.T) {
	exec, err := New(Opts{
		Name:    "test",
		Workers: 4,
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}

	_ = exec.Stop(5 * time.Second)

	err = exec.Resize(8)
	if err != ErrClosed {
		t.Fatalf("Expected ErrClosed, got %v", err)
	}
}

func TestExec_Stats(t *testing.T) {
	exec, err := New(Opts{
		Name:      "test",
		Workers:   4,
		QueueSize: 10,
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}
	defer func() { _ = exec.Stop(5 * time.Second) }()

	stats := exec.Stats()
	if stats.Cap != 4 {
		t.Fatalf("Expected cap 4, got %d", stats.Cap)
	}

	// Submit tasks
	for i := 0; i < 5; i++ {
		_ = exec.Do(func() {
			time.Sleep(10 * time.Millisecond)
		})
	}

	// Check stats
	stats = exec.Stats()
	if stats.Queued < 0 || stats.Queued > 5 {
		t.Fatalf("Unexpected queue length: %d", stats.Queued)
	}
}

func TestExec_StatsDisabled(t *testing.T) {
	exec, err := New(Opts{
		Name:    "test",
		Workers: 4,
		NoStats: true,
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}
	defer func() { _ = exec.Stop(5 * time.Second) }()

	// Submit tasks
	for i := 0; i < 10; i++ {
		_ = exec.Do(func() {})
	}

	time.Sleep(50 * time.Millisecond)

	stats := exec.Stats()
	if stats.Done != 0 || stats.Dropped != 0 {
		t.Fatalf("Expected zero stats, got done=%d dropped=%d", stats.Done, stats.Dropped)
	}
}

func TestExec_Concurrent(t *testing.T) {
	exec, err := New(Opts{
		Name:      "test",
		Workers:   10,
		QueueSize: 100,
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}
	defer func() { _ = exec.Stop(5 * time.Second) }()

	const numGoroutines = 50
	const tasksPerGoroutine = 100
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < tasksPerGoroutine; j++ {
				_ = exec.Do(func() {
					time.Sleep(1 * time.Millisecond)
				})
			}
		}()
	}

	wg.Wait()
	_ = exec.Wait()
}

func TestExec_OnPanic(t *testing.T) {
	panicCount := atomic.Int64{}
	exec, err := New(Opts{
		Name:    "test",
		Workers: 4,
		OnPanic: func(p interface{}) {
			panicCount.Add(1)
		},
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}
	defer func() { _ = exec.Stop(5 * time.Second) }()

	_ = exec.Do(func() {
		panic("test panic")
	})

	time.Sleep(50 * time.Millisecond)

	if panicCount.Load() != 1 {
		t.Fatalf("Expected 1 panic, got %d", panicCount.Load())
	}
}

func TestExec_DefaultOnPanic(t *testing.T) {
	exec, err := New(Opts{
		Name:    "test",
		Workers: 4,
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}
	defer func() { _ = exec.Stop(5 * time.Second) }()

	// Should not crash
	_ = exec.Do(func() {
		panic("test panic")
	})

	time.Sleep(50 * time.Millisecond)
}

func TestExec_DefaultWorkers(t *testing.T) {
	exec, err := New(Opts{
		Name: "test",
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}
	defer func() { _ = exec.Stop(5 * time.Second) }()

	stats := exec.Stats()
	if stats.Cap <= 0 {
		t.Fatalf("Expected positive cap, got %d", stats.Cap)
	}
}

func TestExec_DefaultQueueSize(t *testing.T) {
	exec, err := New(Opts{
		Name:    "test",
		Workers: 4,
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}
	defer func() { _ = exec.Stop(5 * time.Second) }()

	for i := 0; i < 10; i++ {
		_ = exec.Do(func() {})
	}
}

func TestExec_StopMultipleTimes(t *testing.T) {
	exec, err := New(Opts{
		Name:    "test",
		Workers: 4,
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}

	_ = exec.Stop(5 * time.Second)
	_ = exec.Stop(5 * time.Second)
	_ = exec.Stop(5 * time.Second)
}

func TestExec_StopTimeout(t *testing.T) {
	exec, err := New(Opts{
		Name:    "test",
		Workers: 4,
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}

	// Submit long-running task
	_ = exec.Do(func() {
		time.Sleep(200 * time.Millisecond)
	})

	// Stop with short timeout
	err = exec.Stop(50 * time.Millisecond)
	if err == nil {
		t.Fatal("Expected timeout error")
	}
}

func TestExec_StopWaitsForCompletion(t *testing.T) {
	exec, err := New(Opts{
		Name:    "test",
		Workers: 4,
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}

	var completed atomic.Int64
	_ = exec.Do(func() {
		time.Sleep(50 * time.Millisecond)
		completed.Add(1)
	})

	err = exec.Stop(5 * time.Second)
	if err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	if completed.Load() != 1 {
		t.Fatalf("Expected task to complete, got %d", completed.Load())
	}
}

func TestExec_DoCtx(t *testing.T) {
	exec, err := New(Opts{
		Name:    "test",
		Workers: 4,
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}
	defer func() { _ = exec.Stop(5 * time.Second) }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = exec.DoCtx(ctx, func() {})
	if err != context.Canceled {
		t.Fatalf("Expected context.Canceled, got %v", err)
	}
}

func TestExec_DoCtxCancellation(t *testing.T) {
	exec, err := New(Opts{
		Name:    "test",
		Workers: 4,
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}
	defer func() { _ = exec.Stop(5 * time.Second) }()

	ctx, cancel := context.WithCancel(context.Background())
	var executed atomic.Bool

	_ = exec.DoCtx(ctx, func() {
		executed.Store(true)
	})

	cancel()
	time.Sleep(10 * time.Millisecond)

	// Task may or may not execute depending on timing
	_ = executed.Load()
}

func TestExec_Stress(t *testing.T) {
	exec, err := New(Opts{
		Name:      "test",
		Workers:   20,
		QueueSize: 100,
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}
	defer func() { _ = exec.Stop(10 * time.Second) }()

	const numTasks = 10000
	var completed atomic.Int64
	var wg sync.WaitGroup
	wg.Add(numTasks)

	for i := 0; i < numTasks; i++ {
		_ = exec.Do(func() {
			completed.Add(1)
			wg.Done()
		})
	}

	wg.Wait()

	if completed.Load() != numTasks {
		t.Fatalf("Expected %d completed, got %d", numTasks, completed.Load())
	}
}

func TestExec_ResizeDuringWork(t *testing.T) {
	exec, err := New(Opts{
		Name:      "test",
		Workers:   4,
		QueueSize: 100,
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}
	defer func() { _ = exec.Stop(5 * time.Second) }()

	// Start long-running tasks
	for i := 0; i < 20; i++ {
		_ = exec.Do(func() {
			time.Sleep(100 * time.Millisecond)
		})
	}

	// Resize during work
	_ = exec.Resize(8)
	time.Sleep(50 * time.Millisecond)
	_ = exec.Resize(2)
	time.Sleep(50 * time.Millisecond)

	_ = exec.Wait()
}

func TestExec_StatsAccuracy(t *testing.T) {
	exec, err := New(Opts{
		Name:      "test",
		Workers:   4,
		QueueSize: 10,
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}
	defer func() { _ = exec.Stop(5 * time.Second) }()

	const numTasks = 100
	for i := 0; i < numTasks; i++ {
		_ = exec.Do(func() {})
	}

	_ = exec.Wait()
	time.Sleep(50 * time.Millisecond)

	stats := exec.Stats()
	if stats.Done != numTasks {
		t.Fatalf("Expected %d done, got %d", numTasks, stats.Done)
	}
	if stats.Dropped != 0 {
		t.Fatalf("Expected 0 dropped, got %d", stats.Dropped)
	}
}

func TestExec_LeakPrevention(t *testing.T) {
	// Test that stopping executor doesn't leak goroutines
	exec, err := New(Opts{
		Name:    "test",
		Workers: 10,
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}

	// Submit some tasks
	for i := 0; i < 100; i++ {
		_ = exec.Do(func() {
			time.Sleep(1 * time.Millisecond)
		})
	}

	// Stop executor
	err = exec.Stop(5 * time.Second)
	if err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	// Wait a bit to ensure all goroutines exit
	time.Sleep(100 * time.Millisecond)

	// Check stats - should be 0 running
	stats := exec.Stats()
	if stats.Running != 0 {
		t.Fatalf("Expected 0 running workers, got %d", stats.Running)
	}
}

func TestExec_ErrorHandling(t *testing.T) {
	exec, err := New(Opts{
		Name:    "test",
		Workers: 4,
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}
	defer func() { _ = exec.Stop(5 * time.Second) }()

	var taskErr error
	_ = exec.Do(func() {
		taskErr = errors.New("task error")
	})

	time.Sleep(50 * time.Millisecond)

	if taskErr == nil {
		t.Fatal("Expected task error")
	}
}
