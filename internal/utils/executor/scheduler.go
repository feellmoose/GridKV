package executor

import (
	"container/heap"
	"sync"
	"time"
)

// ScheduledTask represents a single scheduled function.
type ScheduledTask struct {
	when   time.Time
	fn     func()
	index  int  // heap index
	cancel bool // set true when cancelled
}

// taskHeap is a min-heap ordered by ScheduledTask.when.
type taskHeap []*ScheduledTask

func (h taskHeap) Len() int { return len(h) }

func (h taskHeap) Less(i, j int) bool {
	return h[i].when.Before(h[j].when)
}

func (h taskHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *taskHeap) Push(x interface{}) {
	n := len(*h)
	item := x.(*ScheduledTask)
	item.index = n
	*h = append(*h, item)
}

func (h *taskHeap) Pop() interface{} {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	item.index = -1
	*h = old[0 : n-1]
	return item
}

// Scheduler executes scheduled tasks at or after a specific time.
type Scheduler struct {
	mu     sync.Mutex
	tasks  taskHeap
	closed bool
	timer  *time.Timer
	wakeup chan struct{}
}

// NewScheduler creates a new Scheduler and starts its dispatcher goroutine.
func NewScheduler() *Scheduler {
	s := &Scheduler{
		wakeup: make(chan struct{}, 1),
	}
	heap.Init(&s.tasks)
	go s.run()
	return s
}

// Schedule schedules fn to run once after delay d and returns a handle that
// can be used to cancel the task before it fires.
func (s *Scheduler) Schedule(d time.Duration, fn func()) *ScheduledTask {
	if fn == nil || d < 0 {
		return nil
	}
	t := &ScheduledTask{
		when: time.Now().Add(d),
		fn:   fn,
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	heap.Push(&s.tasks, t)
	if t.index == 0 {
		select {
		case s.wakeup <- struct{}{}:
		default:
		}
	}
	s.mu.Unlock()
	return t
}

// Cancel marks a scheduled task as cancelled. It will be skipped when its
// turn comes; we don't remove it eagerly from the heap to keep locking simple.
func (s *Scheduler) Cancel(t *ScheduledTask) {
	if t == nil {
		return
	}
	s.mu.Lock()
	t.cancel = true
	s.mu.Unlock()
}

// Close stops the scheduler and drops all pending tasks.
func (s *Scheduler) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()

	select {
	case s.wakeup <- struct{}{}:
	default:
	}
}

func (s *Scheduler) run() {
	for {
		s.mu.Lock()
		if s.closed && len(s.tasks) == 0 {
			s.mu.Unlock()
			return
		}

		if len(s.tasks) == 0 {
			s.mu.Unlock()
			<-s.wakeup
			continue
		}

		next := s.tasks[0]
		now := time.Now()
		wait := next.when.Sub(now)
		if wait <= 0 {
			heap.Pop(&s.tasks)
			fn := next.fn
			cancelled := next.cancel
			s.mu.Unlock()
			if cancelled || fn == nil {
				continue
			}
			fn()
			continue
		}

		if s.timer == nil {
			s.timer = time.NewTimer(wait)
		} else {
			if !s.timer.Stop() {
				select {
				case <-s.timer.C:
				default:
				}
			}
			s.timer.Reset(wait)
		}
		timer := s.timer
		s.mu.Unlock()

		select {
		case <-timer.C:
		case <-s.wakeup:
		}
	}
}
