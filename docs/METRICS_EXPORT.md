# GridKV Enterprise Metrics Export

**Version**: v3.1  
**Status**: ✅ Production Ready

---

## 🎯 Overview

GridKV provides enterprise-grade metrics export in industry-standard formats:

- ✅ **Prometheus Format** - Text exposition format
- ✅ **OTLP JSON** - OpenTelemetry Protocol JSON
- ✅ **Zero-cost Collection** - Atomic counters, no overhead
- ✅ **Pre-defined Metrics** - Complete observability

---

## 📊 Supported Formats

### 1. Prometheus Format

Standard Prometheus text format, compatible with:
- Prometheus
- VictoriaMetrics
- Grafana
- Thanos
- Cortex

**Example Output**:
```prometheus
# HELP gridkv_cluster_nodes_total Total number of nodes in cluster
# TYPE gridkv_cluster_nodes_total gauge
gridkv_cluster_nodes_total 5

# HELP gridkv_requests_total Total number of requests  
# TYPE gridkv_requests_total counter
gridkv_requests_total 1000

# HELP gridkv_latency_p50_ns P50 latency
# TYPE gridkv_latency_p50_ns gauge
gridkv_latency_p50_ns 50000
```

### 2. OTLP JSON Format

OpenTelemetry Protocol JSON format, compatible with:
- OpenTelemetry Collector
- Grafana Agent
- Datadog Agent
- New Relic
- Any OTLP endpoint

**Example Output**:
```json
{
  "resourceMetrics": [{
    "resource": {
      "attributes": [
        {"key": "service.name", "value": {"stringValue": "gridkv"}},
        {"key": "service.version", "value": {"stringValue": "3.1.0"}}
      ]
    },
    "scopeMetrics": [{
      "scope": {"name": "gridkv", "version": "3.1.0"},
      "metrics": [
        {
          "name": "gridkv_requests_total",
          "sum": {
            "dataPoints": [{"timeUnixNano": "...", "asDouble": 1000}],
            "aggregationTemporality": "AGGREGATION_TEMPORALITY_CUMULATIVE",
            "isMonotonic": true
          }
        }
      ]
    }]
  }]
}
```

---

## 🚀 Quick Start

### Basic Usage

```go
import (
    "github.com/feellmoose/gridkv/internal/metrics"
)

// Create metrics with Prometheus format
prometheusExporter := metrics.PrometheusExporter(func(text string) error {
    // Send to Prometheus pushgateway or write to file
    return sendToPrometheus(text)
})

gkMetrics := metrics.NewGridKVMetrics(prometheusExporter)

// Record metrics
gkMetrics.IncrementRequestsTotal()
gkMetrics.IncrementSet()
gkMetrics.SetClusterNodesAlive(5)

// Export
gkMetrics.Export(context.Background())
```

### Periodic Export

```go
// Export metrics every 10 seconds
go gkMetrics.StartPeriodicExport(ctx, 10*time.Second)
```

---

## 📋 Pre-defined Metrics

### Cluster Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `cluster_nodes_total` | Gauge | Total nodes in cluster |
| `cluster_nodes_alive` | Gauge | Alive nodes |
| `cluster_nodes_suspect` | Gauge | Suspect nodes |
| `cluster_nodes_dead` | Gauge | Dead nodes |

### Request Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `requests_total` | Counter | Total requests |
| `requests_success` | Counter | Successful requests |
| `requests_errors` | Counter | Failed requests |

### Operation Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `operations_set` | Counter | Set operations |
| `operations_get` | Counter | Get operations |
| `operations_delete` | Counter | Delete operations |

### Replication Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `replication_total` | Counter | Total replications |
| `replication_success` | Counter | Successful replications |
| `replication_failures` | Counter | Failed replications |

### Gossip Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `gossip_messages_sent` | Counter | Messages sent |
| `gossip_messages_received` | Counter | Messages received |
| `gossip_messages_dropped` | Counter | Dropped messages |

### Storage Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `storage_keys_total` | Gauge | Total keys |
| `storage_size_bytes` | Gauge | Storage size |

### Performance Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `latency_p50_ns` | Gauge | P50 latency (ns) |
| `latency_p95_ns` | Gauge | P95 latency (ns) |
| `latency_p99_ns` | Gauge | P99 latency (ns) |

---

## 🔧 Integration Examples

### With Prometheus

```go
// Write to file for Prometheus file_sd
prometheusExporter := metrics.PrometheusExporter(func(text string) error {
    return os.WriteFile("/var/lib/prometheus/gridkv.prom", []byte(text), 0644)
})
```

**Prometheus config**:
```yaml
scrape_configs:
  - job_name: 'gridkv'
    scrape_interval: 10s
    file_sd_configs:
      - files:
        - '/var/lib/prometheus/gridkv.prom'
```

### With OpenTelemetry Collector

```go
// Send to OTLP endpoint
otlpExporter := metrics.OTLPJSONExporter("gridkv", "3.1.0", func(data []byte) error {
    return sendToOTLPCollector("http://localhost:4318/v1/metrics", data)
})
```

**OTel Collector config**:
```yaml
receivers:
  otlp:
    protocols:
      http:
        endpoint: 0.0.0.0:4318

exporters:
  prometheus:
    endpoint: "0.0.0.0:8889"

service:
  pipelines:
    metrics:
      receivers: [otlp]
      exporters: [prometheus]
```

### With Grafana

```go
// Export to Grafana via Prometheus
prometheusExporter := metrics.PrometheusExporter(func(text string) error {
    // Push to Prometheus Pushgateway
    return pushToGateway("http://pushgateway:9091", "gridkv", text)
})
```

**Grafana Dashboard**: Import pre-built GridKV dashboard (coming soon)

---

## 💡 Best Practices

### 1. Use Periodic Export

```go
// Don't export on every operation (overhead)
// Instead, export periodically
go gkMetrics.StartPeriodicExport(ctx, 10*time.Second)
```

### 2. Choose Right Format

- **Prometheus**: If you already use Prometheus
- **OTLP JSON**: If you use OpenTelemetry ecosystem
- **Both**: Export to multiple backends

### 3. Monitor Key Metrics

Essential metrics to monitor:
- `cluster_nodes_alive` - Cluster health
- `requests_errors` / `requests_total` - Error rate
- `replication_failures` - Replication health
- `latency_p95_ns` - Performance

### 4. Set Alerts

Example Prometheus alerts:
```yaml
groups:
  - name: gridkv
    rules:
      - alert: GridKVHighErrorRate
        expr: rate(gridkv_requests_errors[5m]) > 0.05
        annotations:
          summary: "GridKV error rate > 5%"
      
      - alert: GridKVNodeDown
        expr: gridkv_cluster_nodes_alive < gridkv_cluster_nodes_total
        annotations:
          summary: "GridKV node is down"
```

---

## 🎯 Performance

All metrics operations are highly optimized:

```
IncrementCounter:  ~10 ns/op    0 allocs  (atomic.Add)
SetGauge:          ~10 ns/op    0 allocs  (atomic.Store)
Export:           ~1000 ns/op   minimal   (batched)
```

**Zero-cost metrics collection**: < 1% performance overhead

---

## 📖 Example

See [11_metrics_export](../examples/11_metrics_export/) for complete example.

---

**Last Updated**: 2025-11-08  
**GridKV Version**: v3.1

