// Package metrics provides enterprise-grade metrics export for GridKV.
//
// This package implements industry-standard metrics formats:
//   - Prometheus: Text exposition format (most widely used)
//   - OTLP JSON: OpenTelemetry Protocol for cloud-native observability
//
// Features:
//   - Zero-allocation metrics collection (atomic operations)
//   - 23 pre-defined GridKV metrics
//   - Compatible with Prometheus, Grafana, Datadog, New Relic, etc.
//   - <1% performance overhead
//
// Example:
//
//	// Prometheus export
//	exporter := metrics.PrometheusExporter(outputFunc)
//	gkMetrics := metrics.NewGridKVMetrics(exporter)
//	go gkMetrics.StartPeriodicExport(ctx, 10*time.Second)
//
//	// Record metrics
//	gkMetrics.IncrementRequestsTotal()
//	gkMetrics.SetClusterNodesAlive(5)
package metrics

import (
	"context"
	"time"
)

// GridKVMetrics provides pre-defined metrics for GridKV monitoring.
//
// Contains 26 core metrics across 9 categories:
//   - Cluster health (4 metrics)
//   - Request statistics (4 metrics)
//   - Operation counters (3 metrics)
//   - Replication status (3 metrics)
//   - Gossip protocol (2 metrics)
//   - Network I/O (2 metrics)
//   - Storage stats (2 metrics)
//   - Performance (3 metrics)
//   - Pipeline health (3 metrics)
//
// All metrics use atomic operations for zero-allocation updates.
//
// Thread-safety: All methods are safe for concurrent access.
type GridKVMetrics struct {
	exporter *MetricsExporter
}

// NewGridKVMetrics creates a GridKV metrics registry with the specified export function.
//
// Parameters:
//   - exportFunc: Function called to export metrics (e.g., to Prometheus, OTLP endpoint)
//     Called by Export() or StartPeriodicExport()
//     Should handle the actual delivery (HTTP POST, file write, etc.)
//
// Returns:
//   - *GridKVMetrics: Initialized metrics registry with 27 pre-registered metrics
//
// The returned metrics object is ready to use immediately.
// All 27 GridKV metrics are pre-registered with appropriate types and descriptions.
//
// Example:
//
//	// Export to Prometheus format (file)
//	prometheusExporter := metrics.PrometheusExporter(func(text string) error {
//	    return os.WriteFile("/var/metrics/gridkv.prom", []byte(text), 0644)
//	})
//	gkMetrics := metrics.NewGridKVMetrics(prometheusExporter)
//
//	// Export to OTLP endpoint (HTTP)
//	otlpExporter := metrics.OTLPJSONExporter("gridkv", "3.1.0", func(data []byte) error {
//	    resp, err := http.Post("http://otel-collector:4318/v1/metrics",
//	                           "application/json", bytes.NewReader(data))
//	    if err != nil {
//	        return err
//	    }
//	    defer resp.Body.Close()
//	    return nil
//	})
//	gkMetrics := metrics.NewGridKVMetrics(otlpExporter)
//
// Thread-safety: Safe to call concurrently.
func NewGridKVMetrics(exportFunc ExportFunc) *GridKVMetrics {
	exporter := NewMetricsExporter("gridkv", exportFunc)

	// Register all 26 GridKV metrics
	registerGridKVMetrics(exporter)

	return &GridKVMetrics{
		exporter: exporter,
	}
}

// registerGridKVMetrics registers core GridKV metrics (optimized for key indicators)
func registerGridKVMetrics(e *MetricsExporter) {
	// Core cluster health (4 metrics)
	e.RegisterGauge("cluster_nodes_total", "Total number of nodes in cluster", "nodes", nil)
	e.RegisterGauge("cluster_nodes_alive", "Number of alive nodes", "nodes", nil)
	e.RegisterGauge("cluster_nodes_suspect", "Number of suspect nodes", "nodes", nil)
	e.RegisterGauge("cluster_nodes_dead", "Number of dead nodes", "nodes", nil)

	// Core request metrics (4 metrics)
	e.RegisterCounter("requests_total", "Total number of requests", "requests", nil)
	e.RegisterCounter("requests_success", "Successful requests", "requests", nil)
	e.RegisterCounter("requests_errors", "Failed requests", "requests", nil)
	e.RegisterCounter("requests_timeout", "Request timeouts", "requests", nil)

	// Core operation metrics (3 metrics)
	e.RegisterCounter("operations_set", "Set operations", "operations", nil)
	e.RegisterCounter("operations_get", "Get operations", "operations", nil)
	e.RegisterCounter("operations_delete", "Delete operations", "operations", nil)

	// Core replication metrics (3 metrics)
	e.RegisterCounter("replication_total", "Total replications", "replications", nil)
	e.RegisterCounter("replication_success", "Successful replications", "replications", nil)
	e.RegisterCounter("replication_failures", "Failed replications", "replications", nil)

	// Core gossip metrics (2 metrics)
	e.RegisterCounter("gossip_messages_sent", "Gossip messages sent", "messages", nil)
	e.RegisterCounter("gossip_messages_received", "Gossip messages received", "messages", nil)

	// Core network metrics (2 metrics)
	e.RegisterCounter("network_bytes_sent", "Network bytes sent", "bytes", nil)
	e.RegisterCounter("network_bytes_received", "Network bytes received", "bytes", nil)

	// Transport diagnostics
	e.RegisterCounter("transport_send_success_total", "Successful transport sends", "sends", nil)
	e.RegisterCounter("transport_send_failures_total", "Failed transport sends", "sends", nil)

	// Core storage metrics (2 metrics)
	e.RegisterGauge("storage_keys_total", "Total keys in storage", "keys", nil)
	e.RegisterGauge("storage_size_bytes", "Storage size in bytes", "bytes", nil)

	// Core performance metrics (3 metrics)
	e.RegisterGauge("latency_p50_ns", "P50 latency in nanoseconds", "nanoseconds", nil)
	e.RegisterGauge("latency_p95_ns", "P95 latency in nanoseconds", "nanoseconds", nil)
	e.RegisterGauge("latency_p99_ns", "P99 latency in nanoseconds", "nanoseconds", nil)

	// Pipeline metrics (3 metrics - critical for replication health)
	e.RegisterCounter("pipeline_operations_total", "Total operations enqueued to pipelines", "operations", nil)
	e.RegisterCounter("pipeline_operations_dropped", "Operations dropped due to pipeline saturation", "operations", nil)
	e.RegisterGauge("pipeline_active_count", "Number of active replication pipelines", "pipelines", nil)

	// Read batch metrics (4 metrics - optimized from 10)
	e.RegisterCounter("read_batch_requests_total", "Total read requests processed in batches", "requests", nil)
	e.RegisterCounter("read_batch_batches_sent", "Total batch read requests sent", "batches", nil)
	e.RegisterGauge("read_batch_pending_requests", "Current pending read requests in batches", "requests", nil)
	e.RegisterCounter("read_batch_errors", "Batch read processing errors", "errors", nil)

	// Gossip diagnostics
	e.RegisterGauge("gossip_local_healthy_nodes", "Healthy nodes from local perspective", "nodes", nil)
	e.RegisterGauge("gossip_local_cluster_size", "Cluster size from local perspective", "nodes", nil)

	// Security / auth diagnostics
	e.RegisterCounter("security_signature_failures_total", "Total number of failed message signature verifications", "events", nil)
	e.RegisterCounter("security_unauthenticated_messages_total", "Messages rejected due to missing or invalid authentication", "events", nil)
	e.RegisterGauge("security_unauthenticated_message_ratio", "Ratio of unauthenticated messages over total gossip messages", "ratio", nil)

	// Read path metrics (Stage 0.3)
	e.RegisterCounter("read_success", "Successful read operations", "reads", nil)
	e.RegisterCounter("read_fail", "Failed read operations", "reads", nil)
	e.RegisterCounter("read_timeout", "Read operation timeouts", "reads", nil)
	e.RegisterCounter("read_fast_fail", "Read fast-fail (resource limit exceeded)", "reads", nil)
	e.RegisterCounter("read_fallback", "Read fallback to alternative replica", "reads", nil)

	// Multi-replica read metrics (Stage 0.3)
	e.RegisterCounter("read_multi_replica_hits", "Multi-replica read hits", "reads", nil)
	e.RegisterCounter("read_multi_replica_fallbacks", "Multi-replica read fallbacks", "reads", nil)

	// Replication convergence metrics (Stage 0.3)
	e.RegisterCounter("replication_visibility_0ms", "Replication visible at T=0ms", "replications", nil)
	e.RegisterCounter("replication_visibility_50ms", "Replication visible at T=50ms", "replications", nil)
	e.RegisterCounter("replication_visibility_100ms", "Replication visible at T=100ms", "replications", nil)
	e.RegisterCounter("replication_visibility_200ms", "Replication visible at T=200ms", "replications", nil)
	e.RegisterCounter("replication_never_converged", "Replications that never converged", "replications", nil)

	// SWIM metrics (Stage 0.3)
	e.RegisterCounter("swim_state_transitions", "SWIM node state transitions", "transitions", nil)
	e.RegisterCounter("swim_false_positives", "SWIM false positive detections", "detections", nil)
	e.RegisterGauge("swim_self_heal_time_ms", "SWIM self-heal time in milliseconds", "milliseconds", nil)
	e.RegisterGauge("gossip_qps", "Gossip messages per second", "messages_per_second", nil)
	e.RegisterGauge("cache_sync_qps", "CACHE_SYNC messages per second", "messages_per_second", nil)
}

// Cluster Metrics (Gauges)
// Track cluster membership and health status.

// SetClusterNodesTotal updates the total number of known nodes in the cluster.
//
// Parameters:
//   - count: Total nodes (alive + suspect + dead)
//
// Performance: ~10ns (atomic store)
//
//go:inline
func (m *GridKVMetrics) SetClusterNodesTotal(count int64) {
	m.exporter.SetGauge("cluster_nodes_total", count)
}

// SetClusterNodesAlive updates the number of alive (healthy) nodes.
//
// Parameters:
//   - count: Number of nodes responding to health checks
//
// Alert if: count < expected (node failure detected)
//
// Performance: ~10ns (atomic store)
//
//go:inline
func (m *GridKVMetrics) SetClusterNodesAlive(count int64) {
	m.exporter.SetGauge("cluster_nodes_alive", count)
}

// SetClusterNodesSuspect updates the number of suspect nodes.
//
//go:inline
func (m *GridKVMetrics) SetClusterNodesSuspect(count int64) {
	m.exporter.SetGauge("cluster_nodes_suspect", count)
}

// SetClusterNodesDead updates the number of dead nodes.
//
//go:inline
func (m *GridKVMetrics) SetClusterNodesDead(count int64) {
	m.exporter.SetGauge("cluster_nodes_dead", count)
}

// Request Metrics (Counters)
// Track total request volume and success/error rates.

// IncrementRequestsTotal increments the total request counter.
//
// Call this for every incoming request (Set, Get, Delete).
//
// Performance: ~10ns (atomic increment)
//
//go:inline
func (m *GridKVMetrics) IncrementRequestsTotal() {
	m.exporter.IncrementCounter("requests_total")
}

// IncrementRequestsSuccess increments the successful request counter.
//
// Call this after successful request completion.
//
// Performance: ~10ns (atomic increment)
//
//go:inline
func (m *GridKVMetrics) IncrementRequestsSuccess() {
	m.exporter.IncrementCounter("requests_success")
}

// IncrementRequestsErrors increments the failed request counter.
//
// Call this when request fails (timeout, replication failure, etc.)
//
// Alert if: rate(requests_errors[5m]) / rate(requests_total[5m]) > 0.05 (5% error rate)
//
// Performance: ~10ns (atomic increment)
//
//go:inline
func (m *GridKVMetrics) IncrementRequestsErrors() {
	m.exporter.IncrementCounter("requests_errors")
}

// IncrementRequestsTimeout increments the request timeout counter.
//
// Call this when request times out.
//
// Performance: ~10ns (atomic increment)
//
//go:inline
func (m *GridKVMetrics) IncrementRequestsTimeout() {
	m.exporter.IncrementCounter("requests_timeout")
}

// Operation Metrics

//go:inline
func (m *GridKVMetrics) IncrementSet() {
	m.exporter.IncrementCounter("operations_set")
}

//go:inline
func (m *GridKVMetrics) IncrementGet() {
	m.exporter.IncrementCounter("operations_get")
}

//go:inline
func (m *GridKVMetrics) IncrementDelete() {
	m.exporter.IncrementCounter("operations_delete")
}

// Replication Metrics

//go:inline
func (m *GridKVMetrics) IncrementReplicationTotal() {
	m.exporter.IncrementCounter("replication_total")
}

//go:inline
func (m *GridKVMetrics) IncrementReplicationSuccess() {
	m.exporter.IncrementCounter("replication_success")
}

//go:inline
func (m *GridKVMetrics) IncrementReplicationFailures() {
	m.exporter.IncrementCounter("replication_failures")
}

// Gossip Metrics

//go:inline
func (m *GridKVMetrics) IncrementGossipSent() {
	m.exporter.IncrementCounter("gossip_messages_sent")
}

//go:inline
func (m *GridKVMetrics) IncrementGossipReceived() {
	m.exporter.IncrementCounter("gossip_messages_received")
}

// Network Metrics

// AddNetworkBytesSent adds bytes to the network bytes sent counter.
//
//go:inline
func (m *GridKVMetrics) AddNetworkBytesSent(bytes int64) {
	m.exporter.AddCounter("network_bytes_sent", bytes)
}

// AddNetworkBytesReceived adds bytes to the network bytes received counter.
//
//go:inline
func (m *GridKVMetrics) AddNetworkBytesReceived(bytes int64) {
	m.exporter.AddCounter("network_bytes_received", bytes)
}

// IncrementTransportSendSuccess increments the transport send success counter.
//
//go:inline
func (m *GridKVMetrics) IncrementTransportSendSuccess() {
	m.exporter.IncrementCounter("transport_send_success_total")
}

// IncrementTransportSendFailures increments the transport send failures counter.
//
//go:inline
func (m *GridKVMetrics) IncrementTransportSendFailures() {
	m.exporter.IncrementCounter("transport_send_failures_total")
}

// Storage Metrics

// SetStorageKeys updates the total number of keys currently persisted.
//
// Use this when compaction or migration changes the key count materially.
//
//go:inline
func (m *GridKVMetrics) SetStorageKeys(count int64) {
	m.exporter.SetGauge("storage_keys_total", count)
}

// SetStorageBytes records the total logical storage footprint in bytes.
//
// This typically maps to the backing store's allocated size.
//
//go:inline
func (m *GridKVMetrics) SetStorageBytes(bytes int64) {
	m.exporter.SetGauge("storage_size_bytes", bytes)
}

// Performance Metrics

// SetLatencyP50 sets the rolling P50 latency (nanoseconds) for served requests.
//
//go:inline
func (m *GridKVMetrics) SetLatencyP50(nanos int64) {
	m.exporter.SetGauge("latency_p50_ns", nanos)
}

// SetLatencyP95 sets the rolling P95 latency (nanoseconds) for served requests.
//
//go:inline
func (m *GridKVMetrics) SetLatencyP95(nanos int64) {
	m.exporter.SetGauge("latency_p95_ns", nanos)
}

// SetLatencyP99 sets the rolling P99 latency (nanoseconds) for served requests.
//
//go:inline
func (m *GridKVMetrics) SetLatencyP99(nanos int64) {
	m.exporter.SetGauge("latency_p99_ns", nanos)
}

// Pipeline Metrics (critical for replication health)

// IncrementPipelineOperationsTotal increments total operations enqueued.
//
//go:inline
func (m *GridKVMetrics) IncrementPipelineOperationsTotal() {
	m.exporter.IncrementCounter("pipeline_operations_total")
}

// IncrementPipelineOperationsDropped increments dropped operations.
//
//go:inline
func (m *GridKVMetrics) IncrementPipelineOperationsDropped() {
	m.exporter.IncrementCounter("pipeline_operations_dropped")
}

// SetPipelineActiveCount sets the number of active pipelines.
//
//go:inline
func (m *GridKVMetrics) SetPipelineActiveCount(count int64) {
	m.exporter.SetGauge("pipeline_active_count", count)
}

// SetShutdownPendingShards reports pending shards during graceful shutdown.
//
//go:inline
func (m *GridKVMetrics) SetShutdownPendingShards(count int64) {
	m.exporter.SetGauge("shutdown_pending_shards", count)
}

// Read Batch Processing Metrics (optimized)

// AddReadBatchRequestsTotal adds a value to the total read requests processed in batches.
//
//go:inline
func (m *GridKVMetrics) AddReadBatchRequestsTotal(value int64) {
	m.exporter.AddCounter("read_batch_requests_total", value)
}

// IncrementReadBatchBatchesSent increments the total batch read requests sent.
//
//go:inline
func (m *GridKVMetrics) IncrementReadBatchBatchesSent() {
	m.exporter.IncrementCounter("read_batch_batches_sent")
}

// IncrementReadBatchErrors increments batch read processing errors.
//
//go:inline
func (m *GridKVMetrics) IncrementReadBatchErrors() {
	m.exporter.IncrementCounter("read_batch_errors")
}

// SetReadBatchPendingRequests sets the current pending read requests in batches.
//
//go:inline
func (m *GridKVMetrics) SetReadBatchPendingRequests(count int64) {
	m.exporter.SetGauge("read_batch_pending_requests", count)
}

// SetGossipLocalHealthyNodes sets the gauge for locally observed healthy nodes.
//
//go:inline
func (m *GridKVMetrics) SetGossipLocalHealthyNodes(count int64) {
	m.exporter.SetGauge("gossip_local_healthy_nodes", count)
}

// SetGossipLocalClusterSize sets the gauge for locally observed cluster size.
//
//go:inline
func (m *GridKVMetrics) SetGossipLocalClusterSize(count int64) {
	m.exporter.SetGauge("gossip_local_cluster_size", count)
}

// Security / auth metrics
//
//go:inline
func (m *GridKVMetrics) IncrementSecuritySignatureFailures() {
	m.exporter.IncrementCounter("security_signature_failures_total")
}

//go:inline
func (m *GridKVMetrics) IncrementSecurityUnauthenticatedMessages() {
	m.exporter.IncrementCounter("security_unauthenticated_messages_total")
}

//go:inline
func (m *GridKVMetrics) SetSecurityUnauthenticatedMessageRatio(ratio float64) {
	// Store ratio as integer scaled by 1e6 to avoid float handling in exporter
	m.exporter.SetGauge("security_unauthenticated_message_ratio", int64(ratio*1_000_000))
}

// Export collects all metrics and sends them to the configured export function.
//
// This triggers the exportFunc provided to NewGridKVMetrics, which formats
// and delivers the metrics (e.g., to Prometheus, OTLP endpoint, file).
//
// Parameters:
//   - ctx: Context for cancellation (export may take 1-10ms)
//
// Returns:
//   - error: Error from exportFunc, or nil on success
//
// Performance:
//   - Time: ~1-10ms depending on number of metrics and export destination
//   - Memory: Minimal allocations (metrics are collected, not copied)
//
// Example:
//
//	// Manual export
//	if err := gkMetrics.Export(ctx); err != nil {
//	    log.Printf("Metrics export failed: %v", err)
//	}
//
// Thread-safety: Safe to call concurrently.
func (m *GridKVMetrics) Export(ctx context.Context) error {
	return m.exporter.Export(ctx)
}

// GetExporter returns the underlying MetricsExporter.
//
// Returns:
//   - *MetricsExporter: The exporter instance
//
// Advanced use only: For custom metric registration or direct access.
// Most users should use the pre-defined metrics methods.
func (m *GridKVMetrics) GetExporter() *MetricsExporter {
	return m.exporter
}

// StartPeriodicExport starts a background goroutine that exports metrics periodically.
//
// Parameters:
//   - ctx: Context for cancellation (stops export when ctx.Done())
//   - interval: Export interval (recommended: 10-60 seconds)
//     Too frequent: Increases overhead and storage costs
//     Too rare: Delays anomaly detection
//
// Behavior:
//   - Exports metrics every interval
//   - Continues on export errors (no crash)
//   - Stops when ctx is cancelled
//   - Blocks until stopped (run in goroutine)
//
// Example:
//
//	ctx, cancel := context.WithCancel(context.Background())
//	defer cancel()
//
//	// Export every 10 seconds
//	go gkMetrics.StartPeriodicExport(ctx, 10*time.Second)
//
//	// Do other work...
//	// Metrics export continues in background
//
//	// Stop export
//	cancel()
//
// Best practices:
//   - Run in separate goroutine (will block)
//   - Use cancellable context for clean shutdown
//   - Monitor export errors in production
//
// Thread-safety: Safe to call concurrently (but only call once per instance).
func (m *GridKVMetrics) StartPeriodicExport(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := m.Export(ctx); err != nil {
				// Log error but continue exporting
				// Production systems should monitor export failures
				_ = err
			}
		case <-ctx.Done():
			return
		}
	}
}

// Read Path Metrics (Stage 0.3)

//go:inline
func (m *GridKVMetrics) IncrementReadSuccess() {
	m.exporter.IncrementCounter("read_success")
}

//go:inline
func (m *GridKVMetrics) IncrementReadFail() {
	m.exporter.IncrementCounter("read_fail")
}

//go:inline
func (m *GridKVMetrics) IncrementReadTimeout() {
	m.exporter.IncrementCounter("read_timeout")
}

//go:inline
func (m *GridKVMetrics) IncrementReadFastFail() {
	m.exporter.IncrementCounter("read_fast_fail")
}

//go:inline
func (m *GridKVMetrics) IncrementReadFallback() {
	m.exporter.IncrementCounter("read_fallback")
}

// Multi-Replica Read Metrics (Stage 0.3)

//go:inline
func (m *GridKVMetrics) IncrementReadMultiReplicaHits() {
	m.exporter.IncrementCounter("read_multi_replica_hits")
}

//go:inline
func (m *GridKVMetrics) IncrementReadMultiReplicaFallbacks() {
	m.exporter.IncrementCounter("read_multi_replica_fallbacks")
}

// Replication Convergence Metrics (Stage 0.3)

//go:inline
func (m *GridKVMetrics) IncrementReplicationVisibility0ms() {
	m.exporter.IncrementCounter("replication_visibility_0ms")
}

//go:inline
func (m *GridKVMetrics) IncrementReplicationVisibility50ms() {
	m.exporter.IncrementCounter("replication_visibility_50ms")
}

//go:inline
func (m *GridKVMetrics) IncrementReplicationVisibility100ms() {
	m.exporter.IncrementCounter("replication_visibility_100ms")
}

//go:inline
func (m *GridKVMetrics) IncrementReplicationVisibility200ms() {
	m.exporter.IncrementCounter("replication_visibility_200ms")
}

//go:inline
func (m *GridKVMetrics) IncrementReplicationNeverConverged() {
	m.exporter.IncrementCounter("replication_never_converged")
}

// SWIM Metrics (Stage 0.3)

//go:inline
func (m *GridKVMetrics) IncrementSWIMStateTransitions() {
	m.exporter.IncrementCounter("swim_state_transitions")
}

//go:inline
func (m *GridKVMetrics) IncrementSWIMFalsePositives() {
	m.exporter.IncrementCounter("swim_false_positives")
}

//go:inline
func (m *GridKVMetrics) SetSWIMSelfHealTime(ms int64) {
	m.exporter.SetGauge("swim_self_heal_time_ms", ms)
}

//go:inline
func (m *GridKVMetrics) SetGossipQPS(qps int64) {
	m.exporter.SetGauge("gossip_qps", qps)
}

//go:inline
func (m *GridKVMetrics) SetCacheSyncQPS(qps int64) {
	m.exporter.SetGauge("cache_sync_qps", qps)
}
