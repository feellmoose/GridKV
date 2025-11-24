package metrics

import (
	"fmt"
	"strings"
	"sync"

	"github.com/feellmoose/gridkv/internal/utils/workerpool"
)

// WorkerPoolRecorder periodically records worker pool statistics into GridKV metrics.
type WorkerPoolRecorder struct {
	exporter *MetricsExporter

	capacityGauge string
	runningGauge  string
	queueGauge    string

	submittedCounter string
	completedCounter string
	droppedCounter   string

	mu            sync.Mutex
	lastSubmitted uint64
	lastCompleted uint64
	lastDropped   uint64
}

// RegisterWorkerPoolRecorder registers metrics for a named worker pool and returns a recorder.
func (m *GridKVMetrics) RegisterWorkerPoolRecorder(poolName string) *WorkerPoolRecorder {
	if m == nil || m.exporter == nil {
		return nil
	}

	sanitized := sanitizeMetricToken(poolName)
	if sanitized == "" {
		sanitized = "pool"
	}

	prefix := fmt.Sprintf("workerpool_%s", sanitized)
	capacityGauge := prefix + "_capacity"
	runningGauge := prefix + "_running"
	queueGauge := prefix + "_queue_len"
	submittedCounter := prefix + "_submitted_total"
	completedCounter := prefix + "_completed_total"
	droppedCounter := prefix + "_dropped_total"

	helpPrefix := fmt.Sprintf("%s worker pool", poolName)
	e := m.exporter
	e.RegisterGauge(capacityGauge, helpPrefix+" capacity", "workers", nil)
	e.RegisterGauge(runningGauge, helpPrefix+" active workers", "workers", nil)
	e.RegisterGauge(queueGauge, helpPrefix+" queue length", "tasks", nil)
	e.RegisterCounter(submittedCounter, helpPrefix+" submitted tasks", "tasks", nil)
	e.RegisterCounter(completedCounter, helpPrefix+" completed tasks", "tasks", nil)
	e.RegisterCounter(droppedCounter, helpPrefix+" dropped tasks", "tasks", nil)

	return &WorkerPoolRecorder{
		exporter:         e,
		capacityGauge:    capacityGauge,
		runningGauge:     runningGauge,
		queueGauge:       queueGauge,
		submittedCounter: submittedCounter,
		completedCounter: completedCounter,
		droppedCounter:   droppedCounter,
	}
}

// RecordStats updates metrics using the provided worker pool stats.
func (r *WorkerPoolRecorder) RecordStats(stats workerpool.Stats) {
	if r == nil || r.exporter == nil {
		return
	}

	r.exporter.SetGauge(r.capacityGauge, int64(stats.Capacity))
	r.exporter.SetGauge(r.runningGauge, int64(stats.Running))
	r.exporter.SetGauge(r.queueGauge, int64(stats.QueueLen))

	r.mu.Lock()
	defer r.mu.Unlock()

	r.recordCounterDelta(r.submittedCounter, stats.Submitted, &r.lastSubmitted)
	r.recordCounterDelta(r.completedCounter, stats.Completed, &r.lastCompleted)
	r.recordCounterDelta(r.droppedCounter, stats.Dropped, &r.lastDropped)
}

func (r *WorkerPoolRecorder) recordCounterDelta(metric string, current uint64, last *uint64) {
	if last == nil {
		return
	}

	var delta int64
	if current >= *last {
		delta = int64(current - *last)
	} else {
		// Pool restarted; use absolute value as delta.
		delta = int64(current)
	}

	if delta > 0 {
		r.exporter.AddCounter(metric, delta)
	}
	*last = current
}

func sanitizeMetricToken(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "pool"
	}

	var b strings.Builder
	b.Grow(len(name))
	prevUnderscore := false
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevUnderscore = false
			continue
		}

		if !prevUnderscore {
			b.WriteByte('_')
			prevUnderscore = true
		}
	}

	s := strings.Trim(b.String(), "_")
	if s == "" {
		return "pool"
	}
	return s
}
