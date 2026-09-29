package reconcile

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

func TestAttachedReferenceMoveAndTombstoneInvalidateOldAndNewNamespaces(t *testing.T) {
	store := NewInputStore()
	definition := &skupperv2alpha1.AttachedConnector{ObjectMeta: metav1.ObjectMeta{Namespace: "source", Name: "orders"}, Spec: skupperv2alpha1.AttachedConnectorSpec{SiteNamespace: "old-site"}}
	if diff := cmp.Diff([]string{"old-site", "source"}, store.UpdateAttached("source/orders", definition)); diff != "" {
		t.Fatalf("initial invalidation mismatch (-want +got):\n%s", diff)
	}
	moved := definition.DeepCopy()
	moved.Spec.SiteNamespace = "new-site"
	if diff := cmp.Diff([]string{"new-site", "old-site", "source"}, store.UpdateAttached("source/orders", moved)); diff != "" {
		t.Fatalf("move invalidation mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"new-site", "source"}, store.UpdateAttached("source/orders", nil)); diff != "" {
		t.Fatalf("tombstone invalidation mismatch (-want +got):\n%s", diff)
	}
}

func TestPodLabelMoveInvalidatesOldAndNewAuthorizedSelectors(t *testing.T) {
	store := NewInputStore()
	store.UpdateAttached("source/orders", &skupperv2alpha1.AttachedConnector{ObjectMeta: metav1.ObjectMeta{Namespace: "source", Name: "orders"}, Spec: skupperv2alpha1.AttachedConnectorSpec{SiteNamespace: "site", Selector: "app=orders"}})
	store.UpdateBinding("site/orders", &skupperv2alpha1.AttachedConnectorBinding{ObjectMeta: metav1.ObjectMeta{Namespace: "site", Name: "orders"}, Spec: skupperv2alpha1.AttachedConnectorBindingSpec{ConnectorNamespace: "source", RoutingKey: "orders"}})
	pod := readyPod("source", "orders-1", "pod-uid", map[string]string{"app": "orders"})
	store.UpdatePod("source/orders-1", pod)
	moved := pod.DeepCopy()
	moved.Labels = map[string]string{"app": "other"}
	if diff := cmp.Diff([]string{"site", "source"}, store.UpdatePod("source/orders-1", moved)); diff != "" {
		t.Fatalf("pod move invalidation mismatch (-want +got):\n%s", diff)
	}
}

func TestCollectedSnapshotOwnsDeepCopies(t *testing.T) {
	store := NewInputStore()
	store.SetNamespace("site", "namespace-uid", Assignment{Controlled: true})
	site := &skupperv2alpha1.Site{ObjectMeta: metav1.ObjectMeta{Namespace: "site", Name: "site", UID: "site-uid"}, Spec: skupperv2alpha1.SiteSpec{Settings: map[string]string{"router-logging": "info"}}}
	store.SetInputs("site", NamespaceInputs{Sites: []*skupperv2alpha1.Site{site}})
	first, err := store.Collect(context.Background(), "site")
	if err != nil {
		t.Fatal(err)
	}
	first.Sites[0].Spec.Settings["router-logging"] = "debug"
	second, err := store.Collect(context.Background(), "site")
	if err != nil {
		t.Fatal(err)
	}
	if second.Sites[0].Spec.Settings["router-logging"] != "info" {
		t.Fatal("snapshot mutation leaked into input store")
	}
	if site.Spec.Settings["router-logging"] != "info" {
		t.Fatal("SetInputs retained caller-owned object")
	}
}
