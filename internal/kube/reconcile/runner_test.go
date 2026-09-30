package reconcile

import (
	"context"
	"errors"
	"testing"
	"time"
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
	plan := Plan{NextReevaluation: p.after}
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
	}{
		{name: "successful deadline", planner: runnerPlanner{after: 20 * time.Millisecond}, waitFor: time.Second},
		{name: "error retry independent of deadline", planner: runnerPlanner{after: time.Hour, fail: true}, waitFor: time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			queue := NewQueue("deadline-test", 1, NamespaceReconciler{Collector: runnerCollector{}, Deriver: runnerDeriver{}, Planner: test.planner, Executor: Executor{}}, nil)
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
