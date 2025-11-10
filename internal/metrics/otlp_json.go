package metrics

import (
	"encoding/json"
	"fmt"
	"time"
)

// ============================================================================
// OTLP JSON Format Exporter
// OpenTelemetry Protocol JSON format for metrics
// Compatible with OpenTelemetry Collector, Grafana Agent, etc.
// ============================================================================

// OTLPMetricsData represents OTLP metrics data structure
type OTLPMetricsData struct {
	ResourceMetrics []OTLPResourceMetrics `json:"resourceMetrics"`
}

// OTLPResourceMetrics contains metrics for a resource
type OTLPResourceMetrics struct {
	Resource     OTLPResource       `json:"resource"`
	ScopeMetrics []OTLPScopeMetrics `json:"scopeMetrics"`
}

// OTLPResource describes the entity producing metrics
type OTLPResource struct {
	Attributes []OTLPAttribute `json:"attributes"`
}

// OTLPScopeMetrics contains metrics for an instrumentation scope
type OTLPScopeMetrics struct {
	Scope   OTLPScope    `json:"scope"`
	Metrics []OTLPMetric `json:"metrics"`
}

// OTLPScope describes the instrumentation scope
type OTLPScope struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// OTLPMetric represents a single metric
type OTLPMetric struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Unit        string         `json:"unit,omitempty"`
	Sum         *OTLPSum       `json:"sum,omitempty"`
	Gauge       *OTLPGauge     `json:"gauge,omitempty"`
	Histogram   *OTLPHistogram `json:"histogram,omitempty"`
}

// OTLPSum represents a counter metric
type OTLPSum struct {
	DataPoints             []OTLPNumberDataPoint `json:"dataPoints"`
	AggregationTemporality string                `json:"aggregationTemporality"`
	IsMonotonic            bool                  `json:"isMonotonic"`
}

// OTLPGauge represents a gauge metric
type OTLPGauge struct {
	DataPoints []OTLPNumberDataPoint `json:"dataPoints"`
}

// OTLPHistogram represents a histogram metric
type OTLPHistogram struct {
	DataPoints             []OTLPHistogramDataPoint `json:"dataPoints"`
	AggregationTemporality string                   `json:"aggregationTemporality"`
}

// OTLPNumberDataPoint represents a numeric data point
type OTLPNumberDataPoint struct {
	Attributes        []OTLPAttribute `json:"attributes,omitempty"`
	StartTimeUnixNano string          `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      string          `json:"timeUnixNano"`
	AsDouble          *float64        `json:"asDouble,omitempty"`
	AsInt             *int64          `json:"asInt,omitempty"`
}

// OTLPHistogramDataPoint represents a histogram data point
type OTLPHistogramDataPoint struct {
	Attributes        []OTLPAttribute `json:"attributes,omitempty"`
	StartTimeUnixNano string          `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      string          `json:"timeUnixNano"`
	Count             int64           `json:"count"`
	Sum               *float64        `json:"sum,omitempty"`
	BucketCounts      []int64         `json:"bucketCounts,omitempty"`
	ExplicitBounds    []float64       `json:"explicitBounds,omitempty"`
	Min               *float64        `json:"min,omitempty"`
	Max               *float64        `json:"max,omitempty"`
}

// OTLPAttribute represents a key-value attribute
type OTLPAttribute struct {
	Key   string    `json:"key"`
	Value OTLPValue `json:"value"`
}

// OTLPValue represents an attribute value
type OTLPValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	IntValue    *int64   `json:"intValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
}

// FormatOTLPJSON formats metrics in OTLP JSON format
func FormatOTLPJSON(metrics []Metric, serviceName, serviceVersion string) ([]byte, error) {
	now := time.Now()
	nanos := fmt.Sprintf("%d", now.UnixNano())

	// Build resource attributes
	resourceAttrs := []OTLPAttribute{
		{Key: "service.name", Value: OTLPValue{StringValue: &serviceName}},
		{Key: "service.version", Value: OTLPValue{StringValue: &serviceVersion}},
	}

	// Group metrics by type
	otlpMetrics := make([]OTLPMetric, 0, len(metrics))

	for _, m := range metrics {
		// Convert labels to attributes
		attrs := make([]OTLPAttribute, 0, len(m.Labels))
		for k, v := range m.Labels {
			vCopy := v
			attrs = append(attrs, OTLPAttribute{
				Key:   k,
				Value: OTLPValue{StringValue: &vCopy},
			})
		}

		valueCopy := m.Value

		switch m.Type {
		case MetricTypeCounter:
			otlpMetrics = append(otlpMetrics, OTLPMetric{
				Name:        m.Name,
				Description: m.Help,
				Unit:        m.Unit,
				Sum: &OTLPSum{
					DataPoints: []OTLPNumberDataPoint{
						{
							Attributes:   attrs,
							TimeUnixNano: nanos,
							AsDouble:     &valueCopy,
						},
					},
					AggregationTemporality: "AGGREGATION_TEMPORALITY_CUMULATIVE",
					IsMonotonic:            true,
				},
			})

		case MetricTypeGauge:
			otlpMetrics = append(otlpMetrics, OTLPMetric{
				Name:        m.Name,
				Description: m.Help,
				Unit:        m.Unit,
				Gauge: &OTLPGauge{
					DataPoints: []OTLPNumberDataPoint{
						{
							Attributes:   attrs,
							TimeUnixNano: nanos,
							AsDouble:     &valueCopy,
						},
					},
				},
			})
		}
	}

	// Build OTLP structure
	otlpData := OTLPMetricsData{
		ResourceMetrics: []OTLPResourceMetrics{
			{
				Resource: OTLPResource{
					Attributes: resourceAttrs,
				},
				ScopeMetrics: []OTLPScopeMetrics{
					{
						Scope: OTLPScope{
							Name:    "gridkv",
							Version: "3.1.0",
						},
						Metrics: otlpMetrics,
					},
				},
			},
		},
	}

	return json.Marshal(otlpData)
}

// OTLPJSONExporter creates an export function for OTLP JSON format
func OTLPJSONExporter(serviceName, serviceVersion string, outputFunc func([]byte) error) ExportFunc {
	return func(metrics []Metric) error {
		data, err := FormatOTLPJSON(metrics, serviceName, serviceVersion)
		if err != nil {
			return fmt.Errorf("failed to format OTLP JSON: %w", err)
		}
		return outputFunc(data)
	}
}
