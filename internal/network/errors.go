package network

import "errors"

var (
	// ErrConnClosed indicates connection is closed
	ErrConnClosed = errors.New("connection closed")

	// ErrConnTimeout indicates connection timeout
	ErrConnTimeout = errors.New("connection timeout")

	// ErrPoolExhausted indicates connection pool exhausted
	ErrPoolExhausted = errors.New("connection pool exhausted")

	// ErrPoolClosed indicates connection pool is closed
	ErrPoolClosed = errors.New("connection pool closed")

	// ErrMessageTooLarge indicates message is too large
	ErrMessageTooLarge = errors.New("message too large")

	// ErrBackpressure indicates backpressure is active
	ErrBackpressure = errors.New("backpressure active")

	// ErrTransportNotSupported indicates transport not supported
	ErrTransportNotSupported = errors.New("transport not supported")

	// ErrHandlerNotFound indicates message handler not found
	ErrHandlerNotFound = errors.New("handler not found")

	// ErrInvalidMessage indicates invalid message format
	ErrInvalidMessage = errors.New("invalid message")
)
