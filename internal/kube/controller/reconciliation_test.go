package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	fakeclient "github.com/skupperproject/skupper/internal/kube/client/fake"
	"github.com/skupperproject/skupper/internal/kube/reconcile"
	auth "github.com/skupperproject/skupper/internal/kube/routercontrol"
	"github.com/skupperproject/skupper/internal/kube/site/sizing"
	"github.com/skupperproject/skupper/internal/routercontrol"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	skupperfake "github.com/skupperproject/skupper/pkg/generated/client/clientset/versioned/fake"
)

type testIntentPublisher struct {
	published chan routercontrol.RouterIntent
	real      *routercontrol.Publisher
}

type noOperationsPlanner struct{}

type recordedAuthorizationInvalidator struct {
	mu     sync.Mutex
	events []string
	notify chan string
}

func (r *recordedAuthorizationInvalidator) InvalidateAuthorization(kind auth.AuthorizationKind, namespace, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	event := string(kind) + ":" + namespace + "/" + name
	r.events = append(r.events, event)
	if r.notify != nil {
		select {
		case r.notify <- event:
		default:
		}
	}
}

func (r *recordedAuthorizationInvalidator) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := append([]string(nil), r.events...)
	r.events = nil
	return result
}

func (noOperationsPlanner) Plan(reconcile.Snapshot, reconcile.DesiredNamespace) reconcile.Plan {
	return reconcile.Plan{}
}

func TestAuthorizationInvalidationsCoverChainAndIgnoreOperationalUpdates(t *testing.T) {
	controllerOwner := true
	deleted := metav1.NewTime(time.Now())
	recorder := &recordedAuthorizationInvalidator{}
	c := &NamespaceController{authorization: recorder}

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant", UID: "namespace-uid"}}
	changedNamespace := namespace.DeepCopy()
	changedNamespace.DeletionTimestamp = &deleted
	c.authorizationNamespaceChanged(namespace, changedNamespace)

	assignment := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "skupper", Namespace: "tenant", UID: "assignment-uid"}, Data: map[string]string{controllerSettingKey: "control/one"}}
	changedAssignment := assignment.DeepCopy()
	changedAssignment.Data[controllerSettingKey] = "control/two"
	c.authorizationConfigMapChanged(assignment, changedAssignment)

	allocation := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: allocationConfigMapName, Namespace: "tenant", UID: "allocation-uid"}, Data: map[string]string{"namespaceUID": "namespace-uid", "siteUID": "site-uid", "ports": "old"}}
	portsOnly := allocation.DeepCopy()
	portsOnly.Data["ports"] = "new"
	c.authorizationConfigMapChanged(allocation, portsOnly)
	changedAllocation := portsOnly.DeepCopy()
	changedAllocation.Data["siteUID"] = "replacement-site"
	c.authorizationConfigMapChanged(portsOnly, changedAllocation)

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "router-pod", Namespace: "tenant", UID: "pod-uid", Labels: map[string]string{"skupper.io/group": "skupper-router"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "router-rs", UID: "rs-uid", Controller: &controllerOwner}}}, Spec: corev1.PodSpec{ServiceAccountName: "router-sa"}}
	statusOnly := pod.DeepCopy()
	statusOnly.Status.Phase = corev1.PodRunning
	c.authorizationPodChanged(pod, statusOnly)
	changedPod := statusOnly.DeepCopy()
	changedPod.DeletionTimestamp = &deleted
	c.authorizationPodChanged(statusOnly, changedPod)

	serviceAccount := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "router-sa", Namespace: "tenant", UID: "sa-uid"}}
	changedServiceAccount := serviceAccount.DeepCopy()
	changedServiceAccount.UID = "replacement-sa"
	c.authorizationServiceAccountChanged(serviceAccount, changedServiceAccount)

	replicaSet := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "router-rs", Namespace: "tenant", UID: "rs-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "skupper-router", UID: "deployment-uid", Controller: &controllerOwner}}}}
	changedReplicaSet := replicaSet.DeepCopy()
	changedReplicaSet.DeletionTimestamp = &deleted
	c.authorizationReplicaSetChanged(replicaSet, changedReplicaSet)

	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "skupper-router", Namespace: "tenant", UID: "deployment-uid"}, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"skupper.io/group": "skupper-router"}}}}}
	changedDeployment := deployment.DeepCopy()
	changedDeployment.UID = "replacement-deployment"
	c.authorizationDeploymentChanged(deployment, changedDeployment)

	site := &skupperv2alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "west", Namespace: "tenant", UID: "site-uid"}}
	changedSite := site.DeepCopy()
	changedSite.Spec.HA = true
	c.authorizationSiteChanged(site, changedSite)

	want := []string{
		string(AuthorizationNamespace) + ":tenant/tenant",
		string(AuthorizationAssignment) + ":tenant/skupper",
		string(AuthorizationAllocation) + ":tenant/" + allocationConfigMapName,
		string(AuthorizationPod) + ":tenant/router-pod",
		string(AuthorizationServiceAccount) + ":tenant/router-sa",
		string(AuthorizationRouterGroup) + ":tenant/skupper-router",
		string(AuthorizationRouterGroup) + ":tenant/skupper-router",
		string(AuthorizationSite) + ":tenant/west",
	}
	if got := recorder.take(); !reflect.DeepEqual(got, want) {
		t.Fatalf("authorization invalidations = %#v, want %#v", got, want)
	}
}

func TestAuthorizationInformerHandlersSynchronizeBeforeServing(t *testing.T) {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant", UID: "namespace-uid"}}
	assignment := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "skupper", Namespace: namespace.Name, UID: "assignment-uid"}, Data: map[string]string{controllerSettingKey: "control/one"}}
	clients, err := fakeclient.NewFakeClient("control", []runtime.Object{namespace, assignment}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	recorder := &recordedAuthorizationInvalidator{notify: make(chan string, 32)}
	controller, err := NewNamespaceController(clients, NamespaceControllerOptions{ControllerID: "control/skupper-controller", Bootstrap: reconcile.DefaultRouterControlBootstrap("control"), Authorization: recorder}, newTestIntentPublisher(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controller.StartCaches(ctx)
	syncCtx, stopSync := context.WithTimeout(ctx, 5*time.Second)
	defer stopSync()
	if err := controller.WaitForCacheSync(syncCtx); err != nil {
		t.Fatal(err)
	}
	for len(recorder.notify) > 0 {
		<-recorder.notify
	}
	updated := assignment.DeepCopy()
	updated.Data[controllerSettingKey] = "control/two"
	if _, err := clients.GetKubeClient().CoreV1().ConfigMaps(namespace.Name).Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-recorder.notify:
		if event != string(AuthorizationAssignment)+":tenant/skupper" {
			t.Fatalf("unexpected informer invalidation %q", event)
		}
	case <-time.After(time.Second):
		t.Fatal("synchronized assignment informer did not invalidate authorization")
	}
}

func TestStandaloneAccessPlanExecutesServiceSecretAndStatusWrites(t *testing.T) {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "controller-ns", UID: "namespace-uid"}}
	issuer := &skupperv2alpha1.Certificate{ObjectMeta: metav1.ObjectMeta{Name: "issuer", Namespace: namespace.Name, UID: "issuer-uid", ResourceVersion: "1", Generation: 1}, Spec: skupperv2alpha1.CertificateSpec{Subject: "issuer", Signing: true}}
	access := &skupperv2alpha1.SecuredAccess{ObjectMeta: metav1.ObjectMeta{Name: "enrollment", Namespace: namespace.Name, UID: "access-uid", ResourceVersion: "1", Generation: 1}, Spec: skupperv2alpha1.SecuredAccessSpec{AccessType: "local", Selector: map[string]string{"app": "controller"}, Ports: []skupperv2alpha1.SecuredAccessPort{{Name: "tls", Port: 443, TargetPort: 8443, Protocol: "TCP"}}, Certificate: "enrollment", Issuer: "issuer"}}
	clients, err := fakeclient.NewFakeClient(namespace.Name, []runtime.Object{namespace}, []runtime.Object{issuer, access}, "")
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewNamespaceController(clients, NamespaceControllerOptions{ControllerID: namespace.Name + "/skupper-controller", Bootstrap: reconcile.DefaultRouterControlBootstrap(namespace.Name)}, newTestIntentPublisher(), nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := reconcile.Snapshot{Namespace: reconcile.NamespaceIdentity{Name: namespace.Name, UID: namespace.UID}, Assignment: reconcile.Assignment{Controller: namespace.Name + "/skupper-controller", Controlled: true}, EvaluationTime: time.Now(), Certificates: []*skupperv2alpha1.Certificate{issuer}, SecuredAccesses: []*skupperv2alpha1.SecuredAccess{access}, DefaultAccessType: "local"}
	desired := (reconcile.NamespaceDeriver{}).Derive(snapshot)
	planner := reconcile.StatusPlanner{Next: reconcile.AccessPlanner{Next: noOperationsPlanner{}, Ensurer: controller}, Writer: controller}
	report := (reconcile.Executor{}).Execute(context.Background(), planner.Plan(snapshot, desired))
	if report.NeedsRetry() {
		t.Fatalf("standalone plan failed: %#v", report)
	}
	if _, err := clients.GetKubeClient().CoreV1().Services(namespace.Name).Get(context.Background(), access.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("standalone Service was not written: %v", err)
	}
	for _, name := range []string{"issuer", "enrollment"} {
		if _, err := clients.GetKubeClient().CoreV1().Secrets(namespace.Name).Get(context.Background(), name, metav1.GetOptions{}); err != nil {
			t.Fatalf("standalone Secret %s was not written: %v", name, err)
		}
	}
	updated, err := clients.GetSkupperClient().SkupperV2alpha1().Certificates(namespace.Name).Get(context.Background(), issuer.Name, metav1.GetOptions{})
	if err != nil || updated.Status.StatusType == "" {
		t.Fatalf("standalone status was not written: certificate=%#v err=%v", updated, err)
	}
}

func (p *testIntentPublisher) Publish(intent routercontrol.RouterIntent) (routercontrol.Digest, error) {
	digest, err := p.real.Publish(intent)
	if err != nil {
		return "", err
	}
	select {
	case p.published <- intent:
	default:
	}
	return digest, nil
}
func (p *testIntentPublisher) SetUnavailable(target routercontrol.TargetIdentity) {
	p.real.SetUnavailable(target)
}

func (p *testIntentPublisher) PublishedIntents(namespaceUID string) map[routercontrol.TargetIdentity]routercontrol.Publication {
	return p.real.PublishedIntents(namespaceUID)
}

func newTestIntentPublisher() *testIntentPublisher {
	return &testIntentPublisher{published: make(chan routercontrol.RouterIntent, 1), real: routercontrol.NewPublisher()}
}

func TestNamespaceControllerSeparatesCacheSyncFromLeaderEffects(t *testing.T) {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "site-ns", UID: "namespace-uid"}}
	site := &skupperv2alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site", Namespace: "site-ns", UID: "site-uid"}}
	clients, err := fakeclient.NewFakeClient("controller-ns", []runtime.Object{namespace}, []runtime.Object{site}, "")
	if err != nil {
		t.Fatal(err)
	}
	clients.GetDynamicClient().(*dynamicfake.FakeDynamicClient).PrependReactor("patch", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		patched := action.(clienttesting.PatchAction)
		object := &unstructured.Unstructured{}
		if err := json.Unmarshal(patched.GetPatch(), object); err != nil {
			return true, nil, err
		}
		object.SetNamespace(action.GetNamespace())
		return true, object, nil
	})
	publisher := newTestIntentPublisher()
	controller, err := NewNamespaceController(clients, NamespaceControllerOptions{ControllerID: "controller-ns/skupper-controller", Workers: 1, Bootstrap: reconcile.DefaultRouterControlBootstrap("controller-ns")}, publisher, nil)
	if err != nil {
		t.Fatal(err)
	}
	cacheContext, stopCaches := context.WithCancel(context.Background())
	defer stopCaches()
	controller.StartCaches(cacheContext)
	syncContext, cancelSync := context.WithTimeout(cacheContext, 5*time.Second)
	defer cancelSync()
	if err := controller.WaitForCacheSync(syncContext); err != nil {
		t.Fatal(err)
	}
	if !controller.CachesSynced() {
		t.Fatal("cache sync state was not exposed")
	}
	if err := controller.SetRouterControlCA([]byte("public-ca")); err != nil {
		t.Fatal(err)
	}
	if _, err := clients.GetKubeClient().CoreV1().ConfigMaps("site-ns").Get(context.Background(), allocationConfigMapName, metav1.GetOptions{}); err == nil {
		t.Fatal("standby cache startup wrote allocation state")
	}

	leaderContext, stopLeader := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- controller.RunLeader(leaderContext) }()
	select {
	case intent := <-publisher.published:
		if intent.Target.NamespaceUID != "namespace-uid" || intent.Target.SiteUID != "site-uid" || intent.Target.RouterGroup != "skupper-router" {
			t.Fatalf("unexpected target identity: %#v", intent.Target)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("leader did not publish derived intent")
	}
	ca, err := clients.GetKubeClient().CoreV1().ConfigMaps("site-ns").Get(context.Background(), "skupper-controller-ca", metav1.GetOptions{})
	if err != nil || ca.Data["ca.crt"] != "public-ca" || !metav1.IsControlledBy(ca, site) {
		t.Fatalf("router-control CA was not applied as a Site-owned prerequisite: ca=%#v err=%v", ca, err)
	}
	stopLeader()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("RunLeader returned %v", err)
	}
}

func TestAllocationCollectionRejectsCorruptOrUnsafeRecords(t *testing.T) {
	tests := []struct {
		name string
		data map[string]string
	}{
		{name: "version", data: map[string]string{"version": "2", "namespaceUID": "namespace-uid", "siteUID": "site-uid", "ports": `{}`}},
		{name: "namespace UID", data: map[string]string{"version": "1", "namespaceUID": "old-uid", "siteUID": "site-uid", "ports": `{}`}},
		{name: "JSON", data: map[string]string{"version": "1", "namespaceUID": "namespace-uid", "siteUID": "site-uid", "ports": `{`}},
		{name: "duplicate", data: map[string]string{"version": "1", "namespaceUID": "namespace-uid", "siteUID": "site-uid", "ports": `{"a":1024,"b":1024}`}},
		{name: "reserved", data: map[string]string{"version": "1", "namespaceUID": "namespace-uid", "siteUID": "site-uid", "ports": `{"a":55671}`}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clients, err := fakeclient.NewFakeClient("controller-ns", nil, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			publisher := newTestIntentPublisher()
			controller, err := NewNamespaceController(clients, NamespaceControllerOptions{ControllerID: "controller-ns/skupper-controller", Bootstrap: reconcile.DefaultRouterControlBootstrap("controller-ns")}, publisher, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := controller.informers.configMaps.GetStore().Add(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: allocationConfigMapName, Namespace: "site-ns", ResourceVersion: "10"}, Data: test.data}); err != nil {
				t.Fatal(err)
			}
			if _, err := controller.allocations("site-ns", "namespace-uid", nil); err == nil {
				t.Fatal("unsafe allocation record was accepted")
			}
		})
	}
}

func TestAllocationOwnershipTransitionsOnlyAfterOldSiteUIDIsGone(t *testing.T) {
	for _, test := range []struct {
		name        string
		oldSiteLive bool
		wantError   bool
	}{{name: "deleted old Site", oldSiteLive: false}, {name: "live old Site", oldSiteLive: true, wantError: true}} {
		t.Run(test.name, func(t *testing.T) {
			namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "site-ns", UID: "namespace-uid"}}
			newSite := &skupperv2alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site", Namespace: namespace.Name, UID: "new-site-uid", ResourceVersion: "2", Generation: 1}}
			skupperObjects := []runtime.Object{newSite}
			if test.oldSiteLive {
				skupperObjects = append(skupperObjects, &skupperv2alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: namespace.Name, UID: "old-site-uid"}})
			}
			allocation := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: allocationConfigMapName, Namespace: namespace.Name, ResourceVersion: "17"}, Data: map[string]string{"version": "1", "namespaceUID": string(namespace.UID), "siteUID": "old-site-uid", "ports": `{"old/listener":12345}`}}
			clients, err := fakeclient.NewFakeClient("controller-ns", []runtime.Object{namespace, allocation}, skupperObjects, "")
			if err != nil {
				t.Fatal(err)
			}
			controller, err := NewNamespaceController(clients, NamespaceControllerOptions{ControllerID: "controller-ns/skupper-controller", Bootstrap: reconcile.DefaultRouterControlBootstrap("controller-ns")}, newTestIntentPublisher(), nil)
			if err != nil {
				t.Fatal(err)
			}
			err = controller.CommitAllocations(context.Background(), reconcile.NamespaceIdentity{Name: namespace.Name, UID: namespace.UID}, newSite, reconcile.AllocationState{SiteUID: newSite.UID, ResourceVersion: allocation.ResourceVersion, Ports: map[string]int{}})
			if test.wantError {
				if err == nil {
					t.Fatal("allocation authority moved while the old Site UID was still live")
				}
				return
			}
			if err != nil {
				t.Fatalf("replacement Site could not take stale allocation authority: %v", err)
			}
			updated, err := clients.GetKubeClient().CoreV1().ConfigMaps(namespace.Name).Get(context.Background(), allocationConfigMapName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if updated.Data["siteUID"] != string(newSite.UID) || updated.Data["ports"] != `{}` {
				t.Fatalf("stale authority or ports were inherited: %#v", updated.Data)
			}
		})
	}
}

func TestCompetingSiteStatusesApplyWithoutActiveOwner(t *testing.T) {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "site-ns", UID: "namespace-uid"}}
	first := &skupperv2alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: namespace.Name, UID: "first-uid", ResourceVersion: "1", Generation: 1}}
	second := &skupperv2alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: namespace.Name, UID: "second-uid", ResourceVersion: "2", Generation: 1}}
	clients, err := fakeclient.NewFakeClient("controller-ns", []runtime.Object{namespace}, []runtime.Object{first, second}, "")
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewNamespaceController(clients, NamespaceControllerOptions{ControllerID: "controller-ns/skupper-controller", Bootstrap: reconcile.DefaultRouterControlBootstrap("controller-ns")}, newTestIntentPublisher(), nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := reconcile.Snapshot{Namespace: reconcile.NamespaceIdentity{Name: namespace.Name, UID: namespace.UID}, Assignment: reconcile.Assignment{Controller: "controller-ns/skupper-controller", Controlled: true}, Sites: []*skupperv2alpha1.Site{first, second}, Allocations: reconcile.AllocationState{Ports: map[string]int{}}}
	desired := (reconcile.NamespaceDeriver{}).Derive(snapshot)
	if desired.Statuses.Owner != nil || len(desired.Statuses.Sites) != 2 {
		t.Fatalf("unexpected competing Site projection: %#v", desired.Statuses)
	}
	if err := controller.ApplyStatuses(context.Background(), snapshot.Namespace, desired.Statuses); err != nil {
		t.Fatalf("competing Site status write required an active owner: %v", err)
	}
	for _, name := range []string{first.Name, second.Name} {
		updated, err := clients.GetSkupperClient().SkupperV2alpha1().Sites(namespace.Name).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil || updated.Status.StatusType != skupperv2alpha1.StatusError {
			t.Fatalf("competing Site %s status was not applied: site=%#v err=%v", name, updated, err)
		}
	}
}

func TestControlsNamespacePreservesAutomaticAndExplicitEmptyAssignment(t *testing.T) {
	const namespace = "site-ns"
	const controllerID = "controller-ns/skupper-controller"
	missingKey := &corev1.ConfigMap{Data: map[string]string{}}
	empty := &corev1.ConfigMap{Data: map[string]string{controllerSettingKey: ""}}
	shortName := &corev1.ConfigMap{Data: map[string]string{controllerSettingKey: "skupper-controller"}}
	qualified := &corev1.ConfigMap{Data: map[string]string{controllerSettingKey: controllerID}}
	if !ControlsNamespace(nil, namespace, controllerID, false) || !ControlsNamespace(missingKey, namespace, controllerID, false) {
		t.Fatal("automatic assignment did not control an absent ConfigMap/key")
	}
	if ControlsNamespace(nil, namespace, controllerID, true) || ControlsNamespace(missingKey, namespace, controllerID, true) {
		t.Fatal("explicit assignment accepted an absent ConfigMap/key")
	}
	if ControlsNamespace(empty, namespace, controllerID, false) {
		t.Fatal("explicit empty assignment was treated as automatic")
	}
	if !ControlsNamespace(shortName, namespace, namespace+"/skupper-controller", true) || !ControlsNamespace(qualified, namespace, controllerID, true) {
		t.Fatal("valid short or qualified assignment was rejected")
	}
}

func TestRouterPrerequisitesAreOwnedLeastPrivilegeAndDoNotClaimForeignObjects(t *testing.T) {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "site-ns", UID: "namespace-uid"}}
	site := &skupperv2alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site", Namespace: "site-ns", UID: "site-uid"}}
	clients, err := fakeclient.NewFakeClient("controller-ns", []runtime.Object{namespace}, []runtime.Object{site}, "")
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewNamespaceController(clients, NamespaceControllerOptions{ControllerID: "controller-ns/skupper-controller", Bootstrap: reconcile.DefaultRouterControlBootstrap("controller-ns")}, newTestIntentPublisher(), nil)
	if err != nil {
		t.Fatal(err)
	}
	desired := (reconcile.NamespaceDeriver{}).Derive(reconcile.Snapshot{Namespace: reconcile.NamespaceIdentity{Name: namespace.Name, UID: namespace.UID}, Assignment: reconcile.Assignment{Controlled: true}, Sites: []*skupperv2alpha1.Site{site}, Allocations: reconcile.AllocationState{Ports: map[string]int{}}})
	if err := controller.EnsureRouterPrerequisites(context.Background(), desired.Namespace, site, desired.ServiceAccount, desired.Role, desired.RoleBinding); err != nil {
		t.Fatal(err)
	}
	role, err := clients.GetKubeClient().RbacV1().Roles(namespace.Name).Get(context.Background(), "skupper-router", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(role, site) || len(role.Rules) != 1 || len(role.Rules[0].Resources) != 1 || role.Rules[0].Resources[0] != "secrets" {
		t.Fatalf("unexpected router Role: %#v", role)
	}
	if err := controller.EnsureRouterPrerequisites(context.Background(), desired.Namespace, site, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := clients.GetKubeClient().CoreV1().ServiceAccounts(namespace.Name).Get(context.Background(), "skupper-router", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("generated ServiceAccount was not retired for custom SA: %v", err)
	}

	foreign := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "skupper-router", Namespace: namespace.Name}}
	if _, err := clients.GetKubeClient().CoreV1().ServiceAccounts(namespace.Name).Create(context.Background(), foreign, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := controller.EnsureRouterPrerequisites(context.Background(), desired.Namespace, site, desired.ServiceAccount, desired.Role, desired.RoleBinding); err == nil {
		t.Fatal("controller claimed a foreign router ServiceAccount")
	}
	if _, err := clients.GetKubeClient().RbacV1().Roles(namespace.Name).Get(context.Background(), "skupper-router", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Role was created after foreign ServiceAccount rejection: %v", err)
	}
}

func TestRouterPrerequisitesDoNotAdoptForeignRoleOrRoleBinding(t *testing.T) {
	tests := []struct {
		name    string
		foreign runtime.Object
		owners  func(internalclient.Clients, string) ([]metav1.OwnerReference, error)
	}{
		{
			name:    "Role",
			foreign: &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "skupper-router", Namespace: "site-ns"}},
			owners: func(clients internalclient.Clients, namespace string) ([]metav1.OwnerReference, error) {
				value, err := clients.GetKubeClient().RbacV1().Roles(namespace).Get(context.Background(), "skupper-router", metav1.GetOptions{})
				return value.OwnerReferences, err
			},
		},
		{
			name:    "RoleBinding",
			foreign: &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "skupper-router", Namespace: "site-ns"}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "foreign"}},
			owners: func(clients internalclient.Clients, namespace string) ([]metav1.OwnerReference, error) {
				value, err := clients.GetKubeClient().RbacV1().RoleBindings(namespace).Get(context.Background(), "skupper-router", metav1.GetOptions{})
				return value.OwnerReferences, err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "site-ns", UID: "namespace-uid"}}
			site := &skupperv2alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site", Namespace: namespace.Name, UID: "site-uid"}}
			clients, err := fakeclient.NewFakeClient("controller-ns", []runtime.Object{namespace, test.foreign}, []runtime.Object{site}, "")
			if err != nil {
				t.Fatal(err)
			}
			controller, err := NewNamespaceController(clients, NamespaceControllerOptions{ControllerID: "controller-ns/skupper-controller", Bootstrap: reconcile.DefaultRouterControlBootstrap("controller-ns")}, newTestIntentPublisher(), nil)
			if err != nil {
				t.Fatal(err)
			}
			desired := (reconcile.NamespaceDeriver{}).Derive(reconcile.Snapshot{Namespace: reconcile.NamespaceIdentity{Name: namespace.Name, UID: namespace.UID}, Assignment: reconcile.Assignment{Controlled: true}, Sites: []*skupperv2alpha1.Site{site}, Allocations: reconcile.AllocationState{Ports: map[string]int{}}})
			if err := controller.EnsureRouterPrerequisites(context.Background(), desired.Namespace, site, desired.ServiceAccount, desired.Role, desired.RoleBinding); err == nil {
				t.Fatalf("controller accepted foreign %s", test.name)
			}
			owners, err := test.owners(clients, namespace.Name)
			if err != nil {
				t.Fatal(err)
			}
			if len(owners) != 0 {
				t.Fatalf("controller adopted foreign %s: %#v", test.name, owners)
			}
		})
	}
}

func TestDefaultedListenerServiceIsQuiet(t *testing.T) {
	controller, clients, namespace, site := listenerServiceTestController(t)
	policy := corev1.ServiceInternalTrafficPolicyCluster
	familyPolicy := corev1.IPFamilyPolicySingleStack
	controllerOwner, block := true, true
	desired := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: namespace.Name, Labels: map[string]string{"internal.skupper.io/listener": "true"}, Annotations: map[string]string{"internal.skupper.io/controlled": "true"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: skupperv2alpha1.SchemeGroupVersion.String(), Kind: "Site", Name: site.Name, UID: site.UID, Controller: &controllerOwner, BlockOwnerDeletion: &block}}}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, SessionAffinity: corev1.ServiceAffinityNone, InternalTrafficPolicy: &policy, Selector: map[string]string{"skupper.io/component": "router"}, Ports: []corev1.ServicePort{{Name: "orders", Port: 8080}}}}
	current := desired.DeepCopy()
	current.Spec.ClusterIP = "10.96.0.12"
	current.Spec.ClusterIPs = []string{"10.96.0.12"}
	current.Spec.IPFamilies = []corev1.IPFamily{corev1.IPv4Protocol}
	current.Spec.IPFamilyPolicy = &familyPolicy
	if _, err := clients.GetKubeClient().CoreV1().Services(namespace.Name).Create(context.Background(), current, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	updates := 0
	clients.GetKubeClient().(*kubefake.Clientset).PrependReactor("update", "services", func(action clienttesting.Action) (bool, runtime.Object, error) {
		updates++
		return false, nil, nil
	})
	if err := controller.EnsureListenerServices(context.Background(), reconcile.NamespaceIdentity{Name: namespace.Name, UID: namespace.UID}, site, []*corev1.Service{desired}); err != nil {
		t.Fatal(err)
	}
	if updates != 0 {
		t.Fatalf("API-defaulted Service caused %d unnecessary updates", updates)
	}
}

func TestAccessServiceFailureDoesNotBlockLaterDesiredMutation(t *testing.T) {
	controller, clients, namespace, _ := listenerServiceTestController(t)
	parent := &skupperv2alpha1.SecuredAccess{ObjectMeta: metav1.ObjectMeta{Name: "access", Namespace: namespace.Name, UID: "access-uid", ResourceVersion: "7", Generation: 2}}
	if _, err := clients.GetSkupperClient().SkupperV2alpha1().SecuredAccesses(namespace.Name).Create(context.Background(), parent, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	owner := accessOwner(parent)
	foreign := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "a-foreign", Namespace: namespace.Name}}
	if _, err := clients.GetKubeClient().CoreV1().Services(namespace.Name).Create(context.Background(), foreign, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	desired := []*corev1.Service{
		{ObjectMeta: metav1.ObjectMeta{Name: foreign.Name, Namespace: namespace.Name, Annotations: map[string]string{"internal.skupper.io/controlled": "true"}, OwnerReferences: []metav1.OwnerReference{owner}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "z-valid", Namespace: namespace.Name, Annotations: map[string]string{"internal.skupper.io/controlled": "true"}, OwnerReferences: []metav1.OwnerReference{owner}}},
	}
	err := controller.ensureAccessServices(context.Background(), reconcile.NamespaceIdentity{Name: namespace.Name, UID: namespace.UID}, nil, desired, []*skupperv2alpha1.SecuredAccess{parent})
	if err == nil {
		t.Fatal("foreign Service was not reported")
	}
	if _, err := clients.GetKubeClient().CoreV1().Services(namespace.Name).Get(context.Background(), "z-valid", metav1.GetOptions{}); err != nil {
		t.Fatalf("later valid Service was not created: %v", err)
	}
	actual, err := clients.GetKubeClient().CoreV1().Services(namespace.Name).Get(context.Background(), foreign.Name, metav1.GetOptions{})
	if err != nil || len(actual.OwnerReferences) != 0 {
		t.Fatalf("foreign Service was claimed: service=%#v err=%v", actual, err)
	}
}

func TestStaleSecuredAccessSnapshotPreventsServiceMutations(t *testing.T) {
	for _, operation := range []string{"create", "update", "delete"} {
		t.Run(operation, func(t *testing.T) {
			controller, clients, namespace, _ := listenerServiceTestController(t)
			live := &skupperv2alpha1.SecuredAccess{ObjectMeta: metav1.ObjectMeta{Name: "access", Namespace: namespace.Name, UID: "access-uid", ResourceVersion: "8", Generation: 3}}
			if _, err := clients.GetSkupperClient().SkupperV2alpha1().SecuredAccesses(namespace.Name).Create(context.Background(), live, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			snapshot := live.DeepCopy()
			snapshot.ResourceVersion = "7"
			owner := accessOwner(snapshot)
			name := "service"
			var desired []*corev1.Service
			if operation != "create" {
				current := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace.Name, UID: "service-uid", Labels: map[string]string{"internal.skupper.io/secured-access": "true"}, Annotations: map[string]string{"internal.skupper.io/controlled": "true"}, OwnerReferences: []metav1.OwnerReference{owner}}}
				if _, err := clients.GetKubeClient().CoreV1().Services(namespace.Name).Create(context.Background(), current, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
				if operation == "update" {
					desired = []*corev1.Service{{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace.Name, Labels: map[string]string{"changed": "true"}, Annotations: map[string]string{"internal.skupper.io/controlled": "true"}, OwnerReferences: []metav1.OwnerReference{owner}}}}
				}
			} else {
				desired = []*corev1.Service{{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace.Name, Annotations: map[string]string{"internal.skupper.io/controlled": "true"}, OwnerReferences: []metav1.OwnerReference{owner}}}}
			}
			if err := controller.ensureAccessServices(context.Background(), reconcile.NamespaceIdentity{Name: namespace.Name, UID: namespace.UID}, nil, desired, []*skupperv2alpha1.SecuredAccess{snapshot}); err == nil {
				t.Fatal("stale parent snapshot was not reported")
			}
			actual, err := clients.GetKubeClient().CoreV1().Services(namespace.Name).Get(context.Background(), name, metav1.GetOptions{})
			if operation == "create" {
				if !apierrors.IsNotFound(err) {
					t.Fatalf("create occurred with stale parent: %v", err)
				}
			} else if err != nil || actual.Labels["changed"] != "" {
				t.Fatalf("%s occurred with stale parent: service=%#v err=%v", operation, actual, err)
			}
		})
	}
}

func TestAssignmentRevokedBetweenOwnerCheckAndMutationPreventsWrite(t *testing.T) {
	controller, clients, namespace, _ := listenerServiceTestController(t)
	parent := &skupperv2alpha1.SecuredAccess{ObjectMeta: metav1.ObjectMeta{Name: "access", Namespace: namespace.Name, UID: "access-uid"}}
	if _, err := clients.GetSkupperClient().SkupperV2alpha1().SecuredAccesses(namespace.Name).Create(context.Background(), parent, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	revoked := false
	clients.GetSkupperClient().(*skupperfake.Clientset).PrependReactor("get", "securedaccesses", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if !revoked {
			revoked = true
			_, err := clients.GetKubeClient().CoreV1().ConfigMaps(namespace.Name).Create(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "skupper", Namespace: namespace.Name}, Data: map[string]string{"controller": ""}}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
		}
		return false, nil, nil
	})
	desired := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "blocked", Namespace: namespace.Name, OwnerReferences: []metav1.OwnerReference{accessOwner(parent)}}}
	if err := controller.ensureAccessServices(context.Background(), reconcile.NamespaceIdentity{Name: namespace.Name, UID: namespace.UID}, nil, []*corev1.Service{desired}, []*skupperv2alpha1.SecuredAccess{parent}); err == nil {
		t.Fatal("assignment revocation was not reported")
	}
	if _, err := clients.GetKubeClient().CoreV1().Services(namespace.Name).Get(context.Background(), desired.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Service was written after assignment revocation: %v", err)
	}
}

func TestGatewayStatefulSetOwnerIsSupportedAndLiveUIDIsFenced(t *testing.T) {
	controllerFlag := true
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "controller-ns", UID: "namespace-uid"}}
	statefulSet := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "skupper-controller", Namespace: namespace.Name, UID: "current-owner"}}
	clients, err := fakeclient.NewFakeClient(namespace.Name, []runtime.Object{namespace, statefulSet}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewNamespaceController(clients, NamespaceControllerOptions{ControllerID: namespace.Name + "/skupper-controller", Bootstrap: reconcile.DefaultRouterControlBootstrap(namespace.Name)}, newTestIntentPublisher(), nil)
	if err != nil {
		t.Fatal(err)
	}
	owner := metav1.OwnerReference{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "StatefulSet", Name: statefulSet.Name, UID: "stale-owner", Controller: &controllerFlag}
	if !supportedGatewayOwner(&owner) {
		t.Fatal("apps/v1 StatefulSet owner was rejected by the option contract")
	}
	gateway := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway", "metadata": map[string]interface{}{"name": "skupper", "namespace": namespace.Name}}}
	gateway.SetOwnerReferences([]metav1.OwnerReference{owner})
	err = controller.ensureGateway(context.Background(), reconcile.NamespaceIdentity{Name: namespace.Name, UID: namespace.UID}, nil, gateway)
	var superseded reconcile.SupersededError
	if !errors.As(err, &superseded) {
		t.Fatalf("stale StatefulSet UID was not fenced: %v", err)
	}
}

func TestDynamicAccessForeignObjectDoesNotBlockIndependentCreate(t *testing.T) {
	controller, clients, namespace, _ := listenerServiceTestController(t)
	parent := &skupperv2alpha1.SecuredAccess{ObjectMeta: metav1.ObjectMeta{Name: "access", Namespace: namespace.Name, UID: "access-uid"}}
	if _, err := clients.GetSkupperClient().SkupperV2alpha1().SecuredAccesses(namespace.Name).Create(context.Background(), parent, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	makeProxy := func(name string) *unstructured.Unstructured {
		value := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "projectcontour.io/v1", "kind": "HTTPProxy", "metadata": map[string]interface{}{}, "spec": map[string]interface{}{"virtualhost": map[string]interface{}{"fqdn": name + ".example"}}}}
		value.SetName(name)
		value.SetNamespace(namespace.Name)
		value.SetLabels(map[string]string{"internal.skupper.io/secured-access": "true"})
		value.SetAnnotations(map[string]string{"internal.skupper.io/controlled": "true"})
		value.SetOwnerReferences([]metav1.OwnerReference{accessOwner(parent)})
		return value
	}
	foreign := makeProxy("a-foreign")
	foreign.SetOwnerReferences(nil)
	if _, err := clients.GetDynamicClient().Resource(reconcile.HTTPProxyGVR).Namespace(namespace.Name).Create(context.Background(), foreign, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	err := controller.ensureDynamicAccess(context.Background(), reconcile.NamespaceIdentity{Name: namespace.Name, UID: namespace.UID}, nil, reconcile.HTTPProxyGVR, []*unstructured.Unstructured{makeProxy("a-foreign"), makeProxy("z-valid")}, []*skupperv2alpha1.SecuredAccess{parent})
	if err == nil {
		t.Fatal("foreign HTTPProxy was not reported")
	}
	if _, err := clients.GetDynamicClient().Resource(reconcile.HTTPProxyGVR).Namespace(namespace.Name).Get(context.Background(), "z-valid", metav1.GetOptions{}); err != nil {
		t.Fatalf("independent HTTPProxy was not created: %v", err)
	}
	actual, err := clients.GetDynamicClient().Resource(reconcile.HTTPProxyGVR).Namespace(namespace.Name).Get(context.Background(), foreign.GetName(), metav1.GetOptions{})
	if err != nil || len(actual.GetOwnerReferences()) != 0 {
		t.Fatalf("foreign HTTPProxy was claimed: object=%#v err=%v", actual, err)
	}
}

func TestDynamicAccessUpdatePreservesDefaultsIsQuietAndRetires(t *testing.T) {
	controller, clients, namespace, _ := listenerServiceTestController(t)
	parent := &skupperv2alpha1.SecuredAccess{ObjectMeta: metav1.ObjectMeta{Name: "access", Namespace: namespace.Name, UID: "access-uid"}}
	if _, err := clients.GetSkupperClient().SkupperV2alpha1().SecuredAccesses(namespace.Name).Create(context.Background(), parent, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	desired := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "projectcontour.io/v1", "kind": "HTTPProxy", "metadata": map[string]interface{}{}, "spec": map[string]interface{}{"virtualhost": map[string]interface{}{"fqdn": "new.example"}}}}
	desired.SetName("proxy")
	desired.SetNamespace(namespace.Name)
	desired.SetLabels(map[string]string{"internal.skupper.io/secured-access": "true"})
	desired.SetAnnotations(map[string]string{"internal.skupper.io/controlled": "true"})
	desired.SetOwnerReferences([]metav1.OwnerReference{accessOwner(parent)})
	current := desired.DeepCopy()
	current.Object["spec"] = map[string]interface{}{"virtualhost": map[string]interface{}{"fqdn": "old.example", "defaulted": true}}
	if _, err := clients.GetDynamicClient().Resource(reconcile.HTTPProxyGVR).Namespace(namespace.Name).Create(context.Background(), current, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	updates := 0
	clients.GetDynamicClient().(*dynamicfake.FakeDynamicClient).PrependReactor("update", "httpproxies", func(action clienttesting.Action) (bool, runtime.Object, error) {
		updates++
		return false, nil, nil
	})
	identity := reconcile.NamespaceIdentity{Name: namespace.Name, UID: namespace.UID}
	if err := controller.ensureDynamicAccess(context.Background(), identity, nil, reconcile.HTTPProxyGVR, []*unstructured.Unstructured{desired}, []*skupperv2alpha1.SecuredAccess{parent}); err != nil {
		t.Fatal(err)
	}
	updated, err := clients.GetDynamicClient().Resource(reconcile.HTTPProxyGVR).Namespace(namespace.Name).Get(context.Background(), desired.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defaulted, _, _ := unstructured.NestedBool(updated.Object, "spec", "virtualhost", "defaulted")
	if !defaulted || updates != 1 {
		t.Fatalf("defaulted field was lost or update missing: object=%#v updates=%d", updated, updates)
	}
	if err := controller.ensureDynamicAccess(context.Background(), identity, nil, reconcile.HTTPProxyGVR, []*unstructured.Unstructured{desired}, []*skupperv2alpha1.SecuredAccess{parent}); err != nil || updates != 1 {
		t.Fatalf("stable dynamic object was not quiet: updates=%d err=%v", updates, err)
	}
	if err := controller.ensureDynamicAccess(context.Background(), identity, nil, reconcile.HTTPProxyGVR, nil, []*skupperv2alpha1.SecuredAccess{parent}); err != nil {
		t.Fatal(err)
	}
	if _, err := clients.GetDynamicClient().Resource(reconcile.HTTPProxyGVR).Namespace(namespace.Name).Get(context.Background(), desired.GetName(), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("stale dynamic object was not retired: %v", err)
	}
}

func accessOwner(parent *skupperv2alpha1.SecuredAccess) metav1.OwnerReference {
	controlled := true
	return metav1.OwnerReference{APIVersion: skupperv2alpha1.SchemeGroupVersion.String(), Kind: "SecuredAccess", Name: parent.Name, UID: parent.UID, Controller: &controlled}
}

func listenerServiceTestController(t *testing.T) (*NamespaceController, internalclient.Clients, *corev1.Namespace, *skupperv2alpha1.Site) {
	t.Helper()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "site-ns", UID: "namespace-uid"}}
	site := &skupperv2alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site", Namespace: namespace.Name, UID: "site-uid"}}
	clients, err := fakeclient.NewFakeClient("controller-ns", []runtime.Object{namespace}, []runtime.Object{site}, "")
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewNamespaceController(clients, NamespaceControllerOptions{ControllerID: "controller-ns/skupper-controller", Bootstrap: reconcile.DefaultRouterControlBootstrap("controller-ns")}, newTestIntentPublisher(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return controller, clients, namespace, site
}

func TestNamespaceControllerOwnsSizingAndLabellingConfiguration(t *testing.T) {
	clients, err := fakeclient.NewFakeClient("controller-ns", nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewNamespaceController(clients, NamespaceControllerOptions{WatchNamespace: "site-ns", ControllerID: "controller-ns/skupper-controller", Bootstrap: reconcile.DefaultRouterControlBootstrap("controller-ns")}, newTestIntentPublisher(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if controller.configurationFactory == nil {
		t.Fatal("namespace-scoped controller did not create a controller-namespace configuration informer")
	}
	sizeConfig := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "small", Namespace: "controller-ns", Labels: map[string]string{sizing.SiteSizingLabel: "small"}, Annotations: map[string]string{sizing.DefaultSiteSizingAnnotation: "true"}}, Data: map[string]string{"router-cpu-request": "250m"}}
	labelConfig := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "labels", Namespace: "site-ns", Labels: map[string]string{"skupper.io/label-template": "", "acme.example/environment": "test"}}, Data: map[string]string{"kind": "Deployment"}}
	controller.updateConfiguration(sizeConfig, false)
	controller.updateConfiguration(labelConfig, false)
	siteSize, err := controller.sizing.GetSizing(&skupperv2alpha1.Site{})
	if err != nil || siteSize.Router.Requests[string(corev1.ResourceCPU)] != "250m" {
		t.Fatalf("cached default sizing was not applied: size=%#v err=%v", siteSize, err)
	}
	metadata := &metav1.ObjectMeta{}
	if !controller.labelling.SetObjectMetadata("site-ns", "skupper-router", "Deployment", metadata) || metadata.Labels["acme.example/environment"] != "test" {
		t.Fatalf("cached labelling template was not applied: %#v", metadata)
	}

	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for iteration := 0; iteration < 100; iteration++ {
				controller.updateConfiguration(sizeConfig, false)
				_, _ = controller.sizing.GetSizing(&skupperv2alpha1.Site{})
				controller.updateConfiguration(labelConfig, false)
				controller.labelling.SetObjectMetadata("site-ns", "skupper-router", "Deployment", &metav1.ObjectMeta{})
			}
		}()
	}
	workers.Wait()
	controller.updateConfiguration(sizeConfig, true)
	controller.updateConfiguration(labelConfig, true)
	siteSize, err = controller.sizing.GetSizing(&skupperv2alpha1.Site{})
	if err != nil || siteSize.Router.NotEmpty() {
		t.Fatalf("deleted sizing remained active: size=%#v err=%v", siteSize, err)
	}
	metadata = &metav1.ObjectMeta{}
	controller.labelling.SetObjectMetadata("site-ns", "skupper-router", "Deployment", metadata)
	if metadata.Labels["acme.example/environment"] != "" {
		t.Fatalf("deleted labelling template remained active: %#v", metadata)
	}
}
