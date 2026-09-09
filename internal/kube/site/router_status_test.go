package site

import (
	"context"
	"testing"

	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	routerstatus "github.com/skupperproject/skupper/pkg/skrouter/status"
	"gotest.tools/v3/assert"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

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
	assert.NilError(t, s.RouterStatusUpdated("group1", down))
	assert.NilError(t, s.RouterStatusUpdated("group2", up))

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
