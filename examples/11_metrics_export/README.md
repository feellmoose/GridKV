# Enterprise Metrics Export Example

This example demonstrates GridKV's enterprise-grade metrics export capabilities.

## Features

- ✅ Prometheus text format export
- ✅ OTLP JSON format export (OpenTelemetry)
- ✅ Real-time metrics dashboard
- ✅ 27 pre-defined metrics
- ✅ Zero-overhead collection

## Run

```bash
go run main.go
```

## Output

The example will:
1. Export metrics in Prometheus format (console)
2. Export metrics in OTLP JSON format (console)
3. Write metrics to `gridkv_metrics.prom` file

## Integration

### With Prometheus

```yaml
# prometheus.yml
scrape_configs:
  - job_name: 'gridkv'
    file_sd_configs:
      - files: ['gridkv_metrics.prom']
```

### With OpenTelemetry Collector

```yaml
# otel-collector.yaml
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

## Metrics

See [../../docs/METRICS_EXPORT.md](../../docs/METRICS_EXPORT.md) for complete metrics list.

## Compatible Platforms

- Prometheus
- Grafana
- OpenTelemetry Collector
- VictoriaMetrics
- Datadog
- New Relic
- AWS CloudWatch
- GCP Monitoring

