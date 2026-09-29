package adaptor

import (
	"fmt"
	"sort"
	"strings"

	"github.com/skupperproject/skupper/internal/qdr"
	"github.com/skupperproject/skupper/internal/routercontrol"
)

func (r *AMQPLocalRouter) ObserveResources(sessionID, fallbackIncarnation string, sample uint64, compiled CompiledIntent, refreshID string) routercontrol.ObservationSnapshot {
	observation := routercontrol.ObservationSnapshot{SessionID: sessionID, Scope: routercontrol.ObservationScopeResources, SampleSequence: sample, RefreshRequestID: refreshID, Knowledge: routercontrol.KnowledgeComplete, RouterIncarnation: fallbackIncarnation}
	actual, err := r.Read()
	var connections []qdr.Connection
	var connectionErr error
	if err == nil {
		connections, connectionErr = r.ReadConnections()
	}
	incarnation, verified := r.CurrentRouterIncarnation()
	if incarnation != "" {
		observation.RouterIncarnation = incarnation
	}
	if err != nil {
		observation.Knowledge = routercontrol.KnowledgeUnknown
		observation.Reason = err.Error()
		return observation
	}
	if connectionErr != nil {
		return buildResourceObservation(observation, actual, compiled, nil, connectionErr)
	}
	if !verified {
		observation.Knowledge = routercontrol.KnowledgeUnknown
		observation.Reason = ErrRouterRealizationUnverified.Error()
		return observation
	}
	return buildResourceObservation(observation, actual, compiled, connections, nil)
}

type connectorEndpoint struct {
	host string
	role string
}

func buildResourceObservation(observation routercontrol.ObservationSnapshot, actual *qdr.RouterConfig, compiled CompiledIntent, connections []qdr.Connection, connectionErr error) routercontrol.ObservationSnapshot {
	if connectionErr != nil {
		observation.Knowledge = routercontrol.KnowledgeUnknown
		observation.Reason = connectionErr.Error()
		observation.Resources = nil
		return observation
	}
	// The local connection entity has no connector name or identity. For
	// outgoing connections, QDR reports the connector's exact host:port and
	// role, so duplicate connector claims make the evidence ambiguous.
	connectorClaims := map[connectorEndpoint]int{}
	for _, connector := range actual.Connectors {
		connectorClaims[connectorEndpoint{host: connector.Host + ":" + connector.Port, role: string(connector.Role)}]++
	}
	openedConnections := map[connectorEndpoint]bool{}
	for _, connection := range connections {
		if connection.Opened && strings.EqualFold(connection.Dir, "out") {
			openedConnections[connectorEndpoint{host: connection.Host, role: connection.Role}] = true
		}
	}
	ids := make([]string, 0, len(compiled.ResourceNames))
	for id := range compiled.ResourceNames {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	for _, id := range ids {
		resource := routercontrol.LocalResourceObservation{ResourceID: routercontrol.ResourceID(id), RealizationID: compiled.RealizationID, Operational: routercontrol.OperationalUnknown}
		for _, name := range compiled.ResourceNames[routercontrol.ResourceID(id)] {
			if connector, found := actual.Connectors[name]; found {
				endpoint := connectorEndpoint{host: connector.Host + ":" + connector.Port, role: string(connector.Role)}
				switch {
				case connectorClaims[endpoint] != 1:
					resource.Operational = routercontrol.OperationalUnknown
					resource.Message = "local outbound connection ownership is ambiguous"
				case openedConnections[endpoint]:
					resource.Operational = routercontrol.OperationalUp
					resource.Message = "local outbound AMQP connection is open"
				default:
					resource.Operational = routercontrol.OperationalDown
					resource.Message = "no open local outbound AMQP connection"
				}
			}
			if listener, found := actual.Bridges.TcpListeners[name]; found {
				switch strings.ToLower(listener.OperStatus) {
				case "up":
					resource.Operational = routercontrol.OperationalUp
				case "down":
					resource.Operational = routercontrol.OperationalDown
				default:
					resource.Operational = routercontrol.OperationalUnknown
				}
				resource.Message = listener.ConnectionMsg
			}
		}
		observation.Resources = append(observation.Resources, resource)
	}
	return observation
}

func (r *AMQPLocalRouter) ObserveAddresses(sessionID, fallbackIncarnation string, sample uint64, intent routercontrol.RouterIntent, refreshID string) routercontrol.ObservationSnapshot {
	observation := routercontrol.ObservationSnapshot{SessionID: sessionID, Scope: routercontrol.ObservationScopeAddresses, SampleSequence: sample, RefreshRequestID: refreshID, Knowledge: routercontrol.KnowledgeComplete, RouterIncarnation: fallbackIncarnation}
	wanted := map[string]struct{}{}
	for _, listener := range intent.ServiceListeners {
		for _, key := range listener.RoutingKeys {
			wanted[key] = struct{}{}
		}
	}
	addresses, err := r.GetLocalAddresses(wanted)
	if incarnation, _ := r.CurrentRouterIncarnation(); incarnation != "" {
		observation.RouterIncarnation = incarnation
	}
	return buildAddressObservation(observation, intent.Settings.Mode, wanted, addresses, err)
}

func buildAddressObservation(observation routercontrol.ObservationSnapshot, _ routercontrol.RoutingMode, wanted map[string]struct{}, addresses []qdr.LocalAddress, queryErr error) routercontrol.ObservationSnapshot {
	if queryErr != nil {
		observation.Knowledge = routercontrol.KnowledgeUnknown
		observation.Reason = queryErr.Error()
		return observation
	}
	// Application addresses use one M class byte followed directly by the exact
	// routing key. H is an edge-summary class and is not application traffic.
	class := byte('M')
	keys := make([]string, 0, len(wanted))
	for key := range wanted {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		var matched *localAddressCounts
		for _, address := range addresses {
			if address.Class != class || address.RoutingKey != key {
				continue
			}
			if matched != nil {
				observation.Knowledge = routercontrol.KnowledgeUnknown
				observation.Addresses = nil
				observation.Reason = fmt.Sprintf("multiple local address records for %q class %c", key, class)
				return observation
			}
			matched = &localAddressCounts{subscriber: address.SubscriberCount, inProcess: address.InProcess, remote: address.RemoteCount}
		}
		item := routercontrol.LocalAddressObservation{RoutingKey: key}
		if matched != nil {
			item.SubscriberCount = uint64(matched.subscriber)
			item.InProcessCount = uint64(matched.inProcess)
			item.RemoteCount = uint64(matched.remote)
			item.Reachable = matched.subscriber > 0 || matched.inProcess > 0 || matched.remote > 0
		}
		observation.Addresses = append(observation.Addresses, item)
	}
	return observation
}

type localAddressCounts struct{ subscriber, inProcess, remote int }
