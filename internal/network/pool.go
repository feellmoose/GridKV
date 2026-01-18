package network

import (
	"context"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

type ConnPool interface {
	Get(ctx context.Context, address string) (Conn, error)
	Put(conn Conn)
	Remove(conn Conn)
	Close() error
	Stats() PoolStats
	DebugStats() PoolDebugStats
}

type PoolStats struct {
	// Connection stats
	Total   int64
	Active  int64
	Idle    int64
	Waiters int64
	Created uint64
	Closed  uint64
	Errors  uint64

	// Performance stats
	AvgWaitTime time.Duration
	MaxWaitTime time.Duration
	AvgHoldTime time.Duration
	RequestRate float64
	WaitSamples uint64
	HoldSamples uint64
}

type PoolConfig struct {
	MaxIdle         int
	MaxActive       int
	IdleTimeout     time.Duration
	MaxLifetime     time.Duration
	WaitTimeout     time.Duration
	CleanupInterval time.Duration
	Transport       Transport
}

func DefaultPoolConfig(transport Transport) PoolConfig {
	return PoolConfig{
		MaxIdle:         2048,
		MaxActive:       30000,
		IdleTimeout:     30 * time.Second,
		MaxLifetime:     5 * time.Minute,
		WaitTimeout:     5 * time.Second,
		CleanupInterval: 10 * time.Second,
		Transport:       transport,
	}
}

const poolShards = 256

type connPool struct {
	cfg         PoolConfig
	shards      [poolShards]*poolShard
	stats       PoolStats
	closed      int32
	cleanupDone chan struct{}

	waitTimeEMA    atomic.Uint64
	waitTimeCount  atomic.Uint64
	waitTimeMax    atomic.Uint64
	holdTimeEMA    atomic.Uint64
	holdTimeCount  atomic.Uint64
	requestCount   atomic.Uint64
	lastResetTime  atomic.Int64
	alphaEMA       float64
	windowDuration time.Duration

	adaptive     *adaptive
	debugStats   PoolDebugStats
	debugEnabled atomic.Bool
}

type poolShard struct {
	mu     sync.RWMutex
	pools  map[string]*addrPool
	active map[Conn]struct{}
}

func (p *connPool) getShard(address string) *poolShard {
	hash := uint32(0)
	for i := 0; i < len(address); i++ {
		hash = hash*31 + uint32(address[i])
	}
	return p.shards[hash%poolShards]
}

type idleConn struct {
	conn      Conn
	idleSince time.Time
}

type addrPool struct {
	mu      sync.Mutex
	idle    []idleConn
	active  int
	waiters []chan struct{}
}

func NewConnPool(cfg PoolConfig) ConnPool {
	p := &connPool{
		cfg:            cfg,
		cleanupDone:    make(chan struct{}),
		alphaEMA:       0.2,
		windowDuration: 10 * time.Second,
	}
	if debugEnabled := os.Getenv("DEBUG_POOL"); debugEnabled == "true" {
		p.debugEnabled.Store(true)
	}
	for i := 0; i < poolShards; i++ {
		p.shards[i] = &poolShard{
			pools:  make(map[string]*addrPool),
			active: make(map[Conn]struct{}),
		}
	}
	if cfg.CleanupInterval > 0 {
		go p.cleanupLoop()
	}
	return p
}

func (p *connPool) EnableAdaptive(cfg AdaptiveCfg) {
	p.adaptive = newAdaptive(cfg)
	go p.adjustLoop()
}

func (p *connPool) adjustLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if atomic.LoadInt32(&p.closed) != 0 {
				return
			}
			if p.adaptive != nil {
				stats := p.Stats()
				newSize := p.adaptive.adjust(stats, stats)
				if newSize != p.cfg.MaxActive {
					p.cfg.MaxActive = newSize
				}
			}
			if p.debugEnabled.Load() {
				p.updateDebugStats()
			}
		case <-p.cleanupDone:
			return
		}
	}
}

func (p *connPool) cleanupLoop() {
	ticker := time.NewTicker(p.cfg.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if atomic.LoadInt32(&p.closed) != 0 {
				close(p.cleanupDone)
				return
			}
			p.cleanupIdleConns()
			if p.debugEnabled.Load() {
				p.updateDebugStats()
			}
		case <-p.cleanupDone:
			return
		}
	}
}

func (p *connPool) cleanupIdleConns() {
	now := time.Now()
	closedCount := 0

	for _, shard := range p.shards {
		shard.mu.RLock()
		pools := make([]*addrPool, 0, len(shard.pools))
		for _, ap := range shard.pools {
			pools = append(pools, ap)
		}
		shard.mu.RUnlock()

		for _, ap := range pools {
			ap.mu.Lock()
			validIdle := ap.idle[:0]
			for _, ic := range ap.idle {
				if p.cfg.IdleTimeout > 0 && now.Sub(ic.idleSince) > p.cfg.IdleTimeout {
					atomic.AddInt64(&p.stats.Idle, -1)
					atomic.AddInt64(&p.stats.Total, -1)
					atomic.AddUint64(&p.stats.Closed, 1)
					_ = ic.conn.Close()
					closedCount++
					continue
				}
				validIdle = append(validIdle, ic)
			}
			ap.idle = validIdle

			if len(ap.waiters) > 0 && ap.active < p.cfg.MaxActive {
				batchSize := len(ap.waiters)
				available := p.cfg.MaxActive - ap.active
				if batchSize > available {
					batchSize = available
				}
				if batchSize > 10 {
					batchSize = 10
				}
				waiters := ap.waiters[:batchSize]
				ap.waiters = ap.waiters[batchSize:]
				atomic.AddInt64(&p.stats.Waiters, -int64(batchSize))
				for _, waiter := range waiters {
					select {
					case waiter <- struct{}{}:
					default:
					}
				}
			}
			ap.mu.Unlock()
		}
	}

	if closedCount > 10 {
		runtime.GC()
	}
}

func (p *connPool) isConnectionHealthy(conn Conn) bool {
	if conn == nil {
		return false
	}

	if tcpConn, ok := conn.(*tcpConn); ok {
		now := time.Now().Unix()
		lastCheck := atomic.LoadInt64(&tcpConn.lastHealthCheck)
		if now-lastCheck < 30 {
			return atomic.LoadInt32(&tcpConn.healthCheckCached) == 1
		}

		addr := tcpConn.conn.RemoteAddr()
		if addr == nil {
			atomic.StoreInt64(&tcpConn.lastHealthCheck, now)
			atomic.StoreInt32(&tcpConn.healthCheckCached, 0)
			return false
		}

		testDeadline := time.Now().Add(1 * time.Millisecond)
		if err := tcpConn.conn.SetReadDeadline(testDeadline); err != nil {
			atomic.StoreInt64(&tcpConn.lastHealthCheck, now)
			atomic.StoreInt32(&tcpConn.healthCheckCached, 0)
			return false
		}
		_ = tcpConn.conn.SetReadDeadline(time.Time{})

		atomic.StoreInt64(&tcpConn.lastHealthCheck, now)
		atomic.StoreInt32(&tcpConn.healthCheckCached, 1)
		return true
	}

	if err := conn.SetReadDeadline(time.Now().Add(1 * time.Millisecond)); err != nil {
		return false
	}
	_ = conn.SetReadDeadline(time.Time{})
	return true
}

func (p *connPool) Get(ctx context.Context, address string) (Conn, error) {
	if atomic.LoadInt32(&p.closed) != 0 {
		return nil, ErrPoolClosed
	}

	p.requestCount.Add(1)
	if p.debugEnabled.Load() {
		p.debugStats.GetAttempts.Add(1)
	}
	waitStart := time.Now()
	defer func() {
		p.recordWait(time.Since(waitStart))
	}()

	shard := p.getShard(address)
	shard.mu.Lock()

	ap := shard.pools[address]
	if ap == nil {
		ap = &addrPool{}
		shard.pools[address] = ap
	}
	shard.mu.Unlock()

	ap.mu.Lock()

	if n := len(ap.idle); n > 0 {
		ic := ap.idle[n-1]
		ap.idle = ap.idle[:n-1]
		ap.mu.Unlock()
		shard.mu.Lock()
		p.activateConn(shard, ap, ic.conn)
		shard.mu.Unlock()
		if p.debugEnabled.Load() {
			p.debugStats.GetSuccess.Add(1)
		}
		return ic.conn, nil
	}

	if p.cfg.MaxActive > 0 && ap.active >= p.cfg.MaxActive {
		if p.cfg.WaitTimeout > 0 {
			waiter := make(chan struct{}, 1)
			ap.waiters = append(ap.waiters, waiter)
			waitersCount := len(ap.waiters)
			atomic.AddInt64(&p.stats.Waiters, 1)
			ap.mu.Unlock()

			waitTimeout := p.cfg.WaitTimeout
			if waitersCount > 100 {
				waitTimeout = waitTimeout * 2
			} else if waitersCount > 50 {
				waitTimeout = waitTimeout * 3 / 2
			}
			if ctx != nil {
				if ctxDeadline, ok := ctx.Deadline(); ok {
					ctxTimeout := time.Until(ctxDeadline)
					if ctxTimeout < waitTimeout {
						waitTimeout = ctxTimeout
					}
				}
			}

			select {
			case <-waiter:
				atomic.AddInt64(&p.stats.Waiters, -1)
				return p.Get(ctx, address)
			case <-time.After(waitTimeout):
				atomic.AddInt64(&p.stats.Waiters, -1)
				p.removeWaiter(shard, ap, waiter)
				if p.debugEnabled.Load() {
					p.debugStats.GetTimeout.Add(1)
					p.debugStats.GetExhausted.Add(1)
				}
				return nil, ErrPoolExhausted
			case <-ctx.Done():
				atomic.AddInt64(&p.stats.Waiters, -1)
				p.removeWaiter(shard, ap, waiter)
				if p.debugEnabled.Load() {
					p.debugStats.GetContextCancel.Add(1)
				}
				return nil, ctx.Err()
			}
		} else {
			ap.mu.Unlock()
			if p.debugEnabled.Load() {
				p.debugStats.GetExhausted.Add(1)
			}
			return nil, ErrPoolExhausted
		}
	}
	ap.active++
	atomic.AddInt64(&p.stats.Active, 1)
	atomic.AddUint64(&p.stats.Created, 1)
	ap.mu.Unlock()

	conn, err := p.cfg.Transport.Dial(ctx, address)
	if err != nil {
		ap.mu.Lock()
		ap.active--
		ap.mu.Unlock()
		atomic.AddInt64(&p.stats.Active, -1)
		atomic.AddUint64(&p.stats.Errors, 1)
		if p.debugEnabled.Load() {
			p.debugStats.GetDialError.Add(1)
		}
		return nil, err
	}

	atomic.AddInt64(&p.stats.Total, 1)
	shard.mu.Lock()
	shard.active[conn] = struct{}{}
	shard.mu.Unlock()
	if p.debugEnabled.Load() {
		p.debugStats.GetSuccess.Add(1)
	}
	return conn, nil
}

func (p *connPool) activateConn(shard *poolShard, ap *addrPool, conn Conn) {
	ap.mu.Lock()
	ap.active++
	ap.mu.Unlock()
	atomic.AddInt64(&p.stats.Active, 1)
	atomic.AddInt64(&p.stats.Idle, -1)
	shard.active[conn] = struct{}{}
}

func (p *connPool) removeWaiter(shard *poolShard, ap *addrPool, waiter chan struct{}) {
	ap.mu.Lock()
	defer ap.mu.Unlock()

	for i, w := range ap.waiters {
		if w == waiter {
			ap.waiters = append(ap.waiters[:i], ap.waiters[i+1:]...)
			return
		}
	}
}

func (p *connPool) Put(conn Conn) {
	if conn == nil {
		return
	}

	if atomic.LoadInt32(&p.closed) != 0 {
		_ = conn.Close()
		if p.debugEnabled.Load() {
			p.debugStats.PutClosed.Add(1)
		}
		return
	}

	if holdTimeConn, ok := conn.(interface{ HoldTime() time.Duration }); ok {
		p.recordHold(holdTimeConn.HoldTime())
	}

	addr := conn.RemoteAddr()
	shard := p.getShard(addr)
	shard.mu.Lock()

	delete(shard.active, conn)

	ap := shard.pools[addr]
	if ap == nil {
		ap = &addrPool{}
		shard.pools[addr] = ap
	}
	shard.mu.Unlock()

	ap.mu.Lock()
	ap.active--
	atomic.AddInt64(&p.stats.Active, -1)

	if p.cfg.MaxIdle > 0 && len(ap.idle) < p.cfg.MaxIdle {
		if !p.isConnectionHealthy(conn) {
			atomic.AddUint64(&p.stats.Closed, 1)
			atomic.AddInt64(&p.stats.Total, -1)
			ap.mu.Unlock()
			_ = conn.Close()
			return
		}

		ap.idle = append(ap.idle, idleConn{
			conn:      conn,
			idleSince: time.Now(),
		})
		atomic.AddInt64(&p.stats.Idle, 1)
		if p.debugEnabled.Load() {
			p.debugStats.PutSuccess.Add(1)
		}

		if len(ap.waiters) > 0 {
			batchSize := len(ap.waiters)
			if batchSize > 10 {
				batchSize = 10
			}
			waiters := ap.waiters[:batchSize]
			ap.waiters = ap.waiters[batchSize:]
			atomic.AddInt64(&p.stats.Waiters, -int64(batchSize))
			ap.mu.Unlock()
			for _, waiter := range waiters {
				select {
				case waiter <- struct{}{}:
				default:
				}
			}
			return
		}
		ap.mu.Unlock()
		return
	}

	ap.mu.Unlock()
	atomic.AddUint64(&p.stats.Closed, 1)
	atomic.AddInt64(&p.stats.Total, -1)
	_ = conn.Close()
	if p.debugEnabled.Load() {
		p.debugStats.PutClosed.Add(1)
	}
}

func (p *connPool) Remove(conn Conn) {
	if conn == nil {
		return
	}

	addr := conn.RemoteAddr()
	shard := p.getShard(addr)
	shard.mu.Lock()

	delete(shard.active, conn)

	ap := shard.pools[addr]
	if ap == nil {
		shard.mu.Unlock()
		_ = conn.Close()
		return
	}
	shard.mu.Unlock()

	ap.mu.Lock()
	if ap.active > 0 {
		ap.active--
		atomic.AddInt64(&p.stats.Active, -1)
	} else {
		for i, ic := range ap.idle {
			if ic.conn == conn {
				ap.idle = append(ap.idle[:i], ap.idle[i+1:]...)
				atomic.AddInt64(&p.stats.Idle, -1)
				break
			}
		}
	}
	ap.mu.Unlock()
	atomic.AddInt64(&p.stats.Total, -1)
	atomic.AddUint64(&p.stats.Closed, 1)
	// Removed frequent debug log "connPool: removed connection" - too verbose for production
	_ = conn.Close()
}

func (p *connPool) Close() error {
	atomic.StoreInt32(&p.closed, 1)

	if p.cleanupDone != nil {
		select {
		case p.cleanupDone <- struct{}{}:
		default:
		}
	}

	var firstErr error
	var wg sync.WaitGroup

	for _, shard := range p.shards {
		shard.mu.Lock()
		var allConns []Conn
		for _, ap := range shard.pools {
			for _, ic := range ap.idle {
				allConns = append(allConns, ic.conn)
			}
		}
		for conn := range shard.active {
			allConns = append(allConns, conn)
		}
		shard.pools = make(map[string]*addrPool)
		shard.active = make(map[Conn]struct{})
		shard.mu.Unlock()

		if len(allConns) > 0 {
			const maxWorkers = 50
			connCh := make(chan Conn, len(allConns))
			for _, c := range allConns {
				connCh <- c
			}
			close(connCh)

			workerCount := len(allConns)
			if workerCount > maxWorkers {
				workerCount = maxWorkers
			}

			for i := 0; i < workerCount; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for conn := range connCh {
						if err := conn.Close(); err != nil && firstErr == nil {
							firstErr = err
						}
					}
				}()
			}
		}
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}

	atomic.StoreInt64(&p.stats.Total, 0)
	atomic.StoreInt64(&p.stats.Active, 0)
	atomic.StoreInt64(&p.stats.Idle, 0)
	atomic.StoreInt64(&p.stats.Waiters, 0)
	atomic.StoreUint64(&p.stats.Created, 0)
	atomic.StoreUint64(&p.stats.Closed, 0)
	atomic.StoreUint64(&p.stats.Errors, 0)
	return firstErr
}

func (p *connPool) Stats() PoolStats {
	stats := PoolStats{
		Total:   atomic.LoadInt64(&p.stats.Total),
		Active:  atomic.LoadInt64(&p.stats.Active),
		Idle:    atomic.LoadInt64(&p.stats.Idle),
		Waiters: atomic.LoadInt64(&p.stats.Waiters),
		Created: atomic.LoadUint64(&p.stats.Created),
		Closed:  atomic.LoadUint64(&p.stats.Closed),
		Errors:  atomic.LoadUint64(&p.stats.Errors),
	}

	// Add performance stats
	perf := p.metrics()
	stats.AvgWaitTime = perf.AvgWaitTime
	stats.MaxWaitTime = perf.MaxWaitTime
	stats.AvgHoldTime = perf.AvgHoldTime
	stats.RequestRate = perf.RequestRate
	stats.WaitSamples = perf.WaitSamples
	stats.HoldSamples = perf.HoldSamples

	return stats
}

// Metrics is deprecated, use Stats instead.
func (p *connPool) Metrics() PoolMetrics {
	return p.metrics()
}

func (p *connPool) recordWait(d time.Duration) {
	if d < 0 {
		return
	}
	ns := uint64(d.Nanoseconds())

	for {
		old := p.waitTimeMax.Load()
		if ns <= old {
			break
		}
		if p.waitTimeMax.CompareAndSwap(old, ns) {
			break
		}
	}

	p.updateEMA(&p.waitTimeEMA, ns, &p.waitTimeCount)
}

func (p *connPool) recordHold(d time.Duration) {
	if d < 0 {
		return
	}
	p.updateEMA(&p.holdTimeEMA, uint64(d.Nanoseconds()), &p.holdTimeCount)
}

func (p *connPool) updateEMA(ema *atomic.Uint64, ns uint64, count *atomic.Uint64) {
	for {
		oldEMA := ema.Load()
		var newEMA uint64
		if oldEMA == 0 {
			newEMA = ns
		} else {
			newEMA = uint64(float64(ns)*p.alphaEMA + float64(oldEMA)*(1-p.alphaEMA))
		}
		if ema.CompareAndSwap(oldEMA, newEMA) {
			count.Add(1)
			break
		}
	}
}

func (p *connPool) metrics() PoolStats {
	now := time.Now().UnixNano()
	lastReset := p.lastResetTime.Load()

	var rate float64
	if lastReset == 0 {
		p.lastResetTime.Store(now)
	} else {
		elapsed := time.Duration(now - lastReset)
		if elapsed >= p.windowDuration {
			oldCount := p.requestCount.Swap(0)
			p.lastResetTime.Store(now)
			rate = float64(oldCount) / p.windowDuration.Seconds()
		} else if elapsed > 0 {
			rate = float64(p.requestCount.Load()) / elapsed.Seconds()
		}
	}

	return PoolStats{
		AvgWaitTime: time.Duration(p.waitTimeEMA.Load()),
		MaxWaitTime: time.Duration(p.waitTimeMax.Load()),
		AvgHoldTime: time.Duration(p.holdTimeEMA.Load()),
		RequestRate: rate,
		WaitSamples: p.waitTimeCount.Load(),
		HoldSamples: p.holdTimeCount.Load(),
	}
}

// PoolMetrics is deprecated, use PoolStats instead.
type PoolMetrics = PoolStats

type AdaptiveCfg struct {
	MinSize        int
	MaxSize        int
	InitialSize    int
	TargetWaitTime time.Duration
	HighThreshold  time.Duration
	LowThreshold   time.Duration
	IncreaseStep   int
	DecreaseStep   int
	CooldownPeriod time.Duration
	EMAAlpha       float64
}

func DefaultAdaptive() AdaptiveCfg {
	return AdaptiveCfg{
		MinSize:        100,
		MaxSize:        20000,
		InitialSize:    2000,
		TargetWaitTime: 20 * time.Millisecond,
		HighThreshold:  50 * time.Millisecond,
		LowThreshold:   10 * time.Millisecond,
		IncreaseStep:   200,
		DecreaseStep:   100,
		CooldownPeriod: 10 * time.Second,
		EMAAlpha:       0.2,
	}
}

type adaptive struct {
	minSize, maxSize int
	targetWaitTime   time.Duration
	highThreshold    time.Duration
	lowThreshold     time.Duration
	currentSize      int
	emaWaitTime      atomic.Uint64
	emaUtilization   atomic.Uint64
	alpha            float64
	increaseStep     int
	decreaseStep     int
	cooldownPeriod   time.Duration
	lastAdjustTime   atomic.Int64
	mu               sync.Mutex
}

func newAdaptive(cfg AdaptiveCfg) *adaptive {
	if cfg.InitialSize < cfg.MinSize {
		cfg.InitialSize = cfg.MinSize
	}
	if cfg.InitialSize > cfg.MaxSize {
		cfg.InitialSize = cfg.MaxSize
	}
	if cfg.EMAAlpha <= 0 || cfg.EMAAlpha > 1 {
		cfg.EMAAlpha = 0.2
	}

	return &adaptive{
		minSize:        cfg.MinSize,
		maxSize:        cfg.MaxSize,
		targetWaitTime: cfg.TargetWaitTime,
		highThreshold:  cfg.HighThreshold,
		lowThreshold:   cfg.LowThreshold,
		currentSize:    cfg.InitialSize,
		alpha:          cfg.EMAAlpha,
		increaseStep:   cfg.IncreaseStep,
		decreaseStep:   cfg.DecreaseStep,
		cooldownPeriod: cfg.CooldownPeriod,
	}
}

func (a *adaptive) adjust(stats PoolStats, perfStats PoolStats) int {
	now := time.Now()
	lastAdjust := time.Unix(0, a.lastAdjustTime.Load())

	if now.Sub(lastAdjust) < a.cooldownPeriod {
		return a.currentSize
	}

	a.updateEMA(&a.emaWaitTime, uint64(perfStats.AvgWaitTime.Nanoseconds()))

	utilization := float64(0)
	if a.currentSize > 0 {
		utilization = float64(stats.Active) / float64(a.currentSize) * 10000
	}
	a.updateEMA(&a.emaUtilization, uint64(utilization))

	emaWait := time.Duration(a.emaWaitTime.Load())
	emaUtil := float64(a.emaUtilization.Load()) / 10000

	a.mu.Lock()
	defer a.mu.Unlock()

	newSize := a.currentSize

	if (emaWait > a.highThreshold || emaUtil > 0.8 || stats.Waiters > 5) &&
		a.currentSize < a.maxSize {
		newSize = minInt(a.currentSize+a.increaseStep, a.maxSize)
	}

	if emaWait < a.lowThreshold && emaUtil < 0.5 &&
		stats.Idle > int64(float64(a.currentSize)*0.3) &&
		a.currentSize > a.minSize {
		newSize = maxInt(a.currentSize-a.decreaseStep, a.minSize)
	}

	if newSize != a.currentSize {
		a.currentSize = newSize
		a.lastAdjustTime.Store(now.UnixNano())
	}

	return a.currentSize
}

func (a *adaptive) size() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.currentSize
}

func (a *adaptive) updateEMA(ema *atomic.Uint64, val uint64) {
	for {
		oldEMA := ema.Load()
		var newEMA uint64
		if oldEMA == 0 {
			newEMA = val
		} else {
			newEMA = uint64(float64(val)*a.alpha + float64(oldEMA)*(1-a.alpha))
		}
		if ema.CompareAndSwap(oldEMA, newEMA) {
			break
		}
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
