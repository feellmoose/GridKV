package transport

import (
	"fmt"
	"net"
	"sync"

	"github.com/feellmoose/gridkv/internal/utils/logging"
)

type datagramEngine struct {
	config      *UDPConfig
	metrics     *UDPMetrics
	bufferPool  sync.Pool
	messagePool sync.Pool
}

func newDatagramEngine(config *UDPConfig) *datagramEngine {
	if config == nil {
		config = DefaultUDPConfig()
	}
	engine := &datagramEngine{
		config:  config,
		metrics: NewUDPMetrics(),
	}
	engine.bufferPool.New = func() interface{} {
		return make([]byte, config.ReadBufferSize)
	}
	engine.messagePool.New = func() interface{} {
		return &udpMessage{
			data: make([]byte, 0, 4096),
		}
	}
	return engine
}

func (e *datagramEngine) Dial(address string) (TransportConn, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, fmt.Errorf("resolve UDP address %s: %w", address, err)
	}

	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		return nil, fmt.Errorf("dial UDP %s: %w", address, err)
	}
	if err := optimizeUDPSocket(conn); err != nil {
		logging.Debug("UDP socket optimization warning (non-fatal)", "err", err)
	}

	return NewUDPConn(conn, udpAddr, e.config, e.metrics, &e.bufferPool, &e.messagePool), nil
}

func (e *datagramEngine) Metrics() *UDPMetrics {
	if e == nil {
		return nil
	}
	return e.metrics
}
