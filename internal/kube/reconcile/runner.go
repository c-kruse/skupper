package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"k8s.io/client-go/util/workqueue"
)

type NamespaceReconciler struct {
	Collector Collector
	Deriver   Deriver
	Planner   Planner
	Executor  PlanExecutor
}

func (r NamespaceReconciler) Reconcile(ctx context.Context, namespace string) (ExecutionReport, error) {
	if err := ctx.Err(); err != nil {
		return ExecutionReport{}, err
	}
	snapshot, err := r.Collector.Collect(ctx, namespace)
	if err != nil {
		return ExecutionReport{}, fmt.Errorf("collect namespace %q: %w", namespace, err)
	}
	desired := r.Deriver.Derive(snapshot)
	plan := r.Planner.Plan(snapshot, desired)
	return r.Executor.Execute(ctx, plan), nil
}

type Queue struct {
	queue      workqueue.RateLimitingInterface
	reconciler NamespaceReconciler
	metrics    Metrics
	workers    int
	wg         sync.WaitGroup
}

type Metrics interface {
	workqueue.MetricsProvider
	Invalidated(source string)
	ReconcileStarted()
	ReconcileFinished(outcome string)
	SetLeader(active bool)
}

type NoopMetrics struct{}

func (NoopMetrics) NewDepthMetric(string) workqueue.GaugeMetric            { return noopMetric{} }
func (NoopMetrics) NewAddsMetric(string) workqueue.CounterMetric           { return noopMetric{} }
func (NoopMetrics) NewLatencyMetric(string) workqueue.HistogramMetric      { return noopMetric{} }
func (NoopMetrics) NewWorkDurationMetric(string) workqueue.HistogramMetric { return noopMetric{} }
func (NoopMetrics) NewUnfinishedWorkSecondsMetric(string) workqueue.SettableGaugeMetric {
	return noopMetric{}
}
func (NoopMetrics) NewLongestRunningProcessorSecondsMetric(string) workqueue.SettableGaugeMetric {
	return noopMetric{}
}
func (NoopMetrics) NewRetriesMetric(string) workqueue.CounterMetric { return noopMetric{} }
func (NoopMetrics) Invalidated(string)                              {}
func (NoopMetrics) ReconcileStarted()                               {}
func (NoopMetrics) ReconcileFinished(string)                        {}
func (NoopMetrics) SetLeader(bool)                                  {}

type noopMetric struct{}

func (noopMetric) Inc()            {}
func (noopMetric) Dec()            {}
func (noopMetric) Set(float64)     {}
func (noopMetric) Observe(float64) {}

func NewQueue(name string, workers int, reconciler NamespaceReconciler, metrics Metrics) *Queue {
	if workers < 1 {
		workers = 1
	}
	if metrics == nil {
		metrics = NoopMetrics{}
	}
	queue := workqueue.NewRateLimitingQueueWithConfig(workqueue.DefaultControllerRateLimiter(), workqueue.RateLimitingQueueConfig{Name: name, MetricsProvider: metrics})
	return &Queue{queue: queue, workers: workers, reconciler: reconciler, metrics: metrics}
}

// Add records an external invalidation request. The workqueue's additions
// metric records only additions accepted after key deduplication and can also
// include internal retries, so the two counters are intentionally distinct.
func (q *Queue) Add(namespace, source string) {
	if namespace != "" {
		q.metrics.Invalidated(source)
		q.queue.Add(namespace)
	}
}

// RunLeader starts effectful namespace workers and returns after cancellation.
// Informers may be started and synchronized before this method on standbys.
func (q *Queue) RunLeader(ctx context.Context) {
	for i := 0; i < q.workers; i++ {
		q.wg.Add(1)
		go q.worker(ctx)
	}
	<-ctx.Done()
	q.queue.ShutDown()
	q.wg.Wait()
}

func (q *Queue) worker(ctx context.Context) {
	defer q.wg.Done()
	for q.process(ctx) {
	}
}

func (q *Queue) process(ctx context.Context) bool {
	item, shutdown := q.queue.Get()
	if shutdown {
		return false
	}
	defer q.queue.Done(item)
	namespace, ok := item.(string)
	if !ok {
		q.queue.Forget(item)
		return true
	}
	q.metrics.ReconcileStarted()
	report, err := q.reconciler.Reconcile(ctx, namespace)
	if ctx.Err() != nil {
		q.metrics.ReconcileFinished("cancelled")
		q.queue.Forget(item)
		return true
	}
	// Time-dependent inputs have an absolute deadline captured in the plan.
	// Execution time and unrelated failure backoff must not postpone it.
	if !report.NextReevaluation.IsZero() {
		q.queue.AddAfter(namespace, time.Until(report.NextReevaluation))
	}
	if err != nil || report.NeedsRetry() {
		if err != nil {
			q.metrics.ReconcileFinished("error")
		} else {
			q.metrics.ReconcileFinished("retry")
		}
		if err != nil {
			slog.Error("Namespace reconciliation failed", "namespace", namespace, "error", err)
		} else {
			for i, result := range report.Results {
				if i >= 10 {
					break
				}
				if result.State != Succeeded {
					slog.Warn("Namespace reconciliation operation will retry", "namespace", namespace, "operation", result.ID, "state", result.State, "error", result.Error)
				}
			}
		}
		if report.RetryAfter > 0 {
			q.queue.Forget(item)
			q.queue.AddAfter(namespace, report.RetryAfter)
		} else {
			q.queue.AddRateLimited(namespace)
		}
		return true
	}
	q.metrics.ReconcileFinished("success")
	q.queue.Forget(item)
	return true
}
