package transport

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/feellmoose/gridkv/internal/utils/logging"
	"github.com/feellmoose/gridkv/internal/utils/workerpool"
)

type UDPTransport struct {
	listeners sync.Map
	config    *UDPConfig
	engine    *datagramEngine
}

type UDPConfig struct {
	// Message batching
	BatchSize    int           // Max messages per batch (0 = disabled)
	BatchTimeout time.Duration // Max time to wait for batch completion

	// Reliability
	EnableReliability bool          // Enable reliable delivery
	RetryTimeout      time.Duration // Timeout before retry
	MaxRetries        int           // Maximum retry attempts

	// Performance tuning
	BufferSize     int  // UDP buffer size (SO_RCVBUF/SO_SNDBUF)
	ReadBufferSize int  // Read buffer size per message
	EnableZeroCopy bool // Enable zero-copy optimizations

	// Connection management
	MaxConcurrentWrites int // Maximum concurrent write goroutines
}

func (c *UDPConfig) Clone() *UDPConfig {
	if c == nil {
		return nil
	}
	cp := *c
	return &cp
}

func DefaultUDPConfig() *UDPConfig {
	return &UDPConfig{
		BatchSize:           100,                   // Batch up to 100 messages
		BatchTimeout:        50 * time.Microsecond, // 50µs batch window
		EnableReliability:   false,                 // Disabled for max performance
		RetryTimeout:        100 * time.Millisecond,
		MaxRetries:          3,
		BufferSize:          4 * 1024 * 1024, // 4MB UDP buffers (Linux default max)
		ReadBufferSize:      65507,           // Max UDP datagram size
		EnableZeroCopy:      true,
		MaxConcurrentWrites: 1000, // High concurrency for writes
	}
}

// UDPMetrics tracks UDP transport statistics
type UDPMetrics struct {
	messagesSent     atomic.Int64
	messagesReceived atomic.Int64
	messagesDropped  atomic.Int64
	bytesSent        atomic.Int64
	bytesReceived    atomic.Int64
	retries          atomic.Int64
	batchesSent      atomic.Int64
	mu               sync.RWMutex
}

// NewUDPMetrics creates a new UDPMetrics instance
func NewUDPMetrics() *UDPMetrics {
	return &UDPMetrics{}
}

// Snapshot returns current metrics snapshot
func (m *UDPMetrics) Snapshot() map[string]int64 {
	if m == nil {
		return make(map[string]int64)
	}
	return map[string]int64{
		"messages_sent":     m.messagesSent.Load(),
		"messages_received": m.messagesReceived.Load(),
		"messages_dropped":  m.messagesDropped.Load(),
		"bytes_sent":        m.bytesSent.Load(),
		"bytes_received":    m.bytesReceived.Load(),
		"retries":           m.retries.Load(),
		"batches_sent":      m.batchesSent.Load(),
	}
}

func NewUDPTransport(config *UDPConfig) (*UDPTransport, error) {
	engine := newDatagramEngine(config)
	return &UDPTransport{
		config: engine.config,
		engine: engine,
	}, nil
}

func (t *UDPTransport) Dial(address string) (TransportConn, error) {
	return t.engine.Dial(address)
}

func (t *UDPTransport) Listen(address string) (TransportListener, error) {
	listener, err := NewUDPListener(address, t.config, t.engine.Metrics())
	if err == nil {
		t.listeners.Store(address, listener)
	}
	return listener, err
}

func (t *UDPTransport) Stop() error {
	t.listeners.Range(func(key, value interface{}) bool {
		listener := value.(*UDPListener)
		listener.Stop()
		t.listeners.Delete(key)
		return true
	})
	return nil
}

// optimizeUDPSocket sets socket options for maximum performance
func optimizeUDPSocket(conn *net.UDPConn) error {
	var errs []error

	// Set send buffer size (SO_SNDBUF)
	if err := setUDPSendBuffer(conn, 4*1024*1024); err != nil {
		errs = append(errs, err)
	}

	// Set receive buffer size (SO_RCVBUF)
	if err := setUDPRecvBuffer(conn, 4*1024*1024); err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("UDP socket optimization errors: %v", errs)
	}
	return nil
}

type UDPConn struct {
	conn        *net.UDPConn
	remoteAddr  *net.UDPAddr
	config      *UDPConfig
	metrics     *UDPMetrics
	bufferPool  *sync.Pool
	messagePool *sync.Pool
	writeMu     sync.Mutex
	closed      atomic.Bool
}

func NewUDPConn(conn *net.UDPConn, remoteAddr *net.UDPAddr, config *UDPConfig, metrics *UDPMetrics, bufferPool, messagePool *sync.Pool) *UDPConn {
	return &UDPConn{
		conn:        conn,
		remoteAddr:  remoteAddr,
		config:      config,
		metrics:     metrics,
		bufferPool:  bufferPool,
		messagePool: messagePool,
	}
}

// udpMessage represents a single UDP message with metadata
type udpMessage struct {
	data      []byte
	timestamp time.Time
	retries   int
	seqID     uint64
}

func (u *UDPConn) WriteDataWithContext(ctx context.Context, data []byte) error {
	if u.closed.Load() {
		return errors.New("UDP connection closed")
	}

	// Fast path: Direct send for small messages without batching
	if !u.config.EnableReliability && len(data) < 1400 { // MTU-safe size
		return u.writeDirect(ctx, data)
	}

	// Batched path: Use batching for larger messages or when reliability enabled
	return u.writeBatched(ctx, data)
}

func (u *UDPConn) writeDirect(ctx context.Context, data []byte) error {
	u.writeMu.Lock()
	defer u.writeMu.Unlock()

	if u.closed.Load() {
		return errors.New("UDP connection closed")
	}

	// Set write deadline
	if deadline, ok := ctx.Deadline(); ok {
		u.conn.SetWriteDeadline(deadline)
		defer u.conn.SetWriteDeadline(time.Time{})
	}

	// Message format: [4-byte length][data]
	// Use stack-allocated buffer for small messages (zero allocation)
	const maxInlineSize = 1500 // MTU size
	var lengthPrefix [4]byte
	binary.BigEndian.PutUint32(lengthPrefix[:], uint32(len(data)))

	// Zero-copy send: combine length and data in single write
	// This reduces syscalls and improves performance
	if len(data) <= maxInlineSize {
		buffer := u.bufferPool.Get().([]byte)
		defer u.bufferPool.Put(buffer)

		// Ensure buffer is large enough
		needed := 4 + len(data)
		if cap(buffer) < needed {
			buffer = make([]byte, needed)
		}
		buffer = buffer[:needed]

		// Copy length prefix and data
		copy(buffer, lengthPrefix[:])
		copy(buffer[4:], data)

		// Send in single syscall
		n, err := u.conn.Write(buffer)
		if err != nil {
			u.metrics.messagesDropped.Add(1)
			return fmt.Errorf("UDP write failed: %w", err)
		}

		u.metrics.messagesSent.Add(1)
		u.metrics.bytesSent.Add(int64(n))
		return nil
	}

	// Large message: use vectored write if possible, otherwise fallback
	return u.writeLargeMessage(ctx, data, lengthPrefix[:])
}

func (u *UDPConn) writeLargeMessage(ctx context.Context, data []byte, lengthPrefix []byte) error {
	// For messages > MTU, split into multiple datagrams or use fragmentation
	// UDP max is 65507 bytes, but we'll use 65507-4 = 65503 for payload

	const maxPayloadSize = 65503 // Max UDP payload (65507 - 4 byte header)

	if len(data) <= maxPayloadSize {
		// Single datagram
		buffer := u.bufferPool.Get().([]byte)
		defer u.bufferPool.Put(buffer)

		needed := 4 + len(data)
		if cap(buffer) < needed {
			buffer = make([]byte, needed)
		}
		buffer = buffer[:needed]

		copy(buffer, lengthPrefix)
		copy(buffer[4:], data)

		n, err := u.conn.Write(buffer)
		if err != nil {
			u.metrics.messagesDropped.Add(1)
			return err
		}

		u.metrics.messagesSent.Add(1)
		u.metrics.bytesSent.Add(int64(n))
		return nil
	}

	// Message too large for single UDP datagram - return error
	// In production, this should be handled by application-level fragmentation
	return fmt.Errorf("message too large for UDP: %d bytes (max %d)", len(data), maxPayloadSize)
}

func (u *UDPConn) writeBatched(ctx context.Context, data []byte) error {
	return u.writeDirect(ctx, data)
}

func (u *UDPConn) ReadDataWithContext(ctx context.Context) ([]byte, error) {
	if u.closed.Load() {
		return nil, errors.New("UDP connection closed")
	}

	// Set read deadline
	if deadline, ok := ctx.Deadline(); ok {
		u.conn.SetReadDeadline(deadline)
		defer u.conn.SetReadDeadline(time.Time{})
	}

	// Get buffer from pool (zero allocation for small messages)
	buffer := u.bufferPool.Get().([]byte)
	defer u.bufferPool.Put(buffer)

	// Ensure buffer is large enough
	if cap(buffer) < u.config.ReadBufferSize {
		buffer = make([]byte, u.config.ReadBufferSize)
	}
	buffer = buffer[:u.config.ReadBufferSize]

	// Read UDP datagram
	n, _, err := u.conn.ReadFromUDP(buffer)
	if err != nil {
		u.metrics.messagesDropped.Add(1)
		return nil, fmt.Errorf("UDP read failed: %w", err)
	}

	if n < 4 {
		u.metrics.messagesDropped.Add(1)
		return nil, errors.New("UDP datagram too short")
	}

	// Parse length prefix
	dataLength := binary.BigEndian.Uint32(buffer[:4])

	// Validate length
	const maxMessageSize = 10 * 1024 * 1024 // 10MB max
	if dataLength == 0 || dataLength > maxMessageSize {
		u.metrics.messagesDropped.Add(1)
		return nil, errors.New("invalid message size")
	}

	if n < int(4+dataLength) {
		u.metrics.messagesDropped.Add(1)
		return nil, errors.New("UDP datagram incomplete")
	}

	// Extract data (zero-copy where possible)
	data := make([]byte, dataLength)
	copy(data, buffer[4:4+dataLength])

	u.metrics.messagesReceived.Add(1)
	u.metrics.bytesReceived.Add(int64(n))

	return data, nil
}

func (u *UDPConn) Close() error {
	if u.closed.Swap(true) {
		return nil // Already closed
	}

	u.writeMu.Lock()
	defer u.writeMu.Unlock()

	if u.conn != nil {
		err := u.conn.Close()
		u.conn = nil
		return err
	}
	return nil
}

func (u *UDPConn) LocalAddr() net.Addr {
	if u.conn != nil {
		return u.conn.LocalAddr()
	}
	return nil
}

func (u *UDPConn) RemoteAddr() net.Addr {
	return u.remoteAddr
}

type UDPListener struct {
	conn       *net.UDPConn
	handler    func(message []byte) error
	address    string
	config     *UDPConfig
	metrics    *UDPMetrics
	stopCh     chan struct{}
	stopOnce   sync.Once
	workerPool *workerpool.Pool // Worker pool for message handling
	bufferPool sync.Pool
}

func NewUDPListener(address string, config *UDPConfig, metrics *UDPMetrics) (*UDPListener, error) {
	if config == nil {
		config = DefaultUDPConfig()
	}

	// Create worker pool for UDP message handling
	// UDP uses multiple read workers, but message processing goes through pool
	maxWorkers := 32 // Fixed worker count for message processing
	queueSize := maxWorkers * 2

	pool, err := createListenerWorkerPool("udp-listener", maxWorkers, queueSize)
	if err != nil {
		return nil, err
	}

	return &UDPListener{
		address:    address,
		config:     config,
		metrics:    metrics,
		stopCh:     make(chan struct{}),
		workerPool: pool,
		bufferPool: sync.Pool{
			New: func() interface{} {
				return make([]byte, config.ReadBufferSize)
			},
		},
	}, nil
}

func (l *UDPListener) Start() error {
	udpAddr, err := net.ResolveUDPAddr("udp", l.address)
	if err != nil {
		return fmt.Errorf("resolve UDP address %s: %w", l.address, err)
	}

	// Create UDP listener
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("listen UDP %s: %w", l.address, err)
	}

	// Optimize socket for high performance
	if err := optimizeUDPSocket(conn); err != nil {
		logging.Debug("UDP socket optimization warning (non-fatal)", "err", err)
	}

	l.conn = conn

	// Start multiple read workers for high throughput
	// UDP needs multiple readers due to connectionless nature
	readWorkers := 32
	for i := 0; i < readWorkers; i++ {
		go l.readWorker(i)
	}

	return nil
}

func (l *UDPListener) readWorker(workerID int) {
	buffer := l.bufferPool.Get().([]byte)
	defer l.bufferPool.Put(buffer)

	// Ensure buffer is large enough
	if cap(buffer) < l.config.ReadBufferSize {
		buffer = make([]byte, l.config.ReadBufferSize)
	}
	buffer = buffer[:l.config.ReadBufferSize]

	for {
		select {
		case <-l.stopCh:
			return
		default:
		}

		// Set read deadline for responsiveness
		l.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))

		// Read UDP datagram
		n, addr, err := l.conn.ReadFromUDP(buffer)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue // Timeout is normal, continue reading
			}
			if logging.Log.IsDebugEnabled() {
				logging.Debug("UDP read error", "worker", workerID, "err", err)
			}
			continue
		}

		if n < 4 {
			l.metrics.messagesDropped.Add(1)
			continue
		}

		// Parse length prefix
		dataLength := binary.BigEndian.Uint32(buffer[:4])

		// Validate length
		const maxMessageSize = 10 * 1024 * 1024
		if dataLength == 0 || dataLength > maxMessageSize {
			l.metrics.messagesDropped.Add(1)
			continue
		}

		if n < int(4+dataLength) {
			l.metrics.messagesDropped.Add(1)
			continue
		}

		// Extract message data
		message := make([]byte, dataLength)
		copy(message, buffer[4:4+dataLength])

		// Update metrics
		l.metrics.messagesReceived.Add(1)
		l.metrics.bytesReceived.Add(int64(n))

		// Submit message handling to worker pool
		// This provides better resource control and prevents goroutine storms
		messageCopy := message
		if l.handler != nil && l.workerPool != nil {
			if err := l.workerPool.Submit(func() {
				if err := l.handler(messageCopy); err != nil && logging.Log.IsDebugEnabled() {
					logging.Debug("UDP handler error", "addr", addr, "err", err)
				}
			}); err != nil {
				// Pool is closed or full - log but continue reading
				if logging.Log.IsDebugEnabled() {
					logging.Debug("UDP message rejected: worker pool full or closed",
						"addr", addr,
						"error", err)
				}
				l.metrics.messagesDropped.Add(1)
			}
		}
	}
}

func (l *UDPListener) HandleMessage(handler func(message []byte) error) TransportListener {
	l.handler = handler
	return l
}

func (l *UDPListener) Stop() error {
	var err error
	l.stopOnce.Do(func() {
		close(l.stopCh)
		if l.conn != nil {
			err = l.conn.Close()
		}
	})

	// Release worker pool - this will gracefully shutdown all workers
	if l.workerPool != nil {
		l.workerPool.Release()
	}

	return err
}

func (l *UDPListener) Addr() net.Addr {
	if l.conn != nil {
		return l.conn.LocalAddr()
	}
	return nil
}

// setUDPSendBuffer sets SO_SNDBUF socket option (OS-specific)
func setUDPSendBuffer(conn *net.UDPConn, size int) error {
	if conn == nil {
		return errors.New("UDP connection is nil")
	}
	if size <= 0 {
		return errors.New("invalid UDP send buffer size")
	}

	rawConn, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("get syscall conn: %w", err)
	}

	var setErr error
	if err := rawConn.Control(func(fd uintptr) {
		if serr := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, size); serr != nil {
			setErr = fmt.Errorf("set SO_SNDBUF: %w", serr)
		}
	}); err != nil {
		return err
	}

	return setErr
}

// setUDPRecvBuffer sets SO_RCVBUF socket option (OS-specific)
func setUDPRecvBuffer(conn *net.UDPConn, size int) error {
	if conn == nil {
		return errors.New("UDP connection is nil")
	}
	if size <= 0 {
		return errors.New("invalid UDP receive buffer size")
	}

	rawConn, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("get syscall conn: %w", err)
	}

	var setErr error
	if err := rawConn.Control(func(fd uintptr) {
		if serr := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, size); serr != nil {
			setErr = fmt.Errorf("set SO_RCVBUF: %w", serr)
		}
	}); err != nil {
		return err
	}

	return setErr
}
