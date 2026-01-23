package network

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/feellmoose/gridkv/internal/utils/bufferpool"
	"github.com/feellmoose/gridkv/internal/utils/compress"
)

// Network provides unified network interface for cluster operations
// Implements lifecycle.Component for unified resource management
type Network interface {
	// Client returns network client
	Client() Client

	// Server returns network server
	Server() Server

	// Start starts network layer
	Start(ctx context.Context) error

	// Stop stops network layer
	Stop(ctx context.Context) error

	// Name returns component name for lifecycle management
	Name() string

	// Close stops network layer (lifecycle.Component interface)
	Close(ctx context.Context) error

	// Send sends message to address
	Send(ctx context.Context, address string, data []byte) error

	// SendMessage sends typed message
	SendMessage(ctx context.Context, address string, msg *Message) error

	// Request sends request and waits for response
	Request(ctx context.Context, address string, request []byte, timeout time.Duration) ([]byte, error)

	// RequestMessage sends typed message request and waits for typed response
	RequestMessage(ctx context.Context, address string, request *Message, timeout time.Duration) (*Message, error)

	// RegisterHandler registers message handler
	RegisterHandler(msgType MessageType, handler Handler) error

	// Cluster adapter methods
	// SendFunc returns send function for cluster (MemberMgr)
	SendFunc() func(address string, msg interface{}) error
	// SendBytesFunc returns send bytes function for gossip
	SendBytesFunc() func(address string, data []byte) error
	// GetFunc returns get function for remote reads (Reader)
	GetFunc() func(nodeID string, key string) (interface{}, error)
	// ReceiveFunc returns receive function for gossip
	ReceiveFunc() func() ([]byte, error)
	// RegisterMessageHandler registers handler for specific message type
	RegisterMessageHandler(msgType MessageType, handler Handler) error

	// GetPool returns connection pool for metrics
	GetPool() ConnPool
}

// NetworkConfig configures network layer
type NetworkConfig struct {
	// LocalAddress is local listening address
	LocalAddress string

	// TransportType is transport protocol type
	TransportType TransportType

	// TransportConfig is transport configuration
	TransportConfig TransportConfig

	// PoolConfig is connection pool configuration
	PoolConfig PoolConfig

	// ClientConfig is client configuration
	ClientConfig ClientConfig

	// ServerConfig is server configuration
	ServerConfig ServerConfig

	// BackpressureConfig is backpressure configuration
	BackpressureConfig BackpressureConfig

	// EnableMetrics enables metrics collection
	EnableMetrics bool
}

// DefaultNetworkConfig returns default network config
func DefaultNetworkConfig(localAddress string) NetworkConfig {
	transportConfig := DefaultTransportConfig()
	transportConfig.Type = TransportTCP

	poolConfig := PoolConfig{}               // Must be set with actual transport
	clientConfig := DefaultClientConfig(nil) // Must be set with actual pool
	serverConfig := ServerConfig{}           // Must be set with actual transport
	backpressureConfig := DefaultBackpressureConfig()

	return NetworkConfig{
		LocalAddress:       localAddress,
		TransportType:      TransportTCP,
		TransportConfig:    transportConfig,
		PoolConfig:         poolConfig,
		ClientConfig:       clientConfig,
		ServerConfig:       serverConfig,
		BackpressureConfig: backpressureConfig,
		EnableMetrics:      true,
	}
}

type networkImpl struct {
	cfg          NetworkConfig
	transport    Transport
	pool         ConnPool
	client       Client
	server       Server
	router       *simpleRouter
	backpressure *simpleBackpressure
}

func (n *networkImpl) GetPool() ConnPool {
	return n.pool
}

// NewNetwork builds a full stack using provided config.
func NewNetwork(cfg NetworkConfig) (Network, error) {
	transport, err := NewTransport(cfg.TransportConfig)
	if err != nil {
		return nil, err
	}
	poolCfg := cfg.PoolConfig
	poolCfg.Transport = transport
	pool := NewConnPool(poolCfg)
	client := NewClient(ClientConfig{
		Pool:              pool,
		DefaultTimeout:    cfg.ClientConfig.DefaultTimeout,
		RetryCount:        cfg.ClientConfig.RetryCount,
		RetryBackoff:      cfg.ClientConfig.RetryBackoff,
		EnableCompression: cfg.ClientConfig.EnableCompression,
		MaxRetries:        cfg.ClientConfig.MaxRetries,
	})
	server := NewServer(ServerConfig{
		Transport:             transport,
		MaxConns:              cfg.ServerConfig.MaxConns,
		ReadBufferSize:        cfg.ServerConfig.ReadBufferSize,
		WriteBufferSize:       cfg.ServerConfig.WriteBufferSize,
		EnableRequestResponse: cfg.ServerConfig.EnableRequestResponse,
		WorkerPoolSize:        cfg.ServerConfig.WorkerPoolSize,
		EnableBackpressure:    cfg.ServerConfig.EnableBackpressure,
		BackpressureThreshold: cfg.ServerConfig.BackpressureThreshold,
	})
	r := NewRouter()
	// Register a default no-op handler for one-way messages so probes and
	// fire-and-forget messages do not cause handler-not-found errors.
	_ = r.Register(MessageTypeOneWay, func(ctx context.Context, remote string, data []byte) ([]byte, error) {
		return nil, nil
	})

	return &networkImpl{
		cfg:          cfg,
		transport:    transport,
		pool:         pool,
		client:       client,
		server:       server,
		router:       r,
		backpressure: NewBackpressure(cfg.BackpressureConfig),
	}, nil
}

func (n *networkImpl) Client() Client { return n.client }

func (n *networkImpl) Server() Server { return n.server }

func (n *networkImpl) Name() string { return "network" }

func (n *networkImpl) Start(ctx context.Context) error {
	return n.server.StartRequestResponse(ctx, n.cfg.LocalAddress, func(ctx context.Context, remoteAddr string, data []byte) ([]byte, error) {
		if len(data) < 22 {
			return nil, nil
		}
		msg, err := decodeMessage(data)
		if err != nil {
			return nil, nil
		}
		resp, err := n.router.Route(ctx, remoteAddr, msg)
		if err != nil {
			return nil, err
		}
		if resp == nil {
			return nil, nil
		}
		return encodeMessage(resp)
	})
}

func (n *networkImpl) Stop(ctx context.Context) error {
	return n.Close(ctx)
}

func (n *networkImpl) Close(ctx context.Context) error {
	var errs []error

	// Stop server first to prevent new connections
	if n.server != nil {
		if err := n.server.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("server stop failed: %w", err))
		}
	}

	// Close connection pool (client.Close() calls pool.Close(), but we want explicit control)
	// Using sync.Once in pool ensures idempotency even if called multiple times
	if n.pool != nil {
		if err := n.pool.Close(); err != nil {
			errs = append(errs, fmt.Errorf("pool close failed: %w", err))
		}
	}

	// Close client (may call pool.Close() again, but sync.Once protects it)
	if n.client != nil {
		if err := n.client.Close(); err != nil {
			errs = append(errs, fmt.Errorf("client close failed: %w", err))
		}
	}

	// Close transport last
	if n.transport != nil {
		if err := n.transport.Close(); err != nil {
			errs = append(errs, fmt.Errorf("transport close failed: %w", err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("errors during network stop: %v", errs)
	}
	return nil
}

func (n *networkImpl) Send(ctx context.Context, address string, data []byte) error {
	if !n.backpressure.Allow() {
		return ErrBackpressure
	}
	if err := n.backpressure.Acquire(); err != nil {
		return err
	}
	defer n.backpressure.Release()
	return n.client.Send(ctx, address, data)
}

func (n *networkImpl) SendMessage(ctx context.Context, address string, msg *Message) error {
	data, err := encodeMessage(msg)
	if err != nil {
		return err
	}
	return n.Send(ctx, address, data)
}

func (n *networkImpl) Request(ctx context.Context, address string, request []byte, timeout time.Duration) ([]byte, error) {
	msg := &Message{
		Type:      MessageTypeRequest,
		ID:        uint64(time.Now().UnixNano()),
		Data:      request,
		Timestamp: time.Now().UnixNano(),
	}
	data, err := encodeMessage(msg)
	if err != nil {
		return nil, err
	}
	respData, err := n.client.Request(ctx, address, data, timeout)
	if err != nil {
		return nil, err
	}
	respMsg, err := decodeMessage(respData)
	if err != nil {
		return nil, err
	}
	return respMsg.Data, nil
}

func (n *networkImpl) RequestMessage(ctx context.Context, address string, request *Message, timeout time.Duration) (*Message, error) {
	data, err := encodeMessage(request)
	if err != nil {
		return nil, err
	}
	respData, err := n.client.Request(ctx, address, data, timeout)
	if err != nil {
		return nil, err
	}
	respMsg, err := decodeMessage(respData)
	if err != nil {
		return nil, err
	}
	return respMsg, nil
}

func (n *networkImpl) RegisterHandler(msgType MessageType, handler Handler) error {
	return n.router.Register(msgType, handler)
}

// Cluster adapter methods
func (n *networkImpl) SendFunc() func(address string, msg interface{}) error {
	return func(address string, msg interface{}) error {
		ctx, cancel := context.WithTimeout(context.Background(), n.cfg.ClientConfig.DefaultTimeout)
		defer cancel()
		if b, ok := msg.([]byte); ok {
			return n.Send(ctx, address, b)
		}
		return ErrInvalidMessage
	}
}

func (n *networkImpl) SendBytesFunc() func(address string, data []byte) error {
	return func(address string, data []byte) error {
		ctx, cancel := context.WithTimeout(context.Background(), n.cfg.ClientConfig.DefaultTimeout)
		defer cancel()
		return n.Send(ctx, address, data)
	}
}

func (n *networkImpl) GetFunc() func(nodeID string, key string) (interface{}, error) {
	return func(nodeID string, key string) (interface{}, error) {
		return nil, ErrHandlerNotFound
	}
}

func (n *networkImpl) ReceiveFunc() func() ([]byte, error) {
	// server side handler already routes; for adapter we expose blocking receive via channel
	ch := make(chan []byte, 1)
	_ = n.RegisterHandler(MessageTypeOneWay, func(ctx context.Context, remote string, data []byte) ([]byte, error) {
		ch <- data
		return nil, nil
	})
	return func() ([]byte, error) {
		select {
		case d := <-ch:
			return d, nil
		case <-time.After(n.cfg.ClientConfig.DefaultTimeout):
			return nil, context.DeadlineExceeded
		}
	}
}

func (n *networkImpl) RegisterMessageHandler(msgType MessageType, handler Handler) error {
	return n.RegisterHandler(msgType, handler)
}

// encodeMessage encodes message to bytes (inline codec)
// Layout: |type(1)|flags(1)|id(8)|ts(8)|len(4)|payload|
// flags: bit0 compressed
const msgFlagCompressed = 1 << 0

var messagePool = bufferpool.NewTieredBufferPool()

func encodeMessage(msg *Message) ([]byte, error) {
	payload := msg.Data
	flags := byte(0)

	// Compress payload if needed (directly into target buffer if possible)
	const networkCompressThreshold = 256 // Compress messages >256 bytes
	var finalPayload []byte

	if !msg.Compressed && len(payload) > networkCompressThreshold {
		boundSize := compress.CompressBound(len(payload))
		total := 22 + boundSize

		poolBuf := messagePool.Get(total)
		var buf []byte
		var fromPool bool

		if cap(poolBuf) >= total && total > 256 {
			buf = poolBuf[:total]
			fromPool = true
		} else {
			buf = make([]byte, total)
			if cap(poolBuf) > 0 {
				messagePool.Put(poolBuf)
			}
		}

		compressed, n, ok := compress.CompressTo(payload, buf[22:], networkCompressThreshold)
		if ok && n < len(payload)*90/100 {
			finalPayload = compressed
			flags |= msgFlagCompressed
			total = 22 + n
		} else {
			finalPayload = payload
			copy(buf[22:], payload)
		}

		buf[0] = byte(msg.Type)
		buf[1] = flags
		binary.BigEndian.PutUint64(buf[2:], msg.ID)
		binary.BigEndian.PutUint64(buf[10:], uint64(msg.Timestamp))
		binary.BigEndian.PutUint32(buf[18:], uint32(len(finalPayload)))

		if fromPool {
			result := make([]byte, total)
			copy(result, buf[:total])
			messagePool.Put(poolBuf)
			return result, nil
		}
		return buf[:total], nil
	}

	if msg.Compressed {
		flags |= msgFlagCompressed
	}
	finalPayload = payload
	total := 22 + len(finalPayload)

	poolBuf := messagePool.Get(total)
	var buf []byte
	var fromPool bool

	if cap(poolBuf) >= total && total > 256 {
		buf = poolBuf[:total]
		fromPool = true
	} else {
		buf = make([]byte, total)
		if cap(poolBuf) > 0 {
			messagePool.Put(poolBuf)
		}
	}

	buf[0] = byte(msg.Type)
	buf[1] = flags
	binary.BigEndian.PutUint64(buf[2:], msg.ID)
	binary.BigEndian.PutUint64(buf[10:], uint64(msg.Timestamp))
	binary.BigEndian.PutUint32(buf[18:], uint32(len(finalPayload)))
	copy(buf[22:], finalPayload)

	if fromPool {
		result := make([]byte, total)
		copy(result, buf[:total])
		messagePool.Put(poolBuf)
		return result, nil
	}
	return buf[:total], nil
}

// decodeMessage decodes bytes to message (inline codec)
func decodeMessage(data []byte) (*Message, error) {
	if len(data) < 22 {
		return nil, ErrInvalidMessage
	}
	length := int(binary.BigEndian.Uint32(data[18:22]))
	if length < 0 || 22+length > len(data) {
		return nil, ErrInvalidMessage
	}
	flags := data[1]
	isCompressed := flags&msgFlagCompressed != 0

	var payload []byte
	if isCompressed {
		// Use DecompressTo with nil dst to let it handle buffer allocation efficiently
		decompressed, _, err := compress.DecompressTo(data[22:22+length], nil, length*4)
		if err != nil {
			return nil, fmt.Errorf("network decompress failed: %w", err)
		}
		payload = decompressed
	} else {
		// Use zero-copy where safe: create new slice for payload
		payload = make([]byte, length)
		copy(payload, data[22:22+length])
	}

	msg := &Message{
		Type:       MessageType(data[0]),
		ID:         binary.BigEndian.Uint64(data[2:10]),
		Timestamp:  int64(binary.BigEndian.Uint64(data[10:18])),
		Compressed: isCompressed,
		Data:       payload,
	}
	return msg, nil
}

// EncodeMessage encodes message to bytes (public for cluster usage)
func EncodeMessage(msg *Message) ([]byte, error) {
	return encodeMessage(msg)
}

// DecodeMessage decodes bytes to message (public for cluster usage)
func DecodeMessage(data []byte) (*Message, error) {
	return decodeMessage(data)
}

// ClusterMessageTypes defines message types used by cluster
var ClusterMessageTypes = struct {
	Ping           MessageType
	Connect        MessageType
	Leave          MessageType
	GossipPush     MessageType
	GossipPull     MessageType
	GossipResponse MessageType
	ReadRequest    MessageType
	ReadResponse   MessageType
	SyncOperation  MessageType
}{
	Ping:           10, // MessageTypePing
	Connect:        11, // MessageTypeConnect
	Leave:          12, // MessageTypeLeave
	GossipPush:     20, // MessageTypeGossipPush
	GossipPull:     21, // MessageTypeGossipPull
	GossipResponse: 22, // MessageTypeGossipResponse
	ReadRequest:    30, // MessageTypeReadRequest
	ReadResponse:   31, // MessageTypeReadResponse
	SyncOperation:  40, // MessageTypeSyncOperation
}
