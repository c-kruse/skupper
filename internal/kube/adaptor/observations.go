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
	incarnation, verified := r.CurrentRouterIncarnation()
	if incarnation != "" {
		observation.RouterIncarnation = incarnation
	}
	if err != nil {
		observation.Knowledge = routercontrol.KnowledgeUnknown
		observation.Reason = err.Error()
		return observation
	}
	if !verified {
		observation.Knowledge = routercontrol.KnowledgeUnknown
		observation.Reason = ErrRouterRealizationUnverified.Error()
		return observation
	}
	return buildResourceObservation(observation, actual, compiled)
}

func buildResourceObservation(observation routercontrol.ObservationSnapshot, actual *qdr.RouterConfig, compiled CompiledIntent) routercontrol.ObservationSnapshot {
	ids := make([]string, 0, len(compiled.ResourceNames))
	for id := range compiled.ResourceNames {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	for _, id := range ids {
		resource := routercontrol.LocalResourceObservation{ResourceID: routercontrol.ResourceID(id), RealizationID: compiled.RealizationID, Operational: routercontrol.OperationalUnknown}
		for _, name := range compiled.ResourceNames[routercontrol.ResourceID(id)] {
			if connector, found := actual.Connectors[name]; found {
				switch strings.ToUpper(connector.ConnectionStatus) {
				case "SUCCESS":
					resource.Operational = routercontrol.OperationalUp
				case "CONNECTING", "FAILED", "INITIALIZING", "CLOSING":
					resource.Operational = routercontrol.OperationalDown
				default:
					resource.Operational = routercontrol.OperationalUnknown
				}
				resource.Message = connector.ConnectionMsg
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
