package site

import (
	"context"
	"strings"
	"testing"

	routerstatus "github.com/skupperproject/skupper/internal/routerstatus"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	"gotest.tools/v3/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

func eligibleObservation(s *Site, podName string, doc *routerstatus.Document) *routerstatus.Document {
	doc.Router.Hostname = podName
	doc.Router.PodUID = podName + "-uid"
	if s.routerPods == nil {
		s.routerPods = map[string]*corev1.Pod{}
	}
	s.routerPods[s.namespace+"/"+podName] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: s.namespace, UID: types.UID(doc.Router.PodUID)},
	}
	return doc
}

func TestRouterStatusMergesGroupsAndPersistsStatus(t *testing.T) {
	linkDefinition := &skupperv2alpha1.Link{
		ObjectMeta: metav1.ObjectMeta{Name: "east", Namespace: "test"},
		Spec:       skupperv2alpha1.LinkSpec{Endpoints: []skupperv2alpha1.Endpoint{{Name: "inter-router", Host: "east", Port: "55671"}}},
	}
	listener := &skupperv2alpha1.Listener{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "test"},
		Spec:       skupperv2alpha1.ListenerSpec{RoutingKey: "orders", Host: "orders", Port: 8080, Type: "tcp"},
	}
	s, err := newSiteMocks("test", nil, []runtime.Object{linkDefinition, listener}, "", false)
	assert.NilError(t, err)
	// newSiteMocks predates router status and intentionally constructs Site directly.
	s.routerStatus = map[string]*routerstatus.Document{}
	s.SetLegacyNetworkStatus(false)
	managed, err := s.newLink(linkDefinition)
	assert.NilError(t, err)
	s.links[linkDefinition.Name] = managed
	_, err = s.bindings.UpdateListener(listener.Name, listener)
	assert.NilError(t, err)

	down := &routerstatus.Document{
		Version:      1,
		Links:        []routerstatus.Link{{Name: "east", Present: true, ConnectionStatus: "FAILED", Message: "connection refused"}},
		TcpListeners: []routerstatus.TcpListener{{Name: "listener/orders", Present: true, OperStatus: "down", Message: "address unavailable"}},
		Network:      routerstatus.Network{Sites: []routerstatus.Site{{ID: "a"}, {ID: "b"}, {ID: "c"}}},
	}
	up := &routerstatus.Document{
		Version:      1,
		Links:        []routerstatus.Link{{Name: "east", Present: true, ConnectionStatus: "SUCCESS", RemoteSiteID: "remote", RemoteSiteName: "East"}},
		TcpListeners: []routerstatus.TcpListener{{Name: "listener/orders", Present: true, OperStatus: "up"}},
		Network:      routerstatus.Network{Sites: []routerstatus.Site{{ID: "a"}, {ID: "b"}}},
	}
	assert.NilError(t, s.RouterStatusUpdated("group1", eligibleObservation(s, "group1", down)))
	assert.NilError(t, s.RouterStatusUpdated("group2", eligibleObservation(s, "group2", up)))

	link, err := s.clients.GetSkupperClient().SkupperV2alpha1().Links("test").Get(context.Background(), "east", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.Assert(t, meta.IsStatusConditionTrue(link.Status.Conditions, skupperv2alpha1.CONDITION_TYPE_OPERATIONAL))
	assert.Equal(t, link.Status.RemoteSiteId, "remote")
	gotListener, err := s.clients.GetSkupperClient().SkupperV2alpha1().Listeners("test").Get(context.Background(), "orders", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.Assert(t, meta.IsStatusConditionTrue(gotListener.Status.Conditions, skupperv2alpha1.CONDITION_TYPE_MATCHED))
	gotSite, err := s.clients.GetSkupperClient().SkupperV2alpha1().Sites("test").Get(context.Background(), "site1", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.Equal(t, gotSite.Status.SitesInNetwork, 3, "site count must be the maximum group observation, not a sum")

	assert.NilError(t, s.RouterStatusUpdated("group2", nil))
	link, err = s.clients.GetSkupperClient().SkupperV2alpha1().Links("test").Get(context.Background(), "east", metav1.GetOptions{})
	assert.NilError(t, err)
	condition := meta.FindStatusCondition(link.Status.Conditions, skupperv2alpha1.CONDITION_TYPE_OPERATIONAL)
	assert.Assert(t, condition != nil)
	assert.Equal(t, condition.Status, metav1.ConditionFalse)
	assert.Equal(t, condition.Message, "connection refused")
	gotListener, err = s.clients.GetSkupperClient().SkupperV2alpha1().Listeners("test").Get(context.Background(), "orders", metav1.GetOptions{})
	assert.NilError(t, err)
	condition = meta.FindStatusCondition(gotListener.Status.Conditions, skupperv2alpha1.CONDITION_TYPE_MATCHED)
	assert.Assert(t, condition != nil)
	assert.Equal(t, condition.Message, "address unavailable")
}

func TestRouterStatusLocalOnlyClearsPersistedLegacyNetwork(t *testing.T) {
	s, err := newSiteMocks("test", nil, nil, "", false)
	assert.NilError(t, err)
	s.routerStatus = map[string]*routerstatus.Document{}
	s.site.Status.Network = []skupperv2alpha1.SiteRecord{{Id: "legacy"}}
	// Persist the legacy value first, so this checks the fake CR rather than only memory.
	_, err = s.clients.GetSkupperClient().SkupperV2alpha1().Sites("test").UpdateStatus(context.Background(), s.site, metav1.UpdateOptions{})
	assert.NilError(t, err)
	s.SetLegacyNetworkStatus(false)
	assert.NilError(t, s.RefreshRouterStatus())
	got, err := s.clients.GetSkupperClient().SkupperV2alpha1().Sites("test").Get(context.Background(), "site1", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.Equal(t, len(got.Status.Network), 0)
}

func TestRouterStatusReplicaEligibility(t *testing.T) {
	linkDefinition := &skupperv2alpha1.Link{
		ObjectMeta: metav1.ObjectMeta{Name: "east", Namespace: "test"},
		Spec:       skupperv2alpha1.LinkSpec{Endpoints: []skupperv2alpha1.Endpoint{{Name: "inter-router", Host: "east", Port: "55671"}}},
	}
	s, err := newSiteMocks("test", nil, []runtime.Object{linkDefinition}, "", false)
	assert.NilError(t, err)
	s.localOnlyStatus = true
	managed, err := s.newLink(linkDefinition)
	assert.NilError(t, err)
	s.links["east"] = managed
	down := eligibleObservation(s, "pod-a", &routerstatus.Document{Version: 1, Links: []routerstatus.Link{{Name: "east", Present: true, ConnectionStatus: "FAILED", RemoteSiteID: "wrong", Message: "connection refused"}}})
	up := eligibleObservation(s, "pod-b", &routerstatus.Document{Version: 1, Links: []routerstatus.Link{{Name: "east", Present: true, ConnectionStatus: "SUCCESS", RemoteSiteID: "right"}}})
	assert.NilError(t, s.RouterStatusUpdated("pod-a", down))
	assert.NilError(t, s.RouterStatusUpdated("pod-b", up))
	check := func(operational bool) {
		t.Helper()
		link, err := s.clients.GetSkupperClient().SkupperV2alpha1().Links("test").Get(context.Background(), "east", metav1.GetOptions{})
		assert.NilError(t, err)
		assert.Equal(t, meta.IsStatusConditionTrue(link.Status.Conditions, skupperv2alpha1.CONDITION_TYPE_OPERATIONAL), operational)
		if operational {
			assert.Equal(t, link.Status.RemoteSiteId, "right")
			assert.Equal(t, meta.FindStatusCondition(link.Status.Conditions, skupperv2alpha1.CONDITION_TYPE_OPERATIONAL).Message, "OK")
		}
	}
	check(true)
	current := s.routerPods["test/pod-b"].DeepCopy()
	// Pod phase and readiness do not gate observations: a router that is
	// starting or terminating still reports its own links accurately.
	for _, mutate := range []func(*corev1.Pod){
		func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending },
		func(p *corev1.Pod) { now := metav1.Now(); p.DeletionTimestamp = &now },
	} {
		pod := current.DeepCopy()
		mutate(pod)
		assert.NilError(t, s.RouterPodEvent("test/pod-b", pod))
		check(true)
	}
	// A replaced pod invalidates the observation.
	replaced := current.DeepCopy()
	replaced.UID = "replacement-uid"
	assert.NilError(t, s.RouterPodEvent("test/pod-b", replaced))
	check(false)
	assert.NilError(t, s.RouterPodEvent("test/pod-b", current))
	check(true)
	assert.NilError(t, s.RouterPodEvent("test/pod-b", nil))
	check(false)
	assert.NilError(t, s.RouterPodEvent("test/pod-b", current))
	check(true)
	assert.DeepEqual(t, s.eligibleRouterStatus(), []string{"pod-a", "pod-b"})
}

func TestPrefixReplicaRolloutDoesNotRestoreOldTargets(t *testing.T) {
	listener := &skupperv2alpha1.Listener{ObjectMeta: metav1.ObjectMeta{Name: "pods", Namespace: "test"}, Spec: skupperv2alpha1.ListenerSpec{RoutingKey: "backend", Port: 8080, Type: "tcp", ExposePodsByName: true}}
	s, err := newSiteMocks("test", nil, []runtime.Object{listener}, "", false)
	assert.NilError(t, err)
	s.localOnlyStatus = true
	_, err = s.bindings.UpdateListener(listener.Name, listener)
	assert.NilError(t, err)
	old := eligibleObservation(s, "pod-old", &routerstatus.Document{Version: 1, Prefixes: []routerstatus.PrefixQuery{{Prefix: "backend.", Matches: []string{"backend.old"}}}})
	current := eligibleObservation(s, "pod-new", &routerstatus.Document{Version: 1, Prefixes: []routerstatus.PrefixQuery{{Prefix: "backend.", Matches: []string{"backend.new"}}}})
	assert.NilError(t, s.RouterStatusUpdated("pod-old", old))
	assert.NilError(t, s.RouterStatusUpdated("pod-new", current))
	ptl := s.bindings.perTargetListeners["pods"]
	assert.Equal(t, len(ptl.targets), 2, "replicas contribute a union")

	// The old pod is replaced; its observation no longer counts.
	assert.NilError(t, s.RouterPodEvent("test/pod-old", nil))
	assert.Equal(t, len(ptl.targets), 1)
	assert.Assert(t, ptl.targets["new"] != 0)
	assert.NilError(t, s.RouterStatusUpdated("pod-old", old))
	assert.Equal(t, len(ptl.targets), 1, "an observation without a live pod must not restore old targets")

	// A router that has not seen the prefix yet is not an empty result.
	unaware := *current
	unaware.Prefixes = nil
	assert.NilError(t, s.RouterStatusUpdated("pod-new", &unaware))
	assert.Equal(t, len(ptl.targets), 1, "unobserved prefix must not erase target configuration")

	current.Prefixes[0].Matches = nil
	assert.NilError(t, s.RouterStatusUpdated("pod-new", current))
	assert.Equal(t, len(ptl.targets), 0, "current empty result is authoritative")
}

func TestPrefixStatusReportsTruncation(t *testing.T) {
	for _, credentials := range []string{"", "missing-secret"} {
		t.Run(credentials, func(t *testing.T) {
			listener := &skupperv2alpha1.Listener{
				ObjectMeta: metav1.ObjectMeta{Name: "pods", Namespace: "test"},
				Spec:       skupperv2alpha1.ListenerSpec{RoutingKey: "backend", Port: 8080, Type: "tcp", ExposePodsByName: true, TlsCredentials: credentials},
			}
			s, err := newSiteMocks("test", nil, []runtime.Object{listener}, "", false)
			assert.NilError(t, err)
			s.routerStatus = map[string]*routerstatus.Document{}
			s.SetLegacyNetworkStatus(false)
			_, err = s.bindings.UpdateListener(listener.Name, listener)
			assert.NilError(t, err)
			assert.NilError(t, s.updateListenerStatus(listener, s.missingTlsCredentialsErr(credentials)))
			doc := &routerstatus.Document{Version: 1, Prefixes: []routerstatus.PrefixQuery{{Prefix: "backend.", Matches: []string{"backend.pod-a"}, Truncated: true}}}
			eligibleObservation(s, "pod", doc)
			assert.NilError(t, s.RouterStatusUpdated("pod", doc))
			got, err := s.clients.GetSkupperClient().SkupperV2alpha1().Listeners("test").Get(context.Background(), "pods", metav1.GetOptions{})
			assert.NilError(t, err)
			configured := meta.FindStatusCondition(got.Status.Conditions, skupperv2alpha1.CONDITION_TYPE_CONFIGURED)
			assert.Assert(t, configured != nil)
			assert.Equal(t, configured.Status, metav1.ConditionFalse)
			ptl := s.bindings.perTargetListeners["pods"]
			if credentials != "" {
				assert.Equal(t, len(ptl.targets), 0, "missing TLS must prevent target exposure")
				return
			}
			assert.Assert(t, strings.Contains(configured.Message, "truncated"), configured.Message)
			assert.Equal(t, len(ptl.targets), 1)
			// As with legacy network status, ordinary reconciliation restores
			// Configured once the observation no longer reports an error.
			doc.Prefixes[0].Truncated = false
			assert.NilError(t, s.RouterStatusUpdated("pod", doc))
			assert.NilError(t, s.updateListenerStatus(got, nil))
			got, err = s.clients.GetSkupperClient().SkupperV2alpha1().Listeners("test").Get(context.Background(), "pods", metav1.GetOptions{})
			assert.NilError(t, err)
			assert.Assert(t, meta.IsStatusConditionTrue(got.Status.Conditions, skupperv2alpha1.CONDITION_TYPE_CONFIGURED))
		})
	}
}
