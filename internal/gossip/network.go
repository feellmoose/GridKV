package gossip

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feellmoose/gridkv/internal/transport"
	"github.com/feellmoose/gridkv/internal/utils/logging"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/protobuf/proto"
)

var serializationBufferPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 0, 8192) // 8KB initial capacity for messages
		return &buf
	},
}

var protoMarshalBufferPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 0, 8192) // Increased from 4096 to 8192 for larger batches
		return &buf
	},
}

// protoUnmarshalBufferPool for unmarshal operations (if needed)
var protoUnmarshalBufferPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 0, 8192)
		return &buf
	},
}

var (
	compressEncoderPool  sync.Pool
	compressDecoderPool  sync.Pool
	compressionEnabled   = true // Global flag for compression
	compressionThreshold = 1024 // Compress messages > 1KB
)

func init() {
	// Initialize compression pools
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
	TCP     NetworkBackendType = 1
	GnetTCP NetworkBackendType = 2
	// UDP NetworkBackendType = 4  // Not implemented (reserved for future)
)

const maxPendingAcks = 65536 // Increased from 16384 to 65536 for very high concurrency

// NetworkOptions configures the Gossip protocol and network settings.
type NetworkOptions struct {
	Type           NetworkBackendType
	BindAddr       string // Local address to bind for listening (e.g., "0.0.0.0:8080" or ":8080")
	EncryptEnabled bool   // Enable encryption for gossip traffic
	// Connection pool options (used by TransportProtocol)
	MaxIdle  int
	MaxConns int
	Timeout  time.Duration
	// Per-operation timeouts (used by Network)
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

// TransportProtocol represents the low-level connection and pooling layer.
type TransportProtocol struct {
	opts      *NetworkOptions
	transport transport.Transport
	listener  transport.TransportListener
	pools     sync.Map // map[string]*transport.ConnPool
	stopOnce  sync.Once
}

// NewTransportProtocol creates a new low-level protocol handler with connection pooling support.
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
	case GnetTCP:
		tr, err = transport.NewGnetTransport()
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("invalid network type %v (supported: TCP, GnetTCP)", opts.Type)
	}

	// Assuming Transport.Listen returns a TransportListener implementation
	listener, err := tr.Listen(opts.BindAddr)
	if err != nil {
		return nil, err
	}

	return &TransportProtocol{
		opts:      opts,
		transport: tr,
		listener:  listener,
	}, nil
}

// getPool returns or creates a connection pool for the given address.
func (p *TransportProtocol) getPool(address string) *transport.ConnPool {
	v, _ := p.pools.LoadOrStore(address,
		transport.NewConnPool(p.transport, address, p.opts.MaxIdle, p.opts.MaxConns, p.opts.Timeout))
	return v.(*transport.ConnPool)
}

// Send sends raw data to the specified address using a pooled connection.
func (p *TransportProtocol) Send(ctx context.Context, address string, data []byte) error {
	pool := p.getPool(address)
	conn, err := pool.Get(ctx)
	if err != nil {
		return err
	}
	// Defer put/invalidate logic
	defer func() {
		if conn != nil {
			pool.Put(conn)
		}
	}()

	if err := conn.WriteDataWithContext(ctx, data); err != nil {
		pool.Invalidate(conn)
		// Set conn to nil so defer does not call Put
		conn = nil
		return fmt.Errorf("transport send failed: %w", err)
	}
	return nil
}

// Listen registers a message handler and starts the listener.
func (p *TransportProtocol) Listen(handler func(message []byte) error) error {
	return p.listener.HandleMessage(handler).Start()
}

// Stop stops the listener and closes all pools.
func (p *TransportProtocol) Stop() {
	p.stopOnce.Do(func() {
		p.listener.Stop()
		p.pools.Range(func(_, v any) bool {
			v.(*transport.ConnPool).Close()
			return true
		})
	})
}

type Network interface {
	SendWithTimeout(addr string, msg *GossipMessage, timeout time.Duration) error
	SendAndWaitAck(addr string, msg *GossipMessage, timeout time.Duration) (bool, error)
	Send(address string, msg *GossipMessage) error
	Listen(receiver func(msg *GossipMessage) error) error
	Stop() error
	HandleAck(ack *CacheSyncAckPayload) // Process incoming ACK messages
}

// NetworkImpl is the production implementation of the Network interface.
// It provides message-aware communication with support for acknowledgments (ACKs).
//
// ACK Flow:
//  1. Sender calls SendAndWaitAck(msg) with a unique OpId
//  2. NetworkImpl creates a response channel and registers it in pendingAcks[OpId]
//  3. Message is sent over the transport layer
//  4. Sender goroutine blocks waiting for ACK or timeout
//  5. Receiver processes message and sends CACHE_SYNC_ACK back
//  6. GossipManager routes ACK to network.HandleAck()
//  7. HandleAck looks up pendingAcks[OpId] and sends ACK to waiting channel
//  8. Sender goroutine wakes up with success/failure result
//  9. Cleanup: channel closed and removed from pendingAcks
//
// This mechanism enables quorum-based replication with guaranteed delivery confirmation.
type NetworkImpl struct {
	opts            *NetworkOptions
	protocol        *TransportProtocol
	pendingAcks     sync.Map // map[string]chan *CacheSyncAckPayload for tracking ACK responses
	pendingAckCount atomic.Int64
}

// NewNetwork creates a new Network using the provided options, initializing the underlying transport.
func NewNetwork(opts *NetworkOptions) (Network, error) {
	protocol, err := NewTransportProtocol(opts)
	if err != nil {
		return nil, err
	}
	return &NetworkImpl{
		opts:     opts,
		protocol: protocol,
	}, nil
}

// Send marshals a GossipMessage and sends it to the specified address with a write timeout.
func (n *NetworkImpl) Send(address string, msg *GossipMessage) error {
	// Create context with the configured write timeout
	ctx, cancel := context.WithTimeout(context.Background(), n.opts.WriteTimeout)
	defer cancel()

	// This reduces allocations by reusing buffers
	bufPtr := protoMarshalBufferPool.Get().(*[]byte)
	defer func() {
		*bufPtr = (*bufPtr)[:0] // Reset length for reuse
		protoMarshalBufferPool.Put(bufPtr)
	}()

	// MarshalAppend appends to existing buffer, reducing allocations
	data, err := proto.MarshalOptions{}.MarshalAppend(*bufPtr, msg)
	if err != nil {
		logging.Error(err, "Serialization failed for message", "message_type", msg.Type)
		return err
	}

	// Delegate sending to the transport protocol
	if err := n.protocol.Send(ctx, address, data); err != nil {
		logging.Error(err, "Error sending message", "address", address, "message_type", msg.Type)
		return err
	}
	return nil
}

// Listen registers a receiver function to handle incoming GossipMessages and starts the listener.
func (n *NetworkImpl) Listen(receiver func(msg *GossipMessage) error) error {
	// Pass a handler to the TransportProtocol listener to manage unmarshalling
	return n.protocol.Listen(func(data []byte) error {
		// NOTE: Cannot use object pool here because receiver may use message asynchronously
		// Creating fresh message ensures safety
		msg := &GossipMessage{}

		// Unmarshal the incoming data into msg
		if err := proto.Unmarshal(data, msg); err != nil {
			return fmt.Errorf("deserialization failed for message of length %d: %w", len(data), err)
		}

		// Pass the decoded message to the receiver
		if err := receiver(msg); err != nil {
			return err
		}

		return nil
	})
}

// Stop stops the underlying transport and closes all connection pools.
func (n *NetworkImpl) Stop() error {
	n.protocol.Stop()

	// CRITICAL: Clean up pending ACKs to prevent memory leaks
	// Close all pending channels and remove from map
	n.pendingAcks.Range(func(key, value interface{}) bool {
		if ch, ok := value.(chan *CacheSyncAckPayload); ok {
			// SAFETY: Use recover to handle potential panic when closing channel
			func() {
				defer func() {
					if r := recover(); r != nil {
						// Channel already closed, ignore
					}
				}()
				close(ch)
			}()
		}
		n.pendingAcks.Delete(key)
		return true
	})

	return nil
}

// SendWithTimeout marshals and sends a GossipMessage with a custom timeout.
func (n *NetworkImpl) SendWithTimeout(addr string, msg *GossipMessage, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	bufPtr := protoMarshalBufferPool.Get().(*[]byte)
	defer func() {
		*bufPtr = (*bufPtr)[:0] // Reset length
		protoMarshalBufferPool.Put(bufPtr)
	}()

	data, err := proto.MarshalOptions{}.MarshalAppend(*bufPtr, msg)
	if err != nil {
		logging.Error(err, "Serialization failed for message", "message_type", msg.Type)
		return err
	}

	if err := n.protocol.Send(ctx, addr, data); err != nil {
		logging.Error(err, "Error sending message with timeout", "address", addr, "message_type", msg.Type)
		return err
	}
	return nil
}

// SendAndWaitAck sends a message and waits for an acknowledgment with proper correlation ID tracking.
// This is used for quorum-based replication where we need to confirm receipt.
func (n *NetworkImpl) SendAndWaitAck(addr string, msg *GossipMessage, timeout time.Duration) (bool, error) {
	// Validate that the message has an OpId for correlation
	if msg.OpId == "" {
		return false, fmt.Errorf("OpId required for ACK correlation")
	}

	if n.pendingAckCount.Add(1) > maxPendingAcks {
		n.pendingAckCount.Add(-1)
		return false, fmt.Errorf("pending ACK limit exceeded")
	}

	ackCh := getAckChannel()

	// Register the pending ACK with the OpId
	n.pendingAcks.Store(msg.OpId, ackCh)

	stored := true
	closed := atomic.Bool{}
	// Ensure cleanup: remove the pending ACK registration when done
	defer func() {
		if stored {
			n.pendingAcks.Delete(msg.OpId)
			n.pendingAckCount.Add(-1)
		}
		// SAFETY: Only close channel once to prevent "close of closed channel" panic
		if !closed.Swap(true) {
			// Clear any remaining value first
			select {
			case <-ackCh:
			default:
			}
			putAckChannel(ackCh)
		}
	}()

	// Marshal and send the message
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	bufPtr := protoMarshalBufferPool.Get().(*[]byte)
	defer func() {
		*bufPtr = (*bufPtr)[:0] // Reset length
		protoMarshalBufferPool.Put(bufPtr)
	}()

	data, err := proto.MarshalOptions{}.MarshalAppend(*bufPtr, msg)
	if err != nil {
		logging.Error(err, "Serialization failed for SendAndWaitAck", "message_type", msg.Type)
		stored = false
		n.pendingAcks.Delete(msg.OpId)
		n.pendingAckCount.Add(-1)
		return false, err
	}

	if err := n.protocol.Send(ctx, addr, data); err != nil {
		logging.Error(err, "Error sending message for ACK", "address", addr, "message_type", msg.Type, "opId", msg.OpId)
		stored = false
		n.pendingAcks.Delete(msg.OpId)
		n.pendingAckCount.Add(-1)
		return false, err
	}

	sendTime := time.Now()

	// Wait for ACK response or timeout
	select {
	case ack := <-ackCh:
		if ack == nil {
			return false, fmt.Errorf("received nil ACK for opId %s", msg.OpId)
		}
		ackLatency := time.Since(sendTime)
		if ackLatency > timeout/2 {
			if logging.Log.IsDebugEnabled() {
				logging.Debug("ACK received with high latency", "opId", msg.OpId, "peerId", ack.PeerId, "latency", ackLatency)
			}
		}
		if ack.Success {
			if logging.Log.IsDebugEnabled() {
				logging.Debug("ACK received successfully", "opId", msg.OpId, "peerId", ack.PeerId)
			}
		} else {
			logging.Warn("ACK received with failure", "opId", msg.OpId, "peerId", ack.PeerId)
		}
		return ack.Success, nil
	case <-ctx.Done():
		if ctx.Err() == context.DeadlineExceeded {
			if logging.Log.IsDebugEnabled() {
				logging.Debug("ACK timeout", "opId", msg.OpId, "address", addr, "timeout", timeout)
			}
		}
		return false, fmt.Errorf("ACK timeout for opId %s after %v", msg.OpId, timeout)
	}
}

// HandleAck processes an incoming ACK message and routes it to the waiting goroutine.
func (n *NetworkImpl) HandleAck(ack *CacheSyncAckPayload) {
	if ack == nil || ack.OpId == "" {
		return // Skip invalid ACKs silently
	}

	if ch, ok := n.pendingAcks.Load(ack.OpId); ok {
		ackCh := ch.(chan *CacheSyncAckPayload)

		// Use select with default to avoid blocking if channel is full
		select {
		case ackCh <- ack:
			// ACK delivered successfully
			if logging.Log.IsDebugEnabled() && n.pendingAckCount.Load() <= 10 {
				logging.Debug("ACK delivered", "opId", ack.OpId, "success", ack.Success)
			}
		default:
			// Channel full or closed - this is rare but can happen
			func() {
				defer func() {
					if r := recover(); r != nil {
						// Channel was closed (timeout), remove from map
						n.pendingAcks.Delete(ack.OpId)
						if logging.Log.IsDebugEnabled() {
							logging.Debug("ACK channel closed (timeout)", "opId", ack.OpId)
						}
					}
				}()
				// Try blocking send as fallback (shouldn't block long)
				select {
				case ackCh <- ack:
					// Success
				case <-time.After(10 * time.Millisecond):
					// Timeout - channel likely closed
					n.pendingAcks.Delete(ack.OpId)
				}
			}()
		}
	}
}
