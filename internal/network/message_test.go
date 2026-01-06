package network

import (
	"context"
	"testing"
	"time"
)

func TestMessage_EncodeDecode(t *testing.T) {
	msg := &Message{
		Type:       MessageTypeRequest,
		ID:         12345,
		Data:       []byte("test data"),
		Timestamp:  time.Now().UnixNano(),
		Compressed: false,
	}

	encoded, err := EncodeMessage(msg)
	if err != nil {
		t.Fatalf("EncodeMessage() error = %v", err)
	}

	decoded, err := DecodeMessage(encoded)
	if err != nil {
		t.Fatalf("DecodeMessage() error = %v", err)
	}

	if decoded.Type != msg.Type {
		t.Errorf("Decode().Type = %v, want %v", decoded.Type, msg.Type)
	}
	if decoded.ID != msg.ID {
		t.Errorf("Decode().ID = %v, want %v", decoded.ID, msg.ID)
	}
	if decoded.Timestamp != msg.Timestamp {
		t.Errorf("Decode().Timestamp = %v, want %v", decoded.Timestamp, msg.Timestamp)
	}
	if decoded.Compressed != msg.Compressed {
		t.Errorf("Decode().Compressed = %v, want %v", decoded.Compressed, msg.Compressed)
	}
	if string(decoded.Data) != string(msg.Data) {
		t.Errorf("Decode().Data = %v, want %v", string(decoded.Data), string(msg.Data))
	}
}

func TestMessage_Compressed(t *testing.T) {
	msg := &Message{
		Type:       MessageTypeResponse,
		ID:         67890,
		Data:       []byte("compressed data"),
		Timestamp:  time.Now().UnixNano(),
		Compressed: true,
	}

	encoded, err := EncodeMessage(msg)
	if err != nil {
		t.Fatalf("EncodeMessage() error = %v", err)
	}

	decoded, err := DecodeMessage(encoded)
	if err != nil {
		t.Fatalf("DecodeMessage() error = %v", err)
	}

	if !decoded.Compressed {
		t.Error("Decode().Compressed = false, want true")
	}
}

func TestMessage_InvalidData(t *testing.T) {
	// too short
	_, err := DecodeMessage([]byte{1, 2, 3})
	if err != ErrInvalidMessage {
		t.Errorf("DecodeMessage() error = %v, want %v", err, ErrInvalidMessage)
	}

	// invalid length
	invalid := make([]byte, 22)
	invalid[18] = 0xFF
	invalid[19] = 0xFF
	invalid[20] = 0xFF
	invalid[21] = 0xFF
	_, err = DecodeMessage(invalid)
	if err == nil {
		t.Error("DecodeMessage() expected error for invalid length")
	}
}

func TestSimpleRouter_RegisterRoute(t *testing.T) {
	router := NewRouter()

	handler := func(ctx context.Context, remoteAddr string, data []byte) ([]byte, error) {
		return append(data, []byte("_response")...), nil
	}

	msgType := MessageTypeRequest
	if err := router.Register(msgType, handler); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	msg := &Message{
		Type:      msgType,
		ID:        1,
		Data:      []byte("test"),
		Timestamp: time.Now().UnixNano(),
	}

	resp, err := router.Route(context.Background(), "127.0.0.1:8080", msg)
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}

	if resp == nil {
		t.Fatal("Route() returned nil response")
	}
	if resp.Type != MessageTypeResponse {
		t.Errorf("Route().Type = %v, want %v", resp.Type, MessageTypeResponse)
	}
	if resp.ID != msg.ID {
		t.Errorf("Route().ID = %v, want %v", resp.ID, msg.ID)
	}
	if string(resp.Data) != "test_response" {
		t.Errorf("Route().Data = %v, want test_response", string(resp.Data))
	}
}

func TestSimpleRouter_HandlerNotFound(t *testing.T) {
	router := NewRouter()

	msg := &Message{
		Type:      MessageTypeRequest,
		ID:        1,
		Data:      []byte("test"),
		Timestamp: time.Now().UnixNano(),
	}

	_, err := router.Route(context.Background(), "127.0.0.1:8080", msg)
	if err != ErrHandlerNotFound {
		t.Errorf("Route() error = %v, want %v", err, ErrHandlerNotFound)
	}
}

func TestSimpleRouter_Unregister(t *testing.T) {
	router := NewRouter()

	handler := func(ctx context.Context, remoteAddr string, data []byte) ([]byte, error) {
		return data, nil
	}

	msgType := MessageTypeRequest
	_ = router.Register(msgType, handler)

	if err := router.Unregister(msgType); err != nil {
		t.Fatalf("Unregister() error = %v", err)
	}

	msg := &Message{
		Type:      msgType,
		ID:        1,
		Data:      []byte("test"),
		Timestamp: time.Now().UnixNano(),
	}

	_, err := router.Route(context.Background(), "127.0.0.1:8080", msg)
	if err != ErrHandlerNotFound {
		t.Errorf("Route() error = %v, want %v", err, ErrHandlerNotFound)
	}
}

func TestSimpleRouter_OneWay(t *testing.T) {
	router := NewRouter()

	called := false
	handler := func(ctx context.Context, remoteAddr string, data []byte) ([]byte, error) {
		called = true
		return nil, nil
	}

	msgType := MessageTypeOneWay
	_ = router.Register(msgType, handler)

	msg := &Message{
		Type:      msgType,
		ID:        1,
		Data:      []byte("test"),
		Timestamp: time.Now().UnixNano(),
	}

	resp, err := router.Route(context.Background(), "127.0.0.1:8080", msg)
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}

	if resp != nil {
		t.Error("Route() returned non-nil response for one-way message")
	}
	if !called {
		t.Error("Handler was not called")
	}
}
