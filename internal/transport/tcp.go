package transport

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/feellmoose/gridkv/internal/utils/logging"
)

var (
	// Length prefix buffer pool (4 bytes)
	lengthPrefixPool = sync.Pool{
		New: func() interface{} {
			return make([]byte, 4)
		},
	}

	tcpReadBufferPool = sync.Pool{
		New: func() interface{} {
			return make([]byte, 8192)
		},
	}
)

// TCPOptimizationConfig contains TCP tuning parameters based on network characteristics
type TCPOptimizationConfig struct {
	NoDelay         bool          // Disable Nagle's algorithm (lower latency)
	KeepAlive       bool          // Enable TCP keep-alive
	KeepAlivePeriod time.Duration // Keep-alive probe interval
	ReadBufferSize  int           // SO_RCVBUF size
	WriteBufferSize int           // SO_SNDBUF size
	ReadTimeout     time.Duration // Read operation timeout
	WriteTimeout    time.Duration // Write operation timeout
}

// CalculateTCPOptimization calculates optimal TCP parameters based on RTT and bandwidth.
// This implements BDP (Bandwidth-Delay Product) calculation for buffer sizing.
//
// Parameters:
//   - avgRTT: Average round-trip time to the peer
//   - estimatedBandwidth: Estimated network bandwidth in bytes/sec (0 = default 1Gbps)
//
// Returns optimized TCP configuration
func CalculateTCPOptimization(avgRTT time.Duration, estimatedBandwidth int64) *TCPOptimizationConfig {
	config := &TCPOptimizationConfig{
		NoDelay:   true, // Always disable Nagle for low latency
		KeepAlive: true, // Always enable keep-alive
	}

	// Default bandwidth: 1Gbps = 125MB/s
	if estimatedBandwidth == 0 {
		estimatedBandwidth = 125 * 1024 * 1024 // 125 MB/s
	}

	// Calculate BDP: Bandwidth × RTT
	// This is the optimal buffer size to keep the pipe full
	bdp := int64(float64(estimatedBandwidth) * avgRTT.Seconds())

	// Keep-alive period: 3× RTT (detect failures quickly)
	config.KeepAlivePeriod = avgRTT * 3
	if config.KeepAlivePeriod < 10*time.Second {
		config.KeepAlivePeriod = 10 * time.Second // Minimum for stability
	}
	if config.KeepAlivePeriod > 60*time.Second {
		config.KeepAlivePeriod = 60 * time.Second // Maximum to avoid over-probing
	}

	// Buffer size: max(BDP, 256KB) - Increased for high concurrency
	// Larger buffers reduce i/o timeout errors under load
	bufferSize := int(bdp)
	if bufferSize < 256*1024 {
		bufferSize = 256 * 1024 // Minimum 256KB (increased from 128KB)
	}
	if bufferSize > 8*1024*1024 {
		bufferSize = 8 * 1024 * 1024 // Maximum 8MB (increased from 4MB for high load)
	}

	config.ReadBufferSize = bufferSize
	config.WriteBufferSize = bufferSize

	// Timeouts: 10× RTT (reduced from 15× for faster failure in high load)
	// Balance between reliability and fast failure
	config.ReadTimeout = avgRTT * 10
	config.WriteTimeout = avgRTT * 10

	// Minimum timeouts for stability (reduced for faster failure)
	if config.ReadTimeout < 3*time.Second {
		config.ReadTimeout = 3 * time.Second // Reduced from 8s for faster failure
	}
	if config.WriteTimeout < 3*time.Second {
		config.WriteTimeout = 3 * time.Second // Reduced from 8s for faster failure
	}

	return config
}

// OptimizeTCPConn applies performance optimizations to a TCP connection based on
// network characteristics. This should be called immediately after establishing
// a connection for optimal performance.
//
// Optimizations applied:
//   - TCP_NODELAY: Disable Nagle's algorithm for lower latency
//   - Keep-Alive: Enable with RTT-based probing
//   - Buffer sizes: Calculated based on BDP (Bandwidth-Delay Product)
//
// Parameters:
//   - conn: The TCP connection to optimize
//   - avgRTT: Average round-trip time (use 0 for default 10ms)
//
// Returns error if any optimization fails (non-fatal, connection still usable)
func OptimizeTCPConn(conn *net.TCPConn, avgRTT time.Duration) error {
	if avgRTT == 0 {
		avgRTT = 10 * time.Millisecond // Default for LAN
	}

	config := CalculateTCPOptimization(avgRTT, 0)
	return ApplyTCPConfig(conn, config)
}

// ApplyTCPConfig applies a TCPOptimizationConfig to a connection
func ApplyTCPConfig(conn *net.TCPConn, config *TCPOptimizationConfig) error {
	var lastErr error

	// 1. Disable Nagle's algorithm for lower latency
	if config.NoDelay {
		if err := conn.SetNoDelay(true); err != nil {
			if logging.Log.IsDebugEnabled() {
				logging.Debug("Failed to set TCP_NODELAY", "err", err)
			}
			lastErr = err
		}
	}

	// 2. Enable TCP keep-alive to detect dead connections
	if config.KeepAlive {
		if err := conn.SetKeepAlive(true); err != nil {
			if logging.Log.IsDebugEnabled() {
				logging.Debug("Failed to enable TCP keep-alive", "err", err)
			}
			lastErr = err
		}

		if err := conn.SetKeepAlivePeriod(config.KeepAlivePeriod); err != nil {
			if logging.Log.IsDebugEnabled() {
				logging.Debug("Failed to set keep-alive period", "err", err)
			}
			lastErr = err
		}
	}

	// 3. Set optimal buffer sizes (BDP-based)
	if config.ReadBufferSize > 0 {
		if err := conn.SetReadBuffer(config.ReadBufferSize); err != nil {
			if logging.Log.IsDebugEnabled() {
				logging.Debug("Failed to set read buffer", "size", config.ReadBufferSize, "err", err)
			}
			lastErr = err
		}
	}

	if config.WriteBufferSize > 0 {
		if err := conn.SetWriteBuffer(config.WriteBufferSize); err != nil {
			if logging.Log.IsDebugEnabled() {
				logging.Debug("Failed to set write buffer", "size", config.WriteBufferSize, "err", err)
			}
			lastErr = err
		}
	}

	return lastErr
}

// GetTCPStats returns TCP connection statistics (if available)
func GetTCPStats(conn *net.TCPConn) map[string]interface{} {
	stats := make(map[string]interface{})

	if conn == nil {
		return stats
	}

	stats["local_addr"] = conn.LocalAddr().String()
	stats["remote_addr"] = conn.RemoteAddr().String()

	// Additional stats could be gathered from /proc/net/tcp on Linux
	// For now, return basic info
	return stats
}

// TCPTransportConn implements TransportConn using TCP connections.
type TCPTransportConn struct {
	conn *net.TCPConn
}

// WriteDataWithContext sends data with context awareness.
// OPTIMIZATION: Handles timeout errors gracefully with deadline padding for high load.
//
//go:noinline
func (t *TCPTransportConn) WriteDataWithContext(ctx context.Context, data []byte) error {
	const maxMessageSize = 10 * 1024 * 1024 // 10MB max

	// Check message size before sending to avoid connection reset
	if len(data) > maxMessageSize {
		return fmt.Errorf("message too large: %d bytes (max: %d)", len(data), maxMessageSize)
	}

	if deadline, ok := ctx.Deadline(); ok {
		// OPTIMIZATION: Set deadline directly without padding
		// Padding was causing issues - use exact deadline for better timeout handling
		t.conn.SetWriteDeadline(deadline)
		defer t.conn.SetWriteDeadline(time.Time{})
	}

	lengthPrefix := lengthPrefixPool.Get().([]byte)
	defer lengthPrefixPool.Put(lengthPrefix)

	// Encode length prefix (4 bytes) - inlined operation
	binary.BigEndian.PutUint32(lengthPrefix, uint32(len(data)))

	// CRITICAL: Use writev (vectored I/O) for ZERO-COPY
	// This avoids copying data into an intermediate buffer!
	// writev syscall writes both buffers in a single atomic operation
	buffers := net.Buffers{lengthPrefix, data}
	_, err := buffers.WriteTo(t.conn)

	// Check context cancellation - may have timed out during write
	if err != nil && ctx.Err() != nil {
		return ctx.Err() // Return context error for better error handling
	}

	return err // Direct return, no wrapping
}

// ReadDataWithContext reads data with context awareness.
//
//go:noinline
func (t *TCPTransportConn) ReadDataWithContext(ctx context.Context) ([]byte, error) {
	if deadline, ok := ctx.Deadline(); ok {
		t.conn.SetDeadline(deadline)
		defer t.conn.SetDeadline(time.Time{})
	}

	reader := bufio.NewReaderSize(t.conn, 16384) // 16KB buffer

	// Read length prefix (4 bytes) - MUST use io.ReadFull to guarantee full read
	lengthPrefix := make([]byte, 4)
	if _, err := io.ReadFull(reader, lengthPrefix); err != nil {
		return nil, err // Direct return, no wrapping
	}

	// Get the data length and validate
	dataLength := binary.BigEndian.Uint32(lengthPrefix)

	// Negative values become very large unsigned, caught in one check
	const maxMessageSize = 10 * 1024 * 1024 // 10MB max
	if dataLength == 0 || dataLength > maxMessageSize {
		return nil, errors.New("invalid message size")
	}

	if dataLength <= 8192 {
		poolBuf := tcpReadBufferPool.Get().([]byte)
		data := poolBuf[:dataLength]
		defer tcpReadBufferPool.Put(poolBuf)

		if _, err := io.ReadFull(reader, data); err != nil {
			return nil, err
		}

		// Must copy since we're returning the buffer to pool
		result := make([]byte, dataLength)
		copy(result, data)
		return result, nil
	}

	// For large messages, allocate directly
	data := make([]byte, dataLength)
	if _, err := io.ReadFull(reader, data); err != nil {
		return nil, err
	}

	return data, nil
}

// Close closes the connection (inlined)
//
//go:inline
func (t *TCPTransportConn) Close() error {
	return t.conn.Close()
}

// LocalAddr returns local address (inlined)
//
//go:inline
func (t *TCPTransportConn) LocalAddr() net.Addr {
	return t.conn.LocalAddr()
}

// RemoteAddr returns remote address (inlined)
//
//go:inline
func (t *TCPTransportConn) RemoteAddr() net.Addr {
	return t.conn.RemoteAddr()
}

// HealthCheck implements optional health checking for TCP connections
func (t *TCPTransportConn) HealthCheck() error {
	// Use non-blocking state check instead of actual read
	// This avoids false negatives from aggressive read deadlines
	if t.conn == nil {
		return fmt.Errorf("connection is nil")
	}

	// Check connection state by attempting to get remote address
	// This is a lightweight check that doesn't perform I/O
	remoteAddr := t.conn.RemoteAddr()
	if remoteAddr == nil {
		return fmt.Errorf("connection not established")
	}

	// Try to set a write deadline to detect if connection is closed
	// This is a non-blocking operation that will fail if connection is closed
	if err := t.conn.SetWriteDeadline(time.Now().Add(1 * time.Nanosecond)); err != nil {
		return fmt.Errorf("connection closed: %w", err)
	}
	// Reset deadline immediately
	t.conn.SetWriteDeadline(time.Time{})

	// Connection appears valid (not closed and has remote address)
	return nil
}

// TCPTransport implements the transport.Transport interface using TCP connections.
type TCPTransport struct{}

func NewTCPTransport() *TCPTransport {
	return &TCPTransport{}
}

func (t *TCPTransport) Dial(address string) (TransportConn, error) {
	conn, err := net.Dial("tcp", address)
	if err != nil {
		return nil, err
	}

	tcpConn := conn.(*net.TCPConn)

	// For more precise optimization, use DialWithRTT instead
	if err := OptimizeTCPConn(tcpConn, 10*time.Millisecond); err != nil {
		// Non-fatal: connection still usable even if optimization fails
		logging.Debug("TCP optimization warnings (non-fatal)", "err", err)
	}

	return &TCPTransportConn{conn: tcpConn}, nil
}

// DialWithRTT creates an optimized TCP connection with RTT-specific tuning.
// This method should be used when you know the approximate RTT to the target.
//
// Parameters:
//   - address: Target address (host:port)
//   - avgRTT: Expected average RTT to the target
//
// Returns optimized TransportConn
func (t *TCPTransport) DialWithRTT(address string, avgRTT time.Duration) (TransportConn, error) {
	conn, err := net.Dial("tcp", address)
	if err != nil {
		return nil, err
	}

	tcpConn := conn.(*net.TCPConn)

	// Apply RTT-specific optimizations
	if err := OptimizeTCPConn(tcpConn, avgRTT); err != nil {
		logging.Debug("TCP optimization warnings (non-fatal)", "err", err)
	}

	return &TCPTransportConn{conn: tcpConn}, nil
}

func (t *TCPTransport) Listen(address string) (TransportListener, error) {
	return NewTCPTransportListener(address)
}

// TCPTransportListener listens for incoming TCP connections.
type TCPTransportListener struct {
	listener   *net.TCPListener
	handler    func(message []byte) error
	connPool   chan struct{}  // Worker pool to limit concurrent connections
	maxWorkers int            // Maximum concurrent connection handlers
	stopCh     chan struct{}  // Signal channel to stop acceptConnections
	stopOnce   sync.Once      // Ensure stopCh is only closed once
	doneCh     chan struct{}  // Signal channel to indicate acceptConnections has exited
	wg         sync.WaitGroup // WaitGroup to track connection handler goroutines
}

// NewTCPTransportListener creates a new listener bound to the specified address.
func NewTCPTransportListener(addr string) (*TCPTransportListener, error) {
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}

	listener, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		return nil, err
	}

	// Optimize maxWorkers: start smaller, connections are handled per-connection
	// Reduced to prevent excessive goroutine creation - connections are short-lived
	maxWorkers := 256 // Reduced from 500 - connections are handled quickly
	connPool := make(chan struct{}, maxWorkers)

	return &TCPTransportListener{
		listener:   listener,
		connPool:   connPool,
		maxWorkers: maxWorkers,
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}, nil
}

func (l *TCPTransportListener) Start() error {
	if l.listener == nil {
		return errors.New("listener not initialized")
	}
	logging.Debug("listener[net] create", "listen_addr", l.listener.Addr().String())

	// Start the loop for accepting connections
	go l.acceptConnections()

	return nil
}

func (l *TCPTransportListener) Stop() error {
	l.stopOnce.Do(func() {
		close(l.stopCh)
		if l.listener != nil {
			addr := l.listener.Addr()
			l.listener.Close()
			logging.Debug("listener[net] stop gracefully", "listen_addr", addr.String())
		}
	})

	// Wait for acceptConnections to exit with timeout
	select {
	case <-l.doneCh:
	case <-time.After(2 * time.Second):
		logging.Warn("acceptConnections did not exit in time")
	}

	// Wait for all connection handlers to complete with timeout
	// Increase timeout for high-load scenarios
	done := make(chan struct{})
	go func() {
		l.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		logging.Warn("connection handlers did not complete in time")
		// Force close remaining connections by closing the listener
		// This will cause all pending reads to fail
		if l.listener != nil {
			l.listener.Close()
		}
	}
	return nil
}

// Addr returns the listener address (inlined)
//
//go:inline
func (l *TCPTransportListener) Addr() net.Addr {
	if l.listener != nil {
		return l.listener.Addr()
	}
	return nil
}

// HandleMessage registers a handler to process incoming byte messages (inlined)
//
//go:inline
func (l *TCPTransportListener) HandleMessage(handler func(message []byte) error) TransportListener {
	l.handler = handler
	return l
}

func (l *TCPTransportListener) acceptConnections() {
	defer close(l.doneCh)
	defer func() {
		if r := recover(); r != nil {
			logging.Error(fmt.Errorf("recovered from panic: %v", r), "listener[net], accepting connections")
		}
	}()

	for {
		select {
		case <-l.stopCh:
			return
		default:
		}

		l.listener.SetDeadline(time.Now().Add(100 * time.Millisecond))
		conn, err := l.listener.AcceptTCP()
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			if isTemporary(err) {
				continue
			}

			if isClosed(err) {
				logging.Debug("listener[net] stopped gracefully")
				return
			}

			logging.Error(err, "listener[net], error accepting connection")
			return
		}

		select {
		case <-l.stopCh:
			conn.Close()
			return
		case l.connPool <- struct{}{}:
			l.wg.Add(1)
			go func(c *net.TCPConn) {
				defer func() {
					<-l.connPool
					l.wg.Done()
				}()
				l.handleConnection(c)
			}(conn)
		default:
			// Connection pool exhausted - reject with backpressure
			// Log at debug level to avoid spam, but track for metrics
			if logging.Log.IsDebugEnabled() {
				logging.Debug("Connection rejected: worker pool full", "remote", conn.RemoteAddr(), "max_workers", l.maxWorkers)
			}
			// Give a brief moment for the pool to drain before rejecting
			// This helps during transient spikes
			select {
			case <-l.stopCh:
				conn.Close()
				return
			case <-time.After(10 * time.Millisecond):
				// Check again if a slot opened up
				select {
				case l.connPool <- struct{}{}:
					l.wg.Add(1)
					go func(c *net.TCPConn) {
						defer func() {
							<-l.connPool
							l.wg.Done()
						}()
						l.handleConnection(c)
					}(conn)
					continue // Successfully queued, continue accepting
				default:
					// Still full, reject
					conn.Close()
				}
			}
		}
	}
}

// handleConnection handles a single TCP connection - HOT PATH optimized
//
//go:noinline
func (l *TCPTransportListener) handleConnection(conn *net.TCPConn) {
	defer conn.Close()

	// Recover from panics in goroutine
	defer func() {
		if r := recover(); r != nil {
			logging.Error(fmt.Errorf("recovered from panic: %v", r), "listener[net], handling connection")
		}
	}()

	// Use default LAN RTT for server-side connections
	if err := OptimizeTCPConn(conn, 10*time.Millisecond); err != nil {
		if logging.Log.IsDebugEnabled() {
			logging.Debug("TCP optimization warnings on accept (non-fatal)", "err", err)
		}
	}

	reader := bufio.NewReaderSize(conn, 16384) // 16KB buffer

	// Cache handler to reduce struct field access
	handler := l.handler

	lengthPrefix := make([]byte, 4)
	readTimeout := 30 * time.Second // Connection read timeout

	for {
		// Check if listener is stopping
		select {
		case <-l.stopCh:
			return
		default:
		}

		// Set read deadline to prevent blocking forever
		conn.SetReadDeadline(time.Now().Add(readTimeout))

		// Read the length prefix (4 bytes) - MUST use io.ReadFull to guarantee full read
		if _, err := io.ReadFull(reader, lengthPrefix); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return
			}
			// Check for timeout - if listener is stopping, exit immediately
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				select {
				case <-l.stopCh:
					return
				default:
					// Timeout but not stopping, continue
					continue
				}
			}
			// Cold path: unexpected error (connection may be closing)
			logging.Debug("connect[net], error reading length prefix", "err", err, "remote_addr", conn.RemoteAddr().String())
			return
		}

		// Read the actual message based on the length
		dataLength := binary.BigEndian.Uint32(lengthPrefix)

		// Zero or too large checked in one condition
		const maxMessageSize = 10 * 1024 * 1024 // 10MB max
		if dataLength == 0 || dataLength > maxMessageSize {
			// Cold path: invalid message size
			if dataLength > maxMessageSize {
				if logging.Log.IsDebugEnabled() {
					logging.Debug("connect[net], rejecting oversized message",
						"remote_addr", conn.RemoteAddr().String(),
						"size", dataLength, "limit", maxMessageSize)
				}
				// Read and discard the oversized message to keep connection alive
				discardBuf := make([]byte, 8192)
				remaining := int64(dataLength)
				for remaining > 0 {
					// Check if listener is stopping
					select {
					case <-l.stopCh:
						return
					default:
					}
					toRead := int64(len(discardBuf))
					if toRead > remaining {
						toRead = remaining
					}
					if _, err := io.CopyN(io.Discard, reader, toRead); err != nil {
						return
					}
					remaining -= toRead
				}
				continue
			}
			// Zero-length message - continue without logging in hot path
			continue
		}

		data := make([]byte, dataLength)

		// MUST use io.ReadFull to guarantee reading exactly dataLength bytes
		if _, err := io.ReadFull(reader, data); err != nil {
			// Check for timeout - if listener is stopping, exit immediately
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				select {
				case <-l.stopCh:
					return
				default:
					// Timeout but not stopping, continue
					continue
				}
			}
			logging.Debug("connect[net], error reading message", "err", err, "remote_addr", conn.RemoteAddr().String())
			return
		}

		// Check if listener is stopping before handling message
		select {
		case <-l.stopCh:
			return
		default:
		}

		// Handle the message with the provided handler
		if handler != nil {
			if err := handler(data); err != nil {
				logging.Debug("connect[net], error handling message", "err", err, "remote_addr", conn.RemoteAddr().String())
				// Continue processing other messages despite handler error
			}
		}
	}
}

// Helper functions for error handling
func isTemporary(err error) bool {
	if opErr, ok := err.(*net.OpError); ok {
		return opErr.Temporary()
	}
	return false
}

func isClosed(err error) bool {
	return strings.Contains(err.Error(), "use of closed network connection")
}
