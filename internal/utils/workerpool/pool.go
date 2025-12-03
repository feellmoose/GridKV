package workerpool

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrPoolClosed indicates the pool has been released.
	ErrPoolClosed = errors.New("worker pool closed")
	// ErrPoolFull indicates the pool cannot accept more tasks in non-blocking mode.
	ErrPoolFull = errors.New("worker pool queue full")
)

// Options configures a worker pool instance.
type Options struct {
	// Name is used for diagnostics/logging.
	Name string
	// MaxWorkers controls the maximum number of concurrent workers.
	MaxWorkers int
	// QueueSize bounds pending tasks when all workers are busy. Defaults to MaxWorkers.
	QueueSize int
	// NonBlocking controls whether Submit should drop when the queue is full.
	NonBlocking bool
	// PanicHandler handles panics inside worker goroutines.
	PanicHandler func(interface{})
	// DisableStats skips atomic counters for submitted/completed/dropped to minimize overhead.
	// When disabled, Stats() still reports capacity/running/queue length but counters remain zero.
	DisableStats bool
}

// Stats exposes runtime statistics for observability.
type Stats struct {
	Capacity  int
	Running   int
	QueueLen  int
	Submitted uint64
	Completed uint64
	Dropped   uint64
}

// Pool is a goroutine-safe worker pool with dynamic resizing and bounded queues.
type Pool struct {
	opts Options
	// trackStats caches whether we should maintain atomic counters.
	trackStats bool

	taskCh chan func()
	stopCh chan struct{}

	workerCap atomic.Int32
	running   atomic.Int32
	closed    atomic.Bool

	submitted atomic.Uint64
	completed atomic.Uint64
	dropped   atomic.Uint64

	wg sync.WaitGroup
	mu sync.Mutex
}

// New constructs a worker pool with the provided options.
func New(opts Options) (*Pool, error) {
	if opts.MaxWorkers <= 0 {
		opts.MaxWorkers = runtime.NumCPU()
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = opts.MaxWorkers
	}
	if opts.PanicHandler == nil {
		opts.PanicHandler = func(p interface{}) {
			fmt.Printf("workerpool[%s] panic: %v\n", opts.Name, p)
		}
	}
	trackStats := !opts.DisableStats

	p := &Pool{
		opts:       opts,
		trackStats: trackStats,
		taskCh:     make(chan func(), opts.QueueSize),
		stopCh:     make(chan struct{}),
	}
	p.workerCap.Store(int32(opts.MaxWorkers))

	for i := 0; i < opts.MaxWorkers; i++ {
		p.startWorker()
	}

	return p, nil
}

// Submit schedules a task for execution.
func (p *Pool) Submit(task func()) error {
	if task == nil {
		return errors.New("nil task submitted")
	}
	if p.closed.Load() {
		return ErrPoolClosed
	}

	if p.trackStats {
		p.submitted.Add(1)
	}

	if p.opts.NonBlocking {
		select {
		case p.taskCh <- task:
			return nil
		default:
			if p.trackStats {
				p.dropped.Add(1)
			}
			return ErrPoolFull
		}
	}

	select {
	case p.taskCh <- task:
		return nil
	default:
	}

	select {
	case p.taskCh <- task:
		return nil
	case <-p.stopCh:
		return ErrPoolClosed
	}
}

// Resize changes the maximum worker count at runtime.
func (p *Pool) Resize(newSize int) error {
	if newSize <= 0 {
		return fmt.Errorf("invalid pool size %d", newSize)
	}
	if p.closed.Load() {
		return ErrPoolClosed
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	current := int(p.workerCap.Load())
	if newSize == current {
		return nil
	}

	if newSize > current {
		// Scale up: create new workers
		diff := newSize - current
		for i := 0; i < diff; i++ {
			p.startWorker()
		}
		p.workerCap.Store(int32(newSize))
		return nil
	}

	// Scale down: just update capacity, workers will exit naturally when idle
	// Workers check capacity and exit if running > capacity
	p.workerCap.Store(int32(newSize))
	
	// Note: Workers will self-terminate when they finish their current task
	// and see that running > workerCap. This is a lazy scale-down approach
	// that avoids the complexity of scaleDownCh and prevents goroutine leaks.
	return nil
}

// Release gracefully shuts down the pool.
func (p *Pool) Release() {
	if !p.closed.CompareAndSwap(false, true) {
		return
	}

	// Step 1: Close stopCh to signal all workers to exit
	close(p.stopCh)
	
	// Step 2: Close taskCh to wake up all workers blocked on task reception
	// This ensures all workers will hit either taskCh or stopCh case and exit
	close(p.taskCh)
	
	// Step 3: Wait for all workers to exit with timeout
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	
	timeout := 10 * time.Second
	select {
	case <-done:
		// All workers exited cleanly
	case <-time.After(timeout):
		// Timeout - log warning with details
		remaining := p.running.Load()
		capacity := p.workerCap.Load()
		fmt.Printf("workerpool[%s] WARNING: %d/%d workers still running after %v\n", 
			p.opts.Name, remaining, capacity, timeout)
	}
}

// Stats returns current pool statistics.
func (p *Pool) Stats() Stats {
	stats := Stats{
		Capacity: int(p.workerCap.Load()),
		Running:  int(p.running.Load()),
		QueueLen: len(p.taskCh),
	}
	if p.trackStats {
		stats.Submitted = p.submitted.Load()
		stats.Completed = p.completed.Load()
		stats.Dropped = p.dropped.Load()
	}
	return stats
}

func (p *Pool) startWorker() {
	p.wg.Add(1)
	p.running.Add(1)

	go func() {
		defer p.wg.Done()
		defer p.running.Add(-1)

		for {
			// Check if we should exit due to scale-down (lazy approach)
			// This runs after each task, allowing natural termination
			running := p.running.Load()
			capacity := p.workerCap.Load()
			if running > capacity && capacity > 0 {
				// We have too many workers, this one should exit
				// Only exit if we're genuinely over capacity (not during shutdown)
				if !p.closed.Load() {
					return
				}
			}

			select {
			case task, ok := <-p.taskCh:
				if !ok {
					// taskCh closed, pool is shutting down
					return
				}
				p.runTask(task)
			case <-p.stopCh:
				// stopCh closed, pool is shutting down
				return
			}
		}
	}()
}

func (p *Pool) runTask(task func()) {
	if task == nil {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			p.opts.PanicHandler(r)
		}
	}()

	task()
	if p.trackStats {
		p.completed.Add(1)
	}
}
