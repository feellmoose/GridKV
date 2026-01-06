package executor

// Executor pool
// Features:
//   - Minimal allocations
//   - Lock-free stats (when disabled)
//   - Efficient task scheduling
//   - Graceful shutdown with leak prevention
//   - Nil pointer protection

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feellmoose/gridkv/internal/utils/logging"
)

var (
	ErrClosed = errors.New("executor closed")
	ErrFull   = errors.New("queue full")
)

// Executor executes tasks
type Executor interface {
	Do(task func()) error
	DoCtx(ctx context.Context, task func()) error
	Wait() error
	Stop(timeout time.Duration) error
	Stats() Stats
}

// Opts configures executor
type Opts struct {
	Name        string
	Workers     int
	QueueSize   int
	NonBlocking bool
	OnPanic     func(interface{})
	NoStats     bool
}

// Stats exposes runtime metrics
type Stats struct {
	Cap     int
	Running int
	Queued  int
	Done    uint64
	Dropped uint64
}

// Exec is an executor
type Exec struct {
	opts       Opts
	trackStats bool

	tasks chan func()
	stop  chan struct{}

	cap     atomic.Int32
	running atomic.Int32
	closed  atomic.Bool

	done    atomic.Uint64
	dropped atomic.Uint64

	wg sync.WaitGroup
	mu sync.Mutex
}

// New creates an executor
func New(opts Opts) (*Exec, error) {
	if opts.Workers <= 0 {
		opts.Workers = runtime.NumCPU()
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = opts.Workers
	}
	if opts.OnPanic == nil {
		opts.OnPanic = func(p interface{}) {
			logging.Error(nil, "executor panic", "name", opts.Name, "panic", p)
		}
	}

	e := &Exec{
		opts:       opts,
		trackStats: !opts.NoStats,
		tasks:      make(chan func(), opts.QueueSize),
		stop:       make(chan struct{}),
	}
	e.cap.Store(int32(opts.Workers))

	for i := 0; i < opts.Workers; i++ {
		e.startWorker()
	}

	return e, nil
}

// Do schedules task
func (e *Exec) Do(task func()) error {
	if e == nil {
		return errors.New("nil executor")
	}
	if task == nil {
		return errors.New("nil task")
	}
	if e.closed.Load() {
		return ErrClosed
	}

	if e.trackStats {
		e.done.Add(1)
	}

	if e.opts.NonBlocking {
		select {
		case e.tasks <- task:
			return nil
		default:
			if e.trackStats {
				e.dropped.Add(1)
			}
			return ErrFull
		}
	}

	select {
	case e.tasks <- task:
		return nil
	case <-e.stop:
		return ErrClosed
	}
}

// DoCtx schedules task with context
func (e *Exec) DoCtx(ctx context.Context, task func()) error {
	if e == nil {
		return errors.New("nil executor")
	}
	if task == nil {
		return errors.New("nil task")
	}
	if ctx == nil {
		return errors.New("nil context")
	}
	if e.closed.Load() {
		return ErrClosed
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	if e.trackStats {
		e.done.Add(1)
	}

	wrapped := func() {
		if ctx != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
		if task != nil {
			task()
		}
	}

	if e.opts.NonBlocking {
		select {
		case e.tasks <- wrapped:
			return nil
		default:
			if e.trackStats {
				e.dropped.Add(1)
			}
			return ErrFull
		}
	}

	select {
	case e.tasks <- wrapped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stop:
		return ErrClosed
	}
}

// Resize changes worker count
func (e *Exec) Resize(n int) error {
	if e == nil {
		return errors.New("nil executor")
	}
	if n <= 0 {
		return fmt.Errorf("invalid size %d", n)
	}
	if e.closed.Load() {
		return ErrClosed
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	cur := int(e.cap.Load())
	if n == cur {
		return nil
	}

	if n > cur {
		for i := 0; i < n-cur; i++ {
			e.startWorker()
		}
		e.cap.Store(int32(n))
		return nil
	}

	e.cap.Store(int32(n))
	return nil
}

// Wait blocks until queue drains
func (e *Exec) Wait() error {
	if e == nil {
		return errors.New("nil executor")
	}
	if e.closed.Load() {
		return ErrClosed
	}

	timeout := time.After(30 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("wait timeout")
		case <-ticker.C:
			queued := len(e.tasks)
			running := e.running.Load()
			if queued == 0 && running == 0 {
				return nil
			}
		}
	}
}

// Stop gracefully stops executor
func (e *Exec) Stop(timeout time.Duration) error {
	if e == nil {
		return nil
	}
	if !e.closed.CompareAndSwap(false, true) {
		return nil
	}

	close(e.tasks)
	close(e.stop)

	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()

	if timeout <= 0 {
		timeout = 10 * time.Second
		cap := int(e.cap.Load())
		if cap > 100 {
			timeout += time.Duration(cap/100) * 100 * time.Millisecond
			if timeout > 30*time.Second {
				timeout = 30 * time.Second
			}
		}
	}

	select {
	case <-done:
		running := e.running.Load()
		if running != 0 {
			e.running.Store(0)
		}
		return nil
	case <-time.After(timeout):
		remaining := e.running.Load()
		logging.Warn("executor workers still running", "name", e.opts.Name, "workers", remaining)
		e.running.Store(0)
		return fmt.Errorf("stop timeout: %d workers", remaining)
	}
}

// Stats returns current statistics
func (e *Exec) Stats() Stats {
	if e == nil {
		return Stats{}
	}
	s := Stats{
		Cap:     int(e.cap.Load()),
		Running: int(e.running.Load()),
		Queued:  len(e.tasks),
	}
	if e.trackStats {
		s.Done = e.done.Load()
		s.Dropped = e.dropped.Load()
	}
	return s
}

func (e *Exec) startWorker() {
	if e == nil {
		return
	}

	e.wg.Add(1)
	e.running.Add(1)

	go func() {
		defer func() {
			e.wg.Done()
			e.running.Add(-1)
		}()

		for {
			running := e.running.Load()
			cap := e.cap.Load()
			if running > cap && cap > 0 && !e.closed.Load() {
				return
			}

			task, ok := <-e.tasks
			if !ok {
				return
			}
			if task != nil {
				e.run(task)
			}
		}
	}()
}

func (e *Exec) run(task func()) {
	if e == nil || task == nil {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			if e.opts.OnPanic != nil {
				e.opts.OnPanic(r)
			}
		}
	}()

	task()
}
