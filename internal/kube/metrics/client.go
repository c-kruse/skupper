package metrics

import (
	"context"
	"net/url"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/tools/metrics"
)

func MustRegisterClientGoMetrics(registry *prometheus.Registry) {
	httpMetrics := &clientGoHttpMetrics{
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "skupper",
			Subsystem: "kubernetes_client",
			Name:      "http_request_duration_seconds",
			Help:      "Latency of kubernetes client requests in seconds by endpoint.",
		}, []string{"endpoint"}),
		results: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "skupper",
			Subsystem: "kubernetes_client",
			Name:      "http_requests_total",
			Help:      "Total number of kuberentes client requests by status code.",
		}, []string{"status_code"}),
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "skupper",
			Subsystem: "kubernetes_client",
			Name:      "http_retries_total",
			Help:      "Total number of kuberentes client requests retried by status code.",
		}, []string{"status_code"}),
	}
	rateLimiterMetrics := &clientGoRateLimiterMetrics{
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "skupper",
			Subsystem: "kubernetes_client",
			Name:      "rate_limiter_duration_seconds",
			Help:      "Latency of kubernetes client side rate limiting in seconds by endpoint.",
		}, []string{"endpoint"}),
	}

	registry.MustRegister(httpMetrics.latency, httpMetrics.results, httpMetrics.retries, rateLimiterMetrics.latency)
	metrics.Register(metrics.RegisterOpts{
		RequestLatency: httpMetrics,
		RequestResult:  httpMetrics,
		RequestRetry:   httpMetrics,

		RateLimiterLatency: rateLimiterMetrics,
	})
}

var (
	_ metrics.LatencyMetric = (*clientGoHttpMetrics)(nil)
	_ metrics.ResultMetric  = (*clientGoHttpMetrics)(nil)
	_ metrics.RetryMetric   = (*clientGoHttpMetrics)(nil)

	_ metrics.LatencyMetric = (*clientGoRateLimiterMetrics)(nil)
)

type clientGoHttpMetrics struct {
	latency *prometheus.HistogramVec
	results *prometheus.CounterVec
	retries *prometheus.CounterVec
}

func (m *clientGoHttpMetrics) Observe(ctx context.Context, _ string, url url.URL, latency time.Duration) {
	m.latency.WithLabelValues(url.EscapedPath()).Observe(latency.Seconds())
}

func (m *clientGoHttpMetrics) Increment(ctx context.Context, code string, _ string, _ string) {
	m.results.WithLabelValues(code).Inc()
}
func (m *clientGoHttpMetrics) IncrementRetry(ctx context.Context, code string, _ string, _ string) {
	m.retries.WithLabelValues(code).Inc()
}

type clientGoRateLimiterMetrics struct {
	latency *prometheus.HistogramVec
}

func (m *clientGoRateLimiterMetrics) Observe(ctx context.Context, _ string, url url.URL, latency time.Duration) {
	m.latency.WithLabelValues(url.EscapedPath()).Observe(latency.Seconds())
}
