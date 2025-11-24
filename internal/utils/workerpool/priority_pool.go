package workerpool

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// TaskPriority represents the priority level of a task
type TaskPriority int

const (
	PriorityLow TaskPriority = iota
	PriorityNormal
	PriorityHigh
	PriorityCritical
)

const priorityLevels = 4

// TaskFunc represents a cancellable task function
type TaskFunc func(ctx context.Context)

// TaskOptions configures task scheduling hints
type TaskOptions struct {
	Priority TaskPriority
	Context  context.Context
	NUMANode int // -1 => auto
}

// TaskHandle allows cancellation of scheduled tasks
type TaskHandle struct {
	task *taskWrapper
}

// Cancel cancels a scheduled task if it has not yet executed
func (h *TaskHandle) Cancel() {
	if h == nil || h.task == nil {
		return
	}
	if h.task.cancelled.CompareAndSwap(false, true) {
		h.task.finish()
	}
}

// Context returns the task's context for inspection
func (h *TaskHandle) Context() context.Context {
	if h == nil || h.task == nil {
		return context.Background()
	}
	return h.task.ctx
}

type taskWrapper struct {
	fn         TaskFunc
	ctx        context.Context
	cancel     context.CancelFunc
	cancelOnce sync.Once

	priority  TaskPriority
	numaNode  int
	cancelled atomic.Bool
}

func (tw *taskWrapper) finish() {
	tw.cancelOnce.Do(func() {
		if tw.cancel != nil {
			tw.cancel()
		}
	})
}

type taskQueue struct {
	mu  sync.Mutex
	buf []*taskWrapper
	max int
}

func newTaskQueue(capacity int) *taskQueue {
	return &taskQueue{
		buf: make([]*taskWrapper, 0, capacity),
		max: capacity,
	}
}

func (q *taskQueue) push(task *taskWrapper) (wasEmpty bool, pushed bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.buf) >= q.max {
		return false, false
	}
	wasEmpty = len(q.buf) == 0
	q.buf = append(q.buf, task)
	return wasEmpty, true
}

func (q *taskQueue) popTail() *taskWrapper {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.buf)
	if n == 0 {
		return nil
	}
	task := q.buf[n-1]
	q.buf[n-1] = nil
	q.buf = q.buf[:n-1]
	return task
}

func (q *taskQueue) popHead() *taskWrapper {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.buf) == 0 {
		return nil
	}
	task := q.buf[0]
	q.buf[0] = nil
	q.buf = q.buf[1:]
	return task
}

func (q *taskQueue) length() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.buf)
}

// PriorityPool wraps a Pool to provide advanced scheduling capabilities
type PriorityPool struct {
	basePool *Pool

	numDispatchers int
	numaNodes      int
	workStealing   bool

	queues [][]*taskQueue // [numa][priority]

	notifyCh chan struct{}
	stopCh   chan struct{}
	closed   atomic.Bool
	wg       sync.WaitGroup

	rrCounter atomic.Uint64

	submitted [priorityLevels]atomic.Uint64
	dropped   [priorityLevels]atomic.Uint64
}

// PriorityPoolOptions configures a priority pool
type PriorityPoolOptions struct {
	BasePool *Pool // Required

	// Queue sizes for each priority (defaults derived from BasePool capacity)
	CriticalQueueSize int
	HighQueueSize     int
	NormalQueueSize   int
	LowQueueSize      int

	// Advanced options
	Dispatchers        int
	NUMANodes          int
	EnableWorkStealing bool
}

// NewPriorityPool creates a priority-aware wrapper around a base pool
func NewPriorityPool(opts PriorityPoolOptions) (*PriorityPool, error) {
	if opts.BasePool == nil {
		return nil, errors.New("base pool is required")
	}

	baseStats := opts.BasePool.Stats()
	baseCapacity := baseStats.Capacity
	if baseCapacity <= 0 {
		baseCapacity = runtime.NumCPU()
	}

	criticalSize := opts.CriticalQueueSize
	if criticalSize <= 0 {
		criticalSize = baseCapacity / 2
		if criticalSize < 8 {
			criticalSize = 8
		}
	}
	highSize := opts.HighQueueSize
	if highSize <= 0 {
		highSize = baseCapacity / 4
		if highSize < 8 {
			highSize = 8
		}
	}
	normalSize := opts.NormalQueueSize
	if normalSize <= 0 {
		normalSize = baseCapacity / 4
		if normalSize < 8 {
			normalSize = 8
		}
	}
	lowSize := opts.LowQueueSize
	if lowSize <= 0 {
		lowSize = max(4, baseCapacity/8)
	}

	dispatchers := opts.Dispatchers
	if dispatchers <= 0 {
		dispatchers = baseCapacity
		if dispatchers < 4 {
			dispatchers = 4
		}
	}

	numaNodes := opts.NUMANodes
	if numaNodes <= 0 {
		numaNodes = runtime.NumCPU() / 4
		if numaNodes < 1 {
			numaNodes = 1
		}
	}

	pp := &PriorityPool{
		basePool:       opts.BasePool,
		numDispatchers: dispatchers,
		numaNodes:      numaNodes,
		workStealing:   true,
		notifyCh:       make(chan struct{}, dispatchers*2),
		stopCh:         make(chan struct{}),
	}
	if !opts.EnableWorkStealing {
		pp.workStealing = false
	}

	pp.queues = make([][]*taskQueue, pp.numaNodes)
	for node := 0; node < pp.numaNodes; node++ {
		pp.queues[node] = []*taskQueue{
			newTaskQueue(lowSize),
			newTaskQueue(normalSize),
			newTaskQueue(highSize),
			newTaskQueue(criticalSize),
		}
	}

	for i := 0; i < pp.numDispatchers; i++ {
		pp.wg.Add(1)
		go pp.dispatchLoop(i)
	}

	return pp, nil
}

// Submit submits a simple task at normal priority
func (pp *PriorityPool) Submit(task func()) error {
	if task == nil {
		return errors.New("nil task")
	}
	_, err := pp.SubmitTask(func(context.Context) { task() }, TaskOptions{
		Priority: PriorityNormal,
	})
	return err
}

// SubmitTask submits a task with advanced options and returns a cancellable handle
func (pp *PriorityPool) SubmitTask(fn TaskFunc, opts TaskOptions) (*TaskHandle, error) {
	if fn == nil {
		return nil, errors.New("nil task")
	}
	if pp.closed.Load() {
		return nil, ErrPoolClosed
	}

	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)

	priority := opts.Priority
	if priority < PriorityLow || priority > PriorityCritical {
		priority = PriorityNormal
	}

	numaNode := opts.NUMANode
	if numaNode < 0 {
		numaNode = int(pp.rrCounter.Add(1) % uint64(pp.numaNodes))
	} else if numaNode >= pp.numaNodes {
		numaNode = numaNode % pp.numaNodes
	}

	task := &taskWrapper{
		fn:       fn,
		ctx:      ctx,
		cancel:   cancel,
		priority: priority,
		numaNode: numaNode,
	}
	handle := &TaskHandle{task: task}

	queue := pp.queues[numaNode][priority]
	wasEmpty, pushed := queue.push(task)
	if !pushed {
		pp.incrementDropped(priority)
		return handle, ErrPoolFull
	}

	pp.incrementSubmitted(priority)
	if wasEmpty {
		pp.signalTaskAvailable()
	}

	return handle, nil
}

func (pp *PriorityPool) incrementSubmitted(priority TaskPriority) {
	pp.submitted[priority].Add(1)
}

func (pp *PriorityPool) incrementDropped(priority TaskPriority) {
	pp.dropped[priority].Add(1)
}

func (pp *PriorityPool) signalTaskAvailable() {
	select {
	case pp.notifyCh <- struct{}{}:
	default:
	}
}

func (pp *PriorityPool) dispatchLoop(id int) {
	defer pp.wg.Done()

	node := id % pp.numaNodes
	priorities := []TaskPriority{PriorityCritical, PriorityHigh, PriorityNormal, PriorityLow}

	for {
		if task := pp.fetchTask(node, priorities); task != nil {
			pp.executeTask(task)
			continue
		}

		select {
		case <-pp.stopCh:
			return
		case <-pp.notifyCh:
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func (pp *PriorityPool) fetchTask(node int, order []TaskPriority) *taskWrapper {
	for _, priority := range order {
		if task := pp.queues[node][priority].popTail(); task != nil {
			return task
		}
	}

	if !pp.workStealing {
		return nil
	}

	for _, priority := range order {
		for other := 0; other < pp.numaNodes; other++ {
			if other == node {
				continue
			}
			if task := pp.queues[other][priority].popHead(); task != nil {
				return task
			}
		}
	}

	return nil
}

func (pp *PriorityPool) executeTask(task *taskWrapper) {
	if task == nil {
		return
	}

	// Early cancellation check
	if task.cancelled.Load() {
		task.finish()
		return
	}

	// Early context cancellation check
	select {
	case <-task.ctx.Done():
		task.finish()
		return
	default:
	}

	// Execute task in base pool
	err := pp.basePool.Submit(func() {
		defer task.finish()
		pp.runTask(task)
	})

	// Fallback to goroutine if pool is full
	if err != nil {
		go func() {
			defer task.finish()
			pp.runTask(task)
		}()
	}
}

// runTask executes a task with cancellation checks
func (pp *PriorityPool) runTask(task *taskWrapper) {
	if task.cancelled.Load() {
		return
	}
	select {
	case <-task.ctx.Done():
		return
	default:
		task.fn(task.ctx)
	}
}

// Release gracefully shuts down the priority pool
func (pp *PriorityPool) Release() {
	if !pp.closed.CompareAndSwap(false, true) {
		return
	}

	close(pp.stopCh)
	pp.signalTaskAvailable()
	pp.wg.Wait()
}

// PriorityStats provides statistics for priority pool
type PriorityStats struct {
	BaseStats Stats

	// Queue lengths
	CriticalQueueLen int
	HighQueueLen     int
	NormalQueueLen   int
	LowQueueLen      int

	// Submission counts
	SubmittedCritical uint64
	SubmittedHigh     uint64
	SubmittedNormal   uint64
	SubmittedLow      uint64

	// Drop counts
	DroppedCritical uint64
	DroppedHigh     uint64
	DroppedNormal   uint64
	DroppedLow      uint64
}

// Stats returns current priority pool statistics
func (pp *PriorityPool) Stats() PriorityStats {
	baseStats := pp.basePool.Stats()

	return PriorityStats{
		BaseStats:         baseStats,
		CriticalQueueLen:  pp.queueLength(PriorityCritical),
		HighQueueLen:      pp.queueLength(PriorityHigh),
		NormalQueueLen:    pp.queueLength(PriorityNormal),
		LowQueueLen:       pp.queueLength(PriorityLow),
		SubmittedCritical: pp.submitted[PriorityCritical].Load(),
		SubmittedHigh:     pp.submitted[PriorityHigh].Load(),
		SubmittedNormal:   pp.submitted[PriorityNormal].Load(),
		SubmittedLow:      pp.submitted[PriorityLow].Load(),
		DroppedCritical:   pp.dropped[PriorityCritical].Load(),
		DroppedHigh:       pp.dropped[PriorityHigh].Load(),
		DroppedNormal:     pp.dropped[PriorityNormal].Load(),
		DroppedLow:        pp.dropped[PriorityLow].Load(),
	}
}

func (pp *PriorityPool) queueLength(priority TaskPriority) int {
	total := 0
	for node := 0; node < pp.numaNodes; node++ {
		total += pp.queues[node][priority].length()
	}
	return total
}

// NUMANodes returns the configured NUMA node count
func (pp *PriorityPool) NUMANodes() int {
	return pp.numaNodes
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
