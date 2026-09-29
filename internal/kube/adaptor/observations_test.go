package adaptor

import (
	"errors"
	"testing"

	"github.com/skupperproject/skupper/internal/qdr"
	"github.com/skupperproject/skupper/internal/routercontrol"
)

func TestAddressObservationDistinguishesKnownEmptyFromFailedQuery(t *testing.T) {
	base := routercontrol.ObservationSnapshot{Scope: routercontrol.ObservationScopeAddresses, Knowledge: routercontrol.KnowledgeComplete}
	wanted := map[string]struct{}{"orders": {}}
	empty := buildAddressObservation(base, routercontrol.RoutingModeInterior, wanted, nil, nil)
	if empty.Knowledge != routercontrol.KnowledgeComplete || len(empty.Addresses) != 1 || empty.Addresses[0].Reachable {
		t.Fatalf("fresh empty query not represented as known empty: %#v", empty)
	}
	failed := buildAddressObservation(base, routercontrol.RoutingModeInterior, wanted, nil, errors.New("disconnected"))
	if failed.Knowledge != routercontrol.KnowledgeUnknown || len(failed.Addresses) != 0 {
		t.Fatalf("failed query represented as empty: %#v", failed)
	}
}

func TestResourceObservationUsesListenerOperStatusOnly(t *testing.T) {
	listenerName := ownedNamePrefix + "listener"
	connectorName := ownedNamePrefix + "connector"
	actual := basicConfig()
	actual.Bridges.TcpListeners[listenerName] = qdr.TcpEndpoint{Name: listenerName, OperStatus: "up", ConnectionMsg: "listening"}
	actual.Bridges.TcpConnectors[connectorName] = qdr.TcpEndpoint{Name: connectorName, OperStatus: "down", ConnectionMsg: "not connected"}
	compiled := CompiledIntent{RealizationID: "realization", ResourceNames: map[routercontrol.ResourceID][]string{
		"listener":  {listenerName},
		"connector": {connectorName},
	}}
	observation := buildResourceObservation(routercontrol.ObservationSnapshot{Knowledge: routercontrol.KnowledgeComplete}, actual, compiled)
	states := map[routercontrol.ResourceID]routercontrol.OperationalState{}
	for _, resource := range observation.Resources {
		states[resource.ResourceID] = resource.Operational
	}
	if states["listener"] != routercontrol.OperationalUp {
		t.Fatalf("listener status not read from local tcpListener: %#v", states)
	}
	if states["connector"] != routercontrol.OperationalUnknown {
		t.Fatalf("connector connectivity was incorrectly treated as matching: %#v", states)
	}
}
