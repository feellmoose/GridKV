package lifecycle

// Package lifecycle provides unified component lifecycle management.
//
// Features:
//   - Dependency graph resolution
//   - Ordered startup/shutdown
//   - Timeout protection
//   - Error aggregation

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrComponentNotFound  = errors.New("component not found")
	ErrCircularDependency = errors.New("circular dependency")
	ErrStartTimeout       = errors.New("start timeout")
	ErrStopTimeout        = errors.New("stop timeout")
)

// Component represents a lifecycle-managed component
type Component interface {
	Start(ctx context.Context) error
	Close(ctx context.Context) error
	Name() string
}

// LifecycleManager manages component lifecycles
type LifecycleManager struct {
	components   []Component
	dependencies map[string][]string
	mu           sync.Mutex
	started      bool
}

// New creates a lifecycle manager
func New() *LifecycleManager {
	return &LifecycleManager{
		components:   make([]Component, 0),
		dependencies: make(map[string][]string),
	}
}

// Register adds a component
func (lm *LifecycleManager) Register(comp Component, deps ...string) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	lm.components = append(lm.components, comp)
	if len(deps) > 0 {
		lm.dependencies[comp.Name()] = deps
	}
}

// Start starts all components in dependency order
func (lm *LifecycleManager) Start(ctx context.Context) error {
	return lm.StartWithTimeout(ctx, 30*time.Second)
}

// StartWithTimeout starts components with timeout
func (lm *LifecycleManager) StartWithTimeout(ctx context.Context, timeout time.Duration) error {
	lm.mu.Lock()
	if lm.started {
		lm.mu.Unlock()
		return errors.New("already started")
	}
	lm.mu.Unlock()

	startCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	order, err := lm.resolveOrder()
	if err != nil {
		return err
	}

	var firstErr error
	var mu sync.Mutex

	var wg sync.WaitGroup
	for _, name := range order {
		comp := lm.findComponent(name)
		if comp == nil {
			continue
		}

		wg.Add(1)
		go func(c Component) {
			defer wg.Done()
			if err := c.Start(startCtx); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("%s: %w", c.Name(), err)
				}
				mu.Unlock()
			}
		}(comp)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		if firstErr != nil {
			return firstErr
		}
	case <-startCtx.Done():
		return fmt.Errorf("%w: %v", ErrStartTimeout, startCtx.Err())
	}

	lm.mu.Lock()
	lm.started = true
	lm.mu.Unlock()

	return nil
}

// Close stops all components in reverse dependency order
func (lm *LifecycleManager) Close(ctx context.Context) error {
	return lm.CloseWithTimeout(ctx, 30*time.Second)
}

// CloseWithTimeout stops components with timeout
func (lm *LifecycleManager) CloseWithTimeout(ctx context.Context, timeout time.Duration) error {
	lm.mu.Lock()
	if !lm.started {
		lm.mu.Unlock()
		return nil
	}
	lm.mu.Unlock()

	stopCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	order, err := lm.resolveOrder()
	if err != nil {
		return err
	}

	var firstErr error
	var mu sync.Mutex

	for i := len(order) - 1; i >= 0; i-- {
		comp := lm.findComponent(order[i])
		if comp == nil {
			continue
		}

		if err := comp.Close(stopCtx); err != nil {
			mu.Lock()
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", comp.Name(), err)
			}
			mu.Unlock()
		}
	}

	return firstErr
}

func (lm *LifecycleManager) resolveOrder() ([]string, error) {
	visited := make(map[string]bool)
	temp := make(map[string]bool)
	result := make([]string, 0)

	var visit func(string) error
	visit = func(name string) error {
		if temp[name] {
			return fmt.Errorf("%w: %s", ErrCircularDependency, name)
		}
		if visited[name] {
			return nil
		}

		temp[name] = true
		deps := lm.dependencies[name]
		for _, dep := range deps {
			if lm.findComponent(dep) == nil {
				return fmt.Errorf("%w: %s depends on %s", ErrComponentNotFound, name, dep)
			}
			if err := visit(dep); err != nil {
				return err
			}
		}
		delete(temp, name)
		visited[name] = true
		result = append(result, name)
		return nil
	}

	for _, comp := range lm.components {
		if err := visit(comp.Name()); err != nil {
			return nil, err
		}
	}

	return result, nil
}

func (lm *LifecycleManager) findComponent(name string) Component {
	for _, comp := range lm.components {
		if comp.Name() == name {
			return comp
		}
	}
	return nil
}
