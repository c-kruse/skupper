package reconcile

import (
	"context"
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

type emptyPlanner struct{}

func (emptyPlanner) Plan(Snapshot, DesiredNamespace) Plan { return Plan{} }

type publishPlanner struct{}

func (publishPlanner) Plan(Snapshot, DesiredNamespace) Plan {
	return Plan{Operations: []Operation{{ID: "publish/skupper-router", Kind: "PublishRouterIntent", Run: func(context.Context) error { return nil }}}}
}

type recordingStatusWriter struct{ calls int }

func (w *recordingStatusWriter) ApplyStatuses(context.Context, NamespaceIdentity, StatusProjection) error {
	w.calls++
	return nil
}

func TestSiteLessStatusProjectionExecutes(t *testing.T) {
	writer := &recordingStatusWriter{}
	desired := DesiredNamespace{Statuses: StatusProjection{Certificates: []*skupperv2alpha1.Certificate{{ObjectMeta: metav1.ObjectMeta{Name: "standalone"}}}}}
	plan := (StatusPlanner{Next: emptyPlanner{}, Writer: writer}).Plan(Snapshot{Namespace: NamespaceIdentity{Name: "controller-ns", UID: "namespace-uid"}}, desired)
	report := (Executor{}).Execute(context.Background(), plan)
	if writer.calls != 1 || len(report.Results) != 1 || report.Results[0].State != Succeeded {
		t.Fatalf("Site-less status write did not execute: calls=%d report=%#v", writer.calls, report)
	}
}

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

func TestListenerServiceEffectsDoNotBlockPublication(t *testing.T) {
	site := &skupperv2alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site", Namespace: "site", UID: "site-uid"}}
	desired := DesiredNamespace{
		Namespace: NamespaceIdentity{Name: "site", UID: "namespace-uid"},
		SiteUID:   site.UID,
		Site:      site,
		ListenerServices: []*corev1.Service{
			{ObjectMeta: metav1.ObjectMeta{Name: "one"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "two"}},
		},
	}
	plan := (WorkloadPlanner{Next: publishPlanner{}}).Plan(Snapshot{}, desired)
	dependencies := map[OperationID][]OperationID{}
	for _, operation := range plan.Operations {
		dependencies[operation.ID] = operation.Dependencies
	}
	if !reflect.DeepEqual(dependencies["publish/skupper-router"], []OperationID{"ensure-site-workloads"}) {
		t.Fatalf("publication is coupled to listener Service effects: %#v", dependencies["publish/skupper-router"])
	}
	if !reflect.DeepEqual(dependencies["ensure-listener-service/one"], []OperationID{"ensure-site-workloads"}) || !reflect.DeepEqual(dependencies["ensure-listener-service/two"], []OperationID{"ensure-site-workloads"}) {
		t.Fatalf("listener Service operations are not independently planned: %#v", dependencies)
	}
}

func TestAccessEffectsDoNotBlockPublicationOrStatus(t *testing.T) {
	site := &skupperv2alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site", Namespace: "site", UID: "site-uid"}}
	desired := DesiredNamespace{Namespace: NamespaceIdentity{Name: "site", UID: "namespace-uid"}, SiteUID: site.UID, Site: site}
	plan := (AccessPlanner{Next: publishPlanner{}}).Plan(Snapshot{Assignment: Assignment{Controlled: true}}, desired)
	for _, operation := range plan.Operations {
		if operation.Kind == "PublishRouterIntent" && len(operation.Dependencies) != 0 {
			t.Fatalf("unrelated access effects block intent publication: %#v", operation.Dependencies)
		}
	}
}

func TestSupersededPlanIsRetried(t *testing.T) {
	report := (Executor{}).Execute(context.Background(), Plan{Operations: []Operation{{ID: "write", Run: func(context.Context) error { return SupersededError{Reason: "UID changed"} }}}})
	if !report.NeedsRetry() || report.Results[0].State != Superseded {
		t.Fatalf("superseded report was not retryable: %#v", report)
	}
}
