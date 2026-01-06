package hlc

import (
	"strings"
	"sync"
	"sync/atomic"
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

func TestHLC_CounterIncrement(t *testing.T) {
	clock := NewHLC("node1")

	// Force same timestamp by calling Now() rapidly
	times := make([]string, 10)
	for i := 0; i < 10; i++ {
		times[i] = clock.Now()
	}

	// Extract counters
	counters := make([]string, 10)
	for i, ts := range times {
		parts := strings.Split(ts, ":")
		if len(parts) == 3 {
			counters[i] = parts[2]
		}
	}

	// Counters should be increasing (or at least non-decreasing)
	for i := 1; i < len(counters); i++ {
		if counters[i] < counters[i-1] {
			t.Errorf("Counter decreased: counters[%d]=%s < counters[%d]=%s", i, counters[i], i-1, counters[i-1])
		}
	}
}

func TestHLC_UpdateWithSameTimestampHigherCounter(t *testing.T) {
	clock1 := NewHLC("node1")

	// Get timestamp from clock1
	t1 := clock1.Now()
	time.Sleep(1 * time.Millisecond)

	// Parse t1 to get timestamp
	parts1 := strings.Split(t1, ":")
	if len(parts1) != 3 {
		t.Fatalf("Invalid format: %s", t1)
	}

	// Create a remote timestamp with same physical time but higher counter
	remote := parts1[0] + ":" + parts1[1] + ":999"

	clock1.Update(remote)
	t2 := clock1.Now()

	// t2 should reflect the updated counter
	if t2 <= t1 {
		t.Errorf("Expected t2 > t1 after counter update, got t1=%s, t2=%s", t1, t2)
	}
}

func TestHLC_ConcurrentUpdate(t *testing.T) {
	clock1 := NewHLC("node1")
	clock2 := NewHLC("node2")

	const numGoroutines = 20
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				t2 := clock2.Now()
				clock1.Update(t2)
				clock1.Now()
			}
		}()
	}

	wg.Wait()
}

func TestHLC_NodeIDPreservation(t *testing.T) {
	testCases := []string{
		"node1",
		"node-123",
		"127.0.0.1:8080",
		"very-long-node-identifier-name",
	}

	for _, nodeID := range testCases {
		clock := NewHLC(nodeID)
		ts := clock.Now()
		if !strings.HasPrefix(ts, nodeID+":") {
			t.Errorf("Expected timestamp to start with '%s:', got %s", nodeID, ts)
		}
	}
}

func TestHLC_CacheEffectiveness(t *testing.T) {
	clock := NewHLC("node1")

	// First call - should build cache
	_ = clock.Now()

	// Rapid calls - should use cache
	times := make([]string, 100)
	start := time.Now()
	for i := 0; i < 100; i++ {
		times[i] = clock.Now()
	}
	duration := time.Since(start)

	// All should be same (same timestamp, counter increments)
	// But format should be consistent
	if duration > 10*time.Millisecond {
		t.Logf("100 calls took %v", duration)
	}

	// Verify all timestamps are valid
	for i, ts := range times {
		parts := strings.Split(ts, ":")
		if len(parts) != 3 {
			t.Errorf("Invalid timestamp at index %d: %s", i, ts)
		}
		if parts[0] != "node1" {
			t.Errorf("Invalid nodeID at index %d: %s", i, parts[0])
		}
	}
}

func TestHLC_AppendInt(t *testing.T) {
	testCases := []struct {
		input    int64
		expected string
	}{
		{0, "0"},
		{1, "1"},
		{123, "123"},
		{-1, "-1"},
		{-123, "-123"},
		{9223372036854775807, "9223372036854775807"},   // Max int64
		{-9223372036854775808, "-9223372036854775808"}, // Min int64
	}

	for _, tc := range testCases {
		buf := AppendInt(nil, tc.input)
		result := string(buf)
		if result != tc.expected {
			t.Errorf("AppendInt(%d) = %s, expected %s", tc.input, result, tc.expected)
		}
	}
}

func TestHLC_AppendUint(t *testing.T) {
	testCases := []struct {
		input    uint64
		expected string
	}{
		{0, "0"},
		{1, "1"},
		{123, "123"},
		{18446744073709551615, "18446744073709551615"}, // Max uint64
	}

	for _, tc := range testCases {
		buf := AppendUint(nil, tc.input)
		result := string(buf)
		if result != tc.expected {
			t.Errorf("AppendUint(%d) = %s, expected %s", tc.input, result, tc.expected)
		}
	}
}

func TestHLC_StressTest(t *testing.T) {
	clock := NewHLC("node1")
	const iterations = 10000

	var wg sync.WaitGroup
	const numGoroutines = 10
	wg.Add(numGoroutines)

	var errorCount atomic.Int64

	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			lastTS := ""
			for j := 0; j < iterations; j++ {
				ts := clock.Now()
				if ts <= lastTS && lastTS != "" {
					errorCount.Add(1)
				}
				lastTS = ts
			}
		}()
	}

	wg.Wait()

	if errorCount.Load() > 0 {
		t.Errorf("Found %d non-monotonic timestamps in stress test", errorCount.Load())
	}
}

func BenchmarkHLC_Now(b *testing.B) {
	clock := NewHLC("node1")
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			clock.Now()
		}
	})
}

func BenchmarkHLC_Update(b *testing.B) {
	clock1 := NewHLC("node1")
	clock2 := NewHLC("node2")
	t2 := clock2.Now()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			clock1.Update(t2)
		}
	})
}

func BenchmarkHLC_AppendInt(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf := AppendInt(nil, int64(i))
		_ = string(buf)
	}
}
