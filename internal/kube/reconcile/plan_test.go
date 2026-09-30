package reconcile

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/skupperproject/skupper/internal/kube/resource"
	"github.com/skupperproject/skupper/internal/routercontrol"
	appsv1 "k8s.io/api/apps/v1"
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
func (*workloadOrderPublisher) PublishedIntents(string) map[routercontrol.TargetIdentity]routercontrol.Publication {
	return nil
}

type workloadOrderEnsurer struct {
	events              *[]string
	publisher           *workloadOrderPublisher
	prerequisiteError   error
	caError             error
	observedConnections uint32
}

func (e *workloadOrderEnsurer) EnsureRouterPrerequisites(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, *corev1.ServiceAccount, *rbacv1.Role, *rbacv1.RoleBinding, *corev1.ServiceAccount, *rbacv1.Role, *rbacv1.RoleBinding) error {
	*e.events = append(*e.events, "prerequisites")
	return e.prerequisiteError
}

func (e *workloadOrderEnsurer) EnsureRouterControlCA(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, *corev1.ConfigMap, *corev1.ConfigMap) error {
	*e.events = append(*e.events, "trust")
	return e.caError
}

func (e *workloadOrderEnsurer) EnsureDeployment(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, *appsv1.Deployment, *appsv1.Deployment) error {
	*e.events = append(*e.events, "workload")
	if len(e.publisher.published) > 0 {
		e.observedConnections = e.publisher.published[len(e.publisher.published)-1].Settings.DataConnectionCount
	}
	return nil
}

func (*workloadOrderEnsurer) EnsureLocalService(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, *corev1.Service, *corev1.Service) error {
	return nil
}

func (*workloadOrderEnsurer) RetireDeployment(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, *appsv1.Deployment) error {
	return nil
}

func (*workloadOrderEnsurer) EnsureListenerService(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, *corev1.Service, *corev1.Service) error {
	return nil
}

func (e *workloadOrderEnsurer) RetireListenerService(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, *corev1.Service) error {
	*e.events = append(*e.events, "retire-listeners")
	return nil
}

func addWorkloadDesired(desired *DesiredNamespace) {
	controller, block := true, true
	owner := []metav1.OwnerReference{{APIVersion: skupperv2alpha1.SchemeGroupVersion.String(), Kind: "Site", Name: desired.Site.Name, UID: desired.Site.UID, Controller: &controller, BlockOwnerDeletion: &block}}
	desired.ServiceAccount = &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "skupper-router", Namespace: desired.Namespace.Name, OwnerReferences: owner}}
	desired.Role = &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "skupper-router", Namespace: desired.Namespace.Name, OwnerReferences: owner}}
	desired.RoleBinding = &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "skupper-router", Namespace: desired.Namespace.Name, OwnerReferences: owner}}
	desired.RouterControlCA = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "skupper-controller-ca", Namespace: desired.Namespace.Name, OwnerReferences: owner}, Data: map[string]string{"ca.crt": "CA"}}
	desired.WorkloadsKnown = true
	desired.Deployments = []*appsv1.Deployment{{ObjectMeta: metav1.ObjectMeta{Name: "skupper-router", Namespace: desired.Namespace.Name, OwnerReferences: owner}}}
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
	addWorkloadDesired(&desired)
	plan := (WorkloadPlanner{Next: emptyPlanner{}}).Plan(Snapshot{Namespace: desired.Namespace, Allocations: copyAllocations(desired.Allocations)}, desired)
	dependencies := map[OperationID][]OperationID{}
	for _, operation := range plan.Operations {
		dependencies[operation.ID] = operation.Dependencies
	}
	if !reflect.DeepEqual(dependencies["ensure-router-control-ca"], []OperationID{"ensure-router-prerequisites"}) {
		t.Fatalf("CA does not wait for router prerequisites: %#v", dependencies)
	}
	if !reflect.DeepEqual(dependencies["ensure-deployment/skupper-router"], []OperationID{"ensure-router-control-ca"}) {
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
	addWorkloadDesired(&desired)
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
	if want := []string{"prerequisites", "trust", "validate", "publish", "workload"}; !reflect.DeepEqual(events, want) {
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
			addWorkloadDesired(&desired)
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
	if len(dependencies["publish/skupper-router"]) != 0 {
		t.Fatalf("publication is coupled to listener Service effects: %#v", dependencies["publish/skupper-router"])
	}
	if !reflect.DeepEqual(dependencies["ensure-listener-service/one"], []OperationID{"publish/skupper-router"}) || !reflect.DeepEqual(dependencies["ensure-listener-service/two"], []OperationID{"publish/skupper-router"}) {
		t.Fatalf("listener Service operations are not independently planned: %#v", dependencies)
	}
}

func TestConvergedWorkloadPlanHasNoEffectsAndHostOnlyChangeIsIsolated(t *testing.T) {
	site := &skupperv2alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site", Namespace: "site", UID: "site-uid"}}
	desired := DesiredNamespace{Namespace: NamespaceIdentity{Name: "site", UID: "namespace-uid"}, SiteUID: site.UID, Site: site}
	addWorkloadDesired(&desired)
	desired.Deployments[0].TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}
	replicas := int32(1)
	desired.Deployments[0].Spec.Replicas = &replicas
	desired.LocalService = &corev1.Service{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}, ObjectMeta: metav1.ObjectMeta{Name: "skupper-router-local", Namespace: "site", OwnerReferences: append([]metav1.OwnerReference(nil), desired.Deployments[0].OwnerReferences...)}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP}}
	listener := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "site", Labels: map[string]string{"internal.skupper.io/listener": "true"}, Annotations: map[string]string{"internal.skupper.io/controlled": "true"}, OwnerReferences: append([]metav1.OwnerReference(nil), desired.Deployments[0].OwnerReferences...)}}
	desired.ListenerServices = []*corev1.Service{listener}

	deployment := desired.Deployments[0].DeepCopy()
	deployment.UID, deployment.ResourceVersion = "deployment-uid", "7"
	deployment.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "skupper-controller", Operation: metav1.ManagedFieldsOperationApply, APIVersion: "apps/v1", FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:metadata":{"f:ownerReferences":{".":{},"k:{\"uid\":\"site-uid\"}":{}}},"f:spec":{"f:replicas":{}}}`)}}}
	local := desired.LocalService.DeepCopy()
	local.UID, local.ResourceVersion = "service-uid", "8"
	local.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "skupper-controller", Operation: metav1.ManagedFieldsOperationApply, APIVersion: "v1", FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:metadata":{"f:ownerReferences":{".":{},"k:{\"uid\":\"site-uid\"}":{}}},"f:spec":{"f:type":{}}}`)}}}
	serviceAccount := desired.ServiceAccount.DeepCopy()
	role := desired.Role.DeepCopy()
	binding := desired.RoleBinding.DeepCopy()
	ca := desired.RouterControlCA.DeepCopy()
	for index, object := range []metav1.Object{serviceAccount, role, binding, ca, listener} {
		object.SetUID(types.UID(fmt.Sprintf("uid-%d", index)))
		object.SetResourceVersion(fmt.Sprintf("%d", index+1))
	}
	snapshot := Snapshot{Namespace: desired.Namespace, Sites: []*skupperv2alpha1.Site{site.DeepCopy()}, Deployments: []*appsv1.Deployment{deployment}, Services: []*corev1.Service{local, listener.DeepCopy()}, ServiceAccounts: []*corev1.ServiceAccount{serviceAccount}, Roles: []*rbacv1.Role{role}, RoleBindings: []*rbacv1.RoleBinding{binding}, RouterControlCA: ca}
	if !resource.DeploymentApplyEqual(deployment, desired.Deployments[0]) || !resource.ServiceApplyEqual(local, desired.LocalService) {
		t.Fatalf("test fixture is not semantically converged: deployment=%v service=%v", resource.DeploymentApplyEqual(deployment, desired.Deployments[0]), resource.ServiceApplyEqual(local, desired.LocalService))
	}

	if operations := (WorkloadPlanner{Next: emptyPlanner{}}).Plan(snapshot, desired).Operations; len(operations) != 0 {
		t.Fatalf("converged workload plan emitted API effects: %#v", operationKinds(operations))
	}
	hostChange := fixedPlanner{plan: Plan{Operations: []Operation{{ID: "publish/skupper-router", Kind: "PublishRouterIntent", Run: func(context.Context) error { return nil }}}}}
	operations := (WorkloadPlanner{Next: hostChange}).Plan(snapshot, desired).Operations
	if len(operations) != 1 || operations[0].Kind != "PublishRouterIntent" || len(operations[0].Dependencies) != 0 {
		t.Fatalf("host-only intent change scheduled unrelated workload effects: %#v", operationKinds(operations))
	}

	drift := snapshot
	driftedRole := role.DeepCopy()
	driftedRole.Rules = []rbacv1.PolicyRule{{Verbs: []string{"list"}}}
	drift.Roles = []*rbacv1.Role{driftedRole}
	assertOperationIDs(t, (WorkloadPlanner{Next: emptyPlanner{}}).Plan(drift, desired).Operations, "ensure-router-prerequisites")
	drift = snapshot
	drift.RouterControlCA = ca.DeepCopy()
	drift.RouterControlCA.Data["ca.crt"] = "external"
	assertOperationIDs(t, (WorkloadPlanner{Next: emptyPlanner{}}).Plan(drift, desired).Operations, "ensure-router-control-ca")
	drift = snapshot
	driftedDeployment := deployment.DeepCopy()
	driftedReplicas := int32(2)
	driftedDeployment.Spec.Replicas = &driftedReplicas
	drift.Deployments = []*appsv1.Deployment{driftedDeployment}
	assertOperationIDs(t, (WorkloadPlanner{Next: emptyPlanner{}}).Plan(drift, desired).Operations, "ensure-deployment/skupper-router")
	drift = snapshot
	driftedLocal := local.DeepCopy()
	driftedLocal.Spec.Type = corev1.ServiceTypeNodePort
	drift.Services = []*corev1.Service{driftedLocal, listener.DeepCopy()}
	assertOperationIDs(t, (WorkloadPlanner{Next: emptyPlanner{}}).Plan(drift, desired).Operations, "ensure-local-service")
}

func operationKinds(operations []Operation) []string {
	result := make([]string, 0, len(operations))
	for _, operation := range operations {
		result = append(result, string(operation.ID)+":"+operation.Kind)
	}
	return result
}

func assertOperationIDs(t *testing.T, operations []Operation, expected ...OperationID) {
	t.Helper()
	actual := make([]OperationID, 0, len(operations))
	for _, operation := range operations {
		actual = append(actual, operation.ID)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("operations = %v, want %v", actual, expected)
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
