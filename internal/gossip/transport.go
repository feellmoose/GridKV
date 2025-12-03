package gossip

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feellmoose/gridkv/internal/metrics"
	"github.com/feellmoose/gridkv/internal/transport"
	"github.com/feellmoose/gridkv/internal/utils/logging"
	"github.com/klauspost/compress/zstd"
)

var serializationBufferPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 0, 8192)
		return &buf
	},
}

var protoMarshalBufferPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 0, 8192)
		return &buf
	},
}

var protoUnmarshalBufferPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 0, 8192)
		return &buf
	},
}

var (
	compressEncoderPool  sync.Pool
	compressDecoderPool  sync.Pool
	compressionEnabled   = true
	compressionThreshold = 1024
)

func init() {
	compressEncoderPool.New = func() interface{} {
		encoder, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
		return encoder
	}
	compressDecoderPool.New = func() interface{} {
		decoder, _ := zstd.NewReader(nil)
		return decoder
	}
}

type NetworkBackendType int

const (
	TCP  NetworkBackendType = 1
	QUIC NetworkBackendType = 3 // QUIC with UDP fallback
	UDP  NetworkBackendType = 4
)

type NetworkOptions struct {
	Type           NetworkBackendType
	BindAddr       string
	EncryptEnabled bool
	MaxIdle        int
	MaxConns       int
	Timeout        time.Duration
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
}

type TransportProtocol struct {
	opts           *NetworkOptions
	transport      transport.Transport
	listener       transport.TransportListener
	pools          sync.Map
	stopOnce       sync.Once
	poolMu         sync.RWMutex
	maxPools       int
	cleanupRunning atomic.Bool // ensure only one cleanup runs at a time
}

func NewTransportProtocol(opts *NetworkOptions) (*TransportProtocol, error) {
	var (
		tr  transport.Transport
		err error
	)
	switch opts.Type {
	case TCP:
		tr, err = transport.NewTransport("tcp")
		if err != nil {
			return nil, fmt.Errorf("failed to create TCP transport: %w", err)
		}
	case QUIC:
		tr, err = transport.NewTransport("quic")
		if err != nil {
			return nil, fmt.Errorf("failed to create QUIC transport: %w", err)
		}
	case UDP:
		tr, err = transport.NewTransport("udp")
		if err != nil {
			return nil, fmt.Errorf("failed to create UDP transport: %w", err)
		}
	default:
		return nil, fmt.Errorf("invalid network type %v (supported: TCP, QUIC, UDP)", opts.Type)
	}

	listener, err := tr.Listen(opts.BindAddr)
	if err != nil {
		return nil, err
	}

	// Limit max pools to reduce goroutine count
	// Each pool has a cleanupLoop goroutine, so we need to limit pool count
	// For large clusters, use fewer pools per node to reduce total goroutine count
	maxPools := 10 // Further reduced to limit cleanupLoop goroutines in large clusters
	if opts.MaxConns > 0 {
		// Calculate based on MaxConns, but cap to prevent excessive goroutines
		calculated := opts.MaxConns / 100 // Increased divisor to reduce pool count
		if calculated < 3 {
			calculated = 3
		}
		if calculated > 15 {
			calculated = 15 // Further reduced to limit cleanupLoop goroutines
		}
		maxPools = calculated
	}

	return &TransportProtocol{
		opts:      opts,
		transport: tr,
		listener:  listener,
		maxPools:  maxPools,
	}, nil
}

func (p *TransportProtocol) getPool(address string) *transport.ConnPool {
	// Adaptive maxConns: increase for large clusters
	maxConns := p.opts.MaxConns
	if maxConns < 1000 {
		// For large clusters, increase default to handle high concurrency
		maxConns = 2000
	}

	v, loaded := p.pools.LoadOrStore(address,
		transport.NewConnPool(p.transport, address, p.opts.MaxIdle, maxConns, p.opts.Timeout))

	pool := v.(*transport.ConnPool)

	if !loaded {
		// Prewarm pool for better initial performance
		// Increased prewarm count for better connection availability
		prewarmCount := p.opts.MaxIdle / 2 // Prewarm 50% of maxIdle (increased from 25%)
		if prewarmCount < 3 {
			prewarmCount = 3 // Increased minimum from 2
		}
		if prewarmCount > 20 {
			prewarmCount = 20 // Increased cap from 10 to 20 for seed nodes
		}
		go pool.Prewarm(prewarmCount)

		p.poolMu.Lock()
		count := 0
		p.pools.Range(func(_, _ interface{}) bool {
			count++
			return count < p.maxPools+1
		})
		if count > p.maxPools {
			// Run cleanup in background to avoid blocking
			go p.cleanupOldPools()
		}
		p.poolMu.Unlock()
	} else {
		// Check if we need to cleanup old pools
		p.poolMu.RLock()
		count := 0
		p.pools.Range(func(_, _ interface{}) bool {
			count++
			return count < p.maxPools+1
		})
		needsCleanup := count > p.maxPools
		p.poolMu.RUnlock()

		if needsCleanup {
			// Schedule cleanup with single-flight guard to avoid goroutine explosion.
			p.scheduleCleanup()
		}
	}

	return pool
}

// scheduleCleanup starts cleanupOldPools if no other cleanup is running.
// This prevents unbounded goroutine growth under high churn.
func (p *TransportProtocol) scheduleCleanup() {
	if !p.cleanupRunning.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer p.cleanupRunning.Store(false)
		p.cleanupOldPools()
	}()
}

func (p *TransportProtocol) cleanupOldPools() {
	type poolEntry struct {
		address string
		pool    *transport.ConnPool
		total   int
		idle    int
	}

	var entries []poolEntry
	p.pools.Range(func(key, value interface{}) bool {
		pool := value.(*transport.ConnPool)
		stats := pool.GetStats()
		totalConn, _ := stats["total_connections"].(int)
		idleConn, _ := stats["idle_connections"].(int)

		// Close pools with no connections (both total and idle are 0)
		// This helps reduce the number of cleanupLoop goroutines
		if totalConn == 0 && idleConn == 0 {
			entries = append(entries, poolEntry{
				address: key.(string),
				pool:    pool,
				total:   totalConn,
				idle:    idleConn,
			})
		}
		return len(entries) < 100 // Increased from 50 to clean more aggressively
	})

	// Sort by connection count (close empty pools first)
	// This ensures we close pools with no connections first
	for _, entry := range entries {
		entry.pool.Close()
		p.pools.Delete(entry.address)
	}

	// If we still have too many pools, close more aggressively
	p.poolMu.RLock()
	count := 0
	p.pools.Range(func(_, _ interface{}) bool {
		count++
		return true
	})
	p.poolMu.RUnlock()

	if count > p.maxPools {
		// Close pools with minimal connections (idle only, no active)
		// More aggressive cleanup for large clusters
		var moreEntries []poolEntry
		p.pools.Range(func(key, value interface{}) bool {
			pool := value.(*transport.ConnPool)
			stats := pool.GetStats()
			totalConn, _ := stats["total_connections"].(int)
			idleConn, _ := stats["idle_connections"].(int)

			// Close pools with only idle connections (no active connections)
			// Be more aggressive: close pools with <= 3 idle connections
			if totalConn > 0 && totalConn == idleConn && totalConn <= 3 {
				moreEntries = append(moreEntries, poolEntry{
					address: key.(string),
					pool:    pool,
				})
			}
			return len(moreEntries) < 100 // Increased from 50
		})

		for _, entry := range moreEntries {
			entry.pool.Close()
			p.pools.Delete(entry.address)
		}
	}
}

func (p *TransportProtocol) Send(ctx context.Context, address string, data []byte) error {
	pool := p.getPool(address)

	var conn transport.TransportConn
	var err error

	maxRetries := 2
	for i := 0; i < maxRetries; i++ {
		conn, err = pool.Get(ctx)
		if err == nil {
			break
		}

		if i < maxRetries-1 && strings.Contains(err.Error(), "connection pool exhausted") {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Millisecond):
				continue
			}
		}

		return fmt.Errorf("transport get connection failed: %w", err)
	}

	defer func() {
		if conn != nil {
			pool.Put(conn)
		}
	}()

	if healthCheckable, ok := conn.(transport.HealthCheckable); ok {
		if err := healthCheckable.HealthCheck(); err != nil {
			pool.Invalidate(conn)
			conn = nil
			return fmt.Errorf("connection health check failed: %w", err)
		}
	}

	if err := conn.WriteDataWithContext(ctx, data); err != nil {
		pool.Invalidate(conn)
		conn = nil
		if netErr, ok := err.(interface{ Timeout() bool }); ok && netErr.Timeout() {
			return fmt.Errorf("transport send failed: %w", err)
		}
		return fmt.Errorf("transport send failed: %w", err)
	}
	return nil
}

func (p *TransportProtocol) Listen(handler func(message []byte) error) error {
	return p.listener.HandleMessage(handler).Start()
}

func (p *TransportProtocol) Stop() {
	p.stopOnce.Do(func() {
		if p.listener != nil {
			// Listener.Stop() internally waits with WaitGroup (tcp.go, quic.go, udp.go)
			_ = p.listener.Stop()
		}

		var pools []*transport.ConnPool
		p.pools.Range(func(_, v any) bool {
			pool := v.(*transport.ConnPool)
			pools = append(pools, pool)
			return true
		})

		// Close pools sequentially - ConnPool.Close() has internal wait logic
		for _, pool := range pools {
			_ = pool.Close() // Has internal polling wait for in-use connections
		}
		// No additional sleep needed - Close() handles cleanup
	})
}

type Network interface {
	SendWithTimeout(addr string, msg *GossipMessage, timeout time.Duration) error
	Send(address string, msg *GossipMessage) error
	SendBinary(address string, msg *BinaryMessage) error
	SendRaw(ctx context.Context, address string, data []byte) error
	Listen(receiver func(msg *GossipMessage) error) error
	Stop() error
	SetUseBinaryProtocol(useBinary bool)
	SetMetrics(m *metrics.GridKVMetrics)
}

type NetworkImpl struct {
	opts              *NetworkOptions
	protocol          *TransportProtocol
	useBinaryProtocol bool
	metrics           *metrics.GridKVMetrics
}

func NewNetwork(opts *NetworkOptions) (Network, error) {
	protocol, err := NewTransportProtocol(opts)
	if err != nil {
		return nil, err
	}
	return &NetworkImpl{
		opts:              opts,
		protocol:          protocol,
		useBinaryProtocol: true, // Default to binary protocol
		metrics:           nil,  // Set via SetMetrics if needed
	}, nil
}

func NewNetworkWithBinary(opts *NetworkOptions, useBinary bool) (Network, error) {
	protocol, err := NewTransportProtocol(opts)
	if err != nil {
		return nil, err
	}
	return &NetworkImpl{
		opts:              opts,
		protocol:          protocol,
		useBinaryProtocol: useBinary,
		metrics:           nil, // Set via SetMetrics if needed
	}, nil
}

// SetMetrics sets the metrics instance for network byte tracking.
func (n *NetworkImpl) SetMetrics(m *metrics.GridKVMetrics) {
	n.metrics = m
}

func (n *NetworkImpl) Send(address string, msg *GossipMessage) error {
	ctx, cancel := context.WithTimeout(context.Background(), n.opts.WriteTimeout)
	defer cancel()

	var data []byte

	if n.useBinaryProtocol {
		binary := convertGossipMessageToBinary(msg)
		if binary == nil {
			return fmt.Errorf("failed to convert message to binary")
		}
		data = binary.Marshal()
		PutBinaryMessage(binary)
	} else {
		// Fallback: use binary protocol even if useBinaryProtocol is false
		// This ensures compatibility during transition
		binary := convertGossipMessageToBinary(msg)
		if binary == nil {
			return fmt.Errorf("failed to convert message to binary")
		}
		data = binary.Marshal()
		PutBinaryMessage(binary)
	}

	if err := n.protocol.Send(ctx, address, data); err != nil {
		logging.Error(err, "Error sending message", "address", address, "message_type", msg.Type)
		return err
	}

	// Record network bytes sent
	if n.metrics != nil {
		n.metrics.AddNetworkBytesSent(int64(len(data)))
	}

	return nil
}

func (n *NetworkImpl) Listen(receiver func(msg *GossipMessage) error) error {
	return n.protocol.Listen(func(data []byte) error {
		// Record network bytes received
		if n.metrics != nil {
			n.metrics.AddNetworkBytesReceived(int64(len(data)))
		}

		// Try binary protocol first (if enabled or if message looks like binary)
		if n.useBinaryProtocol || len(data) >= 21 {
			binaryMsg, err := UnmarshalBinaryMessage(data)
			if err == nil && binaryMsg.Type > 0 && binaryMsg.Type <= 9 {
				msg := convertBinaryToGossipMessage(binaryMsg)
				if msg != nil {
					return receiver(msg)
				}
			}
		}

		// If binary protocol fails and protobuf is disabled, return error
		return fmt.Errorf("deserialization failed: message is not in binary format and protobuf is disabled")
	})
}

func (n *NetworkImpl) SendBinary(address string, msg *BinaryMessage) error {
	ctx, cancel := context.WithTimeout(context.Background(), n.opts.WriteTimeout)
	defer cancel()

	data := msg.Marshal()
	return n.SendRaw(ctx, address, data)
}

func (n *NetworkImpl) SendRaw(ctx context.Context, address string, data []byte) error {
	if err := n.protocol.Send(ctx, address, data); err != nil {
		// During shutdown or connection pool exhaustion, connection refused is expected
		// Only log as error if it's not a transient connection issue
		errMsg := err.Error()
		isConnectionRefused := strings.Contains(errMsg, "connection refused") ||
			strings.Contains(errMsg, "connection pool exhausted") ||
			strings.Contains(errMsg, "connection pool closed")

		if isConnectionRefused {
			// Log at debug level to reduce spam during shutdown/failures
			if logging.Log.IsDebugEnabled() {
				logging.Debug("Connection refused during send (may be transient)", "address", address, "size", len(data), "err", err)
			}
		} else {
			logging.Error(err, "Error sending raw data", "address", address, "size", len(data))
		}
		return err
	}

	// Record network bytes sent
	if n.metrics != nil {
		n.metrics.AddNetworkBytesSent(int64(len(data)))
	}

	return nil
}

func (n *NetworkImpl) Stop() error {
	n.protocol.Stop()
	return nil
}

func (n *NetworkImpl) SetUseBinaryProtocol(useBinary bool) {
	n.useBinaryProtocol = useBinary
}

func (n *NetworkImpl) SendWithTimeout(addr string, msg *GossipMessage, timeout time.Duration) error {
	// Adaptive timeout: increase for large clusters or high-load scenarios
	adaptiveTimeout := timeout
	if timeout < 5*time.Second {
		// For short timeouts, increase for stability
		adaptiveTimeout = 5 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), adaptiveTimeout)
	defer cancel()

	var data []byte

	if n.useBinaryProtocol {
		binary := convertGossipMessageToBinary(msg)
		if binary == nil {
			return fmt.Errorf("failed to convert message to binary")
		}
		data = binary.Marshal()
		PutBinaryMessage(binary)
	} else {
		// Fallback: use binary protocol even if useBinaryProtocol is false
		// This ensures compatibility during transition
		binary := convertGossipMessageToBinary(msg)
		if binary == nil {
			return fmt.Errorf("failed to convert message to binary")
		}
		data = binary.Marshal()
		PutBinaryMessage(binary)
	}

	// Check message size before sending to avoid TCP limit and connection reset
	const maxMessageSize = 10 * 1024 * 1024 // 10MB max
	if len(data) > maxMessageSize {
		return fmt.Errorf("message too large: %d bytes (max: %d)", len(data), maxMessageSize)
	}

	if err := n.protocol.Send(ctx, addr, data); err != nil {
		// During shutdown, connection pool closed errors are expected
		// Only log as error if not during shutdown
		errMsg := err.Error()
		if strings.Contains(errMsg, "connection pool closed") {
			// During shutdown, this is expected - log at debug level
			if logging.Log.IsDebugEnabled() {
				logging.Debug("Connection pool closed during send (expected during shutdown)",
					"address", addr, "message_type", msg.Type)
			}
		} else if strings.Contains(errMsg, "connection pool exhausted") {
			// Pool exhausted - don't log as error, just return quickly
			// This prevents error spam and allows caller to handle gracefully
			if logging.Log.IsDebugEnabled() {
				logging.Debug("Connection pool exhausted during send", "address", addr, "message_type", msg.Type)
			}
		} else {
			logging.Error(err, "Error sending message with timeout", "address", addr, "message_type", msg.Type)
		}
		return err
	}

	// Record network bytes sent
	if n.metrics != nil {
		n.metrics.AddNetworkBytesSent(int64(len(data)))
	}

	return nil
}
