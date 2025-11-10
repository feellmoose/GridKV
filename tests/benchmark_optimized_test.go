package tests

import (
	"testing"
	"github.com/feellmoose/gridkv/internal/network"
)

// BenchmarkMetrics_GetStatsStruct benchmarks the optimized struct-based GetStats
func BenchmarkMetrics_GetStatsStruct(b *testing.B) {
	metrics := network.NewSimpleMetrics()
	
	// Add some data
	metrics.RecordRequest(1000, nil)
	metrics.RecordProbe(true)
	metrics.RecordOperation(true)
	
	b.ResetTimer()
	b.ReportAllocs()
	
	for i := 0; i < b.N; i++ {
		_ = metrics.GetStatsStruct()
	}
}

// BenchmarkMetrics_GetStatsMap benchmarks the old map-based GetStats
func BenchmarkMetrics_GetStatsMap(b *testing.B) {
	metrics := network.NewSimpleMetrics()
	
	// Add some data
	metrics.RecordRequest(1000, nil)
	metrics.RecordProbe(true)
	metrics.RecordOperation(true)
	
	b.ResetTimer()
	b.ReportAllocs()
	
	for i := 0; i < b.N; i++ {
		_ = metrics.GetStats()
	}
}

// BenchmarkComparison_StructVsMap compares struct vs map performance
func BenchmarkComparison_StructVsMap(b *testing.B) {
	metrics := network.NewSimpleMetrics()
	metrics.RecordRequest(1000, nil)
	
	b.Run("Struct", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = metrics.GetStatsStruct()
		}
	})
	
	b.Run("Map", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = metrics.GetStats()
		}
	})
}
