package network

import (
	"testing"
	"time"
)

func TestSimpleBackpressure_Allow(t *testing.T) {
	cfg := BackpressureConfig{
		Threshold:  5,
		MaxCapacity: 10,
		Strategy:   StrategyReject,
	}

	bp := NewBackpressure(cfg)

	// should allow initially
	if !bp.Allow() {
		t.Error("Allow() = false, want true")
	}

	// fill up to threshold
	for i := 0; i < 5; i++ {
		if err := bp.Acquire(); err != nil {
			t.Fatalf("Acquire() error = %v", err)
		}
	}

	// should not allow now
	if bp.Allow() {
		t.Error("Allow() = true, want false")
	}
}

func TestSimpleBackpressure_StrategyReject(t *testing.T) {
	cfg := BackpressureConfig{
		Threshold:  3,
		MaxCapacity: 5,
		Strategy:   StrategyReject,
	}

	bp := NewBackpressure(cfg)

	// fill to threshold
	for i := 0; i < 3; i++ {
		if err := bp.Acquire(); err != nil {
			t.Fatalf("Acquire() error = %v", err)
		}
	}

	// should reject
	err := bp.Acquire()
	if err != ErrBackpressure {
		t.Errorf("Acquire() error = %v, want %v", err, ErrBackpressure)
	}

	status := bp.Status()
	if status.Rejected == 0 {
		t.Error("Status().Rejected = 0, want > 0")
	}
}

func TestSimpleBackpressure_StrategyBlock(t *testing.T) {
	cfg := BackpressureConfig{
		Threshold:  3,
		MaxCapacity: 5,
		Strategy:   StrategyBlock,
	}

	bp := NewBackpressure(cfg)

	// fill to threshold
	for i := 0; i < 3; i++ {
		if err := bp.Acquire(); err != nil {
			t.Fatalf("Acquire() error = %v", err)
		}
	}

	// should block (but channel allows up to MaxCapacity)
	done := make(chan bool)
	go func() {
		err := bp.Acquire()
		done <- (err == nil)
	}()

	select {
	case success := <-done:
		if !success {
			t.Error("Acquire() should succeed with block strategy")
		}
	case <-time.After(100 * time.Millisecond):
		// should succeed quickly
		t.Error("Acquire() blocked too long")
	}
}

func TestSimpleBackpressure_StrategyDrop(t *testing.T) {
	cfg := BackpressureConfig{
		Threshold:  3,
		MaxCapacity: 5,
		Strategy:   StrategyDrop,
	}

	bp := NewBackpressure(cfg)

	// fill to capacity
	for i := 0; i < 5; i++ {
		if err := bp.Acquire(); err != nil {
			t.Fatalf("Acquire() error = %v", err)
		}
	}

	// should drop oldest and add new
	err := bp.Acquire()
	if err != nil {
		t.Errorf("Acquire() error = %v, want nil", err)
	}
}

func TestSimpleBackpressure_Release(t *testing.T) {
	cfg := BackpressureConfig{
		Threshold:  5,
		MaxCapacity: 10,
		Strategy:   StrategyReject,
	}

	bp := NewBackpressure(cfg)

	// fill to threshold
	for i := 0; i < 5; i++ {
		bp.Acquire()
	}

	if bp.Allow() {
		t.Error("Allow() = true before release")
	}

	// release one
	bp.Release()

	if !bp.Allow() {
		t.Error("Allow() = false after release")
	}

	status := bp.Status()
	if status.Queued != 4 {
		t.Errorf("Status().Queued = %d, want 4", status.Queued)
	}
}

func TestSimpleBackpressure_Status(t *testing.T) {
	cfg := BackpressureConfig{
		Threshold:  5,
		MaxCapacity: 10,
		Strategy:   StrategyReject,
	}

	bp := NewBackpressure(cfg)

	status := bp.Status()
	if status.Capacity != 10 {
		t.Errorf("Status().Capacity = %d, want 10", status.Capacity)
	}
	if status.Queued != 0 {
		t.Errorf("Status().Queued = %d, want 0", status.Queued)
	}
	if status.Blocked {
		t.Error("Status().Blocked = true, want false")
	}

	// fill to threshold
	for i := 0; i < 5; i++ {
		bp.Acquire()
	}

	status = bp.Status()
	if status.Queued != 5 {
		t.Errorf("Status().Queued = %d, want 5", status.Queued)
	}
	if !status.Blocked {
		t.Error("Status().Blocked = false, want true")
	}
}

