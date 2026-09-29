package metrics

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/skupperproject/skupper/internal/routercontrol"
)

func TestNamespaceReconcileMetricSchemaAndSemantics(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	metrics := MustRegisterNamespaceReconcileMetrics(registry)
	metrics.Invalidated("informer")
	metrics.Invalidated("informer")
	metrics.NewAddsMetric("ignored").Inc()
	metrics.ReconcileStarted()
	metrics.ReconcileFinished("success")
	metrics.SetLeader(true)

	expected := `
# HELP skupper_controller_leader Whether this controller Pod is currently inside the fenced serving lifecycle (1 leader, 0 standby).
# TYPE skupper_controller_leader gauge
skupper_controller_leader 1
# HELP skupper_namespace_reconcile_invalidations_total External namespace invalidation requests before workqueue key deduplication.
# TYPE skupper_namespace_reconcile_invalidations_total counter
skupper_namespace_reconcile_invalidations_total{source="informer"} 2
skupper_namespace_reconcile_invalidations_total{source="observation"} 0
skupper_namespace_reconcile_invalidations_total{source="startup"} 0
# HELP skupper_namespace_reconcile_outcomes_total Completed namespace reconciliation attempts by bounded outcome.
# TYPE skupper_namespace_reconcile_outcomes_total counter
skupper_namespace_reconcile_outcomes_total{outcome="cancelled"} 0
skupper_namespace_reconcile_outcomes_total{outcome="error"} 0
skupper_namespace_reconcile_outcomes_total{outcome="retry"} 0
skupper_namespace_reconcile_outcomes_total{outcome="success"} 1
# HELP skupper_namespace_reconcile_queue_adds_total Namespace keys accepted by the workqueue after deduplication, including internal retry additions.
# TYPE skupper_namespace_reconcile_queue_adds_total counter
skupper_namespace_reconcile_queue_adds_total 1
# HELP skupper_namespace_reconcile_workers_active Number of workers currently executing a namespace reconciliation attempt.
# TYPE skupper_namespace_reconcile_workers_active gauge
skupper_namespace_reconcile_workers_active 0
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(expected),
		"skupper_controller_leader", "skupper_namespace_reconcile_invalidations_total", "skupper_namespace_reconcile_outcomes_total", "skupper_namespace_reconcile_queue_adds_total", "skupper_namespace_reconcile_workers_active"); err != nil {
		t.Fatal(err)
	}
}

func TestAdaptorMetricStateIsCurrentSessionAndBounded(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	metrics := MustRegisterAdaptorMetrics(registry)
	metrics.ConnectionAttempt("success", time.Second)
	metrics.IntentAccepted()
	metrics.SetControlStreamUp(true)
	metrics.SetApplicationState("pending")
	metrics.RealizationFinished(routercontrol.ApplicationApplied, 2*time.Second)
	metrics.SetApplicationState("applied")
	metrics.AcceptedToApplied(3 * time.Second)
	metrics.SetControlStreamUp(false)
	metrics.SetApplicationState("unknown")

	expected := `
# HELP skupper_adaptor_application_state Latest accepted intent state for the current control session; one state is 1 and the others 0.
# TYPE skupper_adaptor_application_state gauge
skupper_adaptor_application_state{state="applied"} 0
skupper_adaptor_application_state{state="failed"} 0
skupper_adaptor_application_state{state="pending"} 0
skupper_adaptor_application_state{state="unknown"} 1
# HELP skupper_adaptor_connection_attempts_total Router-control connection attempts, including the initial attempt, by outcome.
# TYPE skupper_adaptor_connection_attempts_total counter
skupper_adaptor_connection_attempts_total{outcome="error"} 0
skupper_adaptor_connection_attempts_total{outcome="success"} 1
# HELP skupper_adaptor_control_stream_up Whether the current router-control stream is connected (1) or absent (0).
# TYPE skupper_adaptor_control_stream_up gauge
skupper_adaptor_control_stream_up 0
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(expected), "skupper_adaptor_application_state", "skupper_adaptor_connection_attempts_total", "skupper_adaptor_control_stream_up"); err != nil {
		t.Fatal(err)
	}
}

func TestLatencyHistogramsUseFactorTwoBuckets(t *testing.T) {
	controllerRegistry := prometheus.NewPedanticRegistry()
	controller := MustRegisterNamespaceReconcileMetrics(controllerRegistry)
	controller.NewLatencyMetric("ignored").Observe(8)
	controller.NewWorkDurationMetric("ignored").Observe(8)
	assertFactorTwoBuckets(t, controllerRegistry, "skupper_namespace_reconcile_queue_wait_duration_seconds", 0.001, 17)
	assertFactorTwoBuckets(t, controllerRegistry, "skupper_namespace_reconcile_work_duration_seconds", 0.001, 17)

	adaptorRegistry := prometheus.NewPedanticRegistry()
	adaptor := MustRegisterAdaptorMetrics(adaptorRegistry)
	adaptor.ConnectionAttempt("success", time.Second)
	adaptor.RealizationFinished(routercontrol.ApplicationApplied, time.Second)
	adaptor.AcceptedToApplied(time.Second)
	adaptor.LocalManagementFinished("verify", "success", time.Second)
	assertFactorTwoBuckets(t, adaptorRegistry, "skupper_adaptor_connection_attempt_duration_seconds", 0.01, 13)
	assertFactorTwoBuckets(t, adaptorRegistry, "skupper_adaptor_realization_duration_seconds", 0.001, 17)
	assertFactorTwoBuckets(t, adaptorRegistry, "skupper_adaptor_accepted_to_verified_applied_seconds", 0.001, 19)
	assertFactorTwoBuckets(t, adaptorRegistry, "skupper_adaptor_local_management_operation_duration_seconds", 0.001, 17)
}

func assertFactorTwoBuckets(t *testing.T, registry *prometheus.Registry, name string, first float64, count int) {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		buckets := family.Metric[0].Histogram.Bucket
		if len(buckets) != count {
			t.Fatalf("%s has %d finite buckets, want %d", name, len(buckets), count)
		}
		for i, bucket := range buckets {
			want := first * math.Pow(2, float64(i))
			if got := bucket.GetUpperBound(); got != want {
				t.Fatalf("%s bucket %d upper bound = %g, want %g", name, i, got, want)
			}
		}
		return
	}
	t.Fatalf("metric family %s not found", name)
}
