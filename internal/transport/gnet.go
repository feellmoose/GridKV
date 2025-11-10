package transport

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/feellmoose/gridkv/internal/utils/logging"
	"github.com/panjf2000/gnet/v2"
)

type GnetTransport struct {
	client  *gnet.Client
	metrics *GnetMetrics
}

func NewGnetTransport() (*GnetTransport, error) {
	client, err := gnet.NewClient(&gnet.BuiltinEventEngine{})
	if err != nil {
		return nil, fmt.Errorf("gnet client init failed: %w", err)
	}
	return &GnetTransport{
		client:  client,
		metrics: NewGnetMetrics(),
	}, nil
}

// GetMetrics returns current metrics snapshot
func (t *GnetTransport) GetMetrics() MetricsSnapshot {
	if t.metrics == nil {
		return MetricsSnapshot{}
	}
	return t.metrics.Snapshot()
}

func (t *GnetTransport) Dial(address string) (TransportConn, error) {
	conn, err := t.client.Dial("tcp", address)
	if err != nil {
		if t.metrics != nil {
			t.metrics.RecordConnectionFailed()
		}
		return nil, fmt.Errorf("dial failed: %w", err)
	}

	if t.metrics != nil {
		t.metrics.RecordConnection(1) // Active connection
	}

	return &GnetTransportConn{
		conn:    conn,
		metrics: t.metrics,
	}, nil
}

func (t *GnetTransport) Listen(address string) (TransportListener, error) {
	return NewGnetTransportListener(address)
}

type GnetTransportConn struct {
	conn    gnet.Conn
	metrics *GnetMetrics
}

func (t *GnetTransportConn) WriteDataWithContext(ctx context.Context, data []byte) error {
	if len(data) == 0 {
		return nil
	}

	start := time.Now()

	lengthPrefix := make([]byte, 4)
	binary.BigEndian.PutUint32(lengthPrefix, uint32(len(data)))

	// Merge into a single write to minimize syscalls
	buf := append(lengthPrefix, data...)
	_, err := t.conn.Write(buf)

	// Record metrics
	if t.metrics != nil {
		if err != nil {
			t.metrics.RecordError("write")
		} else {
			latency := time.Since(start)
			t.metrics.RecordWrite(int64(len(buf)), latency)
		}
	}

	if err != nil {
		return fmt.Errorf("gnet write failed: %w", err)
	}
	return nil
}

func (t *GnetTransportConn) ReadDataWithContext(ctx context.Context) ([]byte, error) {
	// Not used in gnet async mode
	return nil, errors.New("gnet: ReadDataWithContext unsupported in async mode")
}

func (t *GnetTransportConn) Close() error {
	if t.metrics != nil {
		t.metrics.RecordConnection(-1) // Decrement active connections
	}
	return t.conn.Close()
}

func (t *GnetTransportConn) HealthCheck() error {
	// gnet connections are managed by the engine
	// Consider healthy if not nil
	if t.conn == nil {
		return fmt.Errorf("connection is nil")
	}
	return nil
}

func (t *GnetTransportConn) LocalAddr() net.Addr  { return t.conn.LocalAddr() }
func (t *GnetTransportConn) RemoteAddr() net.Addr { return t.conn.RemoteAddr() }

type GnetTransportListener struct {
	address *net.TCPAddr
	engine  gnet.Engine
	handler func(message []byte) error
	metrics *GnetMetrics
	running bool
	started sync.Once
	stopped sync.Once
}

func NewGnetTransportListener(address string) (*GnetTransportListener, error) {
	addr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("resolve %s failed: %w", address, err)
	}
	return &GnetTransportListener{
		address: addr,
		metrics: NewGnetMetrics(),
	}, nil
}

// GetMetrics returns current metrics snapshot
func (l *GnetTransportListener) GetMetrics() MetricsSnapshot {
	if l.metrics == nil {
		return MetricsSnapshot{}
	}
	return l.metrics.Snapshot()
}

func (l *GnetTransportListener) Start() error {
	var startErr error
	l.started.Do(func() {
		logging.Debug("listener[gnet] starting", "listen_addr", l.address)
		engineHandler := &GnetTransportEventEngine{
			listener: l,
			handler:  l.handler,
			address:  l.address,
			metrics:  l.metrics,
		}

		// OPTIMIZATION: gnet with production-ready settings
		startErr = gnet.Run(engineHandler, "tcp://"+l.address.String(),
			gnet.WithMulticore(true),                // Multi-core support
			gnet.WithReusePort(true),                // SO_REUSEPORT for load balancing
			gnet.WithTCPKeepAlive(30*time.Second),   // Keep-alive
			gnet.WithTCPNoDelay(gnet.TCPNoDelay),    // Disable Nagle
			gnet.WithLoadBalancing(gnet.RoundRobin), // Round-robin load balancing
		)
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

func (l *GnetTransportListener) HandleMessage(handler func(message []byte) error) TransportListener {
	l.handler = handler
	return l
}

func (l *GnetTransportListener) Addr() net.Addr { return l.address }

// --- Event Engine ---

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
	return gnet.None
}

func (es *GnetTransportEventEngine) OnShutdown(engine gnet.Engine) {
	es.listener.running = false
	logging.Debug("listener[gnet] shutdown", "listen_addr", es.address)
}

func (es *GnetTransportEventEngine) OnOpen(conn gnet.Conn) (out []byte, action gnet.Action) {
	logging.Debug("gnet connection opened", "remote_addr", conn.RemoteAddr().String())

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
		logging.Warn("gnet connection closed with error", "err", err, "remote_addr", conn.RemoteAddr().String())
	} else {
		logging.Debug("gnet connection closed", "remote_addr", conn.RemoteAddr().String())
	}
	return gnet.None
}

func (es *GnetTransportEventEngine) OnTraffic(conn gnet.Conn) (action gnet.Action) {
	if es.handler == nil {
		logging.Warn("gnet handler not set", "remote_addr", conn.RemoteAddr().String())
		return gnet.None
	}

	// OPTIMIZATION: Process multiple messages in one callback to reduce event loop overhead
	messagesProcessed := 0
	const maxBatchPerCallback = 10 // Process up to 10 messages per callback

	for messagesProcessed < maxBatchPerCallback {
		// Check if we have enough data for the length prefix
		if conn.InboundBuffered() < 4 {
			return gnet.None
		}

		// OPTIMIZATION: Use Next() for zero-copy peek
		prefix, _ := conn.Peek(4)
		msgLen := int(binary.BigEndian.Uint32(prefix))

		// Validate message length
		if msgLen <= 0 || msgLen > 10*1024*1024 {
			logging.Error(errors.New("invalid message length"),
				"len", msgLen, "remote_addr", conn.RemoteAddr().String())

			if es.metrics != nil {
				es.metrics.RecordError("read")
			}

			return gnet.Close
		}

		// Check if complete message is available
		totalNeeded := 4 + msgLen
		if conn.InboundBuffered() < totalNeeded {
			return gnet.None
		}

		// Discard length prefix
		conn.Discard(4)

		// OPTIMIZATION: Zero-copy message access
		// Use Peek to avoid copying, then Discard after processing
		message, _ := conn.Peek(msgLen)

		// Record metrics
		if es.metrics != nil {
			es.metrics.RecordRead(int64(msgLen))
		}

		// CRITICAL: Zero-copy handler
		// The message buffer is only valid until the next Discard/Read call
		// Handler MUST NOT retain references to the message slice
		if err := es.handler(message); err != nil {
			logging.Error(err, "message handling failed", "remote_addr", conn.RemoteAddr().String())

			if es.metrics != nil {
				es.metrics.RecordError("handler")
			}

			return gnet.Close
		}

		// Discard the message now that it's processed
		conn.Discard(msgLen)

		messagesProcessed++
	}

	// If we processed the maximum batch, yield to allow other connections to be serviced
	// This prevents connection starvation under high load
	return gnet.None
}
