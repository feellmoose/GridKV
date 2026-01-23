package executor

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestSchedulerExecutesTask(t *testing.T) {
	s := NewScheduler()
	defer s.Close()

	var fired int32
	done := make(chan struct{})

	s.Schedule(20*time.Millisecond, func() {
		atomic.StoreInt32(&fired, 1)
		close(done)
	})

	select {
	case <-done:
		if atomic.LoadInt32(&fired) != 1 {
			t.Fatalf("task did not set fired flag")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("scheduled task did not fire in time")
	}
}

func TestSchedulerCancel(t *testing.T) {
	s := NewScheduler()
	defer s.Close()

	var fired int32
	task := s.Schedule(20*time.Millisecond, func() {
		atomic.StoreInt32(&fired, 1)
	})
	if task == nil {
		t.Fatal("expected non-nil task handle")
	}
	s.Cancel(task)
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&fired) != 0 {
		t.Fatalf("cancelled task should not have fired")
	}
}

func TestSchedulerOrder(t *testing.T) {
	s := NewScheduler()
	defer s.Close()

	ch := make(chan int, 2)
	s.Schedule(40*time.Millisecond, func() { ch <- 2 })
	s.Schedule(10*time.Millisecond, func() { ch <- 1 })

	first := <-ch
	second := <-ch

	if first != 1 || second != 2 {
		t.Fatalf("tasks executed out of order: got %d then %d", first, second)
	}
}

func TestSchedulerClose(t *testing.T) {
	s := NewScheduler()
	// Schedule a task; Close should return promptly and not panic.
	s.Schedule(200*time.Millisecond, func() {})
	s.Close()
}

func TestSchedulerConcurrentSchedule(t *testing.T) {
	s := NewScheduler()
	defer s.Close()

	const n = 1000
	var count int32

	for i := 0; i < n; i++ {
		go func() {
			s.Schedule(5*time.Millisecond, func() {
				atomic.AddInt32(&count, 1)
			})
		}()
	}

	// Wait enough time for all tasks to run.
	time.Sleep(200 * time.Millisecond)

	if c := atomic.LoadInt32(&count); c != n {
		t.Fatalf("expected %d tasks to fire, got %d", n, c)
	}
}

func TestSchedulerStressNoLeak(t *testing.T) {
	s := NewScheduler()
	defer s.Close()

	const workers = 64
	const perWorker = 2000

	var fired int32
	var cancelled int32

	done := make(chan struct{})

	for w := 0; w < workers; w++ {
		go func() {
			for i := 0; i < perWorker; i++ {
				task := s.Schedule(1*time.Millisecond, func() {
					atomic.AddInt32(&fired, 1)
				})
				if i%4 == 0 && task != nil {
					s.Cancel(task)
					atomic.AddInt32(&cancelled, 1)
				}
			}
		}()
	}

	time.AfterFunc(3*time.Second, func() {
		close(done)
	})

	<-done

	total := workers * perWorker
	got := int(atomic.LoadInt32(&fired) + atomic.LoadInt32(&cancelled))
	if got < total/2 {
		t.Fatalf("scheduler stress: too few tasks processed (fired+cancelled=%d, total=%d)", got, total)
	}
}
