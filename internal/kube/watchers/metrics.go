package watchers

import (
	"github.com/prometheus/client_golang/prometheus"
)

type MetricsProvider interface {
	NewWorkDurationMetric(kind string) ObservableMetric
	NewRetriesMetric(kind string) CounterMetric
}

type CounterMetric interface {
	Inc()
}
type ObservableMetric interface {
	Observe(float64)
}

type noopMetric struct{}

func (noopMetric) Inc()            {}
func (noopMetric) Observe(float64) {}

type noopMetricsProvider struct{}

func (noopMetricsProvider) NewWorkDurationMetric(kind string) ObservableMetric { return noopMetric{} }
func (noopMetricsProvider) NewRetriesMetric(kind string) CounterMetric         { return noopMetric{} }

type metricsSet struct {
	WorkDuration ObservableMetric
	Retries      CounterMetric
}

func PrometheusMetrics(registry *prometheus.Registry) MetricsProvider {
	provider := prometheusProvider{
		workDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "skupper",
			Subsystem: "watcher",
			Name:      "work_duration_seconds",
			Help:      "How long in seconds handling a watch event takes.",
			Buckets:   prometheus.ExponentialBuckets(1e-9, 10, 12),
		}, []string{"kind"}),
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "skupper",
			Subsystem: "watcher",
			Name:      "retries_total",
			Help:      "Total number of retries queued.",
		}, []string{"kind"}),
	}
	registry.MustRegister(provider.workDuration, provider.retries)
	return provider
}

type prometheusProvider struct {
	workDuration *prometheus.HistogramVec
	retries      *prometheus.CounterVec
}

func (p prometheusProvider) NewWorkDurationMetric(kind string) ObservableMetric {
	return p.workDuration.WithLabelValues(kind)
}

func (p prometheusProvider) NewRetriesMetric(kind string) CounterMetric {
	return p.retries.WithLabelValues(kind)
}
