package transport

import (
	"fmt"

	"github.com/feellmoose/gridkv/internal/utils/logging"
	"github.com/feellmoose/gridkv/internal/utils/workerpool"
)

// createListenerWorkerPool creates a worker pool for transport listeners
// This provides a unified approach for TCP, QUIC, and UDP to handle connections/streams
func createListenerWorkerPool(name string, maxWorkers, queueSize int) (*workerpool.Pool, error) {
	if maxWorkers <= 0 {
		maxWorkers = 256 // Default worker count
	}
	if queueSize <= 0 {
		queueSize = maxWorkers * 2 // Default queue size is 2x workers
	}

	pool, err := workerpool.New(workerpool.Options{
		Name:        name,
		MaxWorkers:  maxWorkers,
		QueueSize:   queueSize,
		NonBlocking: false, // Block when queue is full to apply backpressure
		PanicHandler: func(err interface{}) {
			logging.Error(fmt.Errorf("panic in %s worker: %v", name, err), fmt.Sprintf("%s pool panic", name))
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create %s worker pool: %w", name, err)
	}

	return pool, nil
}

