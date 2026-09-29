package reconcile

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/skupperproject/skupper/internal/routercontrol"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
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

func TestLinkSelectsOnlyEndpointForSiteRole(t *testing.T) {
	for _, test := range []struct {
		name string
		edge bool
		role string
		host string
	}{{name: "edge", edge: true, role: "edge", host: "edge.example"}, {name: "interior", role: "inter-router", host: "interior.example"}} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := baseSnapshot()
			snapshot.Sites[0].Spec.Edge = test.edge
			snapshot.Links = []*skupperv2alpha1.Link{{ObjectMeta: metav1.ObjectMeta{Name: "grant-link", Namespace: "site", UID: "link-uid"}, Spec: skupperv2alpha1.LinkSpec{Endpoints: []skupperv2alpha1.Endpoint{{Name: "edge", Host: "edge.example", Port: "45671"}, {Name: "inter-router", Host: "interior.example", Port: "55671"}}}}}
			desired := (NamespaceDeriver{}).Derive(snapshot)
			for _, intent := range desired.Intents {
				if len(intent.RouterConnections) != 1 || intent.RouterConnections[0].Role != test.role || intent.RouterConnections[0].Host != test.host {
					t.Fatalf("wrong role endpoint selected: %#v", intent.RouterConnections)
				}
			}
		})
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
	unknown := Observation{Scopes: map[string]ObservationScope{"resources": {Fresh: true, Snapshot: routercontrol.ObservationSnapshot{Knowledge: routercontrol.KnowledgeUnknown}}}}
	stale := Observation{Scopes: map[string]ObservationScope{"resources": {Fresh: false, Snapshot: routercontrol.ObservationSnapshot{Knowledge: routercontrol.KnowledgeComplete}}}}
	empty := Observation{Scopes: map[string]ObservationScope{"resources": {Fresh: true, Snapshot: routercontrol.ObservationSnapshot{Knowledge: routercontrol.KnowledgeComplete}}}}
	if unknown.KnownEmpty("resources") || stale.KnownEmpty("resources") || !empty.KnownEmpty("resources") {
		t.Fatalf("knowledge semantics are wrong: unknown=%v stale=%v empty=%v", unknown.KnownEmpty("resources"), stale.KnownEmpty("resources"), empty.KnownEmpty("resources"))
	}
}

func TestDerivationDoesNotAliasBootstrapTrust(t *testing.T) {
	snapshot := baseSnapshot()
	snapshot.Bootstrap.PublicCA = []byte("public-ca")
	desired := (NamespaceDeriver{}).Derive(snapshot)
	desired.Bootstrap.PublicCA[0] = 'X'
	if string(snapshot.Bootstrap.PublicCA) != "public-ca" {
		t.Fatal("derived bootstrap mutated snapshot trust bytes")
	}
}

func TestDefaultRouterServiceAccountHasOnlySecretReadPermissions(t *testing.T) {
	snapshot := baseSnapshot()
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if desired.ServiceAccount == nil || desired.Role == nil || desired.RoleBinding == nil {
		t.Fatal("default router ServiceAccount prerequisites were not derived")
	}
	if desired.ServiceAccount.Name != "skupper-router" || len(desired.Role.Rules) != 1 {
		t.Fatalf("unexpected router prerequisites: %#v %#v", desired.ServiceAccount, desired.Role)
	}
	rule := desired.Role.Rules[0]
	if diff := cmp.Diff([]string{"secrets"}, rule.Resources); diff != "" {
		t.Fatalf("router Role has non-Secret resources (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"get", "list", "watch"}, rule.Verbs); diff != "" {
		t.Fatalf("router Role has unexpected verbs (-want +got):\n%s", diff)
	}
	if !metav1.IsControlledBy(desired.ServiceAccount, snapshot.Sites[0]) || !metav1.IsControlledBy(desired.Role, snapshot.Sites[0]) || !metav1.IsControlledBy(desired.RoleBinding, snapshot.Sites[0]) {
		t.Fatal("router prerequisites are not controller-owned by Site")
	}

	snapshot.Sites[0].Spec.ServiceAccount = "custom-router"
	desired = (NamespaceDeriver{}).Derive(snapshot)
	if desired.ServiceAccount != nil || desired.Role != nil || desired.RoleBinding != nil {
		t.Fatal("custom service account unexpectedly derived generated RBAC")
	}
}

func TestForeignRouterPrerequisitesDiagnoseSiteAndRecover(t *testing.T) {
	tests := []struct {
		name    string
		message string
		set     func(*Snapshot)
		clear   func(*Snapshot)
	}{
		{
			name:    "ServiceAccount",
			message: "router ServiceAccount site/skupper-router is not controlled by Site UID site-uid",
			set: func(snapshot *Snapshot) {
				snapshot.ServiceAccounts = []*corev1.ServiceAccount{{ObjectMeta: metav1.ObjectMeta{Name: "skupper-router", Namespace: "site"}}}
			},
			clear: func(snapshot *Snapshot) { snapshot.ServiceAccounts = nil },
		},
		{
			name:    "Role",
			message: "router Role site/skupper-router is not controlled by Site UID site-uid",
			set: func(snapshot *Snapshot) {
				snapshot.Roles = []*rbacv1.Role{{ObjectMeta: metav1.ObjectMeta{Name: "skupper-router", Namespace: "site"}}}
			},
			clear: func(snapshot *Snapshot) { snapshot.Roles = nil },
		},
		{
			name:    "RoleBinding",
			message: "router RoleBinding site/skupper-router is not controlled by Site UID site-uid",
			set: func(snapshot *Snapshot) {
				snapshot.RoleBindings = []*rbacv1.RoleBinding{{ObjectMeta: metav1.ObjectMeta{Name: "skupper-router", Namespace: "site"}}}
			},
			clear: func(snapshot *Snapshot) { snapshot.RoleBindings = nil },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := baseSnapshot()
			test.set(&snapshot)
			desired := (NamespaceDeriver{}).Derive(snapshot)
			if len(desired.Statuses.Sites) != 1 || desired.Statuses.Sites[0].Status.StatusType != skupperv2alpha1.StatusError || desired.Statuses.Sites[0].Status.Message != test.message {
				t.Fatalf("foreign %s did not produce actionable Site error: %#v", test.name, desired.Statuses.Sites)
			}
			test.clear(&snapshot)
			desired = (NamespaceDeriver{}).Derive(snapshot)
			if len(desired.Statuses.Sites) != 1 || conditionStatus(desired.Statuses.Sites[0].Status.Conditions, skupperv2alpha1.CONDITION_TYPE_CONFIGURED) != metav1.ConditionTrue {
				t.Fatalf("Site ownership error did not recover after %s conflict cleared: %#v", test.name, desired.Statuses.Sites)
			}
		})
	}
}

func TestCustomServiceAccountIgnoresUnneededDefaultPrerequisites(t *testing.T) {
	snapshot := baseSnapshot()
	snapshot.Sites[0].Spec.ServiceAccount = "custom-router"
	snapshot.ServiceAccounts = []*corev1.ServiceAccount{{ObjectMeta: metav1.ObjectMeta{Name: "skupper-router", Namespace: "site"}}}
	snapshot.Roles = []*rbacv1.Role{{ObjectMeta: metav1.ObjectMeta{Name: "skupper-router", Namespace: "site"}}}
	snapshot.RoleBindings = []*rbacv1.RoleBinding{{ObjectMeta: metav1.ObjectMeta{Name: "skupper-router", Namespace: "site"}}}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if configured := conditionStatus(desired.Statuses.Sites[0].Status.Conditions, skupperv2alpha1.CONDITION_TYPE_CONFIGURED); configured != metav1.ConditionTrue {
		t.Fatalf("unneeded default prerequisites blocked custom ServiceAccount Site: %#v", desired.Statuses.Sites[0].Status)
	}
}

func TestConnectorSelectorsAreRestrictedToTheirSourceNamespace(t *testing.T) {
	snapshot := baseSnapshot()
	snapshot.Connectors = []*skupperv2alpha1.Connector{{ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: "site", UID: "local-uid"}, Spec: skupperv2alpha1.ConnectorSpec{RoutingKey: "local", Selector: "app=same", Port: 8080}}}
	snapshot.Bindings = []*skupperv2alpha1.AttachedConnectorBinding{{ObjectMeta: metav1.ObjectMeta{Name: "remote", Namespace: "site", UID: "binding-uid"}, Spec: skupperv2alpha1.AttachedConnectorBindingSpec{ConnectorNamespace: "source-a", RoutingKey: "remote"}}}
	snapshot.Attached = []*skupperv2alpha1.AttachedConnector{{ObjectMeta: metav1.ObjectMeta{Name: "remote", Namespace: "source-a", UID: "attached-uid"}, Spec: skupperv2alpha1.AttachedConnectorSpec{SiteNamespace: "site", Selector: "app=same", Port: 9090}}}
	snapshot.Pods = []*corev1.Pod{readyPod("site", "local", "local-pod", map[string]string{"app": "same"}), readyPod("source-a", "allowed", "allowed-pod", map[string]string{"app": "same"}), readyPod("source-b", "wrong-source", "wrong-source-pod", map[string]string{"app": "same"}), readyPod("unrelated", "unrelated", "unrelated-pod", map[string]string{"app": "same"})}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	var connectors []routercontrol.ServiceConnector
	for _, intent := range desired.Intents {
		connectors = intent.ServiceConnectors
		break
	}
	if len(connectors) != 2 {
		t.Fatalf("expected two connectors, got %#v", connectors)
	}
	byKey := map[string]routercontrol.ServiceConnector{}
	for _, connector := range connectors {
		byKey[connector.RoutingKey] = connector
	}
	if diff := cmp.Diff([]routercontrol.Endpoint{{ID: "local-pod", Host: "10.0.0.1", Port: 8080}}, byKey["local"].Endpoints); diff != "" {
		t.Fatalf("local endpoint mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]routercontrol.Endpoint{{ID: "allowed-pod", Host: "10.0.0.1", Port: 9090}}, byKey["remote"].Endpoints); diff != "" {
		t.Fatalf("attached endpoint mismatch (-want +got):\n%s", diff)
	}
}

func TestListenerUsesRouterBindHostAndServerTLS(t *testing.T) {
	snapshot := baseSnapshot()
	value := listener("orders", "listener-uid", "orders")
	value.Spec.Host = "orders.example"
	value.Spec.TlsCredentials = "orders-tls"
	snapshot.Listeners = []*skupperv2alpha1.Listener{value}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	for _, intent := range desired.Intents {
		got := intent.ServiceListeners[0]
		if got.Host != "0.0.0.0" || got.TLS.Mode != routercontrol.TLSModeServer {
			t.Fatalf("unexpected listener intent: %#v", got)
		}
		return
	}
	t.Fatal("no intent derived")
}

func TestListenerServicePortsHaveValidDistinctNamesAndConflictsStayOutOfService(t *testing.T) {
	snapshot := baseSnapshot()
	first := listener("a.listener.name.longer.than.fifteen", "listener-uid", "orders")
	first.Spec.Host = "orders"
	second := listener("same", "second-uid", "payments")
	second.Spec.Host = "orders"
	second.Spec.Port = 9090
	snapshot.Listeners = []*skupperv2alpha1.Listener{first, second}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if len(desired.ListenerServices) != 1 || len(desired.ListenerServices[0].Spec.Ports) != 2 {
		t.Fatalf("expected one two-port Service, got %#v", desired.ListenerServices)
	}
	portNames := map[string]bool{}
	for _, port := range desired.ListenerServices[0].Spec.Ports {
		if len(port.Name) > 15 || port.Name == "" || portNames[port.Name] {
			t.Fatalf("ServicePort names are not valid and distinct: %#v", desired.ListenerServices[0].Spec.Ports)
		}
		portNames[port.Name] = true
	}

	second.Spec.Port = first.Spec.Port
	desired = (NamespaceDeriver{}).Derive(snapshot)
	if len(desired.ListenerServices) != 0 {
		t.Fatalf("conflicting port was rendered into an invalid Service: %#v", desired.ListenerServices)
	}
	if len(desired.Diagnostics) != 2 || desired.Diagnostics[0].Reason != "ServicePortConflict" || desired.Diagnostics[1].Reason != "ServicePortConflict" {
		t.Fatalf("shared host/port conflict was not diagnosed for both resources: %#v", desired.Diagnostics)
	}
	for _, intent := range desired.Intents {
		if len(intent.ServiceListeners) != 2 {
			t.Fatalf("Service exposure conflict prevented valid intent publication: %#v", intent.ServiceListeners)
		}
	}
}

func TestForeignListenerServiceIsDiagnosedWithoutBlockingOtherIntent(t *testing.T) {
	snapshot := baseSnapshot()
	foreign := listener("foreign", "foreign-listener", "foreign-key")
	foreign.Spec.Host = "claimed"
	valid := listener("valid", "valid-listener", "valid-key")
	valid.Spec.Host = "available"
	snapshot.Listeners = []*skupperv2alpha1.Listener{foreign, valid}
	snapshot.Services = []*corev1.Service{{ObjectMeta: metav1.ObjectMeta{Name: "claimed", Namespace: "site", UID: "foreign-service"}}}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if len(desired.ListenerServices) != 1 || desired.ListenerServices[0].Name != "available" {
		t.Fatalf("foreign Service blocked or was included with valid exposure: %#v", desired.ListenerServices)
	}
	if len(desired.Diagnostics) != 1 || desired.Diagnostics[0].Resource != foreign.UID || desired.Diagnostics[0].Reason != "ForeignService" {
		t.Fatalf("foreign ownership was not isolated to affected Listener: %#v", desired.Diagnostics)
	}
	for _, intent := range desired.Intents {
		if len(intent.ServiceListeners) != 2 {
			t.Fatalf("foreign exposure prevented unrelated router intent: %#v", intent.ServiceListeners)
		}
	}
}

func TestWeightedMultiKeyListenerIsNotFlattened(t *testing.T) {
	snapshot := baseSnapshot()
	snapshot.MultiKeyListeners = []*skupperv2alpha1.MultiKeyListener{{ObjectMeta: metav1.ObjectMeta{Name: "weighted", Namespace: "site", UID: "weighted-uid"}, Spec: skupperv2alpha1.MultiKeyListenerSpec{Port: 8080, Strategy: skupperv2alpha1.MultiKeyListenerStrategy{Weighted: &skupperv2alpha1.WeightedStrategySpec{RoutingKeys: map[string]uint{"a": 1, "b": 5}}}}}}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if len(desired.Diagnostics) != 0 {
		t.Fatalf("weighted strategy was diagnosed: %#v", desired.Diagnostics)
	}
	for _, intent := range desired.Intents {
		if len(intent.ServiceListeners) != 1 || intent.ServiceListeners[0].RoutingStrategy != routercontrol.RoutingStrategyWeighted || intent.ServiceListeners[0].RoutingKeyWeights["a"] != 1 || intent.ServiceListeners[0].RoutingKeyWeights["b"] != 5 {
			t.Fatalf("weighted listener was flattened or changed: %#v", intent.ServiceListeners)
		}
	}
}

func TestPriorityMultiKeyListenerPreservesExistingIntentDefaults(t *testing.T) {
	snapshot := baseSnapshot()
	snapshot.MultiKeyListeners = []*skupperv2alpha1.MultiKeyListener{{ObjectMeta: metav1.ObjectMeta{Name: "priority", Namespace: "site", UID: "priority-uid"}, Spec: skupperv2alpha1.MultiKeyListenerSpec{Port: 8080, Strategy: skupperv2alpha1.MultiKeyListenerStrategy{Priority: &skupperv2alpha1.PriorityStrategySpec{RoutingKeys: []string{"xfoo", "foo"}}}}}}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	for _, intent := range desired.Intents {
		listener := intent.ServiceListeners[0]
		if listener.RoutingStrategy != "" || listener.RoutingKeyWeights != nil || listener.RoutingKeys[0] != "xfoo" || listener.RoutingKeys[1] != "foo" {
			t.Fatalf("priority intent defaults or exact order changed: %#v", listener)
		}
	}
}

func TestUnsupportedUDPIsIsolatedFromValidTCPIntent(t *testing.T) {
	snapshot := baseSnapshot()
	udpListener := listener("udp", "udp-listener", "udp")
	udpListener.Spec.Type = "udp"
	tcpListener := listener("tcp", "tcp-listener", "tcp")
	udpConnector := &skupperv2alpha1.Connector{ObjectMeta: metav1.ObjectMeta{Name: "udp", Namespace: "site", UID: "udp-connector"}, Spec: skupperv2alpha1.ConnectorSpec{RoutingKey: "udp", Host: "udp.example", Port: 8080, Type: "udp"}}
	tcpConnector := &skupperv2alpha1.Connector{ObjectMeta: metav1.ObjectMeta{Name: "tcp", Namespace: "site", UID: "tcp-connector"}, Spec: skupperv2alpha1.ConnectorSpec{RoutingKey: "tcp", Host: "tcp.example", Port: 8080, Type: "tcp"}}
	snapshot.Listeners = []*skupperv2alpha1.Listener{udpListener, tcpListener}
	snapshot.Connectors = []*skupperv2alpha1.Connector{udpConnector, tcpConnector}

	desired := (NamespaceDeriver{}).Derive(snapshot)
	if len(desired.Diagnostics) != 2 {
		t.Fatalf("expected one diagnostic per UDP resource, got %#v", desired.Diagnostics)
	}
	for _, diagnostic := range desired.Diagnostics {
		if diagnostic.Reason != "UnsupportedProtocol" || (diagnostic.Resource != udpListener.UID && diagnostic.Resource != udpConnector.UID) {
			t.Fatalf("UDP diagnostic was not resource-specific: %#v", desired.Diagnostics)
		}
	}
	for _, intent := range desired.Intents {
		if len(intent.ServiceListeners) != 1 || intent.ServiceListeners[0].ID != "tcp-listener/listener" || len(intent.ServiceConnectors) != 1 || intent.ServiceConnectors[0].ID != "tcp-connector/connector" {
			t.Fatalf("UDP contribution blocked or entered valid TCP intent: listeners=%#v connectors=%#v", intent.ServiceListeners, intent.ServiceConnectors)
		}
		if _, _, err := routercontrol.CanonicalIntent(intent); err != nil {
			t.Fatalf("isolated TCP intent is not publishable: %v", err)
		}
	}
}

func TestZeroWeightedListenerPreservesKubernetesCompatibility(t *testing.T) {
	snapshot := baseSnapshot()
	snapshot.MultiKeyListeners = []*skupperv2alpha1.MultiKeyListener{{ObjectMeta: metav1.ObjectMeta{Name: "zero", Namespace: "site", UID: "zero-weight"}, Spec: skupperv2alpha1.MultiKeyListenerSpec{Port: 8080, Strategy: skupperv2alpha1.MultiKeyListenerStrategy{Weighted: &skupperv2alpha1.WeightedStrategySpec{RoutingKeys: map[string]uint{"foo": 0, "xfoo": 1}}}}}}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if len(desired.Diagnostics) != 0 {
		t.Fatalf("zero weight accepted by the Kubernetes API was diagnosed: %#v", desired.Diagnostics)
	}
	for _, intent := range desired.Intents {
		if len(intent.ServiceListeners) != 1 {
			t.Fatalf("zero-weight listener was not derived: %#v", intent.ServiceListeners)
		}
		foo, found := intent.ServiceListeners[0].RoutingKeyWeights["foo"]
		if !found || foo != 0 || intent.ServiceListeners[0].RoutingKeyWeights["xfoo"] != 1 {
			t.Fatalf("zero weight was omitted or changed: %#v", intent.ServiceListeners)
		}
	}
}

func TestEdgeHASiteIsRejectedAndGetsIdentityStatus(t *testing.T) {
	snapshot := baseSnapshot()
	snapshot.Assignment.ControllerVersion = "test-version"
	snapshot.Sites[0].Spec.Edge = true
	snapshot.Sites[0].Spec.HA = true
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if desired.Site != nil || desired.SiteUID != "" || desired.ServiceAccount != nil || len(desired.Intents) != 0 || len(desired.Statuses.Sites) != 1 {
		t.Fatalf("invalid edge HA Site was realized or left statusless: %#v", desired)
	}
	status := desired.Statuses.Sites[0].Status
	if status.StatusType != skupperv2alpha1.StatusError || status.DefaultIssuer != "skupper-site-ca" {
		t.Fatalf("invalid Site did not receive an Error/default issuer status: %#v", status)
	}
	if status.Controller == nil || status.Controller.Name != "skupper-controller" || status.Controller.Namespace != "controllers" || status.Controller.Version != "test-version" {
		t.Fatalf("controller identity was not projected: %#v", status.Controller)
	}
}

func TestCompetingSitesEachGetConflictStatus(t *testing.T) {
	snapshot := baseSnapshot()
	snapshot.Sites = append(snapshot.Sites, &skupperv2alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "site", UID: "other-site-uid"}})
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if desired.Site != nil || len(desired.Intents) != 0 || len(desired.Statuses.Sites) != 2 {
		t.Fatalf("competing Sites were selected or left statusless: %#v", desired)
	}
	for _, site := range desired.Statuses.Sites {
		if site.Status.StatusType != skupperv2alpha1.StatusError || site.Status.Message != "multiple Sites exist and no established Site UID identifies the active owner" {
			t.Fatalf("Site %s did not receive conflict status: %#v", site.Name, site.Status)
		}
	}
}

func TestReplacementSiteResetsPortsButRetainsAllocationCAS(t *testing.T) {
	snapshot := baseSnapshot()
	snapshot.Allocations = AllocationState{SiteUID: "deleted-site-uid", ResourceVersion: "17", Ports: map[string]int{"old/listener": 12345}}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if desired.Allocations.SiteUID != snapshot.Sites[0].UID || desired.Allocations.ResourceVersion != "17" || len(desired.Allocations.Ports) != 0 {
		t.Fatalf("replacement allocation did not reset authority under the observed CAS: %#v", desired.Allocations)
	}
}

func TestCompetingSiteCannotReplaceEstablishedOwnerAndGetsQuietError(t *testing.T) {
	snapshot := baseSnapshot()
	snapshot.Allocations.SiteUID = snapshot.Sites[0].UID
	snapshot.Sites = append(snapshot.Sites, &skupperv2alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "site", UID: "other-site-uid"}})
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if desired.SiteUID != "site-uid" || len(desired.Intents) != 1 || len(desired.Statuses.Sites) != 2 {
		t.Fatalf("competitor replaced the active owner or remained statusless: %#v", desired)
	}
	for _, site := range desired.Statuses.Sites {
		if site.Name == "other" {
			if site.Status.StatusType != skupperv2alpha1.StatusError || site.Status.Message != "Site site/site is already active in this namespace" {
				t.Fatalf("competitor did not receive the active owner diagnostic: %#v", site.Status)
			}
		} else if conditionStatus(site.Status.Conditions, skupperv2alpha1.CONDITION_TYPE_CONFIGURED) != metav1.ConditionTrue {
			t.Fatalf("competitor disrupted active Site configuration: %#v", site.Status)
		}
	}
	snapshot.Sites = desired.Statuses.Sites
	if next := (NamespaceDeriver{}).Derive(snapshot); len(next.Statuses.Sites) != 0 {
		t.Fatalf("unchanged competing Site statuses were reprojected: %#v", next.Statuses.Sites)
	}
	snapshot.Sites = snapshot.Sites[1:]
	replacement := (NamespaceDeriver{}).Derive(snapshot)
	if replacement.SiteUID != "other-site-uid" || len(replacement.Statuses.Sites) != 1 || conditionStatus(replacement.Statuses.Sites[0].Status.Conditions, skupperv2alpha1.CONDITION_TYPE_CONFIGURED) != metav1.ConditionTrue {
		t.Fatalf("remaining Site did not recover after old owner removal: %#v", replacement)
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
