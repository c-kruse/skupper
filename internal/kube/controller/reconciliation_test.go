package controller

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
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
	"github.com/skupperproject/skupper/internal/kube/site/sizing"
	"github.com/skupperproject/skupper/internal/routercontrol"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

type testIntentPublisher struct {
	published chan routercontrol.RouterIntent
	real      *routercontrol.Publisher
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
