package executor

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestQueuedExec_Basic(t *testing.T) {
	exec, err := NewQueued(QueuedOpts{
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

func TestQueuedExec_Priority(t *testing.T) {
	exec, err := NewQueued(QueuedOpts{
		Name:      "test",
		Workers:   2,
		QueueSize: 100,
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}
	defer func() { _ = exec.Stop(5 * time.Second) }()

	var completed atomic.Int64
	var wg sync.WaitGroup
	const numTasks = 50
	wg.Add(numTasks)

	for i := 0; i < numTasks; i++ {
		priority := PriorityNormal
		if i%3 == 0 {
			priority = PriorityHigh
		} else if i%3 == 1 {
			priority = PriorityLow
		}

		_ = exec.DoPriority(func() {
			completed.Add(1)
			wg.Done()
		}, priority)
	}

	wg.Wait()

	if completed.Load() != numTasks {
		t.Fatalf("Expected %d completed, got %d", numTasks, completed.Load())
	}
}

func TestQueuedExec_WorkStealing(t *testing.T) {
	exec, err := NewQueued(QueuedOpts{
		Name:      "test",
		Workers:   4,
		QueueSize: 100,
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}
	defer func() { _ = exec.Stop(5 * time.Second) }()

	var completed atomic.Int64
	const numTasks = 100
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

func TestQueuedExec_NonBlocking(t *testing.T) {
	exec, err := NewQueued(QueuedOpts{
		Name:        "test",
		Workers:     2,
		QueueSize:   2,
		NonBlocking: true,
	})
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}
	defer func() { _ = exec.Stop(5 * time.Second) }()

	for i := 0; i < 10; i++ {
		_ = exec.Do(func() {
			time.Sleep(100 * time.Millisecond)
		})
	}

	err = exec.Do(func() {})
	if err != ErrFull {
		t.Fatalf("Expected ErrFull, got %v", err)
	}
}

func TestQueuedExec_DoCtx(t *testing.T) {
	exec, err := NewQueued(QueuedOpts{
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

func TestQueuedExec_Resize(t *testing.T) {
	exec, err := NewQueued(QueuedOpts{
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

	err = exec.Resize(8)
	if err != nil {
		t.Fatalf("Failed to resize: %v", err)
	}

	stats = exec.Stats()
	if stats.Cap != 8 {
		t.Fatalf("Expected cap 8, got %d", stats.Cap)
	}
}

func TestQueuedExec_Concurrent(t *testing.T) {
	exec, err := NewQueued(QueuedOpts{
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
