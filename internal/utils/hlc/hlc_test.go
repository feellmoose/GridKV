package hlc

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHLC_Basic(t *testing.T) {
	clock := NewHLC("node1")

	t1 := clock.Now()
	time.Sleep(1 * time.Millisecond)
	t2 := clock.Now()

	if t2 <= t1 {
		t.Errorf("Expected t2 > t1, got t1=%s, t2=%s", t1, t2)
	}

	if !strings.HasPrefix(t1, "node1:") {
		t.Errorf("Expected timestamp to start with 'node1:', got %s", t1)
	}
}

func TestHLC_Update(t *testing.T) {
	clock1 := NewHLC("node1")
	clock2 := NewHLC("node2")

	t1 := clock1.Now()
	time.Sleep(1 * time.Millisecond)
	t2 := clock2.Now()

	clock1.Update(t2)
	t3 := clock1.Now()

	if t3 < t1 {
		t.Errorf("Expected t3 >= t1 after update, got t1=%s, t3=%s", t1, t3)
	}

	if !strings.HasPrefix(t3, "node1:") {
		t.Errorf("Expected t3 to start with 'node1:', got %s", t3)
	}
}

func TestHLC_ConcurrentAccess(t *testing.T) {
	clock := NewHLC("node1")

	const numGoroutines = 50
	const opsPerGoroutine = 100
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	var lastTime string
	var mu sync.Mutex

	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				ts := clock.Now()
				mu.Lock()
				if ts > lastTime {
					lastTime = ts
				}
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	if lastTime == "" {
		t.Error("Expected non-empty timestamp")
	}
}

func TestHLC_UpdateFromRemote(t *testing.T) {
	clock1 := NewHLC("node1")
	clock2 := NewHLC("node2")

	t1 := clock1.Now()
	time.Sleep(1 * time.Millisecond)
	t2 := clock2.Now()

	clock1.Update(t2)

	t3 := clock1.Now()
	t4 := clock2.Now()

	if t3 < t1 {
		t.Errorf("Expected t3 >= t1 after update, got t3=%s, t1=%s", t3, t1)
	}

	if t4 < t2 {
		t.Errorf("Expected t4 >= t2, got t4=%s, t2=%s", t4, t2)
	}

	if !strings.HasPrefix(t3, "node1:") {
		t.Errorf("Expected t3 to start with 'node1:', got %s", t3)
	}
	if !strings.HasPrefix(t4, "node2:") {
		t.Errorf("Expected t4 to start with 'node2:', got %s", t4)
	}
}

func TestHLC_Monotonicity(t *testing.T) {
	clock := NewHLC("node1")

	times := make([]string, 100)
	for i := 0; i < 100; i++ {
		times[i] = clock.Now()
		time.Sleep(10 * time.Microsecond)
	}

	for i := 1; i < len(times); i++ {
		if times[i] <= times[i-1] {
			t.Errorf("Times not monotonic: times[%d]=%s <= times[%d]=%s", i, times[i], i-1, times[i-1])
		}
	}
}

func TestHLC_UpdateWithLowerTimestamp(t *testing.T) {
	clock := NewHLC("node1")

	t1 := clock.Now()
	time.Sleep(1 * time.Millisecond)
	t2 := clock.Now()

	clock.Update(t1)
	t3 := clock.Now()

	if t3 < t2 {
		t.Errorf("Expected t3 >= t2 after updating with older timestamp, got t3=%s, t2=%s", t3, t2)
	}
}

func TestHLC_Format(t *testing.T) {
	clock := NewHLC("test-node")

	ts := clock.Now()
	parts := strings.Split(ts, ":")

	if len(parts) != 3 {
		t.Fatalf("Expected format 'nodeID:timestamp:counter', got %s", ts)
	}

	if parts[0] != "test-node" {
		t.Errorf("Expected nodeID 'test-node', got %s", parts[0])
	}
}

func TestHLC_EmptyRemote(t *testing.T) {
	clock := NewHLC("node1")

	t1 := clock.Now()
	clock.Update("")
	t2 := clock.Now()

	if t2 < t1 {
		t.Errorf("Expected t2 >= t1 after empty update, got t2=%s, t1=%s", t2, t1)
	}
}

func TestHLC_InvalidRemote(t *testing.T) {
	clock := NewHLC("node1")

	t1 := clock.Now()
	clock.Update("invalid-format")
	t2 := clock.Now()

	if t2 < t1 {
		t.Errorf("Expected t2 >= t1 after invalid update, got t2=%s, t1=%s", t2, t1)
	}
}
