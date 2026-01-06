package executor

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

// Priority levels
const (
	PriorityLow = iota
	PriorityNormal
	PriorityHigh
)

// QueuedOpts configures queued executor
type QueuedOpts struct {
	Name        string
	Workers     int
	QueueSize   int
	NonBlocking bool
	OnPanic     func(interface{})
	NoStats     bool
}

// QueuedTask represents a task with priority
type QueuedTask struct {
	Task     func()
	Priority int
}

// QueuedExec is an executor with multiple queues and work stealing
type QueuedExec struct {
	opts       QueuedOpts
	trackStats bool

	queues []*taskQueue
	stop   chan struct{}

	cap     atomic.Int32
	running atomic.Int32
	closed  atomic.Bool

	done    atomic.Uint64
	dropped atomic.Uint64

	wg sync.WaitGroup
	mu sync.Mutex
}

type taskQueue struct {
	high   chan func()
	normal chan func()
	low    chan func()
	mu     sync.Mutex
}

func newTaskQueue(size int) *taskQueue {
	return &taskQueue{
		high:   make(chan func(), size),
		normal: make(chan func(), size),
		low:    make(chan func(), size),
	}
}

func (q *taskQueue) push(task func(), priority int) bool {
	var target chan func()
	switch priority {
	case PriorityHigh:
		target = q.high
	case PriorityNormal:
		target = q.normal
	default:
		target = q.low
	}

	select {
	case target <- task:
		return true
	default:
		return false
	}
}

func (q *taskQueue) pop() (func(), bool) {
	select {
	case task := <-q.high:
		return task, true
	default:
	}

	select {
	case task := <-q.normal:
		return task, true
	default:
	}

	select {
	case task := <-q.low:
		return task, true
	default:
		return nil, false
	}
}

func (q *taskQueue) steal() (func(), bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	select {
	case task := <-q.high:
		return task, true
	default:
	}

	select {
	case task := <-q.normal:
		return task, true
	default:
	}

	select {
	case task := <-q.low:
		return task, true
	default:
		return nil, false
	}
}

func (q *taskQueue) len() int {
	return len(q.high) + len(q.normal) + len(q.low)
}

// NewQueued creates a queued executor with work stealing
func NewQueued(opts QueuedOpts) (*QueuedExec, error) {
	if opts.Workers <= 0 {
		opts.Workers = runtime.NumCPU()
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = opts.Workers * 2
	}
	if opts.OnPanic == nil {
		opts.OnPanic = func(p interface{}) {
			logging.Error(nil, "queued_executor panic", "name", opts.Name, "panic", p)
		}
	}

	e := &QueuedExec{
		opts:       opts,
		trackStats: !opts.NoStats,
		queues:     make([]*taskQueue, opts.Workers),
		stop:       make(chan struct{}),
	}

	for i := 0; i < opts.Workers; i++ {
		e.queues[i] = newTaskQueue(opts.QueueSize)
	}

	e.cap.Store(int32(opts.Workers))

	for i := 0; i < opts.Workers; i++ {
		e.startWorker(i)
	}

	return e, nil
}

// Do schedules task with normal priority
func (e *QueuedExec) Do(task func()) error {
	return e.DoPriority(task, PriorityNormal)
}

// DoPriority schedules task with specified priority
func (e *QueuedExec) DoPriority(task func(), priority int) error {
	if e == nil {
		return errors.New("nil executor")
	}
	if task == nil {
		return errors.New("nil task")
	}
	if e.closed.Load() {
		return ErrClosed
	}

	if priority < PriorityLow {
		priority = PriorityLow
	}
	if priority > PriorityHigh {
		priority = PriorityHigh
	}

	if e.trackStats {
		e.done.Add(1)
	}

	workerID := int(time.Now().UnixNano()) % len(e.queues)
	queue := e.queues[workerID]

	if !queue.push(task, priority) {
		if e.opts.NonBlocking {
			if e.trackStats {
				e.dropped.Add(1)
			}
			return ErrFull
		}

		for i := 0; i < len(e.queues); i++ {
			idx := (workerID + i) % len(e.queues)
			if e.queues[idx].push(task, priority) {
				return nil
			}
		}

		select {
		case <-e.stop:
			return ErrClosed
		default:
			if e.trackStats {
				e.dropped.Add(1)
			}
			return ErrFull
		}
	}

	return nil
}

// DoCtx schedules task with context
func (e *QueuedExec) DoCtx(ctx context.Context, task func()) error {
	return e.DoCtxPriority(ctx, task, PriorityNormal)
}

// DoCtxPriority schedules task with context and priority
func (e *QueuedExec) DoCtxPriority(ctx context.Context, task func(), priority int) error {
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

	if priority < PriorityLow {
		priority = PriorityLow
	}
	if priority > PriorityHigh {
		priority = PriorityHigh
	}

	wrapped := func() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		task()
	}

	return e.DoPriority(wrapped, priority)
}

// Resize changes worker count
func (e *QueuedExec) Resize(n int) error {
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
		for i := cur; i < n; i++ {
			if i < len(e.queues) {
				e.startWorker(i)
			} else {
				queue := newTaskQueue(e.opts.QueueSize)
				e.queues = append(e.queues, queue)
				e.startWorker(i)
			}
		}
		e.cap.Store(int32(n))
		return nil
	}

	e.cap.Store(int32(n))
	return nil
}

// Wait blocks until queues drain
func (e *QueuedExec) Wait() error {
	if e == nil {
		return errors.New("nil executor")
	}
	if e.closed.Load() {
		return ErrClosed
	}

	timeout := time.After(30 * time.Second)
	ticker := time.NewTicker(1 * time.Millisecond)
	defer ticker.Stop()

	emptyCount := 0
	for {
		select {
		case <-timeout:
			return fmt.Errorf("wait timeout")
		case <-ticker.C:
			totalQueued := 0
			for _, q := range e.queues {
				totalQueued += q.len()
			}
			if totalQueued == 0 {
				emptyCount++
				if emptyCount >= 10 {
					return nil
				}
			} else {
				emptyCount = 0
			}
		}
	}
}

// Stop gracefully stops executor
func (e *QueuedExec) Stop(timeout time.Duration) error {
	if e == nil {
		return nil
	}
	if !e.closed.CompareAndSwap(false, true) {
		return nil
	}

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
		logging.Warn("queued_executor workers still running", "name", e.opts.Name, "workers", remaining)
		e.running.Store(0)
		return fmt.Errorf("stop timeout: %d workers", remaining)
	}
}

// Stats returns current statistics
func (e *QueuedExec) Stats() Stats {
	if e == nil {
		return Stats{}
	}
	totalQueued := 0
	for _, q := range e.queues {
		totalQueued += q.len()
	}
	s := Stats{
		Cap:     int(e.cap.Load()),
		Running: int(e.running.Load()),
		Queued:  totalQueued,
	}
	if e.trackStats {
		s.Done = e.done.Load()
		s.Dropped = e.dropped.Load()
	}
	return s
}

func (e *QueuedExec) startWorker(id int) {
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

		ticker := time.NewTicker(1 * time.Millisecond)
		defer ticker.Stop()

		for {
			running := e.running.Load()
			cap := e.cap.Load()
			if running > cap && cap > 0 && !e.closed.Load() {
				return
			}

			myQueue := e.queues[id]

			task, ok := myQueue.pop()
			if !ok {
				for i := 0; i < len(e.queues); i++ {
					stealID := (id + i + 1) % len(e.queues)
					if stealID == id {
						continue
					}
					task, ok = e.queues[stealID].steal()
					if ok {
						break
					}
				}
			}

			if ok && task != nil {
				e.run(task)
				continue
			}

			select {
			case <-e.stop:
				return
			case <-ticker.C:
				continue
			}
		}
	}()
}

func (e *QueuedExec) run(task func()) {
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
