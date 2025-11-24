package transport

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/feellmoose/gridkv/internal/utils/logging"
	"github.com/quic-go/quic-go"
)

// QUICTransport implements Transport interface using QUIC protocol
type QUICTransport struct {
	connections sync.Map // peer address -> *QUICConnection
	config      *QUICConfig
	metrics     *QUICMetrics
	udpEngine   *datagramEngine
}

// QUICConfig contains QUIC-specific configuration
type QUICConfig struct {
	// Connection settings
	MaxIdleTimeout   time.Duration // Maximum idle timeout for connections
	MaxStreams       int           // Maximum number of concurrent streams per connection
	KeepAlivePeriod  time.Duration // Keep-alive ping interval
	HandshakeTimeout time.Duration // Connection handshake timeout
	// Listener safeguards
	MaxListenerStreams  int           // Maximum concurrent listener streams
	StreamReadTimeout   time.Duration // Deadline for reading a stream
	StreamAcceptTimeout time.Duration // Deadline for accepting a stream

	// UDP fallback settings
	EnableUDPFallback    bool // Enable UDP datagram fallback
	UDPFallbackThreshold int  // Number of QUIC failures before UDP fallback
	UDPFallbackConfig    *UDPConfig

	// Performance tuning
	InitialStreamReceiveWindow     uint64 // Initial stream receive window size
	MaxStreamReceiveWindow         uint64 // Maximum stream receive window size
	InitialConnectionReceiveWindow uint64 // Initial connection receive window size
	MaxConnectionReceiveWindow     uint64 // Maximum connection receive window size
}

// DefaultQUICConfig returns default QUIC configuration optimized for high performance
func DefaultQUICConfig() *QUICConfig {
	return &QUICConfig{
		MaxIdleTimeout:                 180 * time.Second, // Increased from 120s to 180s for better connection reuse under load
		MaxStreams:                     1000,              // Increased for high concurrency
		KeepAlivePeriod:                30 * time.Second,  // Increased to reduce keepalive overhead
		HandshakeTimeout:               30 * time.Second,  // Increased from 15s to 30s for better reliability under load
		MaxListenerStreams:             2048,
		StreamReadTimeout:              30 * time.Second, // Increased from 10s to 30s for high load scenarios
		StreamAcceptTimeout:            2 * time.Second,  // Increased from 1s to 2s to prevent accept timeouts
		EnableUDPFallback:              true,
		UDPFallbackThreshold:           3,
		UDPFallbackConfig:              DefaultUDPConfig(),
		InitialStreamReceiveWindow:     4 * 1024 * 1024,  // 4MB - increased for high throughput
		MaxStreamReceiveWindow:         16 * 1024 * 1024, // 16MB - increased for large messages
		InitialConnectionReceiveWindow: 4 * 1024 * 1024,  // 4MB - increased for better performance
		MaxConnectionReceiveWindow:     32 * 1024 * 1024, // 32MB - increased for multiple streams
	}
}

// QUICConnection wraps a QUIC connection with stream management
type QUICConnection struct {
	conn    *quic.Conn // quic-go uses *Conn, not Connection
	address string
	config  *QUICConfig

	streamMutex sync.RWMutex
	streams     map[uint64]*quic.Stream // Use map of pointers to avoid copying locks
	streamCount int64

	lastUsed time.Time
	mu       sync.Mutex

	// Metrics
	metrics *QUICMetrics
}

// QUICTransportConn implements TransportConn using QUIC streams
type QUICTransportConn struct {
	stream     *quic.Stream // Use pointer to avoid copying lock
	connection *QUICConnection
	localAddr  net.Addr
	remoteAddr net.Addr
	metrics    *QUICMetrics
}

// QUICMetrics tracks QUIC transport statistics
type QUICMetrics struct {
	connectionsCreated atomic.Int64
	connectionsClosed  atomic.Int64
	streamsCreated     atomic.Int64
	streamsClosed      atomic.Int64
	udpFallbacks       atomic.Int64
	quicFailures       atomic.Int64
	mu                 sync.RWMutex
}

// NewQUICMetrics creates a new QUICMetrics instance
func NewQUICMetrics() *QUICMetrics {
	return &QUICMetrics{}
}

// Snapshot returns a snapshot of current metrics
func (m *QUICMetrics) Snapshot() map[string]int64 {
	if m == nil {
		return make(map[string]int64)
	}
	return map[string]int64{
		"connections_created": m.connectionsCreated.Load(),
		"connections_closed":  m.connectionsClosed.Load(),
		"streams_created":     m.streamsCreated.Load(),
		"streams_closed":      m.streamsClosed.Load(),
		"udp_fallbacks":       m.udpFallbacks.Load(),
		"quic_failures":       m.quicFailures.Load(),
	}
}

// NewQUICTransport creates a new QUIC transport instance
func NewQUICTransport(config *QUICConfig) (*QUICTransport, error) {
	if config == nil {
		config = DefaultQUICConfig()
	}

	var fallbackCfg *UDPConfig
	if config.UDPFallbackConfig != nil {
		fallbackCfg = config.UDPFallbackConfig.Clone()
	} else {
		fallbackCfg = DefaultUDPConfig()
	}
	config.UDPFallbackConfig = fallbackCfg

	var engine *datagramEngine
	if config.EnableUDPFallback {
		engine = newDatagramEngine(fallbackCfg)
	}

	return &QUICTransport{
		config:    config,
		metrics:   NewQUICMetrics(),
		udpEngine: engine,
	}, nil
}

// Dial creates a new QUIC connection to the specified address
func (t *QUICTransport) Dial(address string) (TransportConn, error) {
	// Try to get existing connection first
	if conn, ok := t.connections.Load(address); ok {
		qconn := conn.(*QUICConnection)
		if qconn.isAlive() {
			return qconn.openStream()
		}
		// Connection is dead, remove it
		t.connections.Delete(address)
	}

	// QUIC configuration
	quicConfig := &quic.Config{
		MaxIdleTimeout:                 t.config.MaxIdleTimeout,
		MaxIncomingStreams:             int64(t.config.MaxStreams),
		MaxIncomingUniStreams:          int64(t.config.MaxStreams),
		KeepAlivePeriod:                t.config.KeepAlivePeriod,
		HandshakeIdleTimeout:           t.config.HandshakeTimeout,
		InitialStreamReceiveWindow:     t.config.InitialStreamReceiveWindow,
		MaxStreamReceiveWindow:         t.config.MaxStreamReceiveWindow,
		InitialConnectionReceiveWindow: t.config.InitialConnectionReceiveWindow,
		MaxConnectionReceiveWindow:     t.config.MaxConnectionReceiveWindow,
		// Use ALPN for protocol negotiation
		Versions: []quic.Version{quic.Version1, quic.Version2},
	}

	// Generate self-signed cert for client (QUIC requires TLS)
	tlsCert, err := generateSelfSignedCert()
	if err != nil {
		return nil, fmt.Errorf("generate TLS cert: %w", err)
	}

	// Dial QUIC connection - use address directly (host:port format)
	// quic.DialAddr expects "host:port" string format
	conn, err := quic.DialAddr(context.Background(), address, &tls.Config{
		NextProtos:         []string{"gridkv-quic"}, // ALPN protocol
		InsecureSkipVerify: true,                    // Accept self-signed certs
		Certificates:       []tls.Certificate{*tlsCert},
	}, quicConfig)
	if err != nil {
		if t.config.EnableUDPFallback {
			t.metrics.quicFailures.Add(1)
			return t.udpFallback(address)
		}
		return nil, fmt.Errorf("dial QUIC %s: %w", address, err)
	}

	// Wrap connection
	qconn := &QUICConnection{
		conn:     conn,
		address:  address,
		config:   t.config,
		streams:  make(map[uint64]*quic.Stream),
		lastUsed: time.Now(),
		metrics:  t.metrics,
	}

	t.connections.Store(address, qconn)
	t.metrics.connectionsCreated.Add(1)

	// Start keep-alive goroutine
	go qconn.keepAlive()

	// Open stream for this transport connection
	stream, err := qconn.openStream()
	if err != nil {
		return nil, err
	}

	return stream, nil
}

// Listen creates a QUIC listener
func (t *QUICTransport) Listen(address string) (TransportListener, error) {
	return NewQUICTransportListener(address, t.config)
}

// Stop closes all QUIC connections
func (t *QUICTransport) Stop() error {
	t.connections.Range(func(key, value interface{}) bool {
		conn := value.(*QUICConnection)
		conn.close()
		t.connections.Delete(key)
		return true
	})
	return nil
}

// openStream opens a new QUIC stream on the connection
func (qc *QUICConnection) openStream() (TransportConn, error) {
	qc.mu.Lock()
	defer qc.mu.Unlock()

	streamCount := atomic.LoadInt64(&qc.streamCount)
	if int(streamCount) >= qc.config.MaxStreams {
		return nil, errors.New("max streams reached for connection")
	}

	// Open bidirectional stream (OpenStreamSync returns *quic.Stream)
	stream, err := qc.conn.OpenStreamSync(context.Background())
	if err != nil {
		return nil, fmt.Errorf("open stream: %w", err)
	}

	streamID := uint64(stream.StreamID())
	qc.streamMutex.Lock()
	qc.streams[streamID] = stream // stream is already *quic.Stream
	qc.streamMutex.Unlock()

	atomic.AddInt64(&qc.streamCount, 1)
	qc.lastUsed = time.Now()
	qc.metrics.streamsCreated.Add(1)

	// Create transport connection wrapper
	transportConn := &QUICTransportConn{
		stream:     stream, // stream is already *quic.Stream
		connection: qc,
		localAddr:  qc.conn.LocalAddr(),
		remoteAddr: qc.conn.RemoteAddr(),
		metrics:    qc.metrics,
	}

	return transportConn, nil
}

// isAlive checks if the QUIC connection is still alive
func (qc *QUICConnection) isAlive() bool {
	qc.mu.Lock()
	defer qc.mu.Unlock()

	// Check if connection context is done
	select {
	case <-qc.conn.Context().Done():
		return false
	default:
	}

	// Check last used time
	if time.Since(qc.lastUsed) > qc.config.MaxIdleTimeout {
		return false
	}

	return true
}

// keepAlive periodically sends keep-alive pings
func (qc *QUICConnection) keepAlive() {
	ticker := time.NewTicker(qc.config.KeepAlivePeriod)
	defer ticker.Stop()

	// Ensure goroutine exits cleanly when connection closes
	for {
		select {
		case <-qc.conn.Context().Done():
			return // Connection closed, exit immediately
		case <-ticker.C:
			// Check if connection is still alive before updating
			select {
			case <-qc.conn.Context().Done():
				return // Connection closed during tick
			default:
				// Update last used time
				qc.mu.Lock()
				qc.lastUsed = time.Now()
				qc.mu.Unlock()

				// Connection keep-alive is handled by QUIC library
				// This is just for tracking last used time
			}
		}
	}
}

// close closes the QUIC connection
func (qc *QUICConnection) close() error {
	// Close all streams first to prevent new I/O operations
	qc.streamMutex.Lock()
	for _, streamPtr := range qc.streams {
		if streamPtr != nil {
			(*streamPtr).Close()
		}
	}
	qc.streams = make(map[uint64]*quic.Stream)
	qc.streamMutex.Unlock()

	// Close connection - this will cause keepAlive goroutine to exit via Context().Done()
	err := qc.conn.CloseWithError(0, "transport closing")
	qc.metrics.connectionsClosed.Add(1)
	return err
}

// udpFallback creates a UDP-based transport connection as fallback
func (t *QUICTransport) udpFallback(address string) (TransportConn, error) {
	t.metrics.udpFallbacks.Add(1)

	if t.udpEngine == nil {
		t.udpEngine = newDatagramEngine(t.config.UDPFallbackConfig)
	}

	return t.udpEngine.Dial(address)
}

// generateSelfSignedCert creates a throwaway certificate for encrypted QUIC
// sessions when operators do not supply their own material.
func generateSelfSignedCert() (*tls.Certificate, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate RSA key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return nil, fmt.Errorf("generate serial number: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load x509 key pair: %w", err)
	}

	return &tlsCert, nil
}

func (qtc *QUICTransportConn) WriteDataWithContext(ctx context.Context, data []byte) error {
	if qtc.stream == nil {
		return errors.New("QUIC stream closed")
	}

	lengthPrefix := make([]byte, 4)
	binary.BigEndian.PutUint32(lengthPrefix, uint32(len(data)))

	var writeDeadline time.Time
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return context.DeadlineExceeded
		}
		writeDeadline = deadline
	} else {
		writeDeadline = time.Now().Add(30 * time.Second)
	}

	(*qtc.stream).SetWriteDeadline(writeDeadline)
	defer (*qtc.stream).SetWriteDeadline(time.Time{})

	// Combine length prefix and data for single write (reduces syscalls)
	totalLen := 4 + len(data)
	var buffer []byte
	if totalLen <= 4096 {
		// Fast path: use stack-allocated buffer for small messages
		var buf [4096]byte
		buffer = buf[:totalLen]
	} else {
		// Slow path: allocate for large messages
		buffer = make([]byte, totalLen)
	}

	copy(buffer, lengthPrefix)
	copy(buffer[4:], data)

	// Single write for better performance
	n, err := (*qtc.stream).Write(buffer)
	if err != nil {
		return fmt.Errorf("write data: %w", err)
	}
	if n != totalLen {
		return fmt.Errorf("write incomplete: wrote %d/%d bytes", n, totalLen)
	}

	return nil
}

// ReadDataWithContext reads data from the QUIC stream
func (qtc *QUICTransportConn) ReadDataWithContext(ctx context.Context) ([]byte, error) {
	if qtc.stream == nil {
		return nil, errors.New("QUIC stream closed")
	}

	var readDeadline time.Time
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, context.DeadlineExceeded
		}
		readDeadline = deadline
	} else {
		readDeadline = time.Now().Add(30 * time.Second)
	}
	(*qtc.stream).SetReadDeadline(readDeadline)
	defer (*qtc.stream).SetReadDeadline(time.Time{})

	// Read length prefix
	lengthPrefix := make([]byte, 4)
	if _, err := (*qtc.stream).Read(lengthPrefix); err != nil {
		return nil, fmt.Errorf("read length prefix: %w", err)
	}

	// Parse length
	dataLength := binary.BigEndian.Uint32(lengthPrefix)

	// Validate length
	const maxMessageSize = 10 * 1024 * 1024 // 10MB max
	if dataLength == 0 || dataLength > maxMessageSize {
		return nil, errors.New("invalid message size")
	}

	// Read data
	data := make([]byte, dataLength)
	if _, err := (*qtc.stream).Read(data); err != nil {
		return nil, fmt.Errorf("read data: %w", err)
	}

	return data, nil
}

// Close closes the QUIC stream
func (qtc *QUICTransportConn) Close() error {
	if qtc.stream != nil {
		streamID := uint64((*qtc.stream).StreamID())
		qtc.connection.streamMutex.Lock()
		delete(qtc.connection.streams, streamID)
		qtc.connection.streamMutex.Unlock()

		atomic.AddInt64(&qtc.connection.streamCount, -1)
		qtc.connection.metrics.streamsClosed.Add(1)

		err := (*qtc.stream).Close()
		qtc.stream = nil
		return err
	}
	return nil
}

// LocalAddr returns local address
func (qtc *QUICTransportConn) LocalAddr() net.Addr {
	return qtc.localAddr
}

// RemoteAddr returns remote address
func (qtc *QUICTransportConn) RemoteAddr() net.Addr {
	return qtc.remoteAddr
}

// QUICTransportListener implements TransportListener using QUIC
type QUICTransportListener struct {
	listener            *quic.Listener
	handler             func(message []byte) error
	address             string
	config              *QUICConfig
	stopCh              chan struct{}
	stopOnce            sync.Once
	wg                  sync.WaitGroup
	streamLimiter       chan struct{}
	streamReadTimeout   time.Duration
	streamAcceptTimeout time.Duration
}

// NewQUICTransportListener creates a new QUIC listener
func NewQUICTransportListener(address string, config *QUICConfig) (*QUICTransportListener, error) {
	if config == nil {
		config = DefaultQUICConfig()
	}

	streamLimit := config.MaxListenerStreams
	if streamLimit <= 0 {
		streamLimit = 1024
	}

	streamReadTimeout := config.StreamReadTimeout
	if streamReadTimeout <= 0 {
		streamReadTimeout = 5 * time.Second
	}

	streamAcceptTimeout := config.StreamAcceptTimeout
	if streamAcceptTimeout <= 0 {
		streamAcceptTimeout = 500 * time.Millisecond
	}

	return &QUICTransportListener{
		address:             address,
		config:              config,
		stopCh:              make(chan struct{}),
		streamLimiter:       make(chan struct{}, streamLimit),
		streamReadTimeout:   streamReadTimeout,
		streamAcceptTimeout: streamAcceptTimeout,
	}, nil
}

// Start starts the QUIC listener
func (qtl *QUICTransportListener) Start() error {
	udpAddr, err := net.ResolveUDPAddr("udp", qtl.address)
	if err != nil {
		return fmt.Errorf("resolve UDP address %s: %w", qtl.address, err)
	}

	// QUIC configuration
	quicConfig := &quic.Config{
		MaxIdleTimeout:                 qtl.config.MaxIdleTimeout,
		MaxIncomingStreams:             int64(qtl.config.MaxStreams),
		MaxIncomingUniStreams:          int64(qtl.config.MaxStreams),
		KeepAlivePeriod:                qtl.config.KeepAlivePeriod,
		HandshakeIdleTimeout:           qtl.config.HandshakeTimeout,
		InitialStreamReceiveWindow:     qtl.config.InitialStreamReceiveWindow,
		MaxStreamReceiveWindow:         qtl.config.MaxStreamReceiveWindow,
		InitialConnectionReceiveWindow: qtl.config.InitialConnectionReceiveWindow,
		MaxConnectionReceiveWindow:     qtl.config.MaxConnectionReceiveWindow,
		Versions:                       []quic.Version{quic.Version1, quic.Version2},
	}

	// Create QUIC listener with optimized UDP buffer sizes
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("listen UDP %s: %w", qtl.address, err)
	}

	if err := setQUICUDPBuffer(udpConn, 16*1024*1024); err != nil {
		if err2 := setQUICUDPBuffer(udpConn, 8*1024*1024); err2 != nil {
			if err3 := setQUICUDPBuffer(udpConn, 4*1024*1024); err3 != nil {
				logging.Debug("Failed to set UDP buffer (non-fatal)", "err", err3)
			}
		}
	}

	// Generate self-signed cert for listener
	tlsCert, err := generateSelfSignedCert()
	if err != nil {
		return fmt.Errorf("generate TLS cert: %w", err)
	}

	tlsConfig := &tls.Config{
		NextProtos:   []string{"gridkv-quic"},
		Certificates: []tls.Certificate{*tlsCert},
	}

	listener, err := quic.Listen(udpConn, tlsConfig, quicConfig)
	if err != nil {
		return fmt.Errorf("listen QUIC: %w", err)
	}

	qtl.listener = listener

	// Start accepting connections
	qtl.wg.Add(1)
	go qtl.acceptConnections()

	return nil
}

// HandleMessage registers message handler
func (qtl *QUICTransportListener) HandleMessage(handler func(message []byte) error) TransportListener {
	qtl.handler = handler
	return qtl
}

func (qtl *QUICTransportListener) acquireStreamSlot() bool {
	if qtl.streamLimiter == nil {
		return true
	}
	select {
	case qtl.streamLimiter <- struct{}{}:
		return true
	default:
		return false
	}
}

func (qtl *QUICTransportListener) releaseStreamSlot() {
	if qtl.streamLimiter == nil {
		return
	}
	select {
	case <-qtl.streamLimiter:
	default:
	}
}

// Stop stops the QUIC listener
func (qtl *QUICTransportListener) Stop() error {
	var err error
	qtl.stopOnce.Do(func() {
		close(qtl.stopCh)
		if qtl.listener != nil {
			err = qtl.listener.Close()
		}
	})

	qtl.wg.Wait()
	return err
}

// Addr returns listener address
func (qtl *QUICTransportListener) Addr() net.Addr {
	if qtl.listener != nil {
		return qtl.listener.Addr()
	}
	return nil
}

func setQUICUDPBuffer(conn *net.UDPConn, size int) error {
	if conn == nil {
		return errors.New("UDP connection is nil")
	}

	rawConn, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("get syscall conn: %w", err)
	}

	var setErr error
	err = rawConn.Write(func(fd uintptr) bool {
		if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, size); err != nil {
			setErr = fmt.Errorf("set SO_RCVBUF: %w", err)
			return false
		}
		if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, size); err != nil {
			setErr = fmt.Errorf("set SO_SNDBUF: %w", err)
			return false
		}
		return true
	})

	if err != nil {
		return err
	}
	return setErr
}
func (qtl *QUICTransportListener) acceptConnections() {
	defer qtl.wg.Done()

	for {
		select {
		case <-qtl.stopCh:
			return
		default:
		}

		// Accept QUIC connection
		conn, err := qtl.listener.Accept(context.Background())
		if err != nil {
			select {
			case <-qtl.stopCh:
				return
			default:
			}
			if logging.Log.IsDebugEnabled() {
				logging.Debug("QUIC accept error", "err", err)
			}
			continue
		}

		// Handle connection in goroutine
		qtl.wg.Add(1)
		go func(c *quic.Conn) {
			defer qtl.wg.Done()
			qtl.handleConnection(c)
		}(conn)
	}
}

// handleConnection handles a QUIC connection
func (qtl *QUICTransportListener) handleConnection(conn *quic.Conn) {
	defer conn.CloseWithError(0, "connection handler exiting")

	for {
		select {
		case <-qtl.stopCh:
			return
		case <-conn.Context().Done():
			return
		default:
		}

		// Accept stream
		ctx := context.Background()
		var cancel context.CancelFunc
		if qtl.streamAcceptTimeout > 0 {
			ctx, cancel = context.WithTimeout(context.Background(), qtl.streamAcceptTimeout)
		}

		stream, err := conn.AcceptStream(ctx)
		if cancel != nil {
			cancel()
		}
		if err != nil {
			if err == context.DeadlineExceeded {
				continue
			}
			if logging.Log.IsDebugEnabled() {
				logging.Debug("QUIC accept stream error", "err", err)
			}
			return
		}

		if !qtl.acquireStreamSlot() {
			if logging.Log.IsDebugEnabled() {
				logging.Debug("Dropping QUIC stream: listener saturated", "remote", conn.RemoteAddr())
			}
			stream.CancelRead(0)
			stream.Close()
			continue
		}

		// Handle stream in goroutine (stream is already *quic.Stream)
		qtl.wg.Add(1)
		go func(s *quic.Stream) {
			defer qtl.wg.Done()
			defer qtl.releaseStreamSlot()
			qtl.handleStream(s)
		}(stream)
	}
}

// handleStream handles a QUIC stream
func (qtl *QUICTransportListener) handleStream(stream *quic.Stream) {
	defer (*stream).Close()

	if qtl.streamReadTimeout > 0 {
		_ = (*stream).SetReadDeadline(time.Now().Add(qtl.streamReadTimeout))
		defer (*stream).SetReadDeadline(time.Time{})
	}

	// Read length prefix
	lengthPrefix := make([]byte, 4)
	if _, err := io.ReadFull(stream, lengthPrefix); err != nil {
		if logging.Log.IsDebugEnabled() {
			logging.Debug("QUIC read length prefix error", "err", err)
		}
		return
	}

	// Parse length
	dataLength := binary.BigEndian.Uint32(lengthPrefix)

	// Validate length
	const maxMessageSize = 10 * 1024 * 1024 // 10MB max
	if dataLength == 0 || dataLength > maxMessageSize {
		if logging.Log.IsDebugEnabled() {
			logging.Debug("QUIC invalid message size", "size", dataLength)
		}
		return
	}

	// Read message
	data := make([]byte, dataLength)
	if _, err := io.ReadFull(stream, data); err != nil {
		if logging.Log.IsDebugEnabled() {
			logging.Debug("QUIC read message error", "err", err)
		}
		return
	}

	// Handle message
	if qtl.handler != nil {
		if err := qtl.handler(data); err != nil {
			if logging.Log.IsDebugEnabled() {
				logging.Debug("QUIC handler error", "err", err)
			}
		}
	}
}
