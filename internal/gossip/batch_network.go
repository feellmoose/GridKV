package gossip

import (
	"context"
	"sync"
	"time"
)

type BatchNetworkWriter struct {
	target     string
	buffer     [][]byte
	bufferMu   sync.Mutex
	bufferSize int
	flushTick  time.Duration
	network    Network
	stopCh     chan struct{}
	wg         sync.WaitGroup // WaitGroup to ensure goroutine exits
	stopped    sync.Once      // Ensure Stop is only called once
}

func NewBatchNetworkWriter(target string, network Network, bufferSize int, flushTick time.Duration) *BatchNetworkWriter {
	bnw := &BatchNetworkWriter{
		target:     target,
		buffer:     make([][]byte, 0, bufferSize),
		bufferSize: bufferSize,
		flushTick:  flushTick,
		network:    network,
		stopCh:     make(chan struct{}),
	}
	bnw.wg.Add(1)
	go bnw.run()
	return bnw
}

func (bnw *BatchNetworkWriter) Write(data []byte) error {
	bnw.bufferMu.Lock()
	bnw.buffer = append(bnw.buffer, data)
	shouldFlush := len(bnw.buffer) >= bnw.bufferSize
	bnw.bufferMu.Unlock()

	if shouldFlush {
		bnw.flush()
	}
	return nil
}

func (bnw *BatchNetworkWriter) flush() {
	bnw.bufferMu.Lock()
	if len(bnw.buffer) == 0 {
		bnw.bufferMu.Unlock()
		return
	}
	batch := make([][]byte, len(bnw.buffer))
	copy(batch, bnw.buffer)
	bnw.buffer = bnw.buffer[:0]
	bnw.bufferMu.Unlock()

	go bnw.sendBatch(batch)
}

func (bnw *BatchNetworkWriter) sendBatch(batch [][]byte) {
	for _, data := range batch {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := bnw.network.SendRaw(ctx, bnw.target, data); err != nil {
			cancel()
			continue
		}
		cancel()
	}
}

func (bnw *BatchNetworkWriter) run() {
	defer bnw.wg.Done()
	ticker := time.NewTicker(bnw.flushTick)
	defer ticker.Stop()

	for {
		select {
		case <-bnw.stopCh:
			bnw.flush()
			return
		case <-ticker.C:
			bnw.flush()
		}
	}
}

func (bnw *BatchNetworkWriter) Stop() {
	bnw.stopped.Do(func() {
		close(bnw.stopCh)
		// Wait for goroutine to exit with timeout
		done := make(chan struct{})
		go func() {
			bnw.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
			// Goroutine exited successfully
		case <-time.After(1 * time.Second):
			// Timeout - continue anyway to prevent blocking
		}
	})
}
