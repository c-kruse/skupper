package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	applyappsv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	fakeclient "github.com/skupperproject/skupper/internal/kube/client/fake"
	"github.com/skupperproject/skupper/internal/kube/reconcile"
	"github.com/skupperproject/skupper/internal/routercontrol"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	skupperfake "github.com/skupperproject/skupper/pkg/generated/client/clientset/versioned/fake"
)

// Real defaults, managedFields, and informer round-trips must converge through
// the entire pipeline, not merely through each planner's synthetic fixtures.
func TestRealAPIServerNamespacePlanConverges(t *testing.T) {
	kubeconfig := os.Getenv("SKUPPER_REAL_API_TEST")
	if kubeconfig == "" {
		t.Skip("set SKUPPER_REAL_API_TEST to a disposable cluster with Skupper CRDs")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	namespace := fmt.Sprintf("skupper-plan-%d", time.Now().UnixNano())
	clients, err := internalclient.NewClient(namespace, "", kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clients.Kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	defer clients.Kube.CoreV1().Namespaces().Delete(context.Background(), namespace, metav1.DeleteOptions{})
	controllerID := namespace + "/plan-test"
	// Keep any other controller in the disposable cluster out of this fixture.
	if _, err := clients.Kube.CoreV1().ConfigMaps(namespace).Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: namespaceConfigName}, Data: map[string]string{controllerSettingKey: controllerID}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	api := clients.Skupper.SkupperV2alpha1()
	if _, err := api.Sites(namespace).Create(ctx, &skupperv2alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site"}, Spec: skupperv2alpha1.SiteSpec{HA: true}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.Listeners(namespace).Create(ctx, &skupperv2alpha1.Listener{ObjectMeta: metav1.ObjectMeta{Name: "frontend"}, Spec: skupperv2alpha1.ListenerSpec{Host: "frontend", RoutingKey: "backend", Port: 8080}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.Connectors(namespace).Create(ctx, &skupperv2alpha1.Connector{ObjectMeta: metav1.ObjectMeta{Name: "backend"}, Spec: skupperv2alpha1.ConnectorSpec{Host: "192.0.2.10", RoutingKey: "backend", Port: 8080}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	publisher := routercontrol.NewPublisher()
	c, err := NewNamespaceController(clients, NamespaceControllerOptions{ControllerID: controllerID, WatchNamespace: namespace, RequireExplicitControl: true, DefaultAccessType: "local", Bootstrap: reconcile.DefaultRouterControlBootstrap(namespace)}, publisher, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetRouterControlCA([]byte("public-ca")); err != nil {
		t.Fatal(err)
	}
	cacheContext, stopCaches := context.WithCancel(ctx)
	defer stopCaches()
	c.StartCaches(cacheContext)
	if err := c.WaitForCacheSync(ctx); err != nil {
		t.Fatal(err)
	}
	publication := reconcile.PublicationPlanner{Allocations: c, Publisher: publisher, Validator: c.verifyPublication}
	planner := reconcile.StatusPlanner{Next: reconcile.WorkloadPlanner{Next: reconcile.AccessPlanner{Next: publication, Ensurer: c}, Ensurer: c}, Writer: c}
	collectPlan := func() (reconcile.Snapshot, reconcile.Plan) {
		t.Helper()
		snapshot, err := c.Collect(ctx, namespace)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot, planner.Plan(snapshot, (reconcile.NamespaceDeriver{}).Derive(snapshot))
	}
	quiet := 0
	var snapshot reconcile.Snapshot
	var plan reconcile.Plan
	for ctx.Err() == nil && quiet < 3 {
		snapshot, plan = collectPlan()
		if len(plan.Operations) == 0 {
			quiet++
		} else {
			quiet = 0
			report := (reconcile.Executor{}).Execute(ctx, plan)
			for _, result := range report.Results {
				if result.Error != nil {
					t.Logf("%s: %v", result.ID, result.Error)
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if quiet != 3 {
		for _, operation := range plan.Operations {
			t.Logf("remaining operation: %s", operation.ID)
		}
		for _, deployment := range snapshot.Deployments {
			owned, _ := applyappsv1.ExtractDeployment(deployment, "skupper-controller")
			data, _ := json.Marshal(owned)
			t.Logf("owned Deployment: %s", data)
		}
		for _, deployment := range (reconcile.NamespaceDeriver{}).Derive(snapshot).Deployments {
			data, _ := json.Marshal(deployment)
			t.Logf("desired Deployment: %s", data)
		}
		t.Fatal("namespace did not reach an empty plan")
	}
	if len(snapshot.Deployments) != 2 || len(snapshot.PublishedIntents) != 2 || len(snapshot.Certificates) == 0 || len(snapshot.Secrets) == 0 || plan.NextReevaluation.IsZero() {
		t.Fatalf("fixture omitted HA workload, publication, or certificate coverage: deployments=%d publications=%d certificates=%d secrets=%d deadline=%v", len(snapshot.Deployments), len(snapshot.PublishedIntents), len(snapshot.Certificates), len(snapshot.Secrets), plan.NextReevaluation)
	}

	// Informers keep their real clients; only reconciliation effects are replaced
	// with fatal reactors. Collection, derivation, planning, and execution must
	// all succeed without reading or writing through those clients.
	blocked, err := fakeclient.NewFakeClient(namespace, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, client := range []*clienttesting.Fake{&blocked.GetKubeClient().(*kubefake.Clientset).Fake, &blocked.GetSkupperClient().(*skupperfake.Clientset).Fake, &blocked.GetDynamicClient().(*dynamicfake.FakeDynamicClient).Fake} {
		client.PrependReactor("*", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
			t.Fatalf("converged pass made an API request: %s %s", action.GetVerb(), action.GetResource().Resource)
			return true, nil, nil
		})
	}
	c.clients = blocked
	snapshot, plan = collectPlan()
	if len(plan.Operations) != 0 {
		t.Fatalf("converged plan gained %d effects", len(plan.Operations))
	}
	if report := (reconcile.Executor{}).Execute(ctx, plan); report.NeedsRetry() {
		t.Fatalf("empty plan failed: %#v", report)
	}
	c.clients = clients

	connector, err := api.Connectors(namespace).Get(ctx, "backend", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	connector.Spec.Host = "192.0.2.20"
	connector, err = api.Connectors(namespace).Update(ctx, connector, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for ctx.Err() == nil {
		snapshot, plan = collectPlan()
		if len(snapshot.Connectors) == 1 && snapshot.Connectors[0].ResourceVersion == connector.ResourceVersion {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	publications := 0
	for _, operation := range plan.Operations {
		switch operation.Kind {
		case "PublishRouterIntent":
			publications++
		case "ApplyPublicStatus":
		default:
			t.Errorf("host-only change selected unrelated effect: %s", operation.ID)
		}
	}
	if publications != 2 {
		t.Fatalf("host-only change selected %d HA publications, want 2", publications)
	}
	if report := (reconcile.Executor{}).Execute(ctx, plan); report.NeedsRetry() {
		t.Fatalf("host-only plan failed: %#v", report)
	}
	t.Log("real HA namespace: converged empty plan, zero reconciliation API calls, host-only publication/status isolation")
}
