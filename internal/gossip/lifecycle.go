package gossip

import (
	"sync"
	"sync/atomic"
	"time"
)

// LifecycleManager provides unified lifecycle management pattern.
// Encapsulates common stopCh + stopOnce + WaitGroup pattern.
type LifecycleManager struct {
	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
	stopped  atomic.Bool
}

// NewLifecycleManager creates a new lifecycle manager.
func NewLifecycleManager() *LifecycleManager {
	return &LifecycleManager{
		stopCh: make(chan struct{}),
	}
}

// StopCh returns the stop channel for goroutine control.
func (lm *LifecycleManager) StopCh() <-chan struct{} {
	return lm.stopCh
}

// IsStopped returns true if the lifecycle has been stopped.
func (lm *LifecycleManager) IsStopped() bool {
	return lm.stopped.Load()
}

// StartGoroutine starts a goroutine and tracks it with WaitGroup.
func (lm *LifecycleManager) StartGoroutine(fn func()) {
	lm.wg.Add(1)
	go func() {
		defer lm.wg.Done()
		fn()
	}()
}

// Stop stops the lifecycle manager and waits for all goroutines with timeout.
// Optimized implementation that minimizes goroutine creation.
func (lm *LifecycleManager) Stop(timeout time.Duration) {
	lm.stopOnce.Do(func() {
		lm.stopped.Store(true)
		close(lm.stopCh)
	})

	// Fast path: if timeout is 0, don't wait
	if timeout <= 0 {
		return
	}

	// Use a simple approach: always create one goroutine for waiting
	// This is cleaner and the overhead is minimal
	done := make(chan struct{})
	go func() {
		lm.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// All goroutines completed
	case <-time.After(timeout):
		// Timeout reached, continue anyway
	}
}

// StopNow stops without waiting (non-blocking).
func (lm *LifecycleManager) StopNow() {
	lm.stopOnce.Do(func() {
		lm.stopped.Store(true)
		close(lm.stopCh)
	})
}

