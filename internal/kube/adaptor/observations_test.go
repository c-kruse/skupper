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

func TestAddressObservationMatchesExactUnphasedKeyIncludingLeadingDigit(t *testing.T) {
	base := routercontrol.ObservationSnapshot{Scope: routercontrol.ObservationScopeAddresses, Knowledge: routercontrol.KnowledgeComplete}
	wanted := map[string]struct{}{"orders": {}, "0orders": {}}
	addresses := []qdr.LocalAddress{{Key: "M0orders", Class: 'M', RoutingKey: "0orders", SubscriberCount: 1}}
	observation := buildAddressObservation(base, routercontrol.RoutingModeEdge, wanted, addresses, nil)
	got := map[string]routercontrol.LocalAddressObservation{}
	for _, address := range observation.Addresses {
		got[address.RoutingKey] = address
	}
	if got["orders"].Reachable {
		t.Fatalf("suffix match incorrectly reached orders: %#v", observation.Addresses)
	}
	if !got["0orders"].Reachable || got["0orders"].SubscriberCount != 1 {
		t.Fatalf("leading-digit routing key did not match exactly: %#v", observation.Addresses)
	}
}

func TestResourceObservationUsesOnlyApplicableLocalOperationalState(t *testing.T) {
	listenerName := ownedNamePrefix + "listener"
	tcpConnectorName := ownedNamePrefix + "tcp-connector"
	routerConnectorUp := ownedNamePrefix + "router-connector-up"
	routerConnectorDown := ownedNamePrefix + "router-connector-down"
	actual := basicConfig()
	actual.Bridges.TcpListeners[listenerName] = qdr.TcpEndpoint{Name: listenerName, OperStatus: "up", ConnectionMsg: "listening"}
	actual.Bridges.TcpConnectors[tcpConnectorName] = qdr.TcpEndpoint{Name: tcpConnectorName, OperStatus: "down", ConnectionMsg: "not connected"}
	actual.Connectors[routerConnectorUp] = qdr.Connector{Name: routerConnectorUp, Host: "router-a", Port: "5671", Role: qdr.RoleEdge, ConnectionStatus: "SUCCESS"}
	actual.Connectors[routerConnectorDown] = qdr.Connector{Name: routerConnectorDown, Host: "router-b", Port: "45672", Role: qdr.RoleInterRouter, ConnectionStatus: "SUCCESS", ConnectionMsg: "Connection Opened: stale"}
	compiled := CompiledIntent{RealizationID: "realization", ResourceNames: map[routercontrol.ResourceID][]string{
		"listener":              {listenerName},
		"tcp-connector":         {tcpConnectorName},
		"router-connector-up":   {routerConnectorUp},
		"router-connector-down": {routerConnectorDown},
	}}
	connections := []qdr.Connection{
		{Host: "router-a:5671", Role: string(qdr.RoleEdge), Dir: "out", Opened: true},
		{Host: "router-b:45672", Role: string(qdr.RoleInterRouter), Dir: "out", Opened: false},
		{Host: "router-b:45672", Role: string(qdr.RoleInterRouter), Dir: "in", Opened: true},
	}
	observation := buildResourceObservation(routercontrol.ObservationSnapshot{Knowledge: routercontrol.KnowledgeComplete}, actual, compiled, connections, nil)
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
	if resources["router-connector-up"].Operational != routercontrol.OperationalUp {
		t.Fatalf("successful router connector not reported up: %#v", resources)
	}
	if resources["router-connector-down"].Operational != routercontrol.OperationalDown {
		t.Fatalf("pre-OPEN SUCCESS or stale message was treated as open: %#v", resources)
	}
}

func TestResourceObservationLeavesAmbiguousConnectorOwnershipUnknown(t *testing.T) {
	first := ownedNamePrefix + "first"
	second := ownedNamePrefix + "second"
	actual := basicConfig()
	actual.Connectors[first] = qdr.Connector{Name: first, Host: "router", Port: "5671", Role: qdr.RoleEdge}
	actual.Connectors[second] = qdr.Connector{Name: second, Host: "router", Port: "5671", Role: qdr.RoleEdge}
	compiled := CompiledIntent{ResourceNames: map[routercontrol.ResourceID][]string{"first": {first}, "second": {second}}}
	connections := []qdr.Connection{{Host: "router:5671", Role: string(qdr.RoleEdge), Dir: "out", Opened: true}}

	observation := buildResourceObservation(routercontrol.ObservationSnapshot{Knowledge: routercontrol.KnowledgeComplete}, actual, compiled, connections, nil)
	for _, resource := range observation.Resources {
		if resource.Operational != routercontrol.OperationalUnknown {
			t.Fatalf("ambiguous connector %q was reported %q", resource.ResourceID, resource.Operational)
		}
	}
}

func TestResourceObservationIsUnknownWhenConnectionQueryFails(t *testing.T) {
	actual := basicConfig()
	compiled := CompiledIntent{ResourceNames: map[routercontrol.ResourceID][]string{"link": {ownedNamePrefix + "link"}}}

	observation := buildResourceObservation(routercontrol.ObservationSnapshot{Knowledge: routercontrol.KnowledgeComplete}, actual, compiled, nil, errors.New("connection query failed"))
	if observation.Knowledge != routercontrol.KnowledgeUnknown || len(observation.Resources) != 0 || observation.Reason != "connection query failed" {
		t.Fatalf("failed connection query was not reported as unknown: %#v", observation)
	}
}
