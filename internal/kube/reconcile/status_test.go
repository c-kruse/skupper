package reconcile

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/skupperproject/skupper/internal/routercontrol"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

func TestStatusRequiresCurrentPodExactSessionAndFreshLocalObservation(t *testing.T) {
	snapshot := baseSnapshot()
	snapshot.EvaluationTime = time.Unix(100, 0).UTC()
	snapshot.Listeners = []*skupperv2alpha1.Listener{listener("orders", "listener-uid", "orders")}
	snapshot.Pods = []*corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "router", Namespace: "site", UID: "pod-current", Labels: map[string]string{"skupper.io/component": "router", "skupper.io/group": "skupper-router"}}}}
	initial := (NamespaceDeriver{}).Derive(snapshot)
	var target RouterTarget
	var intent routercontrol.RouterIntent
	for target, intent = range initial.Intents {
		break
	}
	_, digest, err := routercontrol.CanonicalIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Observations = map[RouterTarget][]Observation{target: {{
		Key: routercontrol.SessionKey{Target: target, Identity: routercontrol.SessionIdentity{PodUID: "pod-current"}}, SessionID: "session-current", AcceptedDigest: digest,
		Application: &routercontrol.ApplicationReport{SessionID: "session-current", IntentDigest: digest, State: routercontrol.ApplicationApplied, Resources: []routercontrol.ResourceApplication{{ResourceID: "listener-uid/listener", State: routercontrol.ApplicationApplied}}},
		Scopes: map[string]ObservationScope{
			routercontrol.ObservationScopeResources: {Fresh: true, Snapshot: routercontrol.ObservationSnapshot{SessionID: "session-current", Knowledge: routercontrol.KnowledgeComplete, Resources: []routercontrol.LocalResourceObservation{{ResourceID: "listener-uid/listener", Operational: routercontrol.OperationalUp}}}},
			routercontrol.ObservationScopeAddresses: {Fresh: true, Snapshot: routercontrol.ObservationSnapshot{SessionID: "session-current", Knowledge: routercontrol.KnowledgeComplete, Addresses: []routercontrol.LocalAddressObservation{{RoutingKey: "orders", Reachable: true}}}},
		},
	}}}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if len(desired.Statuses.Sites) != 1 || conditionStatus(desired.Statuses.Sites[0].Status.Conditions, skupperv2alpha1.CONDITION_TYPE_RUNNING) != metav1.ConditionTrue {
		t.Fatalf("fresh exact-session application did not make Site running: %#v", desired.Statuses.Sites)
	}
	if len(desired.Statuses.Listeners) != 1 || conditionStatus(desired.Statuses.Listeners[0].Status.Conditions, skupperv2alpha1.CONDITION_TYPE_READY) != metav1.ConditionTrue {
		t.Fatalf("fresh exact local address did not make Listener ready: %#v", desired.Statuses.Listeners)
	}

	wrong := snapshot.Observations[target][0]
	wrong.SessionID = "replacement-session"
	snapshot.Observations[target] = []Observation{wrong}
	desired = (NamespaceDeriver{}).Derive(snapshot)
	if conditionStatus(desired.Statuses.Sites[0].Status.Conditions, skupperv2alpha1.CONDITION_TYPE_RUNNING) != metav1.ConditionUnknown {
		t.Fatal("report from replaced session satisfied Site status")
	}

	stale := snapshot.Observations[target][0]
	stale.SessionID = "session-current"
	stale.Scopes[routercontrol.ObservationScopeResources] = ObservationScope{Fresh: false, Snapshot: stale.Scopes[routercontrol.ObservationScopeResources].Snapshot}
	snapshot.Observations[target] = []Observation{stale}
	desired = (NamespaceDeriver{}).Derive(snapshot)
	if conditionStatus(desired.Statuses.Sites[0].Status.Conditions, skupperv2alpha1.CONDITION_TYPE_RUNNING) != metav1.ConditionUnknown {
		t.Fatal("stale local observation satisfied Site status")
	}
}

func TestStatusRejectsOldPodDuringRollout(t *testing.T) {
	snapshot := baseSnapshot()
	snapshot.EvaluationTime = time.Unix(100, 0).UTC()
	snapshot.Pods = []*corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "new", Namespace: "site", UID: "pod-new", Labels: map[string]string{"skupper.io/component": "router", "skupper.io/group": "skupper-router"}}}}
	initial := (NamespaceDeriver{}).Derive(snapshot)
	for target, intent := range initial.Intents {
		_, digest, _ := routercontrol.CanonicalIntent(intent)
		snapshot.Observations = map[RouterTarget][]Observation{target: {{Key: routercontrol.SessionKey{Target: target, Identity: routercontrol.SessionIdentity{PodUID: "pod-old"}}, SessionID: "old", AcceptedDigest: digest, Application: &routercontrol.ApplicationReport{SessionID: "old", IntentDigest: digest, State: routercontrol.ApplicationApplied}}}}
	}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if conditionStatus(desired.Statuses.Sites[0].Status.Conditions, skupperv2alpha1.CONDITION_TYPE_RUNNING) != metav1.ConditionUnknown {
		t.Fatal("old Pod session satisfied rollout status")
	}
}

func conditionStatus(conditions []metav1.Condition, conditionType string) metav1.ConditionStatus {
	condition := meta.FindStatusCondition(conditions, conditionType)
	if condition == nil {
		return ""
	}
	return condition.Status
}
