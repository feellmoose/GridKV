package transport

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feellmoose/gridkv/internal/utils/logging"
	"github.com/panjf2000/gnet/v2"
)

// Each buffer is 4KB which handles most messages efficiently
var gnetWriteBufferPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 4096) // 4KB buffer
		return &buf
	},
}

type GnetTransport struct {
	client  *gnet.Client
	metrics *GnetMetrics
}

func NewGnetTransport() (*GnetTransport, error) {
	// Create client with event loops to avoid load balancer panics
	client, err := gnet.NewClient(
		&gnet.BuiltinEventEngine{},
		gnet.WithMulticore(true),                // Enable multi-core
		gnet.WithLoadBalancing(gnet.RoundRobin), // RoundRobin for client
	)
	if err != nil {
		return nil, fmt.Errorf("gnet client init failed: %w", err)
	}

	// Start the client engine
	go client.Start()

	// Give client time to initialize
	time.Sleep(100 * time.Millisecond)

	return &GnetTransport{
		client:  client,
		metrics: NewGnetMetrics(),
	}, nil
}

// GetMetrics returns current metrics snapshot (inlined cold path)
//
//go:inline
func (t *GnetTransport) GetMetrics() MetricsSnapshot {
	if t.metrics == nil {
		return MetricsSnapshot{}
	}
	return t.metrics.Snapshot()
}

// Dial creates a new connection to the specified address
func (t *GnetTransport) Dial(address string) (TransportConn, error) {
	conn, err := t.client.Dial("tcp", address)
	if err != nil {
		if t.metrics != nil {
			t.metrics.RecordConnectionFailed()
		}
		return nil, err // Return error directly, no wrapping
	}

	if t.metrics != nil {
		t.metrics.RecordConnection(1)
	}

	tc := &GnetTransportConn{
		conn:    conn,
		metrics: t.metrics,
	}
	tc.metricsEnabled.Store(t.metrics != nil)

	return tc, nil
}

func (t *GnetTransport) Listen(address string) (TransportListener, error) {
	return NewGnetTransportListener(address)
}

type GnetTransportConn struct {
	conn           gnet.Conn
	metrics        *GnetMetrics
	metricsEnabled atomic.Bool // OPTIMIZATION: Cache metrics check
}

// WriteDataWithContext writes data with context support
//
//go:noinline
func (t *GnetTransportConn) WriteDataWithContext(ctx context.Context, data []byte) error {
	dataLen := len(data)
	if dataLen == 0 {
		return nil
	}

	totalLen := 4 + dataLen

	var buf []byte
	var pooledBuf *[]byte

	if totalLen <= 4096 {
		// Fast path: Use pooled buffer for messages <= 4KB (95%+ of cases)
		pooledBuf = gnetWriteBufferPool.Get().(*[]byte)
		buf = (*pooledBuf)[:totalLen]
		defer gnetWriteBufferPool.Put(pooledBuf)
	} else {
		// Slow path: Allocate for large messages (rare case)
		buf = make([]byte, totalLen)
	}

	// Write length prefix and data (inlined operations)
	binary.BigEndian.PutUint32(buf, uint32(dataLen))
	copy(buf[4:], data)

	// Single write syscall
	_, err := t.conn.Write(buf)

	// Check metrics enabled without additional Load() call
	if t.metricsEnabled.Load() && (t.metrics.MessagesSent.Load()&0x0F == 0) {
		if err != nil {
			t.metrics.RecordWriteError()
		} else {
			// Skip latency tracking for even better performance
			t.metrics.MessagesSent.Add(1)
			t.metrics.BytesSent.Add(int64(totalLen))
		}
	}

	return err // Return error directly without wrapping
}

func (t *GnetTransportConn) ReadDataWithContext(ctx context.Context) ([]byte, error) {
	// Not used in gnet async mode
	return nil, errors.New("gnet: ReadDataWithContext unsupported in async mode")
}

// Close closes the connection
//
//go:inline
func (t *GnetTransportConn) Close() error {
	if t.metrics != nil {
		t.metrics.RecordConnection(-1)
	}
	return t.conn.Close()
}

// HealthCheck checks if connection is healthy (inlined)
//
//go:inline
func (t *GnetTransportConn) HealthCheck() error {
	if t.conn == nil {
		return fmt.Errorf("connection is nil")
	}
	return nil
}

// LocalAddr returns local address (inlined)
//
//go:inline
func (t *GnetTransportConn) LocalAddr() net.Addr { return t.conn.LocalAddr() }

// RemoteAddr returns remote address (inlined)
//
//go:inline
func (t *GnetTransportConn) RemoteAddr() net.Addr { return t.conn.RemoteAddr() }

// GnetListenerOptions configures advanced gnet listener settings
type GnetListenerOptions struct {
	// Multicore enables multi-core load balancing (default: true)
	Multicore bool

	// NumEventLoop sets the number of event loops (default: runtime.NumCPU())
	// Use 0 for auto-detection
	NumEventLoop int

	// ReusePort enables SO_REUSEPORT for better load distribution (default: true)
	ReusePort bool

	// ReadBufferCap sets the read buffer capacity in bytes (default: 64KB)
	ReadBufferCap int

	// WriteBufferCap sets the write buffer capacity in bytes (default: 64KB)
	WriteBufferCap int

	// SocketRecvBuffer sets SO_RCVBUF size (default: system default)
	// Use larger values (e.g., 2MB) for high-throughput scenarios
	SocketRecvBuffer int

	// SocketSendBuffer sets SO_SNDBUF size (default: system default)
	// Use larger values (e.g., 2MB) for high-throughput scenarios
	SocketSendBuffer int

	// EdgeTriggered enables edge-triggered I/O mode for better performance (default: false)
	// IMPORTANT: Only supported on Linux with epoll
	EdgeTriggered bool

	// LockOSThread locks each event loop to an OS thread (default: false)
	// Useful for CPU-intensive workloads
	LockOSThread bool
}

// DefaultGnetListenerOptions returns optimized default settings
func DefaultGnetListenerOptions() *GnetListenerOptions {
	return &GnetListenerOptions{
		Multicore:        true,
		NumEventLoop:     0, // Auto-detect
		ReusePort:        true,
		ReadBufferCap:    65536,   // 64KB
		WriteBufferCap:   65536,   // 64KB
		SocketRecvBuffer: 2097152, // 2MB
		SocketSendBuffer: 2097152, // 2MB
		EdgeTriggered:    false,   // Conservative default
		LockOSThread:     false,
	}
}

type GnetTransportListener struct {
	address  *net.TCPAddr
	engine   gnet.Engine
	handler  func(message []byte) error
	metrics  *GnetMetrics
	options  *GnetListenerOptions
	running  bool
	started  sync.Once
	stopped  sync.Once
	bootedCh chan struct{} // Signal when gnet engine has booted
}

func NewGnetTransportListener(address string) (*GnetTransportListener, error) {
	return NewGnetTransportListenerWithOptions(address, DefaultGnetListenerOptions())
}

// NewGnetTransportListenerWithOptions creates a listener with custom options
func NewGnetTransportListenerWithOptions(address string, opts *GnetListenerOptions) (*GnetTransportListener, error) {
	addr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("resolve %s failed: %w", address, err)
	}

	if opts == nil {
		opts = DefaultGnetListenerOptions()
	}

	return &GnetTransportListener{
		address:  addr,
		metrics:  NewGnetMetrics(),
		options:  opts,
		bootedCh: make(chan struct{}),
	}, nil
}

// GetMetrics returns current metrics snapshot (inlined cold path)
//
//go:inline
func (l *GnetTransportListener) GetMetrics() MetricsSnapshot {
	if l.metrics == nil {
		return MetricsSnapshot{}
	}
	return l.metrics.Snapshot()
}

func (l *GnetTransportListener) Start() error {
	var startErr error
	l.started.Do(func() {
		logging.Debug("listener[gnet] starting", "listen_addr", l.address, "options", l.options)
		engineHandler := &GnetTransportEventEngine{
			listener: l,
			handler:  l.handler,
			address:  l.address,
			metrics:  l.metrics,
		}

		opts := l.options

		// Build gnet options slice
		gnetOpts := []gnet.Option{
			gnet.WithMulticore(opts.Multicore),
			gnet.WithReusePort(opts.ReusePort),
			gnet.WithTCPKeepAlive(60 * time.Second), // Increased from 30s for better connection stability
			gnet.WithTCPNoDelay(gnet.TCPNoDelay),
			gnet.WithLoadBalancing(gnet.LeastConnections), // OPTIMIZATION: LeastConnections performs better than RoundRobin
			gnet.WithReadBufferCap(opts.ReadBufferCap),
			gnet.WithWriteBufferCap(opts.WriteBufferCap),
		}

		if opts.NumEventLoop > 0 {
			gnetOpts = append(gnetOpts, gnet.WithNumEventLoop(opts.NumEventLoop))
		}

		if opts.SocketRecvBuffer > 0 {
			gnetOpts = append(gnetOpts, gnet.WithSocketRecvBuffer(opts.SocketRecvBuffer))
		}
		if opts.SocketSendBuffer > 0 {
			gnetOpts = append(gnetOpts, gnet.WithSocketSendBuffer(opts.SocketSendBuffer))
		}

		if opts.EdgeTriggered {
			gnetOpts = append(gnetOpts, gnet.WithEdgeTriggeredIO(true))
		}

		if opts.LockOSThread {
			gnetOpts = append(gnetOpts, gnet.WithLockOSThread(true))
		}

		// Run gnet.Run() in a goroutine to avoid blocking the main thread
		go func() {
			err := gnet.Run(engineHandler, "tcp://"+l.address.String(), gnetOpts...)
			if err != nil {
				logging.Error(err, "gnet.Run() failed", "listen_addr", l.address)
			}
		}()

		// Wait for the engine to boot (with timeout)
		select {
		case <-l.bootedCh:
			logging.Debug("listener[gnet] started successfully", "listen_addr", l.address)
		case <-time.After(5 * time.Second):
			startErr = fmt.Errorf("timeout waiting for gnet engine to boot")
		}
	})
	return startErr
}

func (l *GnetTransportListener) Stop() error {
	var stopErr error
	l.stopped.Do(func() {
		if !l.running {
			stopErr = errors.New("listener[gnet] not started")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		stopErr = l.engine.Stop(ctx)
		if stopErr == nil {
			logging.Debug("listener[gnet] stopped", "listen_addr", l.address)
		}
	})
	return stopErr
}

// HandleMessage sets the message handler (inlined)
//
//go:inline
func (l *GnetTransportListener) HandleMessage(handler func(message []byte) error) TransportListener {
	l.handler = handler
	return l
}

// Addr returns the listener address (inlined)
//
//go:inline
func (l *GnetTransportListener) Addr() net.Addr { return l.address }

type GnetTransportEventEngine struct {
	gnet.BuiltinEventEngine
	listener *GnetTransportListener
	address  *net.TCPAddr
	handler  func([]byte) error
	metrics  *GnetMetrics
}

func (es *GnetTransportEventEngine) OnBoot(engine gnet.Engine) (action gnet.Action) {
	es.listener.engine = engine
	es.listener.running = true
	logging.Debug("listener[gnet] booted", "listen_addr", es.address)

	// Signal that the engine has booted successfully
	close(es.listener.bootedCh)

	return gnet.None
}

func (es *GnetTransportEventEngine) OnShutdown(engine gnet.Engine) {
	es.listener.running = false
	logging.Debug("listener[gnet] shutdown", "listen_addr", es.address)
}

func (es *GnetTransportEventEngine) OnOpen(conn gnet.Conn) (out []byte, action gnet.Action) {
	if logging.Log.IsDebugEnabled() {
		logging.Debug("gnet connection opened", "remote_addr", conn.RemoteAddr().String())
	}

	if es.metrics != nil {
		es.metrics.RecordConnection(1)
	}

	return nil, gnet.None
}

func (es *GnetTransportEventEngine) OnClose(conn gnet.Conn, err error) (action gnet.Action) {
	if es.metrics != nil {
		es.metrics.RecordConnection(-1)
	}

	if err != nil {
		logging.Debug("gnet connection closed with error", "err", err, "remote_addr", conn.RemoteAddr().String())
	} else if logging.Log.IsDebugEnabled() {
		logging.Debug("gnet connection closed", "remote_addr", conn.RemoteAddr().String())
	}
	return gnet.None
}

// OnTraffic handles incoming data - HOT PATH with extreme optimization
//
//go:noinline
func (es *GnetTransportEventEngine) OnTraffic(conn gnet.Conn) (action gnet.Action) {
	// Fast check: handler must be set
	handler := es.handler
	if handler == nil {
		return gnet.None
	}

	// This amortizes event loop overhead across multiple messages
	const maxBatch = 32
	var processed int

	for processed < maxBatch {
		// Fast path: Check if we have enough bytes for header (4 bytes)
		buffered := conn.InboundBuffered()
		if buffered < 4 {
			return gnet.None
		}

		prefix, _ := conn.Peek(4)
		msgLen := int(binary.BigEndian.Uint32(prefix))

		// Single branch: combines negative and too large checks
		if uint(msgLen) > 10*1024*1024 {
			// Cold path: validation failed
			if es.metrics != nil {
				es.metrics.RecordReadError()
			}
			return gnet.Close
		}

		// Check if complete message is available
		totalNeeded := 4 + msgLen
		if buffered < totalNeeded {
			return gnet.None
		}

		// Discard length prefix and peek message (zero-copy)
		conn.Discard(4)
		message, _ := conn.Peek(msgLen)

		// Sample every 16th message (6.25% sampling)
		if es.metrics != nil && (processed&0x0F == 0) {
			es.metrics.MessagesReceived.Add(1)
			es.metrics.BytesReceived.Add(int64(msgLen))
		}

		// CRITICAL: Zero-copy handler - message buffer only valid until next Discard
		// Handler MUST NOT retain references to the message slice
		if err := handler(message); err != nil {
			// Cold path: handler error
			if es.metrics != nil {
				es.metrics.RecordHandlerError()
			}
			return gnet.Close
		}

		// Discard processed message
		conn.Discard(msgLen)
		processed++
	}

	// Processed max batch - return to allow other connections to be serviced
	return gnet.None
}
