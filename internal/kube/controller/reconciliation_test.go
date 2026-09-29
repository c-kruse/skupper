package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	fakeclient "github.com/skupperproject/skupper/internal/kube/client/fake"
	"github.com/skupperproject/skupper/internal/routercontrol"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

type testIntentPublisher struct {
	published chan routercontrol.RouterIntent
}

func (p *testIntentPublisher) Publish(intent routercontrol.RouterIntent) (routercontrol.Digest, error) {
	select {
	case p.published <- intent:
	default:
	}
	return "digest", nil
}
func (p *testIntentPublisher) SetUnavailable(routercontrol.TargetIdentity) {}

func TestNamespaceControllerSeparatesCacheSyncFromLeaderEffects(t *testing.T) {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "site-ns", UID: "namespace-uid"}}
	site := &skupperv2alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site", Namespace: "site-ns", UID: "site-uid"}}
	clients, err := fakeclient.NewFakeClient("controller-ns", []runtime.Object{namespace}, []runtime.Object{site}, "")
	if err != nil {
		t.Fatal(err)
	}
	publisher := &testIntentPublisher{published: make(chan routercontrol.RouterIntent, 1)}
	controller, err := NewNamespaceController(clients, NamespaceControllerOptions{ControllerID: "controller-ns/skupper-controller", Workers: 1}, publisher, nil)
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
	stopLeader()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("RunLeader returned %v", err)
	}
}
