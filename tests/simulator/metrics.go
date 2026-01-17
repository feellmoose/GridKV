package simulator

import (
	"fmt"
	"time"

	"github.com/feellmoose/gridkv/internal/network"
)

func CollectPoolMetrics(sim *Simulator, interval time.Duration, duration time.Duration) []PoolMetricsSnapshot {
	if interval <= 0 {
		interval = 5 * time.Second
	}

	var snapshots []PoolMetricsSnapshot
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	deadline := time.Now().Add(duration)

	for t := range ticker.C {
		if t.After(deadline) {
			return snapshots
		}

		snapshot := collectSnapshot(sim)
		snapshots = append(snapshots, snapshot)
	}
	return snapshots
}

type PoolMetricsSnapshot struct {
	Timestamp      time.Time
	NodeID         string
	PoolStats      PoolStatsSummary
	PoolMetrics    PoolMetricsSummary
	PoolDebugStats PoolDebugStatsSummary
}

type PoolStatsSummary struct {
	Total   int64
	Active  int64
	Idle    int64
	Waiters int64
	Created uint64
	Closed  uint64
	Errors  uint64
}

type PoolMetricsSummary struct {
	AvgWaitTime time.Duration
	MaxWaitTime time.Duration
	AvgHoldTime time.Duration
	RequestRate float64
	WaitSamples uint64
	HoldSamples uint64
}

type PoolDebugStatsSummary struct {
	GetAttempts      uint64
	GetSuccess       uint64
	GetExhausted     uint64
	GetTimeout       uint64
	GetContextCancel uint64
	GetDialError     uint64
	PutAttempts      uint64
	PutSuccess       uint64
	PutClosed        uint64
	WaitQueueLength  uint64
	MaxWaitQueueLen  uint64
	ActiveConnPeak   int64
	IdleConnPeak     int64
}

func collectSnapshot(sim *Simulator) PoolMetricsSnapshot {
	nodes := sim.GetNodes()
	if len(nodes) == 0 {
		return PoolMetricsSnapshot{Timestamp: time.Now()}
	}

	now := time.Now()

	var stats PoolStatsSummary
	var metrics PoolMetricsSummary
	var debugStats PoolDebugStatsSummary

	node := nodes[0]
	nodeID := "node-0"

	pool := getPoolFromNode(node)
	if pool != nil {
		poolStats := pool.Stats()
		stats = PoolStatsSummary{
			Total:   poolStats.Total,
			Active:  poolStats.Active,
			Idle:    poolStats.Idle,
			Waiters: poolStats.Waiters,
			Created: poolStats.Created,
			Closed:  poolStats.Closed,
			Errors:  poolStats.Errors,
		}

		metrics = PoolMetricsSummary{
			AvgWaitTime: poolStats.AvgWaitTime,
			MaxWaitTime: poolStats.MaxWaitTime,
			AvgHoldTime: poolStats.AvgHoldTime,
			RequestRate: poolStats.RequestRate,
			WaitSamples: poolStats.WaitSamples,
			HoldSamples: poolStats.HoldSamples,
		}

		poolDebugStats := pool.DebugStats()
		debugStats = PoolDebugStatsSummary{
			GetAttempts:      poolDebugStats.GetAttempts.Load(),
			GetSuccess:       poolDebugStats.GetSuccess.Load(),
			GetExhausted:     poolDebugStats.GetExhausted.Load(),
			GetTimeout:       poolDebugStats.GetTimeout.Load(),
			GetContextCancel: poolDebugStats.GetContextCancel.Load(),
			GetDialError:     poolDebugStats.GetDialError.Load(),
			PutAttempts:      poolDebugStats.PutAttempts.Load(),
			PutSuccess:       poolDebugStats.PutSuccess.Load(),
			PutClosed:        poolDebugStats.PutClosed.Load(),
			WaitQueueLength:  poolDebugStats.WaitQueueLength.Load(),
			MaxWaitQueueLen:  poolDebugStats.MaxWaitQueueLen.Load(),
			ActiveConnPeak:   poolDebugStats.ActiveConnPeak.Load(),
			IdleConnPeak:     poolDebugStats.IdleConnPeak.Load(),
		}
	}

	return PoolMetricsSnapshot{
		Timestamp:      now,
		NodeID:         nodeID,
		PoolStats:      stats,
		PoolMetrics:    metrics,
		PoolDebugStats: debugStats,
	}
}

func getPoolFromNode(node interface{}) network.ConnPool {
	type networkGetter interface {
		GetNetwork() network.Network
	}
	type poolGetter interface {
		GetPool() network.ConnPool
	}

	if ng, ok := node.(networkGetter); ok {
		if net := ng.GetNetwork(); net != nil {
			if pg, ok := net.(poolGetter); ok {
				return pg.GetPool()
			}
		}
	}
	return nil
}

func PrintMetricsSummary(snapshots []PoolMetricsSnapshot) {
	if len(snapshots) == 0 {
		return
	}

	fmt.Println("\n=== Pool Metrics Summary ===")

	var totalAttempts, totalSuccess, totalExhausted, totalTimeout uint64
	var maxWaiters, maxActive, maxIdle int64

	for _, snap := range snapshots {
		totalAttempts += snap.PoolDebugStats.GetAttempts
		totalSuccess += snap.PoolDebugStats.GetSuccess
		totalExhausted += snap.PoolDebugStats.GetExhausted
		totalTimeout += snap.PoolDebugStats.GetTimeout

		if snap.PoolStats.Waiters > maxWaiters {
			maxWaiters = snap.PoolStats.Waiters
		}
		if snap.PoolStats.Active > maxActive {
			maxActive = snap.PoolStats.Active
		}
		if snap.PoolStats.Idle > maxIdle {
			maxIdle = snap.PoolStats.Idle
		}
	}

	last := snapshots[len(snapshots)-1]

	fmt.Printf("Total Get Attempts: %d\n", totalAttempts)
	fmt.Printf("Total Success: %d (%.2f%%)\n", totalSuccess,
		float64(totalSuccess)/float64(totalAttempts)*100)
	fmt.Printf("Total Exhausted: %d (%.2f%%)\n", totalExhausted,
		float64(totalExhausted)/float64(totalAttempts)*100)
	fmt.Printf("Total Timeout: %d (%.2f%%)\n", totalTimeout,
		float64(totalTimeout)/float64(totalAttempts)*100)
	fmt.Printf("\nPeak Metrics:\n")
	fmt.Printf("  Max Waiters: %d\n", maxWaiters)
	fmt.Printf("  Max Active: %d\n", maxActive)
	fmt.Printf("  Max Idle: %d\n", maxIdle)
	fmt.Printf("  Active Peak: %d\n", last.PoolDebugStats.ActiveConnPeak)
	fmt.Printf("  Idle Peak: %d\n", last.PoolDebugStats.IdleConnPeak)
	fmt.Printf("\nCurrent Metrics:\n")
	fmt.Printf("  Active: %d\n", last.PoolStats.Active)
	fmt.Printf("  Idle: %d\n", last.PoolStats.Idle)
	fmt.Printf("  Waiters: %d\n", last.PoolStats.Waiters)
	fmt.Printf("  Avg Wait Time: %v\n", last.PoolMetrics.AvgWaitTime)
	fmt.Printf("  Max Wait Time: %v\n", last.PoolMetrics.MaxWaitTime)
	fmt.Printf("  Request Rate: %.2f req/s\n", last.PoolMetrics.RequestRate)
}
