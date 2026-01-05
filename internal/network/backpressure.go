package network

import "sync/atomic"

// BackpressureStatus represents backpressure status
type BackpressureStatus struct {
	// Queued is number of queued operations
	Queued int
	
	// Capacity is maximum capacity
	Capacity int
	
	// Blocked indicates if operations are blocked
	Blocked bool
	
	// Rejected is number of rejected operations
	Rejected uint64
}

// BackpressureConfig configures backpressure
type BackpressureConfig struct {
	// Threshold is backpressure threshold
	Threshold int
	
	// MaxCapacity is maximum capacity
	MaxCapacity int
	
	// Strategy is backpressure strategy
	Strategy BackpressureStrategy
}

// BackpressureStrategy is backpressure strategy
type BackpressureStrategy string

const (
	// StrategyReject rejects new operations when threshold exceeded
	StrategyReject BackpressureStrategy = "reject"
	
	// StrategyBlock blocks new operations when threshold exceeded
	StrategyBlock BackpressureStrategy = "block"
	
	// StrategyDrop drops oldest operations when threshold exceeded
	StrategyDrop BackpressureStrategy = "drop"
)

// DefaultBackpressureConfig returns default backpressure config
func DefaultBackpressureConfig() BackpressureConfig {
	return BackpressureConfig{
		Threshold:  1000,
		MaxCapacity: 10000,
		Strategy:   StrategyBlock,
	}
}

// simpleBackpressure is a minimal controller using a buffered channel.
type simpleBackpressure struct {
	cfg      BackpressureConfig
	permits  chan struct{}
	rejected uint64
}

func NewBackpressure(cfg BackpressureConfig) *simpleBackpressure {
	if cfg.MaxCapacity == 0 {
		cfg.MaxCapacity = cfg.Threshold
	}
	return &simpleBackpressure{
		cfg:     cfg,
		permits: make(chan struct{}, cfg.MaxCapacity),
	}
}

func (b *simpleBackpressure) Allow() bool {
	return len(b.permits) < b.cfg.Threshold
}

func (b *simpleBackpressure) Acquire() error {
	switch b.cfg.Strategy {
	case StrategyReject:
		if !b.Allow() {
			atomic.AddUint64(&b.rejected, 1)
			return ErrBackpressure
		}
		b.permits <- struct{}{}
		return nil
	case StrategyDrop:
		select {
		case b.permits <- struct{}{}:
			return nil
		default:
			select {
			case <-b.permits:
			default:
			}
			b.permits <- struct{}{}
			return nil
		}
	default: // block
		b.permits <- struct{}{}
		return nil
	}
}

func (b *simpleBackpressure) Release() {
	select {
	case <-b.permits:
	default:
	}
}

func (b *simpleBackpressure) Status() BackpressureStatus {
	return BackpressureStatus{
		Queued:   len(b.permits),
		Capacity: cap(b.permits),
		Blocked:  len(b.permits) >= b.cfg.Threshold,
		Rejected: atomic.LoadUint64(&b.rejected),
	}
}

