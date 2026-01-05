package lifecycle

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type mockComponent struct {
	name      string
	deps      []string
	started   atomic.Bool
	closed    atomic.Bool
	startErr  error
	closeErr  error
	startTime time.Duration
	closeTime time.Duration
}

func (m *mockComponent) Name() string {
	return m.name
}

func (m *mockComponent) Start(ctx context.Context) error {
	if m.startTime > 0 {
		time.Sleep(m.startTime)
	}
	if m.startErr != nil {
		return m.startErr
	}
	m.started.Store(true)
	return nil
}

func (m *mockComponent) Close(ctx context.Context) error {
	if m.closeTime > 0 {
		time.Sleep(m.closeTime)
	}
	if m.closeErr != nil {
		return m.closeErr
	}
	m.closed.Store(true)
	return nil
}

func TestLifecycleManager_Basic(t *testing.T) {
	lm := New()

	comp1 := &mockComponent{name: "comp1"}
	comp2 := &mockComponent{name: "comp2"}

	lm.Register(comp1)
	lm.Register(comp2)

	ctx := context.Background()
	if err := lm.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if !comp1.started.Load() || !comp2.started.Load() {
		t.Fatal("Components not started")
	}

	if err := lm.Close(ctx); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if !comp1.closed.Load() || !comp2.closed.Load() {
		t.Fatal("Components not closed")
	}
}

func TestLifecycleManager_Dependencies(t *testing.T) {
	lm := New()

	comp1 := &mockComponent{name: "comp1"}
	comp2 := &mockComponent{name: "comp2"}
	comp3 := &mockComponent{name: "comp3"}

	lm.Register(comp1)
	lm.Register(comp2, "comp1")
	lm.Register(comp3, "comp2")

	ctx := context.Background()
	if err := lm.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if !comp1.started.Load() || !comp2.started.Load() || !comp3.started.Load() {
		t.Fatal("Components not started")
	}

	if err := lm.Close(ctx); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

func TestLifecycleManager_CircularDependency(t *testing.T) {
	lm := New()

	comp1 := &mockComponent{name: "comp1"}
	comp2 := &mockComponent{name: "comp2"}

	lm.Register(comp1, "comp2")
	lm.Register(comp2, "comp1")

	ctx := context.Background()
	err := lm.Start(ctx)
	if err == nil {
		t.Fatal("Expected circular dependency error")
	}
	if !errors.Is(err, ErrCircularDependency) {
		t.Fatalf("Expected ErrCircularDependency, got %v", err)
	}
}

func TestLifecycleManager_MissingDependency(t *testing.T) {
	lm := New()

	comp1 := &mockComponent{name: "comp1"}

	lm.Register(comp1, "missing")

	ctx := context.Background()
	err := lm.Start(ctx)
	if err == nil {
		t.Fatal("Expected missing dependency error")
	}
	if !errors.Is(err, ErrComponentNotFound) {
		t.Fatalf("Expected ErrComponentNotFound, got %v", err)
	}
}

func TestLifecycleManager_StartError(t *testing.T) {
	lm := New()

	comp1 := &mockComponent{name: "comp1", startErr: errors.New("start failed")}
	comp2 := &mockComponent{name: "comp2"}

	lm.Register(comp1)
	lm.Register(comp2)

	ctx := context.Background()
	err := lm.Start(ctx)
	if err == nil {
		t.Fatal("Expected start error")
	}
}

func TestLifecycleManager_StartTimeout(t *testing.T) {
	lm := New()

	comp1 := &mockComponent{name: "comp1", startTime: 200 * time.Millisecond}

	lm.Register(comp1)

	ctx := context.Background()
	err := lm.StartWithTimeout(ctx, 100*time.Millisecond)
	if err == nil {
		t.Fatal("Expected timeout error")
	}
	if !errors.Is(err, ErrStartTimeout) {
		t.Fatalf("Expected ErrStartTimeout, got %v", err)
	}
}

type orderComponent struct {
	name      string
	order     *[]string
	mu        *sync.Mutex
	started   atomic.Bool
	closed    atomic.Bool
}

func (o *orderComponent) Name() string {
	return o.name
}

func (o *orderComponent) Start(ctx context.Context) error {
	o.started.Store(true)
	return nil
}

func (o *orderComponent) Close(ctx context.Context) error {
	o.mu.Lock()
	*o.order = append(*o.order, o.name)
	o.mu.Unlock()
	o.closed.Store(true)
	return nil
}

func TestLifecycleManager_CloseOrder(t *testing.T) {
	lm := New()

	var order []string
	var mu sync.Mutex

	comp1 := &orderComponent{name: "comp1", order: &order, mu: &mu}
	comp2 := &orderComponent{name: "comp2", order: &order, mu: &mu}
	comp3 := &orderComponent{name: "comp3", order: &order, mu: &mu}

	lm.Register(comp1)
	lm.Register(comp2, "comp1")
	lm.Register(comp3, "comp2")

	ctx := context.Background()
	if err := lm.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if err := lm.Close(ctx); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	mu.Lock()
	if len(order) != 3 {
		t.Fatalf("Expected 3 components, got %d", len(order))
	}
	if order[0] != "comp3" || order[1] != "comp2" || order[2] != "comp1" {
		t.Fatalf("Wrong close order: %v", order)
	}
	mu.Unlock()
}

func TestLifecycleManager_DoubleStart(t *testing.T) {
	lm := New()

	comp1 := &mockComponent{name: "comp1"}
	lm.Register(comp1)

	ctx := context.Background()
	if err := lm.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	err := lm.Start(ctx)
	if err == nil {
		t.Fatal("Expected error for double start")
	}
}

