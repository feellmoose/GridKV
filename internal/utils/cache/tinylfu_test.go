package cache

import (
	"testing"
)

func TestTinyLFU(t *testing.T) {
	tf := NewTinyLFU()

	// Test record access
	tf.RecordAccess("key1", true)
	tf.RecordAccess("key1", true)
	tf.RecordAccess("key2", false)

	// Test estimate
	freq1 := tf.Estimate("key1")
	if freq1 == 0 {
		t.Error("key1 should have frequency")
	}

	freq2 := tf.Estimate("key2")
	if freq2 != 0 {
		t.Error("key2 should not have frequency (miss only)")
	}

	// Test hit rate
	hitRate := tf.HitRate()
	if hitRate <= 0 || hitRate > 1 {
		t.Errorf("hit rate = %f, should be between 0 and 1", hitRate)
	}

	// Test reset
	tf.Reset()
	freq1After := tf.Estimate("key1")
	if freq1After >= freq1 {
		t.Error("frequency should decrease after reset")
	}
}

func TestCountMinSketch(t *testing.T) {
	cms := NewCountMinSketch(512, 4)

	// Test increment
	cms.Increment("key1")
	cms.Increment("key1")

	// Test estimate
	freq := cms.Estimate("key1")
	if freq == 0 {
		t.Error("key1 should have frequency")
	}

	// Test reset
	cms.Reset()
	freqAfter := cms.Estimate("key1")
	if freqAfter >= freq {
		t.Error("frequency should decrease after reset")
	}
}
