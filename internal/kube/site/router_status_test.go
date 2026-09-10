package site

import (
	"context"
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

func eligibleObservation(s *Site, podName, group string, doc *routerstatus.Document) *routerstatus.Document {
	doc.Group = group
	doc.Router.Hostname = podName
	doc.Router.PodUID = podName + "-uid"
	doc.Applied.ResourceVersion = "revision-a"
	if s.routerConfigVersions == nil {
		s.routerConfigVersions = map[string]string{}
	}
	s.routerConfigVersions[group] = "revision-a"
	if s.routerPods == nil {
		s.routerPods = map[string]*corev1.Pod{}
	}
	s.routerPods[s.namespace+"/"+podName] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: s.namespace, UID: types.UID(doc.Router.PodUID), Labels: map[string]string{"skupper.io/group": group}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
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
	assert.NilError(t, s.RouterStatusUpdated("group1", eligibleObservation(s, "group1", "group1", down)))
	assert.NilError(t, s.RouterStatusUpdated("group2", eligibleObservation(s, "group2", "group2", up)))

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
	down := eligibleObservation(s, "pod-a", "group", &routerstatus.Document{Version: 1, Links: []routerstatus.Link{{Name: "east", Present: true, ConnectionStatus: "FAILED", RemoteSiteID: "wrong", Message: "connection refused"}}})
	up := eligibleObservation(s, "pod-b", "group", &routerstatus.Document{Version: 1, Links: []routerstatus.Link{{Name: "east", Present: true, ConnectionStatus: "SUCCESS", RemoteSiteID: "right"}}})
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
	ready := s.routerPods["test/pod-b"].DeepCopy()
	for _, mutate := range []func(*corev1.Pod){
		func(p *corev1.Pod) { p.Status.Conditions[0].Status = corev1.ConditionFalse },
		func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending },
		func(p *corev1.Pod) { now := metav1.Now(); p.DeletionTimestamp = &now },
		func(p *corev1.Pod) { p.UID = "replacement-uid" },
		func(p *corev1.Pod) { p.Labels["skupper.io/group"] = "other" },
	} {
		pod := ready.DeepCopy()
		mutate(pod)
		assert.NilError(t, s.RouterPodEvent("test/pod-b", pod))
		check(false)
		assert.NilError(t, s.RouterPodEvent("test/pod-b", ready))
		check(true)
	}
	assert.NilError(t, s.RouterPodEvent("test/pod-b", nil))
	check(false)
	assert.NilError(t, s.RouterPodEvent("test/pod-b", ready))
	check(true)
	// Versions are opaque: a lexically smaller new revision invalidates both.
	assert.NilError(t, s.RouterConfigVersionUpdated("group", "a-new"))
	check(false)
	newUp := *up
	newUp.Applied.ResourceVersion = "a-new"
	assert.NilError(t, s.RouterStatusUpdated("pod-b", &newUp))
	check(true)
	assert.DeepEqual(t, s.eligibleRouterStatus(), []string{"pod-b"})
	// Future observation arrives first, then the config event makes it eligible.
	newUp.Applied.ResourceVersion = "another-revision"
	assert.NilError(t, s.RouterStatusUpdated("pod-b", &newUp))
	check(false)
	assert.NilError(t, s.RouterConfigVersionUpdated("group", "another-revision"))
	check(true)
	assert.NilError(t, s.RouterConfigVersionUpdated("group", ""))
	check(false)
}

func TestPrefixReplicaRolloutDoesNotRestoreOldTargets(t *testing.T) {
	listener := &skupperv2alpha1.Listener{ObjectMeta: metav1.ObjectMeta{Name: "pods", Namespace: "test"}, Spec: skupperv2alpha1.ListenerSpec{RoutingKey: "backend", Port: 8080, Type: "tcp", ExposePodsByName: true}}
	s, err := newSiteMocks("test", nil, []runtime.Object{listener}, "", false)
	assert.NilError(t, err)
	s.localOnlyStatus = true
	_, err = s.bindings.UpdateListener(listener.Name, listener)
	assert.NilError(t, err)
	old := eligibleObservation(s, "pod-old", "group", &routerstatus.Document{Version: 1, Prefixes: []routerstatus.PrefixQuery{{Prefix: "backend.", Matches: []string{"backend.old"}}}})
	current := eligibleObservation(s, "pod-new", "group", &routerstatus.Document{Version: 1, Prefixes: []routerstatus.PrefixQuery{{Prefix: "backend.", Matches: []string{"backend.new"}}}})
	assert.NilError(t, s.RouterStatusUpdated("pod-old", old))
	assert.NilError(t, s.RouterStatusUpdated("pod-new", current))
	ptl := s.bindings.perTargetListeners["pods"]
	assert.Equal(t, len(ptl.targets), 2, "same-group replicas contribute a union")
	assert.NilError(t, s.RouterConfigVersionUpdated("group", "revision-b"))
	assert.Equal(t, len(ptl.targets), 2, "no current observation must not erase target configuration")
	current.Applied.ResourceVersion = "revision-b"
	assert.NilError(t, s.RouterStatusUpdated("pod-new", current))
	assert.Equal(t, len(ptl.targets), 1)
	assert.Assert(t, ptl.targets["new"] != 0)
	assert.NilError(t, s.RouterStatusUpdated("pod-old", old))
	assert.Equal(t, len(ptl.targets), 1, "old revision must not restore old targets")
	current.Prefixes[0].Matches = nil
	assert.NilError(t, s.RouterStatusUpdated("pod-new", current))
	assert.Equal(t, len(ptl.targets), 0, "current empty result is authoritative")
}

func TestPrefixStatusPreservesConfigurationErrors(t *testing.T) {
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
			doc := &routerstatus.Document{Version: 1, Prefixes: []routerstatus.PrefixQuery{{Prefix: "backend.", Matches: []string{"backend.pod-a"}, Truncated: true}}}
			eligibleObservation(s, "group", "group", doc)
			for i := 0; i < 3; i++ {
				assert.NilError(t, s.RouterStatusUpdated("group", doc))
				got, err := s.clients.GetSkupperClient().SkupperV2alpha1().Listeners("test").Get(context.Background(), "pods", metav1.GetOptions{})
				assert.NilError(t, err)
				assert.Assert(t, meta.IsStatusConditionFalse(got.Status.Conditions, skupperv2alpha1.CONDITION_TYPE_CONFIGURED))
				assert.NilError(t, s.updateListenerStatus(got, s.missingTlsCredentialsErr(credentials)))
			}
			ptl := s.bindings.perTargetListeners["pods"]
			if credentials != "" {
				assert.Equal(t, len(ptl.targets), 0, "missing TLS must prevent target exposure")
			} else {
				assert.Equal(t, len(ptl.targets), 1)
				doc.Prefixes[0].Truncated = false
				assert.NilError(t, s.RouterStatusUpdated("group", doc))
				got, err := s.clients.GetSkupperClient().SkupperV2alpha1().Listeners("test").Get(context.Background(), "pods", metav1.GetOptions{})
				assert.NilError(t, err)
				assert.Assert(t, meta.IsStatusConditionTrue(got.Status.Conditions, skupperv2alpha1.CONDITION_TYPE_CONFIGURED))
			}
		})
	}
}
