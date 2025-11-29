package metrics

import (
	"sync/atomic"
)

// Sampler provides probabilistic sampling for high-frequency metrics (Stage 0.3).
// Reduces lock contention and memory accumulation by sampling only a fraction of events.
type Sampler struct {
	// sampleRate: 1/N events are sampled (e.g., 100 means 1% sampling)
	sampleRate int64
	// counter: tracks calls, resets when sampleRate is reached
	counter atomic.Int64
}

// NewSampler creates a sampler with specified sample rate.
// sampleRate: 1/N events are sampled (e.g., 100 = 1% sampling, 10 = 10% sampling).
// Higher values = less overhead but less accuracy.
func NewSampler(sampleRate int64) *Sampler {
	if sampleRate < 1 {
		sampleRate = 1 // No sampling
	}
	return &Sampler{
		sampleRate: sampleRate,
	}
}

// ShouldSample returns true if this event should be sampled.
// Thread-safe, lock-free (atomic operations only).
func (s *Sampler) ShouldSample() bool {
	if s == nil || s.sampleRate <= 1 {
		return true // No sampling
	}
	// Increment counter
	count := s.counter.Add(1)
	// Sample when counter reaches sampleRate, then reset
	if count >= s.sampleRate {
		s.counter.Store(0)
		return true
	}
	return false
}

// BucketedCounter provides lock-free bucketed counting for high-frequency metrics (Stage 0.3).
// Uses per-bucket atomic counters to reduce contention.
type BucketedCounter struct {
	buckets []atomic.Int64
	mask    int // mask for bucket selection (must be 2^N - 1)
}

// NewBucketedCounter creates a bucketed counter with 2^N buckets.
// numBuckets must be power of 2 (e.g., 8, 16, 32, 64).
func NewBucketedCounter(numBuckets int) *BucketedCounter {
	// Round up to next power of 2
	actualBuckets := 1
	for actualBuckets < numBuckets {
		actualBuckets <<= 1
	}
	if actualBuckets > 64 {
		actualBuckets = 64 // Cap at 64 buckets
	}

	buckets := make([]atomic.Int64, actualBuckets)
	return &BucketedCounter{
		buckets: buckets,
		mask:    actualBuckets - 1,
	}
}

// Increment increments a random bucket (based on hash of counter value).
// Thread-safe, lock-free (atomic operations only).
func (bc *BucketedCounter) Increment() {
	if bc == nil {
		return
	}
	// Use counter value as hash to select bucket
	// This distributes writes across buckets
	hash := uintptr(0)
	hash = hash*31 + uintptr(bc.buckets[0].Load())
	bucketIdx := int(hash) & bc.mask
	bc.buckets[bucketIdx].Add(1)
}

// Sum returns the sum of all buckets.
// Thread-safe, but may have slight inaccuracy during concurrent updates.
func (bc *BucketedCounter) Sum() int64 {
	if bc == nil {
		return 0
	}
	var sum int64
	for i := range bc.buckets {
		sum += bc.buckets[i].Load()
	}
	return sum
}

// Reset resets all buckets to zero.
func (bc *BucketedCounter) Reset() {
	if bc == nil {
		return
	}
	for i := range bc.buckets {
		bc.buckets[i].Store(0)
	}
}

