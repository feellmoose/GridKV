package transport

import (
	"fmt"
	"sync"
)

// TransportFactory is a function that creates a new Transport instance
type TransportFactory func() (Transport, error)

// transportRegistry stores registered transport factories
var transportRegistry = sync.Map{} // map[string]TransportFactory

// RegisterTransport registers a TransportFactory for a given transport type.
// This function is typically called in init() functions of specific transport implementations.
func RegisterTransport(transportType string, factory TransportFactory) {
	transportRegistry.Store(transportType, factory)
}

// NewTransport creates a new Transport instance based on the transport type.
// It looks up the appropriate factory in the registry.
func NewTransport(transportType string) (Transport, error) {
	factory, ok := transportRegistry.Load(transportType)
	if !ok {
		return nil, fmt.Errorf("unsupported or uncompiled transport: %s", transportType)
	}

	return factory.(TransportFactory)()
}

// ListAvailableTransports returns a list of all registered transport types
func ListAvailableTransports() []string {
	var transports []string
	transportRegistry.Range(func(key, value interface{}) bool {
		transports = append(transports, key.(string))
		return true
	})
	return transports
}

func init() {
	// Auto-register TCP transport (default, always available)
	RegisterTransport("tcp", func() (Transport, error) {
		return NewTCPTransport(), nil
	})

	// Auto-register QUIC transport
	RegisterTransport("quic", func() (Transport, error) {
		config := DefaultQUICConfig()
		return NewQUICTransport(config)
	})

	// Auto-register UDP transport
	RegisterTransport("udp", func() (Transport, error) {
		config := DefaultUDPConfig()
		return NewUDPTransport(config)
	})
}
