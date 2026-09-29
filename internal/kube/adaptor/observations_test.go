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

func TestResourceObservationUsesOnlyApplicableLocalOperationalState(t *testing.T) {
	listenerName := ownedNamePrefix + "listener"
	tcpConnectorName := ownedNamePrefix + "tcp-connector"
	routerConnectorUp := ownedNamePrefix + "router-connector-up"
	routerConnectorDown := ownedNamePrefix + "router-connector-down"
	routerConnectorUnknown := ownedNamePrefix + "router-connector-unknown"
	actual := basicConfig()
	actual.Bridges.TcpListeners[listenerName] = qdr.TcpEndpoint{Name: listenerName, OperStatus: "up", ConnectionMsg: "listening"}
	actual.Bridges.TcpConnectors[tcpConnectorName] = qdr.TcpEndpoint{Name: tcpConnectorName, OperStatus: "down", ConnectionMsg: "not connected"}
	actual.Connectors[routerConnectorUp] = qdr.Connector{Name: routerConnectorUp, ConnectionStatus: "SUCCESS", ConnectionMsg: "Connection Opened: dir=out"}
	actual.Connectors[routerConnectorDown] = qdr.Connector{Name: routerConnectorDown, ConnectionStatus: "CONNECTING", ConnectionMsg: "Connection failed: refused"}
	actual.Connectors[routerConnectorUnknown] = qdr.Connector{Name: routerConnectorUnknown, ConnectionStatus: "unexpected", ConnectionMsg: "future router state"}
	compiled := CompiledIntent{RealizationID: "realization", ResourceNames: map[routercontrol.ResourceID][]string{
		"listener":                 {listenerName},
		"tcp-connector":            {tcpConnectorName},
		"router-connector-up":      {routerConnectorUp},
		"router-connector-down":    {routerConnectorDown},
		"router-connector-unknown": {routerConnectorUnknown},
	}}
	observation := buildResourceObservation(routercontrol.ObservationSnapshot{Knowledge: routercontrol.KnowledgeComplete}, actual, compiled)
	resources := map[routercontrol.ResourceID]routercontrol.LocalResourceObservation{}
	for _, resource := range observation.Resources {
		resources[resource.ResourceID] = resource
	}
	if resources["listener"].Operational != routercontrol.OperationalUp {
		t.Fatalf("listener status not read from local tcpListener: %#v", resources)
	}
	if resources["tcp-connector"].Operational != routercontrol.OperationalUnknown {
		t.Fatalf("TCP connector connectivity was incorrectly treated as matching: %#v", resources)
	}
	if resources["router-connector-up"].Operational != routercontrol.OperationalUp || resources["router-connector-up"].Message != "Connection Opened: dir=out" {
		t.Fatalf("successful router connector not reported up: %#v", resources)
	}
	if resources["router-connector-down"].Operational != routercontrol.OperationalDown || resources["router-connector-down"].Message != "Connection failed: refused" {
		t.Fatalf("connecting router connector not reported down: %#v", resources)
	}
	if resources["router-connector-unknown"].Operational != routercontrol.OperationalUnknown {
		t.Fatalf("unknown router connector state was guessed: %#v", resources)
	}
}
