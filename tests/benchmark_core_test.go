package tests

import (
	"testing"

	"github.com/feellmoose/gridkv/internal/gossip"
)

// BenchmarkConsistentHash_Get benchmarks the consistent hash Get operation
func BenchmarkConsistentHash_Get(b *testing.B) {
	hash := gossip.NewConsistentHash(150, nil)
	hash.Add("node1")
	hash.Add("node2")
	hash.Add("node3")

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = hash.Get("test-key")
	}
}

// BenchmarkConsistentHash_GetN benchmarks the GetN operation
func BenchmarkConsistentHash_GetN(b *testing.B) {
	hash := gossip.NewConsistentHash(150, nil)
	for i := 0; i < 10; i++ {
		hash.Add("node" + string(rune('0'+i)))
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = hash.GetN("test-key", 3)
	}
}

// BenchmarkConsistentHash_Add benchmarks adding nodes
func BenchmarkConsistentHash_Add(b *testing.B) {
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		hash := gossip.NewConsistentHash(150, nil)
		hash.Add("node1")
	}
}

// BenchmarkConsistentHash_Members benchmarks getting all members
func BenchmarkConsistentHash_Members(b *testing.B) {
	hash := gossip.NewConsistentHash(150, nil)
	for i := 0; i < 10; i++ {
		hash.Add("node" + string(rune('0'+i)))
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = hash.Members()
	}
}
