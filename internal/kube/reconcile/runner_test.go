package reconcile

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/client-go/util/workqueue"
)

type runnerCollector struct{}

func (runnerCollector) Collect(context.Context, string) (Snapshot, error) { return Snapshot{}, nil }

type runnerDeriver struct{}

func (runnerDeriver) Derive(Snapshot) DesiredNamespace { return DesiredNamespace{} }

type runnerPlanner struct {
	after time.Duration
	fail  bool
}

func (p runnerPlanner) Plan(Snapshot, DesiredNamespace) Plan {
	plan := Plan{NextReevaluation: time.Now().Add(p.after)}
	if p.fail {
		plan.Operations = []Operation{{ID: "fail", Run: func(context.Context) error { return errors.New("failed") }}}
	}
	return plan
}

func TestQueueSchedulesSuccessfulReevaluationAndDoesNotDelayErrorsUntilDeadline(t *testing.T) {
	for _, test := range []struct {
		name    string
		planner runnerPlanner
		waitFor time.Duration
		backoff time.Duration
	}{
		{name: "successful deadline", planner: runnerPlanner{after: 20 * time.Millisecond}, waitFor: time.Second},
		{name: "error retry independent of deadline", planner: runnerPlanner{after: time.Hour, fail: true}, waitFor: time.Second},
		{name: "deadline independent of error backoff", planner: runnerPlanner{after: 20 * time.Millisecond, fail: true}, waitFor: time.Second, backoff: time.Hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			queue := NewQueue("deadline-test", 1, NamespaceReconciler{Collector: runnerCollector{}, Deriver: runnerDeriver{}, Planner: test.planner, Executor: Executor{}}, nil)
			if test.backoff > 0 {
				queue.queue.ShutDown()
				queue.queue = workqueue.NewRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(test.backoff, test.backoff))
			}
			defer queue.queue.ShutDown()
			queue.queue.Add("site")
			if !queue.process(context.Background()) {
				t.Fatal("queue stopped while processing deadline")
			}
			deadline := time.Now().Add(test.waitFor)
			for queue.queue.Len() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if queue.queue.Len() != 1 {
				t.Fatalf("namespace was not requeued within %s", test.waitFor)
			}
		})
	}
}

func TestQueueDoesNotPostponeDeadlinePassedDuringExecution(t *testing.T) {
	deadline := time.Now().Add(-time.Second)
	planner := fixedPlanner{plan: Plan{NextReevaluation: deadline}}
	queue := NewQueue("elapsed-deadline", 1, NamespaceReconciler{Collector: runnerCollector{}, Deriver: runnerDeriver{}, Planner: planner, Executor: Executor{}}, nil)
	defer queue.queue.ShutDown()
	queue.queue.Add("site")
	queue.process(context.Background())
	if queue.queue.Len() != 1 {
		t.Fatal("expired absolute deadline did not immediately requeue")
	}
}
