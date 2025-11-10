package network

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// MetricsCollector collects and aggregates network metrics
type MetricsCollector struct {
	mu              sync.RWMutex
	startTime       time.Time
	metrics         map[string]*Metric
	timeSeries      map[string]*TimeSeries
	anomalyDetector *AnomalyDetector
	alertCallbacks  []AlertCallback
}

// Metric represents a single metric value
type Metric struct {
	Name        string
	Value       atomic.Int64
	Type        MetricType
	Description string
	Labels      map[string]string
	LastUpdated time.Time
}

// MetricType defines the type of metric
type MetricType int

const (
	MetricTypeCounter MetricType = iota
	MetricTypeGauge
	MetricTypeHistogram
	MetricTypeSummary
)

func (mt MetricType) String() string {
	switch mt {
	case MetricTypeCounter:
		return "Counter"
	case MetricTypeGauge:
		return "Gauge"
	case MetricTypeHistogram:
		return "Histogram"
	case MetricTypeSummary:
		return "Summary"
	default:
		return "Unknown"
	}
}

// TimeSeries stores time-series data for a metric
type TimeSeries struct {
	mu         sync.RWMutex
	Name       string
	MaxSamples int
	samples    []TimeSeriesSample
}

// TimeSeriesSample represents a single data point
type TimeSeriesSample struct {
	Timestamp time.Time
	Value     float64
}

// AlertCallback is called when an alert is triggered
type AlertCallback func(alert *Alert)

// Alert represents a triggered alert
type Alert struct {
	Timestamp   time.Time
	Severity    AlertSeverity
	Title       string
	Description string
	MetricName  string
	Threshold   float64
	ActualValue float64
	Labels      map[string]string
}

// AlertSeverity defines alert severity levels
type AlertSeverity int

const (
	AlertSeverityInfo AlertSeverity = iota
	AlertSeverityWarning
	AlertSeverityError
	AlertSeverityCritical
)

func (as AlertSeverity) String() string {
	switch as {
	case AlertSeverityInfo:
		return "Info"
	case AlertSeverityWarning:
		return "Warning"
	case AlertSeverityError:
		return "Error"
	case AlertSeverityCritical:
		return "Critical"
	default:
		return "Unknown"
	}
}

// MetricsCollectorOptions configures the metrics collector
type MetricsCollectorOptions struct {
	EnableTimeSeries bool
	TimeSeriesWindow int
	AnomalyDetection bool
}

// NewMetricsCollector creates a new metrics collector
func NewMetricsCollector(opts *MetricsCollectorOptions) *MetricsCollector {
	if opts == nil {
		opts = &MetricsCollectorOptions{
			EnableTimeSeries: true,
			TimeSeriesWindow: 120, // 2 minutes at 1s granularity
			AnomalyDetection: true,
		}
	}

	mc := &MetricsCollector{
		startTime:      time.Now(),
		metrics:        make(map[string]*Metric),
		timeSeries:     make(map[string]*TimeSeries),
		alertCallbacks: make([]AlertCallback, 0),
	}

	if opts.AnomalyDetection {
		mc.anomalyDetector = NewAnomalyDetector(&AnomalyDetectorOptions{
			Window:        opts.TimeSeriesWindow,
			AlertCallback: mc.triggerAlert,
		})
	}

	return mc
}

// RegisterMetric registers a new metric
func (mc *MetricsCollector) RegisterMetric(name string, metricType MetricType, description string, labels map[string]string) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	if _, exists := mc.metrics[name]; exists {
		return
	}

	mc.metrics[name] = &Metric{
		Name:        name,
		Type:        metricType,
		Description: description,
		Labels:      labels,
		LastUpdated: time.Now(),
	}

	// Create time series if enabled
	mc.timeSeries[name] = &TimeSeries{
		Name:       name,
		MaxSamples: 120,
		samples:    make([]TimeSeriesSample, 0, 120),
	}
}

// Increment increments a counter metric
func (mc *MetricsCollector) Increment(name string, delta int64) {
	mc.mu.RLock()
	metric, exists := mc.metrics[name]
	mc.mu.RUnlock()

	if !exists {
		mc.RegisterMetric(name, MetricTypeCounter, "", nil)
		mc.mu.RLock()
		metric = mc.metrics[name]
		mc.mu.RUnlock()
	}

	metric.Value.Add(delta)
	metric.LastUpdated = time.Now()
}

// Set sets a gauge metric value
func (mc *MetricsCollector) Set(name string, value int64) {
	mc.mu.RLock()
	metric, exists := mc.metrics[name]
	mc.mu.RUnlock()

	if !exists {
		mc.RegisterMetric(name, MetricTypeGauge, "", nil)
		mc.mu.RLock()
		metric = mc.metrics[name]
		mc.mu.RUnlock()
	}

	metric.Value.Store(value)
	metric.LastUpdated = time.Now()

	// Add to time series
	mc.addTimeSeriesSample(name, float64(value))

	// Check for anomalies
	if mc.anomalyDetector != nil {
		mc.anomalyDetector.CheckValue(name, float64(value))
	}
}

// Get retrieves a metric value
func (mc *MetricsCollector) Get(name string) (int64, bool) {
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	metric, exists := mc.metrics[name]
	if !exists {
		return 0, false
	}

	return metric.Value.Load(), true
}

// addTimeSeriesSample adds a sample to the time series
func (mc *MetricsCollector) addTimeSeriesSample(name string, value float64) {
	mc.mu.RLock()
	ts, exists := mc.timeSeries[name]
	mc.mu.RUnlock()

	if !exists {
		return
	}

	ts.mu.Lock()
	defer ts.mu.Unlock()

	ts.samples = append(ts.samples, TimeSeriesSample{
		Timestamp: time.Now(),
		Value:     value,
	})

	if len(ts.samples) > ts.MaxSamples {
		ts.samples = ts.samples[1:]
	}
}

// GetTimeSeries returns time series data for a metric
func (mc *MetricsCollector) GetTimeSeries(name string) []TimeSeriesSample {
	mc.mu.RLock()
	ts, exists := mc.timeSeries[name]
	mc.mu.RUnlock()

	if !exists {
		return nil
	}

	ts.mu.RLock()
	defer ts.mu.RUnlock()

	// Return a copy
	result := make([]TimeSeriesSample, len(ts.samples))
	copy(result, ts.samples)
	return result
}

// RegisterAlertCallback registers a callback for alerts
func (mc *MetricsCollector) RegisterAlertCallback(callback AlertCallback) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.alertCallbacks = append(mc.alertCallbacks, callback)
}

// triggerAlert triggers an alert
func (mc *MetricsCollector) triggerAlert(alert *Alert) {
	mc.mu.RLock()
	callbacks := make([]AlertCallback, len(mc.alertCallbacks))
	copy(callbacks, mc.alertCallbacks)
	mc.mu.RUnlock()

	for _, callback := range callbacks {
		callback(alert)
	}
}

// GetAllMetrics returns all metrics
func (mc *MetricsCollector) GetAllMetrics() map[string]interface{} {
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	result := make(map[string]interface{})
	for name, metric := range mc.metrics {
		result[name] = map[string]interface{}{
			"value":        metric.Value.Load(),
			"type":         metric.Type.String(),
			"description":  metric.Description,
			"labels":       metric.Labels,
			"last_updated": metric.LastUpdated.Format(time.RFC3339),
		}
	}

	result["uptime_seconds"] = time.Since(mc.startTime).Seconds()
	return result
}

// AnomalyDetector detects anomalies in metric values
type AnomalyDetector struct {
	mu            sync.RWMutex
	window        int
	values        map[string][]float64
	stats         map[string]*AnomalyStats
	alertCallback func(alert *Alert)
}

// AnomalyStats tracks statistics for anomaly detection
type AnomalyStats struct {
	Mean   float64
	StdDev float64
	Min    float64
	Max    float64
	Count  int
}

// AnomalyDetectorOptions configures the anomaly detector
type AnomalyDetectorOptions struct {
	Window        int
	AlertCallback func(alert *Alert)
}

// NewAnomalyDetector creates a new anomaly detector
func NewAnomalyDetector(opts *AnomalyDetectorOptions) *AnomalyDetector {
	return &AnomalyDetector{
		window:        opts.Window,
		values:        make(map[string][]float64),
		stats:         make(map[string]*AnomalyStats),
		alertCallback: opts.AlertCallback,
	}
}

// CheckValue checks if a value is anomalous
func (ad *AnomalyDetector) CheckValue(name string, value float64) {
	ad.mu.Lock()
	defer ad.mu.Unlock()

	// Initialize if needed
	if _, exists := ad.values[name]; !exists {
		ad.values[name] = make([]float64, 0, ad.window)
		ad.stats[name] = &AnomalyStats{}
	}

	// Add value
	ad.values[name] = append(ad.values[name], value)
	if len(ad.values[name]) > ad.window {
		ad.values[name] = ad.values[name][1:]
	}

	// Need at least 10 samples for meaningful statistics
	if len(ad.values[name]) < 10 {
		return
	}

	// Calculate statistics
	stats := ad.calculateStats(ad.values[name])
	ad.stats[name] = stats

	// Check for anomalies using 3-sigma rule
	threshold := stats.Mean + 3*stats.StdDev
	if value > threshold {
		if ad.alertCallback != nil {
			ad.alertCallback(&Alert{
				Timestamp:   time.Now(),
				Severity:    AlertSeverityWarning,
				Title:       fmt.Sprintf("Anomaly detected in %s", name),
				Description: fmt.Sprintf("Value %.2f exceeds threshold %.2f (mean: %.2f, stddev: %.2f)", value, threshold, stats.Mean, stats.StdDev),
				MetricName:  name,
				Threshold:   threshold,
				ActualValue: value,
			})
		}
	}

	// Check for sudden drops (below 3 sigma)
	lowerThreshold := stats.Mean - 3*stats.StdDev
	if value < lowerThreshold && lowerThreshold > 0 {
		if ad.alertCallback != nil {
			ad.alertCallback(&Alert{
				Timestamp:   time.Now(),
				Severity:    AlertSeverityWarning,
				Title:       fmt.Sprintf("Sudden drop detected in %s", name),
				Description: fmt.Sprintf("Value %.2f below threshold %.2f (mean: %.2f, stddev: %.2f)", value, lowerThreshold, stats.Mean, stats.StdDev),
				MetricName:  name,
				Threshold:   lowerThreshold,
				ActualValue: value,
			})
		}
	}
}

// calculateStats calculates basic statistics
func (ad *AnomalyDetector) calculateStats(values []float64) *AnomalyStats {
	if len(values) == 0 {
		return &AnomalyStats{}
	}

	// Calculate mean
	var sum float64
	min := values[0]
	max := values[0]

	for _, v := range values {
		sum += v
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}

	mean := sum / float64(len(values))

	// Calculate standard deviation
	var varianceSum float64
	for _, v := range values {
		diff := v - mean
		varianceSum += diff * diff
	}

	variance := varianceSum / float64(len(values))
	stdDev := 0.0
	if variance > 0 {
		// Simple square root approximation
		stdDev = variance
		for i := 0; i < 10; i++ {
			stdDev = (stdDev + variance/stdDev) / 2
		}
	}

	return &AnomalyStats{
		Mean:   mean,
		StdDev: stdDev,
		Min:    min,
		Max:    max,
		Count:  len(values),
	}
}

// GetStats returns anomaly statistics for a metric
func (ad *AnomalyDetector) GetStats(name string) (*AnomalyStats, bool) {
	ad.mu.RLock()
	defer ad.mu.RUnlock()

	stats, exists := ad.stats[name]
	if !exists {
		return nil, false
	}

	// Return a copy
	statsCopy := *stats
	return &statsCopy, true
}

// IntegratedNetworkMetrics provides comprehensive network metrics
type IntegratedNetworkMetrics struct {
	collector       *MetricsCollector
	latencyMatrix   *LatencyMatrix
	multiDCManager  *MultiDCManager
	alertManager    *AlertManager
}

// AlertManager manages alerts and notifications
type AlertManager struct {
	mu              sync.RWMutex
	alerts          []*Alert
	maxAlerts       int
	alertCallbacks  []AlertCallback
	suppressionMap  map[string]time.Time // Prevent alert spam
	suppressionTime time.Duration
}

// NewAlertManager creates a new alert manager
func NewAlertManager(maxAlerts int, suppressionTime time.Duration) *AlertManager {
	return &AlertManager{
		alerts:          make([]*Alert, 0, maxAlerts),
		maxAlerts:       maxAlerts,
		alertCallbacks:  make([]AlertCallback, 0),
		suppressionMap:  make(map[string]time.Time),
		suppressionTime: suppressionTime,
	}
}

// RegisterCallback registers an alert callback
func (am *AlertManager) RegisterCallback(callback AlertCallback) {
	am.mu.Lock()
	defer am.mu.Unlock()
	am.alertCallbacks = append(am.alertCallbacks, callback)
}

// TriggerAlert triggers a new alert
func (am *AlertManager) TriggerAlert(alert *Alert) {
	am.mu.Lock()
	
	// Check suppression
	key := fmt.Sprintf("%s:%s", alert.MetricName, alert.Title)
	if lastAlert, exists := am.suppressionMap[key]; exists {
		if time.Since(lastAlert) < am.suppressionTime {
			am.mu.Unlock()
			return // Suppressed
		}
	}
	
	am.suppressionMap[key] = time.Now()
	
	// Add to history
	am.alerts = append(am.alerts, alert)
	if len(am.alerts) > am.maxAlerts {
		am.alerts = am.alerts[1:]
	}

	callbacks := make([]AlertCallback, len(am.alertCallbacks))
	copy(callbacks, am.alertCallbacks)
	am.mu.Unlock()

	// Trigger callbacks
	for _, callback := range callbacks {
		go callback(alert)
	}
}

// GetRecentAlerts returns recent alerts
func (am *AlertManager) GetRecentAlerts(count int) []*Alert {
	am.mu.RLock()
	defer am.mu.RUnlock()

	if count > len(am.alerts) {
		count = len(am.alerts)
	}

	start := len(am.alerts) - count
	result := make([]*Alert, count)
	copy(result, am.alerts[start:])
	return result
}

// NewIntegratedNetworkMetrics creates a comprehensive metrics system
func NewIntegratedNetworkMetrics(
	collector *MetricsCollector,
	latencyMatrix *LatencyMatrix,
	multiDCManager *MultiDCManager,
) *IntegratedNetworkMetrics {
	inm := &IntegratedNetworkMetrics{
		collector:      collector,
		latencyMatrix:  latencyMatrix,
		multiDCManager: multiDCManager,
		alertManager:   NewAlertManager(1000, 5*time.Minute),
	}

	// Register standard metrics
	inm.registerStandardMetrics()

	// Link anomaly detection to alert manager
	if collector != nil {
		collector.RegisterAlertCallback(inm.alertManager.TriggerAlert)
	}

	// Link network anomalies to alert manager
	if latencyMatrix != nil {
		// This would be set in the latency matrix options
	}

	return inm
}

// registerStandardMetrics registers standard network metrics
func (inm *IntegratedNetworkMetrics) registerStandardMetrics() {
	if inm.collector == nil {
		return
	}

	// Network metrics
	inm.collector.RegisterMetric("network.rtt.avg", MetricTypeGauge, "Average RTT across all nodes", nil)
	inm.collector.RegisterMetric("network.rtt.max", MetricTypeGauge, "Maximum RTT", nil)
	inm.collector.RegisterMetric("network.packet_loss", MetricTypeGauge, "Average packet loss rate", nil)
	inm.collector.RegisterMetric("network.probes.total", MetricTypeCounter, "Total probes sent", nil)
	inm.collector.RegisterMetric("network.probes.failed", MetricTypeCounter, "Failed probes", nil)

	// Multi-DC metrics
	inm.collector.RegisterMetric("multidc.operations.intra", MetricTypeCounter, "Intra-DC operations", nil)
	inm.collector.RegisterMetric("multidc.operations.inter", MetricTypeCounter, "Inter-DC operations", nil)
	inm.collector.RegisterMetric("multidc.reads.local", MetricTypeCounter, "Local DC reads", nil)
	inm.collector.RegisterMetric("multidc.reads.remote", MetricTypeCounter, "Remote DC reads", nil)
	inm.collector.RegisterMetric("multidc.writes.local", MetricTypeCounter, "Local DC writes", nil)
	inm.collector.RegisterMetric("multidc.writes.remote", MetricTypeCounter, "Remote DC writes", nil)

	// Anomaly metrics
	inm.collector.RegisterMetric("anomalies.total", MetricTypeCounter, "Total anomalies detected", nil)
	inm.collector.RegisterMetric("alerts.triggered", MetricTypeCounter, "Total alerts triggered", nil)
}

// Update updates all metrics from underlying sources
func (inm *IntegratedNetworkMetrics) Update() {
	if inm.latencyMatrix != nil {
		stats := inm.latencyMatrix.GetStats()
		if avgRTT, ok := stats["average_global_rtt"].(string); ok {
			if duration, err := time.ParseDuration(avgRTT); err == nil {
				inm.collector.Set("network.rtt.avg", int64(duration.Nanoseconds()))
			}
		}
		if totalProbes, ok := stats["total_probes"].(int64); ok {
			inm.collector.Set("network.probes.total", totalProbes)
		}
		if totalFailures, ok := stats["total_failures"].(int64); ok {
			inm.collector.Set("network.probes.failed", totalFailures)
		}
		if totalAnomalies, ok := stats["total_anomalies"].(int64); ok {
			inm.collector.Set("anomalies.total", totalAnomalies)
		}
	}

	if inm.multiDCManager != nil {
		stats := inm.multiDCManager.GetStats()
		if intraDC, ok := stats["intra_dc_operations"].(int64); ok {
			inm.collector.Set("multidc.operations.intra", intraDC)
		}
		if interDC, ok := stats["inter_dc_operations"].(int64); ok {
			inm.collector.Set("multidc.operations.inter", interDC)
		}
		if localReads, ok := stats["local_reads"].(int64); ok {
			inm.collector.Set("multidc.reads.local", localReads)
		}
		if remoteReads, ok := stats["remote_reads"].(int64); ok {
			inm.collector.Set("multidc.reads.remote", remoteReads)
		}
	}
}

// GetDashboard returns a dashboard view of all metrics
func (inm *IntegratedNetworkMetrics) GetDashboard() map[string]interface{} {
	dashboard := make(map[string]interface{})

	if inm.collector != nil {
		dashboard["metrics"] = inm.collector.GetAllMetrics()
	}

	if inm.latencyMatrix != nil {
		dashboard["latency"] = inm.latencyMatrix.GetStats()
	}

	if inm.multiDCManager != nil {
		dashboard["multidc"] = inm.multiDCManager.GetStats()
	}

	if inm.alertManager != nil {
		recentAlerts := inm.alertManager.GetRecentAlerts(10)
		alertsData := make([]map[string]interface{}, len(recentAlerts))
		for i, alert := range recentAlerts {
			alertsData[i] = map[string]interface{}{
				"timestamp":    alert.Timestamp.Format(time.RFC3339),
				"severity":     alert.Severity.String(),
				"title":        alert.Title,
				"description":  alert.Description,
				"metric":       alert.MetricName,
				"threshold":    alert.Threshold,
				"actual_value": alert.ActualValue,
			}
		}
		dashboard["recent_alerts"] = alertsData
	}

	return dashboard
}

