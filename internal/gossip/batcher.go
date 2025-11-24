package gossip

import (
	"sync"
	"time"
)

type batchCleanupManager struct {
	gm              *GossipManager
	cleanupInterval time.Duration
	stopCh          chan struct{}
	stopOnce        sync.Once
	wg              sync.WaitGroup
}

func newBatchCleanupManager(gm *GossipManager) *batchCleanupManager {
	return &batchCleanupManager{
		gm:              gm,
		cleanupInterval: 30 * time.Second,
		stopCh:          make(chan struct{}),
	}
}

func (bcm *batchCleanupManager) start() {
	bcm.wg.Add(1)
	go bcm.run()
}

func (bcm *batchCleanupManager) stop() {
	bcm.stopOnce.Do(func() {
		close(bcm.stopCh)
	})

	done := make(chan struct{})
	go func() {
		bcm.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
	}
}

func (bcm *batchCleanupManager) run() {
	defer bcm.wg.Done()

	ticker := time.NewTicker(bcm.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-bcm.stopCh:
			return
		case <-ticker.C:
			bcm.cleanup()
		}
	}
}

func (bcm *batchCleanupManager) cleanup() {
}
