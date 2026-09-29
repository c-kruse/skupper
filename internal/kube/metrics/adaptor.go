package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/skupperproject/skupper/internal/kube/adaptor"
	"github.com/skupperproject/skupper/internal/routercontrol"
)

func MustRegisterAdaptorMetrics(registry *prometheus.Registry) adaptor.RuntimeMetrics {
	m := &adaptorMetrics{
		connectionAttempts:  prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "skupper", Subsystem: "adaptor", Name: "connection_attempts_total", Help: "Router-control connection attempts, including the initial attempt, by outcome."}, []string{"outcome"}),
		connectionDuration:  prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: "skupper", Subsystem: "adaptor", Name: "connection_attempt_duration_seconds", Help: "Router-control enrollment and connection attempt duration by outcome.", Buckets: prometheus.ExponentialBuckets(0.01, 2, 13)}, []string{"outcome"}),
		streamUp:            prometheus.NewGauge(prometheus.GaugeOpts{Namespace: "skupper", Subsystem: "adaptor", Name: "control_stream_up", Help: "Whether the current router-control stream is connected (1) or absent (0)."}),
		accepted:            prometheus.NewCounter(prometheus.CounterOpts{Namespace: "skupper", Subsystem: "adaptor", Name: "accepted_intents_total", Help: "Intents accepted at the transport boundary; acceptance does not mean verified application."}),
		realizations:        prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "skupper", Subsystem: "adaptor", Name: "realizations_total", Help: "Local realization attempts by verified application outcome."}, []string{"outcome"}),
		realizationDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: "skupper", Subsystem: "adaptor", Name: "realization_duration_seconds", Help: "Cost of one local realization attempt, not end-to-end convergence time.", Buckets: prometheus.ExponentialBuckets(0.001, 2, 17)}, []string{"outcome"}),
		applicationState:    prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: "skupper", Subsystem: "adaptor", Name: "application_state", Help: "Latest accepted intent state for the current control session; one state is 1 and the others 0."}, []string{"state"}),
		acceptedToApplied:   prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: "skupper", Subsystem: "adaptor", Name: "accepted_to_verified_applied_seconds", Help: "Time from accepting the newest intent until its first verified Applied result, including retries.", Buckets: prometheus.ExponentialBuckets(0.001, 2, 19)}),
		management:          prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "skupper", Subsystem: "adaptor", Name: "local_management_operations_total", Help: "High-level local-router management phases, each potentially containing multiple AMQP requests."}, []string{"operation", "outcome"}),
		managementDuration:  prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: "skupper", Subsystem: "adaptor", Name: "local_management_operation_duration_seconds", Help: "Duration of high-level local-router management phases, not individual AMQP requests.", Buckets: prometheus.ExponentialBuckets(0.001, 2, 17)}, []string{"operation", "outcome"}),
	}
	registry.MustRegister(m.connectionAttempts, m.connectionDuration, m.streamUp, m.accepted, m.realizations, m.realizationDuration, m.applicationState, m.acceptedToApplied, m.management, m.managementDuration)
	for _, outcome := range []string{"success", "error"} {
		m.connectionAttempts.WithLabelValues(outcome)
	}
	for _, state := range []string{"unknown", "pending", "applied", "failed"} {
		m.applicationState.WithLabelValues(state)
	}
	m.SetApplicationState("unknown")
	return m
}

type adaptorMetrics struct {
	connectionAttempts  *prometheus.CounterVec
	connectionDuration  *prometheus.HistogramVec
	streamUp            prometheus.Gauge
	accepted            prometheus.Counter
	realizations        *prometheus.CounterVec
	realizationDuration *prometheus.HistogramVec
	applicationState    *prometheus.GaugeVec
	acceptedToApplied   prometheus.Histogram
	management          *prometheus.CounterVec
	managementDuration  *prometheus.HistogramVec
}

func (m *adaptorMetrics) ConnectionAttempt(outcome string, duration time.Duration) {
	m.connectionAttempts.WithLabelValues(outcome).Inc()
	m.connectionDuration.WithLabelValues(outcome).Observe(duration.Seconds())
}
func (m *adaptorMetrics) SetControlStreamUp(up bool) {
	if up {
		m.streamUp.Set(1)
	} else {
		m.streamUp.Set(0)
	}
}
func (m *adaptorMetrics) IntentAccepted() { m.accepted.Inc() }
func (m *adaptorMetrics) RealizationFinished(state routercontrol.ApplicationState, duration time.Duration) {
	outcome := string(state)
	m.realizations.WithLabelValues(outcome).Inc()
	m.realizationDuration.WithLabelValues(outcome).Observe(duration.Seconds())
}
func (m *adaptorMetrics) SetApplicationState(current string) {
	for _, state := range []string{"unknown", "pending", "applied", "failed"} {
		value := 0.0
		if state == current {
			value = 1
		}
		m.applicationState.WithLabelValues(state).Set(value)
	}
}
func (m *adaptorMetrics) AcceptedToApplied(duration time.Duration) {
	m.acceptedToApplied.Observe(duration.Seconds())
}
func (m *adaptorMetrics) LocalManagementFinished(operation, outcome string, duration time.Duration) {
	m.management.WithLabelValues(operation, outcome).Inc()
	m.managementDuration.WithLabelValues(operation, outcome).Observe(duration.Seconds())
}
