package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	fakeclient "github.com/skupperproject/skupper/internal/kube/client/fake"
	"github.com/skupperproject/skupper/internal/kube/reconcile"
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
