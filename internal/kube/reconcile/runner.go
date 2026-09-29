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
	workers    int
	wg         sync.WaitGroup
}

func NewQueue(name string, workers int, reconciler NamespaceReconciler) *Queue {
	if workers < 1 {
		workers = 1
	}
	return &Queue{queue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), name), workers: workers, reconciler: reconciler}
}

func (q *Queue) Add(namespace string) {
	if namespace != "" {
		q.queue.Add(namespace)
	}
}

func (q *Queue) AddAfter(namespace string, delay time.Duration) {
	if namespace != "" {
		q.queue.AddAfter(namespace, delay)
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
	report, err := q.reconciler.Reconcile(ctx, namespace)
	if ctx.Err() != nil {
		q.queue.Forget(item)
		return true
	}
	if err != nil || report.NeedsRetry() {
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
	q.queue.Forget(item)
	return true
}
