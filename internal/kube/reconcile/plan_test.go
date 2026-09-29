package reconcile

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/skupperproject/skupper/internal/routercontrol"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

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

type workloadOrderPublisher struct {
	events    *[]string
	published []routercontrol.RouterIntent
	err       error
}

func (p *workloadOrderPublisher) Publish(intent routercontrol.RouterIntent) (routercontrol.Digest, error) {
	*p.events = append(*p.events, "publish")
	if p.err != nil {
		return "", p.err
	}
	p.published = append(p.published, intent)
	return "digest", nil
}

func (*workloadOrderPublisher) SetUnavailable(routercontrol.TargetIdentity) {}

type workloadOrderEnsurer struct {
	events              *[]string
	publisher           *workloadOrderPublisher
	prerequisiteError   error
	caError             error
	observedConnections uint32
}

func (e *workloadOrderEnsurer) EnsureRouterPrerequisites(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, *corev1.ServiceAccount, *rbacv1.Role, *rbacv1.RoleBinding) error {
	*e.events = append(*e.events, "prerequisites")
	return e.prerequisiteError
}

func (e *workloadOrderEnsurer) EnsureRouterControlCA(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, RouterControlBootstrap) error {
	*e.events = append(*e.events, "trust")
	return e.caError
}

func (e *workloadOrderEnsurer) EnsureSite(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, []string, RouterControlBootstrap) error {
	*e.events = append(*e.events, "workload")
	if len(e.publisher.published) > 0 {
		e.observedConnections = e.publisher.published[len(e.publisher.published)-1].Settings.DataConnectionCount
	}
	return nil
}

func (*workloadOrderEnsurer) EnsureListenerService(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, *corev1.Service) error {
	return nil
}

func (e *workloadOrderEnsurer) RetireListenerServices(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, []string) error {
	*e.events = append(*e.events, "retire-listeners")
	return nil
}

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

func TestCurrentIntentIsPublishedBeforeWorkloadUpdate(t *testing.T) {
	events := []string{}
	target := routercontrol.TargetIdentity{NamespaceUID: "namespace-uid", SiteUID: "site-uid", RouterGroup: "skupper-router"}
	site := &skupperv2alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site", Namespace: "site", UID: "site-uid"}}
	oldIntent := routercontrol.RouterIntent{Target: target, Settings: routercontrol.RouterSettings{DataConnectionCount: 1}}
	newIntent := routercontrol.RouterIntent{Target: target, Settings: routercontrol.RouterSettings{DataConnectionCount: 2}}
	allocations := AllocationState{SiteUID: site.UID, Ports: map[string]int{}}
	desired := DesiredNamespace{Namespace: NamespaceIdentity{Name: "site", UID: "namespace-uid"}, SiteUID: site.UID, Site: site, Allocations: copyAllocations(allocations), Intents: map[RouterTarget]routercontrol.RouterIntent{target: newIntent}}
	publisher := &workloadOrderPublisher{events: &events, published: []routercontrol.RouterIntent{oldIntent}}
	ensurer := &workloadOrderEnsurer{events: &events, publisher: publisher}
	publication := PublicationPlanner{Allocations: &recordingCommitter{}, Publisher: publisher, Validator: func(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, map[string]types.UID) error {
		events = append(events, "validate")
		return nil
	}}
	plan := (WorkloadPlanner{Next: publication, Ensurer: ensurer}).Plan(Snapshot{Namespace: desired.Namespace, Allocations: allocations}, desired)
	report := (Executor{}).Execute(context.Background(), plan)
	if report.NeedsRetry() {
		t.Fatalf("ordered rollout unexpectedly failed: %#v", report)
	}
	if want := []string{"prerequisites", "trust", "validate", "publish", "workload", "retire-listeners"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("workload did not follow current intent publication: got %v, want %v", events, want)
	}
	if ensurer.observedConnections != 2 {
		t.Fatalf("replacement workload observed stale publisher state: data connections=%d", ensurer.observedConnections)
	}
}

func TestWorkloadAndPublicationRespectPreparationFailures(t *testing.T) {
	tests := []struct {
		name              string
		allocationError   error
		prerequisiteError error
		caError           error
		validationError   error
		publicationError  error
		wantEvents        []string
	}{
		{name: "allocation", allocationError: errors.New("allocation conflict"), wantEvents: []string{}},
		{name: "prerequisites", prerequisiteError: errors.New("foreign ServiceAccount"), wantEvents: []string{"prerequisites"}},
		{name: "trust", caError: errors.New("CA write failed"), wantEvents: []string{"prerequisites", "trust"}},
		{name: "validation", validationError: errors.New("Site superseded"), wantEvents: []string{"prerequisites", "trust", "validate"}},
		{name: "publication", publicationError: errors.New("publisher rejected intent"), wantEvents: []string{"prerequisites", "trust", "validate", "publish"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			events := []string{}
			target := routercontrol.TargetIdentity{NamespaceUID: "namespace-uid", SiteUID: "site-uid", RouterGroup: "skupper-router"}
			site := &skupperv2alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site", Namespace: "site", UID: "site-uid"}}
			allocations := AllocationState{SiteUID: site.UID, Ports: map[string]int{"listener": 1024}}
			snapshotAllocations := copyAllocations(allocations)
			if test.allocationError != nil {
				snapshotAllocations = AllocationState{Ports: map[string]int{}}
			}
			desired := DesiredNamespace{
				Namespace:   NamespaceIdentity{Name: "site", UID: "namespace-uid"},
				SiteUID:     site.UID,
				Site:        site,
				Allocations: copyAllocations(allocations),
				Intents:     map[RouterTarget]routercontrol.RouterIntent{target: {Target: target}},
				Statuses:    StatusProjection{Sites: []*skupperv2alpha1.Site{site.DeepCopy()}},
			}
			publisher := &workloadOrderPublisher{events: &events, err: test.publicationError}
			ensurer := &workloadOrderEnsurer{events: &events, publisher: publisher, prerequisiteError: test.prerequisiteError, caError: test.caError}
			publication := PublicationPlanner{Allocations: &recordingCommitter{err: test.allocationError}, Publisher: publisher, Validator: func(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, map[string]types.UID) error {
				events = append(events, "validate")
				return test.validationError
			}}
			writer := &recordingStatusWriter{}
			plan := (StatusPlanner{Next: WorkloadPlanner{Next: publication, Ensurer: ensurer}, Writer: writer}).Plan(Snapshot{Namespace: desired.Namespace, Allocations: snapshotAllocations}, desired)
			report := (Executor{}).Execute(context.Background(), plan)
			if !report.NeedsRetry() {
				t.Fatalf("failed preparation/publication was not retryable: %#v", report)
			}
			if !reflect.DeepEqual(events, test.wantEvents) {
				t.Fatalf("effects crossed failed boundary: got %v, want %v", events, test.wantEvents)
			}
			if ensurer.observedConnections != 0 {
				t.Fatalf("workload ran despite %s failure", test.name)
			}
			if writer.calls != 1 {
				t.Fatalf("%s failure suppressed status projection", test.name)
			}
		})
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
	if !reflect.DeepEqual(dependencies["publish/skupper-router"], []OperationID{"ensure-router-control-ca"}) {
		t.Fatalf("publication is coupled to listener Service effects: %#v", dependencies["publish/skupper-router"])
	}
	if !reflect.DeepEqual(dependencies["ensure-site-workloads"], []OperationID{"publish/skupper-router"}) {
		t.Fatalf("workload does not wait for intent publication: %#v", dependencies["ensure-site-workloads"])
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
