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
	
	PutAttempts      atomic.Uint64
	PutSuccess       atomic.Uint64
	PutClosed        atomic.Uint64
	
	WaitQueueLength  atomic.Uint64
	MaxWaitQueueLen  atomic.Uint64
	TotalWaitTime    atomic.Uint64
	WaitSamples      atomic.Uint64
	
	ActiveConnPeak   atomic.Int64
	IdleConnPeak     atomic.Int64
}

func (p *connPool) DebugStats() PoolDebugStats {
	return p.debugStats
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
