package watchers

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type MetricsProvider interface {
	NewAddedMetric(kind string) CounterMetric
	NewDelayedMetric(kind string) ObservableMetric
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

func (noopMetricsProvider) NewAddedMetric(kind string) CounterMetric           { return noopMetric{} }
func (noopMetricsProvider) NewDelayedMetric(kind string) ObservableMetric      { return noopMetric{} }
func (noopMetricsProvider) NewWorkDurationMetric(kind string) ObservableMetric { return noopMetric{} }
func (noopMetricsProvider) NewRetriesMetric(kind string) CounterMetric         { return noopMetric{} }

type metricsSet struct {
	Added        CounterMetric
	Delayed      ObservableMetric
	WorkDuration ObservableMetric
	Retries      CounterMetric
}

func PrometheusMetrics(registry *prometheus.Registry) MetricsProvider {
	provider := prometheusProvider{
		adds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "skupper",
			Subsystem: "watcher",
			Name:      "adds_total",
			Help:      "Total number of events queued.",
		}, []string{"kind"}),
		delayDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "skupper",
			Subsystem: "watcher",
			Name:      "work_delay_seconds",
			Help:      "How long in seconds an event is queued before it is handled.",
			Buckets:   prometheus.ExponentialBuckets(1e-9, 10, 12),
		}, []string{"kind"}),
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
	adds          *prometheus.CounterVec
	delayDuration *prometheus.HistogramVec
	workDuration  *prometheus.HistogramVec
	retries       *prometheus.CounterVec
}

func (p prometheusProvider) NewAddedMetric(kind string) CounterMetric {
	return p.adds.WithLabelValues(kind)
}
func (p prometheusProvider) NewDelayedMetric(kind string) ObservableMetric {
	return p.delayDuration.WithLabelValues(kind)
}
func (p prometheusProvider) NewWorkDurationMetric(kind string) ObservableMetric {
	return p.workDuration.WithLabelValues(kind)
}

func (p prometheusProvider) NewRetriesMetric(kind string) CounterMetric {
	return p.retries.WithLabelValues(kind)
}

type metricsQueue struct {
	provider MetricsProvider
	metrics  map[string]metricsSet
	pending  map[ResourceChange]time.Time
}

func (q *metricsQueue) add(evt ResourceChange) {
	if _, ok := q.pending[evt]; ok {
		return
	}
	q.pending[evt] = time.Now()
	q.metricsFor(evt.Handler.Kind()).Added.Inc()
}

type metricsClose func(evt ResourceChange, retry bool)

func (q *metricsQueue) get(evt ResourceChange) metricsClose {
	queuedAt, ok := q.pending[evt]
	if !ok {
		return q.done(time.Now())
	}
	q.metricsFor(evt.Handler.Kind()).Delayed.Observe(time.Since(queuedAt).Seconds())
	delete(q.pending, evt)
	return q.done(time.Now())
}

func (q *metricsQueue) done(startTime time.Time) metricsClose {
	return func(evt ResourceChange, retry bool) {
		ms := q.metricsFor(evt.Handler.Kind())
		ms.WorkDuration.Observe(time.Since(startTime).Seconds())
		if retry {
			ms.Retries.Inc()
			q.add(evt)
		}
	}
}

func (q *metricsQueue) metricsFor(kind string) metricsSet {
	if q.metrics == nil {
		q.metrics = map[string]metricsSet{}
	}
	if m, ok := q.metrics[kind]; ok {
		return m
	}
	m := metricsSet{
		Added:        q.provider.NewAddedMetric(kind),
		Delayed:      q.provider.NewDelayedMetric(kind),
		WorkDuration: q.provider.NewWorkDurationMetric(kind),
		Retries:      q.provider.NewRetriesMetric(kind),
	}
	q.metrics[kind] = m
	return m
}
