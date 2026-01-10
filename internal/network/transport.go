package network

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/feellmoose/gridkv/internal/utils/logging"
)

// Error type constants for efficient error checking
const (
	errClosedConnection = "use of closed network connection"
	errConnectionReset  = "connection reset by peer"
)

// isNetworkError checks if error is a common network error that shouldn't be logged
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, errClosedConnection) ||
		strings.Contains(errStr, errConnectionReset)
}

// TransportType specifies transport protocol
type TransportType string

const (
	TransportTCP  TransportType = "tcp"
	TransportQUIC TransportType = "quic"
)

// Transport constructors
func NewTransport(cfg TransportConfig) (Transport, error) {
	switch cfg.Type {
	case TransportTCP:
		return NewTCPTransport(cfg), nil
	case TransportQUIC:
		return NewQUICTransport(cfg), nil
	default:
		return nil, ErrTransportNotSupported
	}
}

// Conn represents a network connection
type Conn interface {
	// Send sends data to remote endpoint
	Send(ctx context.Context, data []byte) error

	// Receive receives data from remote endpoint
	Receive(ctx context.Context) ([]byte, error)

	// RemoteAddr returns remote address
	RemoteAddr() string

	// Close closes connection
	Close() error
}

// Listener listens for incoming connections
type Listener interface {
	// Accept accepts incoming connection
	Accept(ctx context.Context) (Conn, error)

	// Address returns listening address
	Address() string

	// Close closes listener
	Close() error
}

// Transport provides network transport abstraction
type Transport interface {
	// Dial creates connection to remote address
	Dial(ctx context.Context, address string) (Conn, error)

	// Listen starts listening on address
	Listen(ctx context.Context, address string) (Listener, error)

	// Close closes transport
	Close() error
}

// TransportConfig configures transport
type TransportConfig struct {
	// Type is transport protocol type
	Type TransportType

	// Timeout is connection timeout
	Timeout time.Duration

	// ReadTimeout is read operation timeout
	ReadTimeout time.Duration

	// WriteTimeout is write operation timeout
	WriteTimeout time.Duration

	// KeepAlive enables keep-alive
	KeepAlive bool

	// KeepAliveInterval is keep-alive interval
	KeepAliveInterval time.Duration

	// MaxMessageSize is maximum message size (bytes)
	MaxMessageSize int

	// EnableZeroCopy enables zero-copy
	EnableZeroCopy bool
}

// DefaultTransportConfig returns default transport config
func DefaultTransportConfig() TransportConfig {
	return TransportConfig{
		Type:              TransportTCP,
		Timeout:           5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		KeepAlive:         true,
		KeepAliveInterval: 30 * time.Second,
		MaxMessageSize:    10 * 1024 * 1024, // 10MB
		EnableZeroCopy:    true,
	}
}

// TCP transport implementation
type tcpTransport struct {
	cfg TransportConfig
}

func NewTCPTransport(cfg TransportConfig) Transport {
	return &tcpTransport{cfg: cfg}
}

func (t *tcpTransport) Dial(ctx context.Context, address string) (Conn, error) {
	// Use context timeout if available, otherwise use config timeout
	timeout := t.cfg.Timeout
	if ctx != nil {
		if dl, ok := ctx.Deadline(); ok {
			timeout = time.Until(dl)
			if timeout < 0 {
				timeout = t.cfg.Timeout
			}
		}
	}
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
		defer cancel()
	} else if ctx.Err() != nil {
		return nil, fmt.Errorf("context already cancelled: %w", ctx.Err())
	}
	dialer := &net.Dialer{
		Timeout:   timeout,
		DualStack: false,
	}
	if t.cfg.KeepAlive && t.cfg.KeepAliveInterval > 0 {
		dialer.KeepAlive = t.cfg.KeepAliveInterval
	}
	raw, err := dialer.DialContext(ctx, "tcp4", address)
	if err != nil {
		raw, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return nil, fmt.Errorf("dial %s failed (timeout=%v): %w", address, timeout, err)
	}
	if tc, ok := raw.(*net.TCPConn); ok && t.cfg.KeepAlive {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(t.cfg.KeepAliveInterval)
	}
	return &tcpConn{conn: raw, cfg: t.cfg}, nil
}

func (t *tcpTransport) Listen(ctx context.Context, address string) (Listener, error) {
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	return &tcpListener{ln: ln, cfg: t.cfg}, nil
}

func (t *tcpTransport) Close() error { return nil }

type tcpConn struct {
	conn              net.Conn
	cfg               TransportConfig
	reader            *bufio.Reader // Buffered reader for efficient reading
	once              sync.Once     // Lazy initialization
	lastHealthCheck   int64         // Unix timestamp of last health check
	healthCheckCached int32         // Cached health check result
}

func deadlineFromContext(ctx context.Context, fallback time.Duration) time.Time {
	if ctx != nil {
		if dl, ok := ctx.Deadline(); ok {
			return dl
		}
	}
	if fallback > 0 {
		return time.Now().Add(fallback)
	}
	return time.Time{}
}

var lengthBufPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 4)
	},
}

func (c *tcpConn) Send(ctx context.Context, data []byte) error {
	if deadline := deadlineFromContext(ctx, c.cfg.WriteTimeout); !deadline.IsZero() {
		_ = c.conn.SetWriteDeadline(deadline)
	}
	if len(data) > c.cfg.MaxMessageSize {
		return fmt.Errorf("message too large: %d", len(data))
	}

	if c.cfg.EnableZeroCopy && len(data) > 4096 {
		buf := lengthBufPool.Get().([]byte)
		binary.BigEndian.PutUint32(buf, uint32(len(data)))
		// Write length header
		if _, err := c.conn.Write(buf); err != nil {
			lengthBufPool.Put(buf)
			logging.Debug("tcpConn.Send: failed to write length header", "remote", c.conn.RemoteAddr(), "error", err)
			return err
		}
		lengthBufPool.Put(buf)
		// Use io.Copy for zero-copy (kernel handles copy)
		_, err := io.Copy(c.conn, bytes.NewReader(data))
		if err != nil {
			logging.Debug("tcpConn.Send: failed in io.Copy", "remote", c.conn.RemoteAddr(), "error", err, "dataLen", len(data))
		}
		return err
	}

	if len(data) <= 4096 {
		buf := lengthBufPool.Get().([]byte)
		binary.BigEndian.PutUint32(buf, uint32(len(data)))
		// Write both header and data in sequence (TCP will coalesce small writes)
		// Use buffered writer for better batching
		if _, err := c.conn.Write(buf); err != nil {
			lengthBufPool.Put(buf)
			logging.Debug("tcpConn.Send: failed to write length header", "remote", c.conn.RemoteAddr(), "error", err)
			return err
		}
		lengthBufPool.Put(buf)
		_, err := c.conn.Write(data)
		if err != nil {
			logging.Debug("tcpConn.Send: failed to write data", "remote", c.conn.RemoteAddr(), "error", err, "dataLen", len(data))
		}
		return err
	}

	// Fallback for medium-sized messages
	buf := lengthBufPool.Get().([]byte)
	binary.BigEndian.PutUint32(buf, uint32(len(data)))
	if _, err := c.conn.Write(buf); err != nil {
		lengthBufPool.Put(buf)
		logging.Debug("tcpConn.Send: failed to write length header", "remote", c.conn.RemoteAddr(), "error", err)
		return err
	}
	lengthBufPool.Put(buf)
	_, err := c.conn.Write(data)
	if err != nil {
		logging.Debug("tcpConn.Send: failed to write data", "remote", c.conn.RemoteAddr(), "error", err, "dataLen", len(data))
	}
	return err
}

var lengthReadBufPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 4)
	},
}

func (c *tcpConn) Receive(ctx context.Context) ([]byte, error) {
	// Lazy initialization of buffered reader (thread-safe)
	c.once.Do(func() {
		c.reader = bufio.NewReader(c.conn)
	})

	if deadline := deadlineFromContext(ctx, c.cfg.ReadTimeout); !deadline.IsZero() {
		_ = c.conn.SetReadDeadline(deadline)
	}

	// Read length header using buffered reader
	var length uint32
	if err := binary.Read(c.reader, binary.BigEndian, &length); err != nil {
		// Filter out common non-actionable errors to reduce log noise
		if err != io.EOF {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				// Timeout errors are normal and expected
			} else if isNetworkError(err) {
				// Connection issues during shutdown/normal network behavior - don't log
			} else {
				logging.Debug("tcpConn.Receive: failed to read length header", "remote", c.conn.RemoteAddr(), "error", err)
			}
		}
		return nil, err
	}

	size := int(length)
	if size > c.cfg.MaxMessageSize {
		return nil, ErrMessageTooLarge
	}

	// Read payload using buffered reader (reduces syscalls)
	buf := make([]byte, size)
	if _, err := io.ReadFull(c.reader, buf); err != nil {
		// Filter out common non-actionable errors to reduce log noise
		if err != io.EOF {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				// Timeout errors are normal and expected
			} else if isNetworkError(err) {
				// Connection issues during shutdown/normal network behavior - don't log
			} else {
				logging.Debug("tcpConn.Receive: failed to read payload", "remote", c.conn.RemoteAddr(), "error", err)
			}
		}
		return nil, err
	}
	return buf, nil
}

func (c *tcpConn) RemoteAddr() string { return c.conn.RemoteAddr().String() }

func (c *tcpConn) Close() error { return c.conn.Close() }

type tcpListener struct {
	ln  net.Listener
	cfg TransportConfig
}

func (l *tcpListener) Accept(ctx context.Context) (Conn, error) {
	conn, err := l.ln.Accept()
	if err != nil {
		return nil, err
	}
	return &tcpConn{conn: conn, cfg: l.cfg}, nil
}

func (l *tcpListener) Address() string { return l.ln.Addr().String() }

func (l *tcpListener) Close() error { return l.ln.Close() }

// QUIC transport implementation (minimal)
type quicTransport struct {
	cfg   TransportConfig
	tls   *tls.Config
	quic  *quic.Config
	close func() error
}

func NewQUICTransport(cfg TransportConfig) Transport {
	// use ephemeral self-signed credentials for internal traffic; caller can override
	tlsCfg, err := defaultQUICServerTLSConfig()
	if err != nil {
		// fall back to insecure config to preserve compatibility
		tlsCfg = &tls.Config{InsecureSkipVerify: true, ServerName: "localhost", NextProtos: []string{"gridkv"}}
	}
	quicCfg := &quic.Config{
		KeepAlivePeriod:      time.Second,
		HandshakeIdleTimeout: 10 * time.Second,
		MaxIdleTimeout:       30 * time.Second,
		EnableDatagrams:      true,
	}
	return &quicTransport{cfg: cfg, tls: tlsCfg, quic: quicCfg}
}

func defaultQUICServerTLSConfig() (*tls.Config, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"localhost"},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return nil, err
	}

	cert := tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  priv,
	}

	return &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true, // tests rely on self-signed cert
		ServerName:         "localhost",
		NextProtos:         []string{"gridkv"},
	}, nil
}

func (t *quicTransport) Dial(ctx context.Context, address string) (Conn, error) {
	session, err := quic.DialAddr(ctx, address, t.tls, t.quic)
	if err != nil {
		return nil, fmt.Errorf("quic dial failed: %w", err)
	}
	stream, err := session.OpenStreamSync(ctx)
	if err != nil {
		_ = session.CloseWithError(0, "stream open failed")
		return nil, fmt.Errorf("quic stream open failed: %w", err)
	}
	return &quicConn{session: session, stream: stream, cfg: t.cfg}, nil
}

func (t *quicTransport) Listen(ctx context.Context, address string) (Listener, error) {
	ln, err := quic.ListenAddr(address, t.tls, t.quic)
	if err != nil {
		return nil, err
	}
	return &quicListener{ln: ln, cfg: t.cfg}, nil
}

func (t *quicTransport) Close() error {
	if t.close != nil {
		return t.close()
	}
	return nil
}

type quicConn struct {
	session *quic.Conn
	stream  *quic.Stream
	cfg     TransportConfig
}

func (c *quicConn) Send(ctx context.Context, data []byte) error {
	if deadline := deadlineFromContext(ctx, c.cfg.WriteTimeout); !deadline.IsZero() {
		_ = c.stream.SetWriteDeadline(deadline)
	}
	if len(data) > c.cfg.MaxMessageSize {
		return fmt.Errorf("message too large: %d bytes (max %d)", len(data), c.cfg.MaxMessageSize)
	}

	// Reuse buffer from pool
	buf := lengthBufPool.Get().([]byte)
	defer lengthBufPool.Put(buf)
	binary.BigEndian.PutUint32(buf, uint32(len(data)))

	if _, err := c.stream.Write(buf); err != nil {
		logging.Debug("quicConn.Send: failed to write length header", "remote", c.session.RemoteAddr(), "error", err)
		return fmt.Errorf("write length header: %w", err)
	}
	if _, err := c.stream.Write(data); err != nil {
		logging.Debug("quicConn.Send: failed to write data", "remote", c.session.RemoteAddr(), "error", err, "dataLen", len(data))
		return fmt.Errorf("write data: %w", err)
	}
	return nil
}

func (c *quicConn) Receive(ctx context.Context) ([]byte, error) {
	if deadline := deadlineFromContext(ctx, c.cfg.ReadTimeout); !deadline.IsZero() {
		_ = c.stream.SetReadDeadline(deadline)
	}

	// Reuse buffer from pool
	lenBuf := lengthReadBufPool.Get().([]byte)
	defer lengthReadBufPool.Put(lenBuf)

	if _, err := io.ReadFull(c.stream, lenBuf); err != nil {
		// Don't log EOF as it's normal connection closure
		if err != io.EOF {
			logging.Debug("quicConn.Receive: failed to read length header", "remote", c.session.RemoteAddr(), "error", err)
		}
		return nil, fmt.Errorf("read length header: %w", err)
	}
	size := int(binary.BigEndian.Uint32(lenBuf))
	if size > c.cfg.MaxMessageSize {
		return nil, fmt.Errorf("%w: %d bytes (max %d)", ErrMessageTooLarge, size, c.cfg.MaxMessageSize)
	}
	if size < 0 {
		return nil, fmt.Errorf("invalid message size: %d", size)
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(c.stream, buf); err != nil {
		// Don't log EOF as it's normal connection closure
		if err != io.EOF {
			logging.Debug("quicConn.Receive: failed to read payload", "remote", c.session.RemoteAddr(), "error", err, "size", size)
		}
		return nil, fmt.Errorf("read payload: %w", err)
	}
	return buf, nil
}

func (c *quicConn) RemoteAddr() string { return c.session.RemoteAddr().String() }

func (c *quicConn) Close() error {
	var errs []error
	if err := c.stream.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close stream: %w", err))
	}
	if err := c.session.CloseWithError(0, "connection closed"); err != nil {
		errs = append(errs, fmt.Errorf("close session: %w", err))
	}
	if len(errs) > 0 {
		return fmt.Errorf("quic close errors: %v", errs)
	}
	return nil
}

type quicListener struct {
	ln  *quic.Listener
	cfg TransportConfig
}

func (l *quicListener) Accept(ctx context.Context) (Conn, error) {
	sess, err := l.ln.Accept(ctx)
	if err != nil {
		return nil, fmt.Errorf("accept session: %w", err)
	}
	stream, err := sess.AcceptStream(ctx)
	if err != nil {
		_ = sess.CloseWithError(0, "stream accept failed")
		return nil, fmt.Errorf("accept stream: %w", err)
	}
	return &quicConn{session: sess, stream: stream, cfg: l.cfg}, nil
}

func (l *quicListener) Address() string { return l.ln.Addr().String() }

func (l *quicListener) Close() error { return l.ln.Close() }
