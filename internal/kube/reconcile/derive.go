package reconcile

import (
	"fmt"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	"github.com/skupperproject/skupper/internal/routercontrol"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

const (
	firstDynamicPort = 1024
	lastDynamicPort  = 65535
)

type NamespaceDeriver struct{}

func (NamespaceDeriver) Derive(snapshot Snapshot) DesiredNamespace {
	desired := DesiredNamespace{
		Namespace:   snapshot.Namespace,
		Intents:     map[RouterTarget]routercontrol.RouterIntent{},
		Allocations: AllocationState{Ports: map[string]int{}},
	}
	active := activeSite(snapshot, &desired)
	if active == nil || !snapshot.Assignment.Controlled {
		return desired
	}
	desired.SiteUID = active.UID
	desired.Allocations.SiteUID = active.UID
	if snapshot.Allocations.SiteUID == active.UID {
		for key, port := range snapshot.Allocations.Ports {
			desired.Allocations.Ports[key] = port
		}
	}

	groups := []string{"skupper-router"}
	if active.Spec.HA {
		groups = append(groups, "skupper-router-2")
	}
	listeners := make([]routercontrol.ServiceListener, 0, len(snapshot.Listeners)+len(snapshot.MultiKeyListeners))
	for _, listener := range sortedListeners(snapshot.Listeners) {
		if listener.Spec.ExposePodsByName {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: listener.UID, Reason: "Unsupported", Message: "exposePodsByName is not supported by namespace reconciliation"})
			continue
		}
		port, err := allocatePort(desired.Allocations.Ports, string(listener.UID)+"/listener")
		if err != nil {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: listener.UID, Reason: "PortExhausted", Message: err.Error()})
			continue
		}
		listeners = append(listeners, routercontrol.ServiceListener{
			ID:          resourceID(listener.UID, "listener"),
			RoutingKeys: []string{listener.Spec.RoutingKey},
			Host:        listener.Spec.Host,
			Port:        uint16(port),
			Protocol:    protocol(listener.Spec.Type),
			Observer:    listener.Spec.Observer,
			TLS:         tlsIntent(listener.Spec.TlsCredentials, false, false),
		})
	}
	for _, listener := range sortedMultiKeyListeners(snapshot.MultiKeyListeners) {
		port, err := allocatePort(desired.Allocations.Ports, string(listener.UID)+"/listener")
		if err != nil {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: listener.UID, Reason: "PortExhausted", Message: err.Error()})
			continue
		}
		listeners = append(listeners, routercontrol.ServiceListener{ID: resourceID(listener.UID, "listener"), RoutingKeys: routingKeys(listener), Host: listener.Spec.Host, Port: uint16(port), Protocol: routercontrol.ProtocolTCP, Observer: listener.Spec.Observer, TLS: tlsIntent(listener.Spec.TlsCredentials, listener.Spec.RequireClientCert, false)})
	}
	connectors := deriveConnectors(snapshot, &desired)
	connections := make([]routercontrol.RouterConnection, 0, len(snapshot.Links))
	for _, link := range sortedLinks(snapshot.Links) {
		for _, endpoint := range link.Spec.Endpoints {
			port, err := strconv.ParseUint(endpoint.Port, 10, 16)
			if err != nil {
				desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: link.UID, Reason: "InvalidPort", Message: err.Error()})
				continue
			}
			connections = append(connections, routercontrol.RouterConnection{ID: resourceID(link.UID, "link/"+endpoint.Name), Host: endpoint.Host, Port: uint16(port), Role: endpoint.Name, Cost: uint32(max(link.Spec.Cost, 0)), TLS: tlsIntent(link.Spec.TlsCredentials, true, false)})
		}
	}
	access := make([]routercontrol.RouterListener, 0)
	for _, routerAccess := range sortedRouterAccess(snapshot.RouterAccesses) {
		for _, role := range routerAccess.Spec.Roles {
			access = append(access, routercontrol.RouterListener{ID: resourceID(routerAccess.UID, "access/"+role.Name), Role: role.Name, Port: uint16(role.GetPort()), Host: routerAccess.Spec.BindHost, TLS: tlsIntent(routerAccess.Spec.TlsCredentials, true, false)})
		}
	}
	for _, group := range groups {
		target := RouterTarget{NamespaceUID: string(snapshot.Namespace.UID), SiteUID: string(active.UID), RouterGroup: group}
		desired.Intents[target] = routercontrol.RouterIntent{SchemaVersion: routercontrol.SchemaVersion, Target: target, Settings: routercontrol.RouterSettings{Mode: map[bool]routercontrol.RoutingMode{true: routercontrol.RoutingModeEdge, false: routercontrol.RoutingModeInterior}[active.Spec.Edge]}, ServiceListeners: copyListeners(listeners), ServiceConnectors: copyConnectors(connectors, target), RouterConnections: append([]routercontrol.RouterConnection(nil), connections...), RouterListeners: append([]routercontrol.RouterListener(nil), access...), CredentialBindings: credentialBindings(listeners, connectors, connections, access)}
	}
	return desired
}

func activeSite(snapshot Snapshot, desired *DesiredNamespace) *skupperv2alpha1.Site {
	if len(snapshot.Sites) == 0 {
		return nil
	}
	if snapshot.Allocations.SiteUID != "" {
		for _, site := range snapshot.Sites {
			if site.UID == snapshot.Allocations.SiteUID {
				return site
			}
		}
	}
	if len(snapshot.Sites) != 1 {
		desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Reason: "SiteConflict", Message: "multiple Sites exist and no established Site UID identifies the active owner"})
		return nil
	}
	return snapshot.Sites[0]
}

func deriveConnectors(snapshot Snapshot, desired *DesiredNamespace) []routercontrol.ServiceConnector {
	var result []routercontrol.ServiceConnector
	for _, connector := range sortedConnectors(snapshot.Connectors) {
		if connector.Spec.ExposePodsByName {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: connector.UID, Reason: "Unsupported", Message: "exposePodsByName is not supported by namespace reconciliation"})
			continue
		}
		endpoints, err := connectorEndpoints(connector.Spec.Host, connector.Spec.Selector, connector.Spec.Port, connector.Spec.IncludeNotReadyPods, snapshot.Pods)
		if err != nil {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: connector.UID, Reason: "InvalidSelector", Message: err.Error()})
			continue
		}
		result = append(result, routercontrol.ServiceConnector{ID: resourceID(connector.UID, "connector"), RoutingKey: connector.Spec.RoutingKey, Protocol: protocol(connector.Spec.Type), Endpoints: endpoints, TLS: tlsIntent(connector.Spec.TlsCredentials, connector.Spec.UseClientCert, connector.Spec.VerifyHostname)})
	}
	for _, binding := range sortedBindings(snapshot.Bindings) {
		if binding.Spec.ExposePodsByName {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: binding.UID, Reason: "Unsupported", Message: "exposePodsByName is not supported by namespace reconciliation"})
			continue
		}
		definition := attachedForBinding(binding, snapshot.Attached)
		if definition == nil {
			continue
		}
		endpoints, err := connectorEndpoints("", definition.Spec.Selector, definition.Spec.Port, definition.Spec.IncludeNotReadyPods, snapshot.Pods)
		if err != nil {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: definition.UID, Reason: "InvalidSelector", Message: err.Error()})
			continue
		}
		result = append(result, routercontrol.ServiceConnector{ID: resourceID(binding.UID, "attached-connector"), RoutingKey: binding.Spec.RoutingKey, Protocol: protocol(definition.Spec.Type), Endpoints: endpoints, TLS: tlsIntent(definition.Spec.TlsCredentials, definition.Spec.UseClientCert, false)})
	}
	return result
}

func connectorEndpoints(host, selector string, port int, includeNotReady bool, pods []*corev1.Pod) ([]routercontrol.Endpoint, error) {
	if selector == "" {
		if host == "" {
			return nil, nil
		}
		return []routercontrol.Endpoint{{ID: host + ":" + strconv.Itoa(port), Host: host, Port: uint16(port)}}, nil
	}
	parsed, err := labels.Parse(selector)
	if err != nil {
		return nil, err
	}
	var result []routercontrol.Endpoint
	for _, pod := range pods {
		if !parsed.Matches(labels.Set(pod.Labels)) || pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" || (!includeNotReady && !podReady(pod)) {
			continue
		}
		result = append(result, routercontrol.Endpoint{ID: string(pod.UID), Host: pod.Status.PodIP, Port: uint16(port)})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func attachedForBinding(binding *skupperv2alpha1.AttachedConnectorBinding, attached []*skupperv2alpha1.AttachedConnector) *skupperv2alpha1.AttachedConnector {
	for _, definition := range attached {
		if definition.Name == binding.Name && definition.Namespace == binding.Spec.ConnectorNamespace && definition.Spec.SiteNamespace == binding.Namespace {
			return definition
		}
	}
	return nil
}

func resourceID(uid types.UID, role string) routercontrol.ResourceID {
	return routercontrol.ResourceID(string(uid) + "/" + role)
}

func protocol(value string) routercontrol.Protocol {
	switch value {
	case "http":
		return routercontrol.ProtocolHTTP
	case "http2":
		return routercontrol.ProtocolHTTP2
	default:
		return routercontrol.ProtocolTCP
	}
}

func tlsIntent(reference string, mutual, verifyHostname bool) routercontrol.TLSIntent {
	if reference == "" {
		return routercontrol.TLSIntent{Mode: routercontrol.TLSModeDisabled}
	}
	mode := routercontrol.TLSModeClient
	if mutual {
		mode = routercontrol.TLSModeMutual
	}
	return routercontrol.TLSIntent{Mode: mode, CredentialBinding: credentialID(reference), VerifyHostname: verifyHostname}
}

func credentialID(reference string) routercontrol.ResourceID {
	return routercontrol.ResourceID("credential/" + reference)
}

func allocatePort(allocated map[string]int, key string) (int, error) {
	if port, ok := allocated[key]; ok {
		if port < firstDynamicPort || port > lastDynamicPort {
			return 0, fmt.Errorf("persisted port %d for %s is outside the valid range", port, key)
		}
		return port, nil
	}
	inUse := make(map[int]bool, len(allocated)+4)
	for _, port := range allocated {
		inUse[port] = true
	}
	for _, port := range []int{45671, 55671, 5671, 5672, 9090} {
		inUse[port] = true
	}
	for port := firstDynamicPort; port <= lastDynamicPort; port++ {
		if !inUse[port] {
			allocated[key] = port
			return port, nil
		}
	}
	return 0, fmt.Errorf("no ports available for %s", key)
}

func routingKeys(listener *skupperv2alpha1.MultiKeyListener) []string {
	if listener.Spec.Strategy.Priority != nil {
		return append([]string(nil), listener.Spec.Strategy.Priority.RoutingKeys...)
	}
	var result []string
	if listener.Spec.Strategy.Weighted != nil {
		for key := range listener.Spec.Strategy.Weighted.RoutingKeys {
			result = append(result, key)
		}
		sort.Strings(result)
	}
	return result
}

func credentialBindings(listeners []routercontrol.ServiceListener, connectors []routercontrol.ServiceConnector, connections []routercontrol.RouterConnection, access []routercontrol.RouterListener) []routercontrol.CredentialBinding {
	ids := map[routercontrol.ResourceID]bool{}
	for _, listener := range listeners {
		ids[listener.TLS.CredentialBinding] = true
	}
	for _, connector := range connectors {
		ids[connector.TLS.CredentialBinding] = true
	}
	for _, connection := range connections {
		ids[connection.TLS.CredentialBinding] = true
	}
	for _, listener := range access {
		ids[listener.TLS.CredentialBinding] = true
	}
	delete(ids, "")
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, string(id))
	}
	sort.Strings(ordered)
	result := make([]routercontrol.CredentialBinding, 0, len(ordered))
	for _, value := range ordered {
		result = append(result, routercontrol.CredentialBinding{ID: routercontrol.ResourceID(value), Provider: "kubernetes", Reference: value[len("credential/"):], Usages: []string{"tls"}})
	}
	return result
}

func copyListeners(in []routercontrol.ServiceListener) []routercontrol.ServiceListener {
	out := append([]routercontrol.ServiceListener(nil), in...)
	for i := range out {
		out[i].RoutingKeys = append([]string(nil), in[i].RoutingKeys...)
	}
	return out
}

func copyConnectors(in []routercontrol.ServiceConnector, target routercontrol.TargetIdentity) []routercontrol.ServiceConnector {
	out := append([]routercontrol.ServiceConnector(nil), in...)
	for i := range out {
		out[i].Target = target
		out[i].Endpoints = append([]routercontrol.Endpoint(nil), in[i].Endpoints...)
	}
	return out
}

func sortedListeners(in []*skupperv2alpha1.Listener) []*skupperv2alpha1.Listener {
	return sortedByUID(in, func(value *skupperv2alpha1.Listener) types.UID { return value.UID })
}

func sortedMultiKeyListeners(in []*skupperv2alpha1.MultiKeyListener) []*skupperv2alpha1.MultiKeyListener {
	return sortedByUID(in, func(value *skupperv2alpha1.MultiKeyListener) types.UID { return value.UID })
}

func sortedConnectors(in []*skupperv2alpha1.Connector) []*skupperv2alpha1.Connector {
	return sortedByUID(in, func(value *skupperv2alpha1.Connector) types.UID { return value.UID })
}

func sortedLinks(in []*skupperv2alpha1.Link) []*skupperv2alpha1.Link {
	return sortedByUID(in, func(value *skupperv2alpha1.Link) types.UID { return value.UID })
}

func sortedRouterAccess(in []*skupperv2alpha1.RouterAccess) []*skupperv2alpha1.RouterAccess {
	return sortedByUID(in, func(value *skupperv2alpha1.RouterAccess) types.UID { return value.UID })
}

func sortedBindings(in []*skupperv2alpha1.AttachedConnectorBinding) []*skupperv2alpha1.AttachedConnectorBinding {
	return sortedByUID(in, func(value *skupperv2alpha1.AttachedConnectorBinding) types.UID { return value.UID })
}

func sortedByUID[T any](in []T, uid func(T) types.UID) []T {
	out := append([]T(nil), in...)
	sort.Slice(out, func(i, j int) bool { return uid(out[i]) < uid(out[j]) })
	return out
}
