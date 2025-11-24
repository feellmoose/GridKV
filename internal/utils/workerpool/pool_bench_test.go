package workerpool

import (
	"sync"
	"testing"
	"time"
)

func benchmarkPool(b *testing.B, submit func(func()) error, cleanup func()) {
	defer cleanup()

	var wg sync.WaitGroup
	wg.Add(b.N)

	task := func() {
		time.Sleep(50 * time.Microsecond)
		wg.Done()
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := submit(task); err != nil {
			b.Fatalf("submit failed: %v", err)
		}
	}

	wg.Wait()
}

func BenchmarkGridKVWorkerPool(b *testing.B) {
	pool, err := New(Options{
		Name:         "bench-gridkv",
		MaxWorkers:   256,
		QueueSize:    512,
		NonBlocking:  false,
		DisableStats: true,
	})
	if err != nil {
		b.Fatalf("failed to create worker pool: %v", err)
	}

	benchmarkPool(b, pool.Submit, pool.Release)
}
