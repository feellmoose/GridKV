package network

import (
	"context"
	"fmt"
	"strings"
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

	// DefaultTimeout is default operation timeout (increased for high-load scenarios)
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

// DefaultClientConfig returns default client config for cluster communication
func DefaultClientConfig(pool ConnPool) ClientConfig {
	return ClientConfig{
		Pool:              pool,
		DefaultTimeout:    10 * time.Second,
		RetryCount:        5,
		RetryBackoff:      200 * time.Millisecond,
		EnableCompression: false,
		MaxRetries:        5,
	}
}

type networkClient struct {
	cfg ClientConfig
}

func (c *networkClient) GetPool() ConnPool {
	return c.cfg.Pool
}

func NewClient(cfg ClientConfig) Client {
	return &networkClient{cfg: cfg}
}

func (c *networkClient) Send(ctx context.Context, address string, data []byte) error {
	return c.SendWithTimeout(ctx, address, data, c.cfg.DefaultTimeout)
}

func (c *networkClient) SendWithTimeout(ctx context.Context, address string, data []byte, timeout time.Duration) error {
	// Only create new context if current context doesn't have timeout or deadline is longer
	var cancel context.CancelFunc
	if ctx == nil || ctx == context.Background() {
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
		defer cancel()
	} else if ctxDeadline, hasDeadline := ctx.Deadline(); !hasDeadline || time.Until(ctxDeadline) > timeout {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	for attempt := 0; attempt < 2; attempt++ {
		conn, err := c.cfg.Pool.Get(ctx, address)
		if err != nil {
			if attempt == 0 && !isConnectionRefused(err) {
				continue
			}
			return err
		}

		if err := conn.Send(ctx, data); err == nil {
			c.cfg.Pool.Put(conn)
			return nil
		}

		c.cfg.Pool.Remove(conn)
		if attempt == 0 && !isConnectionRefused(err) {
			continue
		}
		return err
	}
	return fmt.Errorf("unexpected: send loop completed without result")
}

func (c *networkClient) Request(ctx context.Context, address string, request []byte, timeout time.Duration) ([]byte, error) {
	// Only create new context if current context doesn't have timeout or deadline is longer
	var cancel context.CancelFunc
	if ctx == nil || ctx == context.Background() {
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
		defer cancel()
	} else if ctxDeadline, hasDeadline := ctx.Deadline(); !hasDeadline || time.Until(ctxDeadline) > timeout {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	for attempt := 0; attempt < 2; attempt++ {
		conn, err := c.cfg.Pool.Get(ctx, address)
		if err != nil {
			if attempt == 0 {
				logging.Warn("Request: pool.Get failed - possible network connectivity issue",
					"address", address, "attempt", attempt+1, "error", err)
			} else {
				logging.Warn("Request: pool.Get failed on retry",
					"address", address, "attempt", attempt+1, "error", err)
			}
			return nil, fmt.Errorf("connection pool error for %s: %w", address, err)
		}

		if err := conn.Send(ctx, request); err != nil {
			c.cfg.Pool.Remove(conn)
			if attempt == 0 {
				continue
			}
			return nil, err
		}

		resp, rerr := conn.Receive(ctx)
		c.cfg.Pool.Put(conn)
		if rerr != nil {
			// On first attempt, retry once; on second attempt, return error
			if attempt == 0 {
				continue
			}
			return resp, rerr
		}
		return resp, nil
	}
	// Unreachable: loop always returns or continues
	return nil, fmt.Errorf("unexpected: request loop completed without result")
}

func (c *networkClient) Broadcast(ctx context.Context, addresses []string, data []byte) error {
	if len(addresses) == 0 {
		return nil
	}

	if len(addresses) <= 5 {
		// Serial execution for small sets (lower overhead)
		var firstErr error
		for _, addr := range addresses {
			if err := c.SendWithTimeout(ctx, addr, data, c.cfg.DefaultTimeout); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				// Continue to attempt all addresses even if some fail
			}
		}
		return firstErr
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
				case errCh <- fmt.Errorf("%s: %w", a, err):
				default:
					// Channel full, log but don't block
					logging.Debug("Broadcast error channel full, dropping error", "address", a, "error", err)
				}
			}
		}(addr)
	}

	wg.Wait()
	close(errCh)

	// Aggregate all errors
	if len(errCh) > 0 {
		var errs []error
		for err := range errCh {
			errs = append(errs, err)
		}
		if len(errs) == 1 {
			return errs[0]
		}
		return fmt.Errorf("broadcast failed for %d/%d addresses: %v", len(errs), len(addresses), errs[0])
	}
	return nil
}

func (c *networkClient) Close() error {
	if c.cfg.Pool != nil {
		return c.cfg.Pool.Close()
	}
	return nil
}

func isConnectionRefused(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "connect: connection refused")
}
