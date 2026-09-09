package status

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/skupperproject/skupper/pkg/skrouter/mgmt/entities"
)

func TestConnectionAssociationUsesEndpoint(t *testing.T) {
	connections := []entities.Connection{
		{Name: "connection/west:4567", Host: "west:4567", Dir: entities.Direction("out"), Role: "edge", Opened: true, Container: "router-west"},
		{Name: "connection/east:5678", Host: "east:5678", Dir: entities.Direction("out"), Role: "edge", Opened: true, Container: "router-east"},
		{Host: "east:5678", Dir: entities.Direction("out"), Role: "edge", Opened: false, Container: "stale"},
		{LocalSocket: "[::]:4567", Dir: entities.Direction("in"), Role: "edge", Opened: true, Container: "peer-a"},
		{LocalSocket: "127.0.0.1:5678", Dir: entities.Direction("in"), Role: "edge", Opened: true, Container: "peer-b"},
	}
	connection, ok := connectorConnection(connections, "east", "5678", "edge")
	if !ok || connection.Container != "router-east" {
		t.Fatalf("east connection = %#v, %v", connection, ok)
	}
	if _, ok := connectorConnection(connections, "missing", "5678", "edge"); ok {
		t.Fatal("missing connector acquired an invented peer")
	}
	if got := listenerPeers(connections, "5678", "edge"); !reflect.DeepEqual(got, []string{"peer-b"}) {
		t.Fatalf("access-b peers = %v", got)
	}
}

func TestMobileAddressesAndReachability(t *testing.T) {
	got := mobileAddresses([]entities.RouterAddress{
		{Name: "Mlocal", RemoteCount: 1},
		{Name: "Llocal", SubscriberCount: 1},
		{Name: "Mremote", RemoteCount: 2},
	})
	if len(got) != 2 || !reachable(got["local"]) || !reachable(got["remote"]) {
		t.Fatalf("mobile addresses = %#v", got)
	}
	if _, ok := got["Llocal"]; ok {
		t.Fatal("non-mobile address included")
	}
}

func TestRouterLinkRoleExcludesNormal(t *testing.T) {
	if routerLinkRole(entities.Role("normal")) || routerLinkRole(entities.Role("route-container")) {
		t.Fatal("non-router role accepted")
	}
	if !routerLinkRole(entities.Role("edge")) || !routerLinkRole(entities.Role("inter-router")) {
		t.Fatal("router role rejected")
	}
}

func TestPrefixQuerySortsAndTruncates129(t *testing.T) {
	addresses := map[string]entities.RouterAddress{}
	for i := 128; i >= 0; i-- {
		name := fmt.Sprintf("svc-%03d", i)
		addresses[name] = entities.RouterAddress{Name: "M" + name, RemoteCount: 1}
	}
	addresses["svc-unreachable"] = entities.RouterAddress{Name: "Msvc-unreachable"}
	query := prefixQuery("svc-", addresses)
	if len(query.Matches) != MaxPrefixMatches || !query.Truncated {
		t.Fatalf("prefix query = %#v", query)
	}
	if query.Matches[0] != "svc-000" || query.Matches[127] != "svc-127" {
		t.Fatalf("prefix order/boundary = %q ... %q", query.Matches[0], query.Matches[127])
	}
}
