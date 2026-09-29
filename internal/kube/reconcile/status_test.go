package reconcile

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

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
		Application: &routercontrol.ApplicationReport{SessionID: "session-current", IntentDigest: digest, RealizationID: "router-realization", State: routercontrol.ApplicationApplied, Resources: []routercontrol.ResourceApplication{{ResourceID: "listener-uid/listener", RealizationID: "listener-realization", State: routercontrol.ApplicationApplied}}},
		Scopes: map[string]ObservationScope{
			routercontrol.ObservationScopeResources: {Fresh: true, Snapshot: routercontrol.ObservationSnapshot{SessionID: "session-current", Knowledge: routercontrol.KnowledgeComplete, Resources: []routercontrol.LocalResourceObservation{{ResourceID: "listener-uid/listener", RealizationID: "listener-realization", Operational: routercontrol.OperationalUp}}}},
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
		snapshot.Observations = map[RouterTarget][]Observation{target: {{Key: routercontrol.SessionKey{Target: target, Identity: routercontrol.SessionIdentity{PodUID: "pod-old"}}, SessionID: "old", AcceptedDigest: digest, Application: &routercontrol.ApplicationReport{SessionID: "old", IntentDigest: digest, RealizationID: "old-realization", State: routercontrol.ApplicationApplied}}}}
	}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if conditionStatus(desired.Statuses.Sites[0].Status.Conditions, skupperv2alpha1.CONDITION_TYPE_RUNNING) != metav1.ConditionUnknown {
		t.Fatal("old Pod session satisfied rollout status")
	}
}

func TestListenerSocketAndReachabilityAreIndependent(t *testing.T) {
	snapshot := baseSnapshot()
	snapshot.EvaluationTime = time.Unix(100, 0).UTC()
	snapshot.Listeners = []*skupperv2alpha1.Listener{listener("orders", "listener-uid", "orders")}
	snapshot.Pods = []*corev1.Pod{routerPod("pod-current", "skupper-router")}
	initial := (NamespaceDeriver{}).Derive(snapshot)
	for target, intent := range initial.Intents {
		_, digest, err := routercontrol.CanonicalIntent(intent)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Observations = map[RouterTarget][]Observation{target: {appliedObservation(target, digest, "pod-current", "listener-uid/listener", routercontrol.OperationalDown, routercontrol.KnowledgeComplete, []routercontrol.LocalAddressObservation{{RoutingKey: "orders", Reachable: true}})}}
	}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	status := desired.Statuses.Listeners[0].Status
	if conditionStatus(status.Conditions, skupperv2alpha1.CONDITION_TYPE_CONFIGURED) != metav1.ConditionTrue || conditionStatus(status.Conditions, skupperv2alpha1.CONDITION_TYPE_MATCHED) != metav1.ConditionTrue {
		t.Fatalf("applied listener with reachable address lost independent states: %#v", status.Conditions)
	}
	if conditionStatus(status.Conditions, skupperv2alpha1.CONDITION_TYPE_OPERATIONAL) != metav1.ConditionFalse || conditionStatus(status.Conditions, skupperv2alpha1.CONDITION_TYPE_READY) != metav1.ConditionFalse {
		t.Fatalf("failed bind incorrectly made listener ready: %#v", status.Conditions)
	}
}

func TestStatusRequiresFreshMatchingRealizationButIgnoresWarmingRolloutPod(t *testing.T) {
	target := RouterTarget{NamespaceUID: "namespace-uid", SiteUID: "site-uid", RouterGroup: "skupper-router"}
	digest := routercontrol.Digest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	serving := appliedObservation(target, digest, "serving", "listener-uid/listener", routercontrol.OperationalUp, routercontrol.KnowledgeComplete, nil)
	warming := Observation{Key: routercontrol.SessionKey{Target: target, Identity: routercontrol.SessionIdentity{PodUID: "warming"}}, SessionID: "warming-session"}
	evidence := map[RouterTarget]targetEvidence{target: {digest: digest, currentPods: 2, observations: []Observation{warming, serving}}}
	if state := resourcesApplied(evidence, []routercontrol.ResourceID{"listener-uid/listener"}); state.Status != metav1.ConditionTrue {
		t.Fatalf("warming rollout Pod erased verified serving realization: %#v", state)
	}

	stale := serving
	stale.Scopes = map[string]ObservationScope{}
	for name, scope := range serving.Scopes {
		scope.Fresh = false
		stale.Scopes[name] = scope
	}
	evidence[target] = targetEvidence{digest: digest, currentPods: 1, observations: []Observation{stale}}
	if state := resourcesApplied(evidence, []routercontrol.ResourceID{"listener-uid/listener"}); state.Status != metav1.ConditionUnknown {
		t.Fatalf("stale Applied evidence remained configured: %#v", state)
	}

	mismatch := serving
	scope := mismatch.Scopes[routercontrol.ObservationScopeResources]
	scope.Snapshot.Resources[0].RealizationID = "replaced-realization"
	mismatch.Scopes[routercontrol.ObservationScopeResources] = scope
	evidence[target] = targetEvidence{digest: digest, currentPods: 1, observations: []Observation{mismatch}}
	if state := resourcesOperational(evidence, []routercontrol.ResourceID{"listener-uid/listener"}); state.Status != metav1.ConditionUnknown {
		t.Fatalf("replaced realization satisfied operational status: %#v", state)
	}

	if state := resourcesApplied(nil, []routercontrol.ResourceID{"listener-uid/listener"}); state.Status != metav1.ConditionUnknown {
		t.Fatalf("empty evidence was treated as ready: %#v", state)
	}
}

func TestHARequiresOneServingRealizationPerGroup(t *testing.T) {
	snapshot := baseSnapshot()
	snapshot.EvaluationTime = time.Unix(100, 0).UTC()
	snapshot.Sites[0].Spec.HA = true
	snapshot.Pods = []*corev1.Pod{routerPod("primary", "skupper-router"), routerPod("secondary", "skupper-router-2")}
	initial := (NamespaceDeriver{}).Derive(snapshot)
	snapshot.Observations = map[RouterTarget][]Observation{}
	for target, intent := range initial.Intents {
		if target.RouterGroup != "skupper-router" {
			continue
		}
		_, digest, _ := routercontrol.CanonicalIntent(intent)
		observation := appliedObservation(target, digest, "primary", "", routercontrol.OperationalUp, routercontrol.KnowledgeComplete, nil)
		observation.Application.Resources = nil
		observation.Scopes[routercontrol.ObservationScopeResources] = ObservationScope{Fresh: true, Snapshot: routercontrol.ObservationSnapshot{SessionID: observation.SessionID, Knowledge: routercontrol.KnowledgeComplete}}
		snapshot.Observations[target] = []Observation{observation}
	}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if got := conditionStatus(desired.Statuses.Sites[0].Status.Conditions, skupperv2alpha1.CONDITION_TYPE_RUNNING); got != metav1.ConditionUnknown {
		t.Fatalf("one HA group satisfied Site Running while another was unknown: %s", got)
	}
}

func TestPartialMissingAddressIsUnknownAndHostConnectorCanConfigure(t *testing.T) {
	target := RouterTarget{NamespaceUID: "namespace-uid", SiteUID: "site-uid", RouterGroup: "skupper-router"}
	digest := routercontrol.Digest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	observation := appliedObservation(target, digest, "pod", "listener-uid/listener", routercontrol.OperationalUp, routercontrol.KnowledgePartial, []routercontrol.LocalAddressObservation{{RoutingKey: "other", Reachable: false}})
	state, _ := routingKeysReachable(map[RouterTarget]targetEvidence{target: {digest: digest, currentPods: 1, observations: []Observation{observation}}}, []string{"orders"})
	if state.Status != metav1.ConditionUnknown {
		t.Fatalf("partial scope missing exact key was treated as known-unmatched: %#v", state)
	}

	snapshot := baseSnapshot()
	snapshot.EvaluationTime = time.Unix(100, 0).UTC()
	snapshot.Connectors = []*skupperv2alpha1.Connector{{ObjectMeta: metav1.ObjectMeta{Name: "database", Namespace: "site", UID: "connector-uid"}, Spec: skupperv2alpha1.ConnectorSpec{RoutingKey: "database", Host: "database.example", Port: 5432, Type: "tcp"}}}
	snapshot.Pods = []*corev1.Pod{routerPod("pod", "skupper-router")}
	initial := (NamespaceDeriver{}).Derive(snapshot)
	for connectorTarget, intent := range initial.Intents {
		_, connectorDigest, _ := routercontrol.CanonicalIntent(intent)
		snapshot.Observations = map[RouterTarget][]Observation{connectorTarget: {appliedObservation(connectorTarget, connectorDigest, "pod", "connector-uid/connector", routercontrol.OperationalUp, routercontrol.KnowledgeComplete, nil)}}
	}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if got := conditionStatus(desired.Statuses.Connectors[0].Status.Conditions, skupperv2alpha1.CONDITION_TYPE_CONFIGURED); got != metav1.ConditionTrue {
		t.Fatalf("host connector with no selected Pods remained pending: %s", got)
	}
}

func TestSelectedPodStatusesAreQuietAfterJSONRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		attached bool
		host     bool
	}{
		{name: "host Connector", host: true},
		{name: "selector Connector"},
		{name: "AttachedConnector", attached: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := baseSnapshot()
			snapshot.EvaluationTime = time.Unix(100, 0).UTC()
			if test.attached {
				snapshot.SourceNamespaces = map[string]types.UID{"source": "source-uid"}
				snapshot.SourceAssignments = map[string]Assignment{"source": snapshot.Assignment}
				snapshot.Bindings = []*skupperv2alpha1.AttachedConnectorBinding{{ObjectMeta: metav1.ObjectMeta{Name: "database", Namespace: "site", UID: "binding-uid"}, Spec: skupperv2alpha1.AttachedConnectorBindingSpec{ConnectorNamespace: "source", RoutingKey: "database"}}}
				snapshot.Attached = []*skupperv2alpha1.AttachedConnector{{ObjectMeta: metav1.ObjectMeta{Name: "database", Namespace: "source", UID: "attached-uid"}, Spec: skupperv2alpha1.AttachedConnectorSpec{SiteNamespace: "site", Selector: "app=database", Port: 5432}}}
				snapshot.Pods = []*corev1.Pod{readyPod("source", "database-1", "database-pod-uid", map[string]string{"app": "database"})}
			} else {
				connector := &skupperv2alpha1.Connector{ObjectMeta: metav1.ObjectMeta{Name: "database", Namespace: "site", UID: "connector-uid"}, Spec: skupperv2alpha1.ConnectorSpec{RoutingKey: "database", Port: 5432}}
				if test.host {
					connector.Spec.Host = "database.example"
				} else {
					connector.Spec.Selector = "app=database"
					snapshot.Pods = []*corev1.Pod{readyPod("site", "database-1", "database-pod-uid", map[string]string{"app": "database"})}
				}
				snapshot.Connectors = []*skupperv2alpha1.Connector{connector}
			}

			first := (NamespaceDeriver{}).Derive(snapshot)
			if test.attached {
				if len(first.Statuses.Attached) != 1 {
					t.Fatalf("expected initial AttachedConnector status: %#v", first.Statuses.Attached)
				}
				snapshot.Attached[0] = jsonRoundTrip(t, first.Statuses.Attached[0])
			} else {
				if len(first.Statuses.Connectors) != 1 {
					t.Fatalf("expected initial Connector status: %#v", first.Statuses.Connectors)
				}
				snapshot.Connectors[0] = jsonRoundTrip(t, first.Statuses.Connectors[0])
			}
			second := (NamespaceDeriver{}).Derive(snapshot)
			if len(second.Statuses.Connectors) != 0 || len(second.Statuses.Attached) != 0 {
				t.Fatalf("JSON roundtrip re-projected unchanged status: connectors=%d attached=%d", len(second.Statuses.Connectors), len(second.Statuses.Attached))
			}
		})
	}
}

func jsonRoundTrip[T any](t *testing.T, value *T) *T {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result T
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	return &result
}

func TestMultiKeyReachableKeysArePopulatedAndCleared(t *testing.T) {
	snapshot := baseSnapshot()
	snapshot.EvaluationTime = time.Unix(100, 0).UTC()
	snapshot.MultiKeyListeners = []*skupperv2alpha1.MultiKeyListener{{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "site", UID: "multi-uid"}, Spec: skupperv2alpha1.MultiKeyListenerSpec{Host: "orders", Port: 8080, Strategy: skupperv2alpha1.MultiKeyListenerStrategy{Priority: &skupperv2alpha1.PriorityStrategySpec{RoutingKeys: []string{"primary", "backup"}}}}}}
	snapshot.Pods = []*corev1.Pod{routerPod("pod", "skupper-router")}
	initial := (NamespaceDeriver{}).Derive(snapshot)
	for target, intent := range initial.Intents {
		_, digest, _ := routercontrol.CanonicalIntent(intent)
		snapshot.Observations = map[RouterTarget][]Observation{target: {appliedObservation(target, digest, "pod", "multi-uid/listener", routercontrol.OperationalUp, routercontrol.KnowledgeComplete, []routercontrol.LocalAddressObservation{{RoutingKey: "backup", Reachable: true}})}}
	}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if len(desired.Statuses.MultiKey) != 1 || desired.Statuses.MultiKey[0].Status.Strategy == nil || desired.Statuses.MultiKey[0].Status.Strategy.Priority == nil || len(desired.Statuses.MultiKey[0].Status.Strategy.Priority.RoutingKeysReachable) != 1 || desired.Statuses.MultiKey[0].Status.Strategy.Priority.RoutingKeysReachable[0] != "backup" {
		t.Fatalf("reachable MultiKey routing key was not projected: %#v", desired.Statuses.MultiKey)
	}
	snapshot.MultiKeyListeners = []*skupperv2alpha1.MultiKeyListener{desired.Statuses.MultiKey[0].DeepCopy()}
	for target, observations := range snapshot.Observations {
		observation := observations[0]
		observation.Scopes[routercontrol.ObservationScopeAddresses] = ObservationScope{Fresh: true, Snapshot: routercontrol.ObservationSnapshot{SessionID: observation.SessionID, Knowledge: routercontrol.KnowledgeComplete}}
		snapshot.Observations[target] = []Observation{observation}
	}
	desired = (NamespaceDeriver{}).Derive(snapshot)
	if len(desired.Statuses.MultiKey) != 1 || len(desired.Statuses.MultiKey[0].Status.Strategy.Priority.RoutingKeysReachable) != 0 {
		t.Fatalf("stale MultiKey reachable keys were not cleared: %#v", desired.Statuses.MultiKey)
	}
}

func TestWeightedMultiKeyReachableKeysRetainConfiguredWeights(t *testing.T) {
	snapshot := baseSnapshot()
	snapshot.EvaluationTime = time.Unix(100, 0).UTC()
	snapshot.MultiKeyListeners = []*skupperv2alpha1.MultiKeyListener{{ObjectMeta: metav1.ObjectMeta{Name: "weighted", Namespace: "site", UID: "weighted-uid"}, Spec: skupperv2alpha1.MultiKeyListenerSpec{Host: "weighted", Port: 8080, Strategy: skupperv2alpha1.MultiKeyListenerStrategy{Weighted: &skupperv2alpha1.WeightedStrategySpec{RoutingKeys: map[string]uint{"foo": 0, "xfoo": 1}}}}}}
	snapshot.Pods = []*corev1.Pod{routerPod("pod", "skupper-router")}
	initial := (NamespaceDeriver{}).Derive(snapshot)
	for target, intent := range initial.Intents {
		_, digest, _ := routercontrol.CanonicalIntent(intent)
		snapshot.Observations = map[RouterTarget][]Observation{target: {appliedObservation(target, digest, "pod", "weighted-uid/listener", routercontrol.OperationalUp, routercontrol.KnowledgeComplete, []routercontrol.LocalAddressObservation{{RoutingKey: "foo", Reachable: true}, {RoutingKey: "xfoo", Reachable: true}})}}
	}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if len(desired.Statuses.MultiKey) != 1 || desired.Statuses.MultiKey[0].Status.Strategy == nil || desired.Statuses.MultiKey[0].Status.Strategy.Weighted == nil {
		t.Fatalf("weighted status was not projected: %#v", desired.Statuses.MultiKey)
	}
	if got := desired.Statuses.MultiKey[0].Status.Strategy.Weighted.RoutingKeysReachable; len(got) != 2 || got["foo"] != 0 || got["xfoo"] != 1 {
		t.Fatalf("reachable weighted key lost its configured weight: %#v", got)
	}
}

func TestResourceCredentialFailureUsesSafeSpecificDiagnostic(t *testing.T) {
	target := RouterTarget{NamespaceUID: "namespace-uid", SiteUID: "site-uid", RouterGroup: "skupper-router"}
	digest := routercontrol.Digest(strings.Repeat("a", 64))
	observation := appliedObservation(target, digest, "pod", "connector-uid/connector", routercontrol.OperationalUnknown, routercontrol.KnowledgeUnknown, nil)
	observation.Application.State = routercontrol.ApplicationFailed
	observation.Application.Resources[0].State = routercontrol.ApplicationFailed
	observation.Application.Resources[0].Reason = "required traffic credential is unavailable"
	state := resourceApplied(map[RouterTarget]targetEvidence{target: {digest: digest, currentPods: 1, observations: []Observation{observation}}}, "connector-uid/connector")
	if state.Reason != skupperv2alpha1.StatusError || state.Message != "required traffic credential is unavailable" {
		t.Fatalf("safe resource diagnostic was hidden by generic application failure: %#v", state)
	}
}

func appliedObservation(target RouterTarget, digest routercontrol.Digest, podUID string, resourceID routercontrol.ResourceID, operational routercontrol.OperationalState, addressKnowledge routercontrol.Knowledge, addresses []routercontrol.LocalAddressObservation) Observation {
	sessionID := podUID + "-session"
	application := &routercontrol.ApplicationReport{SessionID: sessionID, IntentDigest: digest, RealizationID: podUID + "-router", State: routercontrol.ApplicationApplied}
	resources := []routercontrol.LocalResourceObservation{}
	if resourceID != "" {
		realizationID := string(resourceID) + "-realization"
		application.Resources = []routercontrol.ResourceApplication{{ResourceID: resourceID, RealizationID: realizationID, State: routercontrol.ApplicationApplied}}
		resources = append(resources, routercontrol.LocalResourceObservation{ResourceID: resourceID, RealizationID: realizationID, Operational: operational})
	}
	return Observation{
		Key:            routercontrol.SessionKey{Target: target, Identity: routercontrol.SessionIdentity{PodUID: podUID}},
		SessionID:      sessionID,
		AcceptedDigest: digest,
		Application:    application,
		Scopes: map[string]ObservationScope{
			routercontrol.ObservationScopeResources: {Fresh: true, Snapshot: routercontrol.ObservationSnapshot{SessionID: sessionID, Knowledge: routercontrol.KnowledgeComplete, Resources: resources}},
			routercontrol.ObservationScopeAddresses: {Fresh: true, Snapshot: routercontrol.ObservationSnapshot{SessionID: sessionID, Knowledge: addressKnowledge, Addresses: addresses}},
		},
	}
}

func routerPod(uid, group string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: uid, Namespace: "site", UID: types.UID(uid), Labels: map[string]string{"skupper.io/component": "router", "skupper.io/group": group}}}
}

func conditionStatus(conditions []metav1.Condition, conditionType string) metav1.ConditionStatus {
	condition := meta.FindStatusCondition(conditions, conditionType)
	if condition == nil {
		return ""
	}
	return condition.Status
}
