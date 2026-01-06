package network

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/feellmoose/gridkv/internal/utils/logging"
)

// Client provides high-level network client interface
type Client interface {
	// Send sends message to address
	Send(ctx context.Context, address string, data []byte) error

	// SendWithTimeout sends message with timeout
	SendWithTimeout(ctx context.Context, address string, data []byte, timeout time.Duration) error

	// Request sends request and waits for response
	Request(ctx context.Context, address string, request []byte, timeout time.Duration) ([]byte, error)

	// Broadcast sends message to multiple addresses
	Broadcast(ctx context.Context, addresses []string, data []byte) error

	// Close closes client
	Close() error
}

// ClientConfig configures client
type ClientConfig struct {
	// Pool is connection pool
	Pool ConnPool

	// DefaultTimeout is default operation timeout
	DefaultTimeout time.Duration

	// RetryCount is retry count on failure
	RetryCount int

	// RetryBackoff is retry backoff duration
	RetryBackoff time.Duration

	// EnableCompression enables message compression
	EnableCompression bool

	// MaxRetries is maximum retry attempts
	MaxRetries int
}

// DefaultClientConfig returns default client config
func DefaultClientConfig(pool ConnPool) ClientConfig {
	return ClientConfig{
		Pool:              pool,
		DefaultTimeout:    5 * time.Second,
		RetryCount:        3,
		RetryBackoff:      100 * time.Millisecond,
		EnableCompression: false,
		MaxRetries:        3,
	}
}

type networkClient struct {
	cfg ClientConfig
}

func NewClient(cfg ClientConfig) Client {
	return &networkClient{cfg: cfg}
}

func (c *networkClient) Send(ctx context.Context, address string, data []byte) error {
	return c.SendWithTimeout(ctx, address, data, c.cfg.DefaultTimeout)
}

func (c *networkClient) SendWithTimeout(ctx context.Context, address string, data []byte, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Fast path: try once with retry on error
	for attempt := 0; attempt < 2; attempt++ {
		conn, err := c.cfg.Pool.Get(ctx, address)
		if err != nil {
			if attempt == 0 {
				logging.Debug("pool.Get failed", "address", address, "error", err)
			}
			return err
		}

		if err := conn.Send(ctx, data); err == nil {
			c.cfg.Pool.Put(conn)
			return nil
		}

		// On send error, remove the connection and retry once
		c.cfg.Pool.Remove(conn)
		if attempt == 0 {
			logging.Debug("conn.Send failed, retrying", "address", address, "error", err, "dataLen", len(data))
			continue
		}
		return err
	}
	return nil // Should not reach here
}

func (c *networkClient) Request(ctx context.Context, address string, request []byte, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Fast path: try once with retry on error and better diagnostics
	for attempt := 0; attempt < 2; attempt++ {
		conn, err := c.cfg.Pool.Get(ctx, address)
		if err != nil {
			if attempt == 0 {
				logging.Warn("Request: pool.Get failed - possible network connectivity issue",
					"address", address, "error", err, "attempt", attempt+1)
			} else {
				logging.Warn("Request: pool.Get failed on retry",
					"address", address, "error", err, "attempt", attempt+1)
			}
			return nil, fmt.Errorf("connection pool error for %s: %w", address, err)
		}

		if err := conn.Send(ctx, request); err != nil {
			c.cfg.Pool.Remove(conn)
			if attempt == 0 {
				logging.Debug("Request: Send failed, retrying", "address", address, "error", err, "requestLen", len(request))
				continue
			}
			return nil, err
		}

		resp, rerr := conn.Receive(ctx)
		c.cfg.Pool.Put(conn)
		if rerr != nil {
			if attempt == 0 {
				logging.Debug("Request: receive failed", "address", address, "error", rerr)
			}
			return resp, rerr
		}
		return resp, nil
	}
	return nil, nil // Should not reach here
}

func (c *networkClient) Broadcast(ctx context.Context, addresses []string, data []byte) error {
	if len(addresses) == 0 {
		return nil
	}

	if len(addresses) <= 5 {
		// Serial execution for small sets (lower overhead)
		for _, addr := range addresses {
			if err := c.SendWithTimeout(ctx, addr, data, c.cfg.DefaultTimeout); err != nil {
				return err
			}
		}
		return nil
	}

	// Concurrent execution for large sets
	const maxConcurrency = 10
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	errCh := make(chan error, len(addresses))

	for _, addr := range addresses {
		wg.Add(1)
		go func(a string) {
			defer wg.Done()
			sem <- struct{}{}        // Acquire semaphore
			defer func() { <-sem }() // Release semaphore

			if err := c.SendWithTimeout(ctx, a, data, c.cfg.DefaultTimeout); err != nil {
				select {
				case errCh <- err:
				default:
				}
			}
		}(addr)
	}

	wg.Wait()
	close(errCh)

	// Return first error if any
	if len(errCh) > 0 {
		return <-errCh
	}
	return nil
}

func (c *networkClient) Close() error {
	if c.cfg.Pool != nil {
		return c.cfg.Pool.Close()
	}
	return nil
}
