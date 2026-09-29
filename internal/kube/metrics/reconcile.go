package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/skupperproject/skupper/internal/kube/reconcile"
	"k8s.io/client-go/util/workqueue"
)

// MustRegisterNamespaceReconcileMetrics registers metrics for the namespace
// reconciliation architecture. Labels are deliberately bounded and contain no
// namespace or resource identity.
func MustRegisterNamespaceReconcileMetrics(registry *prometheus.Registry) reconcile.Metrics {
	m := &namespaceReconcileMetrics{
		leader:        prometheus.NewGauge(prometheus.GaugeOpts{Namespace: "skupper", Subsystem: "controller", Name: "leader", Help: "Whether this controller Pod is currently inside the fenced serving lifecycle (1 leader, 0 standby)."}),
		invalidations: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "skupper", Subsystem: "namespace_reconcile", Name: "invalidations_total", Help: "External namespace invalidation requests before workqueue key deduplication."}, []string{"source"}),
		adds:          prometheus.NewCounter(prometheus.CounterOpts{Namespace: "skupper", Subsystem: "namespace_reconcile", Name: "queue_adds_total", Help: "Namespace keys accepted by the workqueue after deduplication, including internal retry additions."}),
		depth:         prometheus.NewGauge(prometheus.GaugeOpts{Namespace: "skupper", Subsystem: "namespace_reconcile", Name: "queue_depth", Help: "Current number of namespace keys waiting in the workqueue."}),
		wait:          prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: "skupper", Subsystem: "namespace_reconcile", Name: "queue_wait_duration_seconds", Help: "Time from an accepted queue addition until the namespace key starts processing.", Buckets: prometheus.ExponentialBuckets(0.001, 2, 17)}),
		work:          prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: "skupper", Subsystem: "namespace_reconcile", Name: "work_duration_seconds", Help: "Duration of one namespace reconciliation attempt, from dequeue through completion.", Buckets: prometheus.ExponentialBuckets(0.001, 2, 17)}),
		unfinished:    prometheus.NewGauge(prometheus.GaugeOpts{Namespace: "skupper", Subsystem: "namespace_reconcile", Name: "unfinished_work_seconds", Help: "Total elapsed seconds represented by currently executing reconciliation attempts."}),
		longest:       prometheus.NewGauge(prometheus.GaugeOpts{Namespace: "skupper", Subsystem: "namespace_reconcile", Name: "longest_running_processor_seconds", Help: "Elapsed seconds of the longest currently executing reconciliation attempt."}),
		retries:       prometheus.NewCounter(prometheus.CounterOpts{Namespace: "skupper", Subsystem: "namespace_reconcile", Name: "queue_retries_total", Help: "Internal rate-limited namespace requeue requests."}),
		outcomes:      prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "skupper", Subsystem: "namespace_reconcile", Name: "outcomes_total", Help: "Completed namespace reconciliation attempts by bounded outcome."}, []string{"outcome"}),
		active:        prometheus.NewGauge(prometheus.GaugeOpts{Namespace: "skupper", Subsystem: "namespace_reconcile", Name: "workers_active", Help: "Number of workers currently executing a namespace reconciliation attempt."}),
	}
	registry.MustRegister(m.leader, m.invalidations, m.adds, m.depth, m.wait, m.work, m.unfinished, m.longest, m.retries, m.outcomes, m.active)
	for _, source := range []string{"informer", "observation", "startup"} {
		m.invalidations.WithLabelValues(source)
	}
	for _, outcome := range []string{"success", "retry", "error", "cancelled"} {
		m.outcomes.WithLabelValues(outcome)
	}
	return m
}

type namespaceReconcileMetrics struct {
	leader        prometheus.Gauge
	invalidations *prometheus.CounterVec
	adds          prometheus.Counter
	depth         prometheus.Gauge
	wait          prometheus.Histogram
	work          prometheus.Histogram
	unfinished    prometheus.Gauge
	longest       prometheus.Gauge
	retries       prometheus.Counter
	outcomes      *prometheus.CounterVec
	active        prometheus.Gauge
}

func (m *namespaceReconcileMetrics) NewDepthMetric(string) workqueue.GaugeMetric       { return m.depth }
func (m *namespaceReconcileMetrics) NewAddsMetric(string) workqueue.CounterMetric      { return m.adds }
func (m *namespaceReconcileMetrics) NewLatencyMetric(string) workqueue.HistogramMetric { return m.wait }
func (m *namespaceReconcileMetrics) NewWorkDurationMetric(string) workqueue.HistogramMetric {
	return m.work
}
func (m *namespaceReconcileMetrics) NewUnfinishedWorkSecondsMetric(string) workqueue.SettableGaugeMetric {
	return m.unfinished
}
func (m *namespaceReconcileMetrics) NewLongestRunningProcessorSecondsMetric(string) workqueue.SettableGaugeMetric {
	return m.longest
}
func (m *namespaceReconcileMetrics) NewRetriesMetric(string) workqueue.CounterMetric {
	return m.retries
}
func (m *namespaceReconcileMetrics) Invalidated(source string) {
	m.invalidations.WithLabelValues(source).Inc()
}
func (m *namespaceReconcileMetrics) ReconcileStarted() { m.active.Inc() }
func (m *namespaceReconcileMetrics) ReconcileFinished(outcome string) {
	m.active.Dec()
	m.outcomes.WithLabelValues(outcome).Inc()
}
func (m *namespaceReconcileMetrics) SetLeader(active bool) {
	if active {
		m.leader.Set(1)
	} else {
		m.leader.Set(0)
	}
}
