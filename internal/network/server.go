package network

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feellmoose/gridkv/internal/utils/logging"
)

// Handler handles incoming messages
type Handler func(ctx context.Context, remoteAddr string, data []byte) ([]byte, error)

// RequestHandler handles request-response pattern
type RequestHandler func(ctx context.Context, remoteAddr string, request []byte) ([]byte, error)

// Server provides network server interface
type Server interface {
	// Start starts server
	Start(ctx context.Context, address string, handler Handler) error

	// StartRequestResponse starts server with request-response handler
	StartRequestResponse(ctx context.Context, address string, handler RequestHandler) error

	// Stop stops server
	Stop(ctx context.Context) error

	// Address returns server address
	Address() string

	// Stats returns server statistics
	Stats() ServerStats
}

// ServerStats represents server statistics (simplified, key metrics only)
type ServerStats struct {
	Connections uint64 // Total connections (atomic)
	Messages    uint64 // Total messages (received + sent) (atomic)
	Bytes       uint64 // Total bytes (received + sent) (atomic)
	Errors      uint64 // Errors (atomic)
	ActiveConns int64  // Active connections (atomic)
}

// ServerConfig configures server
type ServerConfig struct {
	// Transport is underlying transport
	Transport Transport

	// MaxConns is maximum concurrent connections
	MaxConns int

	// ReadBufferSize is read buffer size
	ReadBufferSize int

	// WriteBufferSize is write buffer size
	WriteBufferSize int

	// EnableRequestResponse enables request-response pattern
	EnableRequestResponse bool

	// WorkerPoolSize is worker pool size for message handling
	WorkerPoolSize int

	// EnableBackpressure enables backpressure control
	EnableBackpressure bool

	// BackpressureThreshold is backpressure threshold (queued messages)
	BackpressureThreshold int
}

// DefaultServerConfig returns default server config
func DefaultServerConfig(transport Transport) ServerConfig {
	return ServerConfig{
		Transport:             transport,
		MaxConns:              1000,
		ReadBufferSize:        64 * 1024, // 64KB
		WriteBufferSize:       64 * 1024, // 64KB
		EnableRequestResponse: false,
		WorkerPoolSize:        100,
		EnableBackpressure:    true,
		BackpressureThreshold: 10000,
	}
}

type networkServer struct {
	cfg      ServerConfig
	listener Listener
	handler  Handler
	stats    struct {
		Connections atomic.Uint64
		Messages    atomic.Uint64
		Bytes       atomic.Uint64
		Errors      atomic.Uint64
		ActiveConns atomic.Int64
	}
	stopOnce    sync.Once
	connChOnce  sync.Once // Protect connCh close from multiple calls
	stopCh      chan struct{}
	wg          sync.WaitGroup
	activeConns sync.Map  // map[Conn]struct{} - track active connections
	connCh      chan Conn // Channel for connection distribution to workers
	workerWg    sync.WaitGroup
}

func NewServer(cfg ServerConfig) Server {
	workerPoolSize := cfg.WorkerPoolSize
	if workerPoolSize <= 0 {
		workerPoolSize = 100 // Reasonable default
	}
	if workerPoolSize > 5000 {
		workerPoolSize = 5000 // Cap to prevent excessive goroutines
	}
	return &networkServer{
		cfg:    cfg,
		stopCh: make(chan struct{}),
		connCh: make(chan Conn, workerPoolSize*2), // Buffer for connection distribution
	}
}

func (s *networkServer) Start(ctx context.Context, address string, handler Handler) error {
	s.handler = handler
	ln, err := s.cfg.Transport.Listen(ctx, address)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", address, err)
	}
	s.listener = ln

	// Start worker pool
	workerPoolSize := s.cfg.WorkerPoolSize
	if workerPoolSize <= 0 {
		workerPoolSize = 100
	}
	for i := 0; i < workerPoolSize; i++ {
		s.workerWg.Add(1)
		go s.workerLoop()
	}

	s.wg.Add(1)
	go s.acceptLoop()
	return nil
}

func (s *networkServer) StartRequestResponse(ctx context.Context, address string, handler RequestHandler) error {
	return s.Start(ctx, address, func(ctx context.Context, remoteAddr string, data []byte) ([]byte, error) {
		return handler(ctx, remoteAddr, data)
	})
}

func (s *networkServer) acceptLoop() {
	defer s.wg.Done()

	for {
		select {
		case <-s.stopCh:
			return
		default:
		}
		// Use context with timeout for Accept to allow checking stopCh
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		conn, err := s.listener.Accept(ctx)
		cancel()
		if err != nil {
			// Check if we should stop
			select {
			case <-s.stopCh:
				return
			default:
			}
			// Continue on timeout or other errors
			continue
		}
		s.stats.Connections.Add(1)
		s.stats.ActiveConns.Add(1)
		s.activeConns.Store(conn, struct{}{})

		// Distribute connection to worker pool
		// Use non-blocking check first to avoid race condition with connCh close
		select {
		case <-s.stopCh:
			conn.Close()
			return
		default:
		}

		// Try to send with timeout to avoid blocking if connCh is being closed
		select {
		case s.connCh <- conn:
		case <-s.stopCh:
			conn.Close()
			return
		case <-time.After(10 * time.Millisecond):
			// If we can't send quickly, connection might be closing
			// Check stopCh again and close connection
			select {
			case <-s.stopCh:
				conn.Close()
				return
			default:
				// Still try to send, but if it fails, close connection
				select {
				case s.connCh <- conn:
				default:
					conn.Close()
					return
				}
			}
		}
	}
}

func (s *networkServer) workerLoop() {
	defer s.workerWg.Done()
	for conn := range s.connCh {
		s.handleConn(conn)
	}
}

func (s *networkServer) handleConn(conn Conn) {
	defer func() {
		s.stats.ActiveConns.Add(-1)
		s.activeConns.Delete(conn)
		conn.Close()
	}()

	const maxHandlerErrors = 10
	handlerErrorCount := 0
	const maxMessageSize = 64 * 1024 * 1024 // 64MB max message size

	for {
		// Check if we should stop before each receive
		select {
		case <-s.stopCh:
			return
		default:
		}
		// Use shorter timeout to allow faster shutdown
		// When Stop() is called, connections will be closed, causing Receive() to return immediately
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		data, err := conn.Receive(ctx)
		cancel()
		if err != nil {
			// Check if we should stop
			select {
			case <-s.stopCh:
				return
			default:
			}
			// If this was a read timeout, keep the connection open and continue waiting.
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				handlerErrorCount = 0
				continue
			}
			// For non-timeout errors, close connection
			return
		}

		// Validate message size
		if len(data) > maxMessageSize {
			s.stats.Errors.Add(1)
			logging.Warn("message too large, closing connection",
				"remoteAddr", conn.RemoteAddr(),
				"size", len(data),
				"maxSize", maxMessageSize)
			return
		}

		s.stats.Messages.Add(1)
		s.stats.Bytes.Add(uint64(len(data)))
		resp, err := s.handler(context.Background(), conn.RemoteAddr(), data)
		if err != nil {
			s.stats.Errors.Add(1)
			// Response message types (MessageTypeResponse, ReadResponse, GossipResponse)
			// don't need handlers and are expected to fail
			if err == ErrHandlerNotFound {
				var msgType string = "unknown"
				var msgTypeVal uint8 = 0
				if len(data) >= 22 {
					msgTypeVal = data[0]
					msgType = getMessageTypeName(MessageType(msgTypeVal))
				}
				logging.Debug("Message handler not found",
					"remoteAddr", conn.RemoteAddr(),
					"messageType", msgType,
					"messageTypeVal", msgTypeVal,
					"dataLen", len(data),
					"error", err)
			} else {
				logging.Debug("message handler error", "remoteAddr", conn.RemoteAddr(), "error", err)
			}
			handlerErrorCount++
			if handlerErrorCount >= maxHandlerErrors {
				logging.Warn("too many handler errors, closing connection", "remoteAddr", conn.RemoteAddr(), "errorCount", handlerErrorCount)
				return
			}
			continue
		}
		handlerErrorCount = 0
		if resp != nil {
			s.stats.Messages.Add(1)
			s.stats.Bytes.Add(uint64(len(resp)))
			if err := conn.Send(context.Background(), resp); err != nil {
				logging.Debug("Failed to send response", "remoteAddr", conn.RemoteAddr(), "error", err)
				return
			}
		}
	}
}

func (s *networkServer) Stop(ctx context.Context) error {
	var err error
	s.stopOnce.Do(func() {
		// Signal stop first to prevent acceptLoop from accepting new connections
		close(s.stopCh)

		// Close listener to stop accepting new connections
		// This must be done after closing stopCh to ensure acceptLoop checks stopCh first
		if s.listener != nil {
			err = s.listener.Close()
		}

		// Wait for acceptLoop to exit before closing connCh to avoid race condition
		acceptDone := make(chan struct{})
		go func() {
			s.wg.Wait()
			close(acceptDone)
		}()

		// Wait for accept loop with timeout, but don't block indefinitely
		select {
		case <-acceptDone:
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
			// Timeout waiting for accept loop, proceed anyway
		}

		// Force close all active connections to unblock Receive() calls in handleConn
		// This ensures workers can exit even if blocked in Receive()
		deadline := time.Now().Add(-1 * time.Second) // Past time to force immediate timeout
		var conns []Conn
		s.activeConns.Range(func(key, value interface{}) bool {
			if conn, ok := key.(Conn); ok {
				conns = append(conns, conn)
			}
			return true
		})

		// Close all collected connections to unblock Receive() calls
		for _, conn := range conns {
			// Set read deadline to force immediate timeout on blocked Receive() calls
			_ = conn.SetReadDeadline(deadline)
			// Close connection to unblock any blocked operations
			conn.Close()
		}

		// Close connCh after acceptLoop has exited to avoid race condition
		// This signals workerLoop to exit (for conn := range s.connCh will end)
		s.connChOnce.Do(func() {
			close(s.connCh)
		})

		// Wait for all worker goroutines to exit after connections are closed
		// Use context timeout or extended timeout, whichever is shorter
		workerDone := make(chan struct{})
		go func() {
			s.workerWg.Wait()
			close(workerDone)
		}()

		// Calculate timeout based on context
		waitTimeout := 10 * time.Second
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining > 0 && remaining < waitTimeout {
				waitTimeout = remaining
			}
		}

		select {
		case <-workerDone:
			// All worker goroutines exited
		case <-ctx.Done():
			// Context timeout, return anyway
		case <-time.After(waitTimeout):
			// Extended timeout for large clusters
		}
	})
	return err
}

func (s *networkServer) Address() string {
	if s.listener == nil {
		return ""
	}
	return s.listener.Address()
}

func (s *networkServer) Stats() ServerStats {
	return ServerStats{
		Connections: s.stats.Connections.Load(),
		Messages:    s.stats.Messages.Load(),
		Bytes:       s.stats.Bytes.Load(),
		Errors:      s.stats.Errors.Load(),
		ActiveConns: s.stats.ActiveConns.Load(),
	}
}

// getMessageTypeName returns a human-readable name for a message type
func getMessageTypeName(msgType MessageType) string {
	switch msgType {
	case MessageTypeUnknown:
		return "Unknown"
	case MessageTypeRequest:
		return "Request"
	case MessageTypeResponse:
		return "Response"
	case MessageTypeOneWay:
		return "OneWay"
	case MessageTypeHeartbeat:
		return "Heartbeat"
	case MessageTypeError:
		return "Error"
	case MessageTypePing:
		return "Ping"
	case MessageTypeConnect:
		return "Connect"
	case MessageTypeLeave:
		return "Leave"
	case MessageTypeGossipPush:
		return "GossipPush"
	case MessageTypeGossipPull:
		return "GossipPull"
	case MessageTypeGossipResponse:
		return "GossipResponse"
	case MessageTypeReadRequest:
		return "ReadRequest"
	case MessageTypeReadResponse:
		return "ReadResponse"
	case MessageTypeSyncOperation:
		return "SyncOperation"
	default:
		return "UnknownType"
	}
}
