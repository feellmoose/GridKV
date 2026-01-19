package network

import (
	"sync/atomic"
)

type PoolDebugStats struct {
	GetAttempts      atomic.Uint64
	GetSuccess       atomic.Uint64
	GetExhausted     atomic.Uint64
	GetTimeout       atomic.Uint64
	GetContextCancel atomic.Uint64
	GetDialError     atomic.Uint64

	PutAttempts atomic.Uint64
	PutSuccess  atomic.Uint64
	PutClosed   atomic.Uint64

	WaitQueueLength atomic.Uint64
	MaxWaitQueueLen atomic.Uint64
	TotalWaitTime   atomic.Uint64
	WaitSamples     atomic.Uint64

	ActiveConnPeak atomic.Int64
	IdleConnPeak   atomic.Int64
}

type PoolDebugStatsSnapshot struct {
	GetAttempts      uint64
	GetSuccess       uint64
	GetExhausted     uint64
	GetTimeout       uint64
	GetContextCancel uint64
	GetDialError     uint64

	PutAttempts uint64
	PutSuccess  uint64
	PutClosed   uint64

	WaitQueueLength uint64
	MaxWaitQueueLen uint64
	TotalWaitTime   uint64
	WaitSamples     uint64

	ActiveConnPeak int64
	IdleConnPeak   int64
}

func (p *connPool) DebugStats() PoolDebugStatsSnapshot {
	return PoolDebugStatsSnapshot{
		GetAttempts:      p.debugStats.GetAttempts.Load(),
		GetSuccess:       p.debugStats.GetSuccess.Load(),
		GetExhausted:     p.debugStats.GetExhausted.Load(),
		GetTimeout:       p.debugStats.GetTimeout.Load(),
		GetContextCancel: p.debugStats.GetContextCancel.Load(),
		GetDialError:     p.debugStats.GetDialError.Load(),
		PutAttempts:      p.debugStats.PutAttempts.Load(),
		PutSuccess:       p.debugStats.PutSuccess.Load(),
		PutClosed:        p.debugStats.PutClosed.Load(),
		WaitQueueLength:  p.debugStats.WaitQueueLength.Load(),
		MaxWaitQueueLen:  p.debugStats.MaxWaitQueueLen.Load(),
		TotalWaitTime:    p.debugStats.TotalWaitTime.Load(),
		WaitSamples:      p.debugStats.WaitSamples.Load(),
		ActiveConnPeak:   p.debugStats.ActiveConnPeak.Load(),
		IdleConnPeak:     p.debugStats.IdleConnPeak.Load(),
	}
}

func (p *connPool) updateDebugStats() {
	if !p.debugEnabled.Load() {
		return
	}
	stats := p.Stats()

	if stats.Active > p.debugStats.ActiveConnPeak.Load() {
		p.debugStats.ActiveConnPeak.Store(stats.Active)
	}
	if stats.Idle > p.debugStats.IdleConnPeak.Load() {
		p.debugStats.IdleConnPeak.Store(stats.Idle)
	}
	if stats.Waiters > int64(p.debugStats.MaxWaitQueueLen.Load()) {
		p.debugStats.MaxWaitQueueLen.Store(uint64(stats.Waiters))
	}
	p.debugStats.WaitQueueLength.Store(uint64(stats.Waiters))
}
