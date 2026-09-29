package reconcile

import (
	"context"
	"errors"
	"fmt"
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

type failingPublishPlanner struct{}

func (failingPublishPlanner) Plan(Snapshot, DesiredNamespace) Plan {
	return Plan{Operations: []Operation{{ID: "publish/skupper-router", Kind: "PublishRouterIntent", Run: func(context.Context) error { return errors.New("prerequisite failed") }}}}
}

type recordingStatusWriter struct {
	calls int
	run   func()
}

func (w *recordingStatusWriter) ApplyStatuses(context.Context, NamespaceIdentity, StatusProjection) error {
	w.calls++
	if w.run != nil {
		w.run()
	}
	return nil
}

type fixedPlanner struct{ plan Plan }

func (p fixedPlanner) Plan(Snapshot, DesiredNamespace) Plan { return p.plan }

func TestStatusProjectionFollowsEffectsEvenOnFailure(t *testing.T) {
	for _, effectError := range []error{nil, errors.New("foreign prerequisite"), SupersededError{Reason: "external edit"}} {
		t.Run(fmt.Sprint(effectError), func(t *testing.T) {
			var ran []string
			// A public Site status write increments the resourceVersion used by
			// every effect's verifySite check. It must not supersede its own plan.
			resourceVersion := "collected"
			writer := &recordingStatusWriter{run: func() {
				resourceVersion = "status-written"
				ran = append(ran, "status")
			}}
			planner := fixedPlanner{Plan{Operations: []Operation{
				{ID: "z-prerequisite", Run: func(context.Context) error {
					if resourceVersion != "collected" {
						t.Error("status write superseded the plan before its effects ran")
					}
					ran = append(ran, "prerequisite")
					return effectError
				}},
				{ID: "a-dependent", Dependencies: []OperationID{"z-prerequisite"}, Run: func(context.Context) error {
					ran = append(ran, "dependent")
					return nil
				}},
			}}}
			desired := DesiredNamespace{Statuses: StatusProjection{Sites: []*skupperv2alpha1.Site{{ObjectMeta: metav1.ObjectMeta{Name: "site"}}}}}
			report := (Executor{}).Execute(context.Background(), (StatusPlanner{Next: planner, Writer: writer}).Plan(Snapshot{}, desired))
			want := []string{"prerequisite", "dependent", "status"}
			if effectError != nil {
				want = []string{"prerequisite", "status"}
			}
			if !reflect.DeepEqual(ran, want) || writer.calls != 1 || report.Results[len(report.Results)-1].ID != "apply-public-status" {
				t.Fatalf("status must follow all completed/skipped effects without requiring their success: ran=%v report=%#v", ran, report)
			}
		})
	}
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

func TestStatusProjectionExecutesDespitePublicationFailure(t *testing.T) {
	writer := &recordingStatusWriter{}
	desired := DesiredNamespace{Statuses: StatusProjection{Sites: []*skupperv2alpha1.Site{{ObjectMeta: metav1.ObjectMeta{Name: "site"}}}}}
	plan := (StatusPlanner{Next: failingPublishPlanner{}, Writer: writer}).Plan(Snapshot{Namespace: NamespaceIdentity{Name: "site", UID: "namespace-uid"}}, desired)
	report := (Executor{}).Execute(context.Background(), plan)
	if writer.calls != 1 {
		t.Fatalf("effect failure suppressed public status: report=%#v", report)
	}
	states := map[OperationID]ResultState{}
	for _, result := range report.Results {
		states[result.ID] = result.State
	}
	if states["apply-public-status"] != Succeeded || states["publish/skupper-router"] != Failed {
		t.Fatalf("unexpected independent effect states: %#v", states)
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
