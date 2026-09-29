package reconcile

import (
	"context"
	"errors"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

type emptyPlanner struct{}

func (emptyPlanner) Plan(Snapshot, DesiredNamespace) Plan { return Plan{} }

func TestExecutorRetainsIndependentSuccessAndBlocksDependents(t *testing.T) {
	var ran []string
	plan := Plan{Operations: []Operation{
		{ID: "publish", Dependencies: []OperationID{"allocation"}, Run: func(context.Context) error { ran = append(ran, "publish"); return nil }},
		{ID: "status", Run: func(context.Context) error { ran = append(ran, "status"); return nil }},
		{ID: "allocation", Run: func(context.Context) error {
			ran = append(ran, "allocation")
			return Ambiguous(errors.New("timeout after write"))
		}},
	}}
	report := (Executor{}).Execute(context.Background(), plan)
	if !reflect.DeepEqual(ran, []string{"allocation", "status"}) {
		t.Fatalf("unexpected operations ran: %v", ran)
	}
	states := map[OperationID]ResultState{}
	for _, result := range report.Results {
		states[result.ID] = result.State
	}
	if states["allocation"] != UnknownOutcome || states["publish"] != SkippedDependency || states["status"] != Succeeded {
		t.Fatalf("unexpected result states: %#v", states)
	}
}

func TestRouterPrerequisitesPrecedeCAAndWorkload(t *testing.T) {
	site := &skupperv2alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site", Namespace: "site", UID: "site-uid"}}
	desired := DesiredNamespace{Namespace: NamespaceIdentity{Name: "site", UID: "namespace-uid"}, SiteUID: site.UID, Site: site, Allocations: AllocationState{SiteUID: site.UID, Ports: map[string]int{}}}
	plan := (WorkloadPlanner{Next: emptyPlanner{}}).Plan(Snapshot{Namespace: desired.Namespace, Allocations: copyAllocations(desired.Allocations)}, desired)
	dependencies := map[OperationID][]OperationID{}
	for _, operation := range plan.Operations {
		dependencies[operation.ID] = operation.Dependencies
	}
	if !reflect.DeepEqual(dependencies["ensure-router-control-ca"], []OperationID{"ensure-router-prerequisites"}) {
		t.Fatalf("CA does not wait for router prerequisites: %#v", dependencies)
	}
	if !reflect.DeepEqual(dependencies["ensure-site-workloads"], []OperationID{"ensure-router-control-ca"}) {
		t.Fatalf("workload does not wait for CA prerequisite: %#v", dependencies)
	}
}

func TestSupersededPlanIsRetried(t *testing.T) {
	report := (Executor{}).Execute(context.Background(), Plan{Operations: []Operation{{ID: "write", Run: func(context.Context) error { return SupersededError{Reason: "UID changed"} }}}})
	if !report.NeedsRetry() || report.Results[0].State != Superseded {
		t.Fatalf("superseded report was not retryable: %#v", report)
	}
}
