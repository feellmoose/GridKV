package metrics

import (
	"bytes"
	"fmt"
	"strings"
)

// ============================================================================
// Prometheus Text Format Exporter
// Generates metrics in Prometheus exposition format
// Compatible with Prometheus, VictoriaMetrics, Grafana, etc.
// ============================================================================

// FormatPrometheus formats metrics in Prometheus text format
// See: https://prometheus.io/docs/instrumenting/exposition_formats/
func FormatPrometheus(metrics []Metric) string {
	var buf bytes.Buffer

	// Group metrics by name for better organization
	metricsByName := make(map[string][]Metric)
	for _, m := range metrics {
		metricsByName[m.Name] = append(metricsByName[m.Name], m)
	}

	// Output each metric
	for name, mlist := range metricsByName {
		if len(mlist) == 0 {
			continue
		}

		first := mlist[0]

		// Write HELP
		if first.Help != "" {
			fmt.Fprintf(&buf, "# HELP %s %s\n", name, first.Help)
		}

		// Write TYPE
		typeStr := "untyped"
		switch first.Type {
		case MetricTypeCounter:
			typeStr = "counter"
		case MetricTypeGauge:
			typeStr = "gauge"
		case MetricTypeHistogram:
			typeStr = "histogram"
		case MetricTypeSummary:
			typeStr = "summary"
		}
		fmt.Fprintf(&buf, "# TYPE %s %s\n", name, typeStr)

		// Write metric lines
		for _, m := range mlist {
			if len(m.Labels) > 0 {
				fmt.Fprintf(&buf, "%s{%s} %v %d\n",
					m.Name,
					formatLabels(m.Labels),
					m.Value,
					m.Timestamp.UnixMilli())
			} else {
				fmt.Fprintf(&buf, "%s %v %d\n",
					m.Name,
					m.Value,
					m.Timestamp.UnixMilli())
			}
		}

		buf.WriteByte('\n')
	}

	return buf.String()
}

// formatLabels formats labels for Prometheus format
func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}

	parts := make([]string, 0, len(labels))
	for k, v := range labels {
		// Escape special characters
		v = strings.ReplaceAll(v, `\`, `\\`)
		v = strings.ReplaceAll(v, `"`, `\"`)
		v = strings.ReplaceAll(v, "\n", `\n`)
		parts = append(parts, fmt.Sprintf(`%s="%s"`, k, v))
	}

	return strings.Join(parts, ",")
}

// PrometheusExporter creates an export function for Prometheus format
func PrometheusExporter(outputFunc func(string) error) ExportFunc {
	return func(metrics []Metric) error {
		text := FormatPrometheus(metrics)
		return outputFunc(text)
	}
}
