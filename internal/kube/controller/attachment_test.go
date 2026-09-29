package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"

	"github.com/skupperproject/skupper/internal/kube/reconcile"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

func TestAttachmentPublicationRevalidatesSourceAuthority(t *testing.T) {
	for _, change := range []string{"other controller", "unassigned", "replacement namespace", "deleting namespace"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			controller, clients, namespace, site := listenerServiceTestController(t)
			source := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "apps", UID: "source-uid"}}
			attached := &skupperv2alpha1.AttachedConnector{ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: source.Name, UID: "attached-uid"}, Spec: skupperv2alpha1.AttachedConnectorSpec{SiteNamespace: namespace.Name, Selector: "app=backend", Port: 8080}}
			binding := &skupperv2alpha1.AttachedConnectorBinding{ObjectMeta: metav1.ObjectMeta{Name: attached.Name, Namespace: namespace.Name, UID: "binding-uid"}, Spec: skupperv2alpha1.AttachedConnectorBindingSpec{ConnectorNamespace: source.Name, RoutingKey: "backend"}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: source.Name, UID: "backend-pod", Labels: map[string]string{"app": "backend"}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.2.3.4", Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
			if _, err := clients.GetKubeClient().CoreV1().Namespaces().Create(ctx, source, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			api := clients.GetSkupperClient().SkupperV2alpha1()
			if _, err := api.AttachedConnectors(source.Name).Create(ctx, attached, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := api.AttachedConnectorBindings(namespace.Name).Create(ctx, binding, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			for _, item := range []struct {
				store cache.Store
				value runtime.Object
			}{
				{controller.informers.namespaces.GetStore(), namespace},
				{controller.informers.namespaces.GetStore(), source},
				{controller.informers.sites.GetStore(), site},
				{controller.informers.attached.GetStore(), attached},
				{controller.informers.bindings.GetStore(), binding},
				{controller.informers.pods.GetStore(), pod},
			} {
				if err := item.store.Add(item.value.DeepCopyObject()); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := controller.Collect(ctx, namespace.Name)
			if err != nil {
				t.Fatal(err)
			}
			desired := (reconcile.NamespaceDeriver{}).Derive(snapshot)
			if desired.AttachedSources[source.Name] != source.UID {
				t.Fatal("collector lost automatically managed attachment authority")
			}
			// Isolate publication from allocation writes. Build the plan before
			// changing the API; the informer snapshot remains intentionally stale.
			snapshot.Allocations = desired.Allocations
			publisher := newTestIntentPublisher()
			planner := reconcile.PublicationPlanner{Allocations: controller, Publisher: publisher, Validator: controller.verifyPublication}
			plan := planner.Plan(snapshot, desired)
			assignment := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: namespaceConfigName, Namespace: source.Name}, Data: map[string]string{controllerSettingKey: ""}}
			if change == "other controller" || change == "unassigned" {
				if change == "other controller" {
					assignment.Data[controllerSettingKey] = "other/skupper-controller"
				}
				if _, err := clients.GetKubeClient().CoreV1().ConfigMaps(source.Name).Create(ctx, assignment, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			} else {
				updated := source.DeepCopy()
				if change == "replacement namespace" {
					updated.UID = "new-source-uid"
				} else {
					now := metav1.Now()
					updated.DeletionTimestamp = &now
				}
				if _, err := clients.GetKubeClient().CoreV1().Namespaces().Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			report := (reconcile.Executor{}).Execute(ctx, plan)
			if !report.NeedsRetry() || len(publisher.published) != 0 {
				t.Fatalf("stale source authority permitted publication: %#v", report)
			}
			if change != "other controller" && change != "unassigned" {
				return
			}
			if err := controller.informers.configMaps.GetStore().Add(assignment); err != nil {
				t.Fatal(err)
			}
			snapshot, err = controller.Collect(ctx, namespace.Name)
			if err != nil {
				t.Fatal(err)
			}
			desired = (reconcile.NamespaceDeriver{}).Derive(snapshot)
			for _, intent := range desired.Intents {
				if len(intent.ServiceConnectors) != 0 {
					t.Fatal("recollected plan retained the forbidden attachment")
				}
			}
			if len(desired.Statuses.Attached) != 0 || len(desired.Statuses.Bindings) != 1 || desired.Statuses.Bindings[0].Status.StatusType != skupperv2alpha1.StatusError {
				t.Fatalf("wrong ownership-conflict projection: %#v", desired.Statuses)
			}
			if err := controller.ApplyStatuses(ctx, snapshot.Namespace, desired.Statuses); err != nil {
				t.Fatalf("foreign source blocked local status: %v", err)
			}
			foreign, err := api.AttachedConnectors(source.Name).Get(ctx, attached.Name, metav1.GetOptions{})
			if err != nil || foreign.Status.StatusType != "" {
				t.Fatalf("foreign status was touched: object=%#v err=%v", foreign, err)
			}
			updatedSite, err := api.Sites(namespace.Name).Get(ctx, site.Name, metav1.GetOptions{})
			if err != nil || updatedSite.Status.StatusType == "" {
				t.Fatalf("Site status was starved: object=%#v err=%v", updatedSite, err)
			}
			assignment.Data[controllerSettingKey] = "controller-ns/skupper-controller"
			if _, err := clients.GetKubeClient().CoreV1().ConfigMaps(source.Name).Update(ctx, assignment, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := controller.informers.configMaps.GetStore().Update(assignment); err != nil {
				t.Fatal(err)
			}
			snapshot, err = controller.Collect(ctx, namespace.Name)
			if err != nil {
				t.Fatal(err)
			}
			desired = (reconcile.NamespaceDeriver{}).Derive(snapshot)
			if desired.AttachedSources[source.Name] != source.UID {
				t.Fatal("restoring same-controller assignment did not restore attachment")
			}
			snapshot.Allocations = desired.Allocations
			if report := (reconcile.Executor{}).Execute(ctx, planner.Plan(snapshot, desired)); report.NeedsRetry() || len(publisher.published) != 1 {
				t.Fatalf("restored attachment did not publish: %#v", report)
			}
		})
	}
}
