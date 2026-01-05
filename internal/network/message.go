package network

import (
	"context"
	"sync"
	"time"
)

// EncodeAny encodes any data to bytes (simple binary format for cluster messages)
func EncodeAny(data interface{}) ([]byte, error) {
	if b, ok := data.([]byte); ok {
		return b, nil
	}
	// For struct messages, use simple binary encoding
	return nil, ErrInvalidMessage
}

// DecodeAny decodes bytes to target type (simple binary format)
func DecodeAny(data []byte, target interface{}) error {
	if b, ok := target.(*[]byte); ok {
		*b = data
		return nil
	}
	return ErrInvalidMessage
}

// MessageType is message type
type MessageType uint8

// Message type constants - unified definition for all layers
const (
	MessageTypeUnknown MessageType = iota

	// Core network message types
	MessageTypeRequest
	MessageTypeResponse
	MessageTypeOneWay
	MessageTypeHeartbeat
	MessageTypeError

	// Cluster-specific message types (start from 10 to avoid conflicts)
	MessageTypePing           = 10
	MessageTypeConnect        = 11
	MessageTypeLeave          = 12
	MessageTypeGossipPush     = 20
	MessageTypeGossipPull     = 21
	MessageTypeGossipResponse = 22
	MessageTypeReadRequest    = 30 // Unified with ClusterMessageTypes.ReadRequest
	MessageTypeReadResponse   = 31 // Unified with ClusterMessageTypes.ReadResponse
	MessageTypeSyncOperation  = 40
)

// Message represents network message
type Message struct {
	// Type is message type
	Type MessageType

	// ID is message ID (for request-response correlation)
	ID uint64

	// Data is message payload
	Data []byte

	// Timestamp is message timestamp
	Timestamp int64

	// Compressed indicates if data is compressed
	Compressed bool
}

// simpleRouter is a minimal in-process router
type simpleRouter struct {
	handlers sync.Map
}

func NewRouter() *simpleRouter {
	return &simpleRouter{}
}

func (r *simpleRouter) Register(msgType MessageType, handler Handler) error {
	r.handlers.Store(msgType, handler)
	return nil
}

func (r *simpleRouter) Unregister(msgType MessageType) error {
	r.handlers.Delete(msgType)
	return nil
}

func (r *simpleRouter) Route(ctx context.Context, remoteAddr string, msg *Message) (*Message, error) {
	// Skip handler lookup for response message types (they don't need handlers)
	// Response messages are sent from server to client, not received by server
	if msg.Type == MessageTypeResponse || msg.Type == ClusterMessageTypes.ReadResponse ||
		msg.Type == ClusterMessageTypes.GossipResponse {
		return nil, ErrHandlerNotFound
	}

	// Fast path: use sync.Map for lock-free read (better for concurrent access)
	handlerVal, ok := r.handlers.Load(msg.Type)
	if !ok {
		return nil, ErrHandlerNotFound
	}
	handler := handlerVal.(Handler)

	// Call handler
	resp, err := handler(ctx, remoteAddr, msg.Data)
	if err != nil {
		return nil, err
	}

	// Fast path: one-way messages don't need response
	if msg.Type == MessageTypeOneWay || msg.Type == MessageTypeHeartbeat {
		return nil, nil
	}

	now := time.Now().UnixNano()

	// Map request types to appropriate response message types
	switch msg.Type {
	case MessageTypeRequest:
		return &Message{
			Type:      MessageTypeResponse,
			ID:        msg.ID,
			Data:      resp,
			Timestamp: now,
		}, nil
	case ClusterMessageTypes.ReadRequest:
		return &Message{
			Type:      ClusterMessageTypes.ReadResponse,
			ID:        msg.ID,
			Data:      resp,
			Timestamp: now,
		}, nil
	default:
		// For other message types, send response only when handler returned data
		if resp != nil {
			return &Message{Type: MessageTypeResponse, ID: msg.ID, Data: resp, Timestamp: now}, nil
		}
		return nil, nil
	}
}
