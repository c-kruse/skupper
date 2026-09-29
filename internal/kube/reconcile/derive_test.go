package reconcile

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

func TestDeriveIsPermutationInvariantAndDoesNotMutateSnapshot(t *testing.T) {
	snapshot := baseSnapshot()
	a := listener("a", "uid-a", "alpha")
	b := listener("b", "uid-b", "beta")
	snapshot.Listeners = []*skupperv2alpha1.Listener{b, a}

	first := (NamespaceDeriver{}).Derive(snapshot)
	snapshot.Listeners = []*skupperv2alpha1.Listener{a, b}
	second := (NamespaceDeriver{}).Derive(snapshot)
	if diff := cmp.Diff(first, second); diff != "" {
		t.Fatalf("derivation depends on input order (-first +second):\n%s", diff)
	}
	if a.Spec.RoutingKey != "alpha" || b.Spec.RoutingKey != "beta" {
		t.Fatal("derivation mutated snapshot inputs")
	}
	for target, intent := range first.Intents {
		intent.ServiceListeners[0].RoutingKeys[0] = "changed"
		first.Intents[target] = intent
		break
	}
	if a.Spec.RoutingKey != "alpha" {
		t.Fatal("derived intent aliases snapshot input")
	}
}

func TestAllocationIsStableWhenEarlierResourceIsInserted(t *testing.T) {
	snapshot := baseSnapshot()
	existing := listener("existing", "uid-z", "existing")
	snapshot.Listeners = []*skupperv2alpha1.Listener{existing}
	first := (NamespaceDeriver{}).Derive(snapshot)
	port := first.Allocations.Ports["uid-z/listener"]
	snapshot.Allocations = first.Allocations
	snapshot.Listeners = append(snapshot.Listeners, listener("new", "uid-a", "new"))
	second := (NamespaceDeriver{}).Derive(snapshot)
	if got := second.Allocations.Ports["uid-z/listener"]; got != port {
		t.Fatalf("existing allocation changed from %d to %d", port, got)
	}
	if second.Allocations.Ports["uid-a/listener"] == port {
		t.Fatal("new listener reused an active allocation")
	}
}

func TestExposePodsByNameIsRejected(t *testing.T) {
	snapshot := baseSnapshot()
	value := listener("unsupported", "uid-listener", "alpha")
	value.Spec.ExposePodsByName = true
	snapshot.Listeners = []*skupperv2alpha1.Listener{value}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if len(desired.Diagnostics) != 1 || desired.Diagnostics[0].Reason != "Unsupported" {
		t.Fatalf("expected Unsupported diagnostic, got %#v", desired.Diagnostics)
	}
	for _, intent := range desired.Intents {
		if len(intent.ServiceListeners) != 0 {
			t.Fatal("unsupported listener was silently included")
		}
	}
}

func TestUnknownIsNotKnownEmpty(t *testing.T) {
	unknown := Observation{Completeness: Unknown, Fresh: true}
	stale := Observation{Completeness: Complete, Fresh: false}
	empty := Observation{Completeness: Complete, Fresh: true}
	if unknown.KnownEmpty() || stale.KnownEmpty() || !empty.KnownEmpty() {
		t.Fatalf("knowledge semantics are wrong: unknown=%v stale=%v empty=%v", unknown.KnownEmpty(), stale.KnownEmpty(), empty.KnownEmpty())
	}
}

func baseSnapshot() Snapshot {
	return Snapshot{
		Namespace:   NamespaceIdentity{Name: "site", UID: "namespace-uid"},
		Assignment:  Assignment{Controller: "controllers/skupper-controller", Controlled: true},
		Sites:       []*skupperv2alpha1.Site{{ObjectMeta: metav1.ObjectMeta{Name: "site", Namespace: "site", UID: "site-uid"}}},
		Allocations: AllocationState{Ports: map[string]int{}},
	}
}

func listener(name string, uid types.UID, routingKey string) *skupperv2alpha1.Listener {
	return &skupperv2alpha1.Listener{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "site", UID: uid}, Spec: skupperv2alpha1.ListenerSpec{RoutingKey: routingKey, Host: "0.0.0.0", Port: 8080, Type: "tcp"}}
}

func readyPod(namespace, name string, uid types.UID, podLabels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: uid, Labels: podLabels}, Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.1", Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
}
