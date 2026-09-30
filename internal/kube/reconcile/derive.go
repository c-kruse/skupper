package reconcile

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/skupperproject/skupper/internal/qdr"
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
		Bootstrap:   copyBootstrap(snapshot.Bootstrap),
	}
	if !snapshot.Assignment.Controlled {
		return desired
	}
	active := activeSite(snapshot, &desired)
	if active == nil {
		deriveAccessComposition(snapshot, &desired, nil, nil)
		deriveStandaloneAccessStatuses(snapshot, &desired)
		deriveInactiveSiteStatuses(snapshot, &desired)
		return desired
	}
	desired.SiteUID = active.UID
	desired.Site = active.DeepCopy()
	if active.Spec.Edge && active.Spec.HA {
		desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: active.UID, Reason: "InvalidSite", Message: "Edge sites cannot have HA enabled"})
		deriveStatuses(snapshot, &desired)
		desired.SiteUID = ""
		desired.Site = nil
		return desired
	}
	settings, validSettings := routerSettings(active, &desired)
	if !validSettings {
		deriveStatuses(snapshot, &desired)
		desired.SiteUID = ""
		desired.Site = nil
		return desired
	}
	deriveRouterPrerequisites(active, &desired)
	diagnoseRouterPrerequisites(snapshot, active, &desired)
	controller, block := true, true
	desired.RouterControlCA = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: desired.Bootstrap.CABundleConfigMap, Namespace: snapshot.Namespace.Name, OwnerReferences: []metav1.OwnerReference{{APIVersion: skupperv2alpha1.SchemeGroupVersion.String(), Kind: "Site", Name: active.Name, UID: active.UID, Controller: &controller, BlockOwnerDeletion: &block}}}, Data: map[string]string{desired.Bootstrap.CABundleKey: string(desired.Bootstrap.PublicCA)}}
	if rendered, ok := snapshot.RenderedWorkloads[active.UID]; ok {
		desired.WorkloadsKnown = true
		for _, deployment := range rendered.Deployments {
			desired.Deployments = append(desired.Deployments, deployment.DeepCopy())
		}
		if rendered.LocalService != nil {
			desired.LocalService = rendered.LocalService.DeepCopy()
		}
	}
	diagnoseWorkloadOwnership(snapshot, active, &desired)
	if snapshot.Allocations.SiteUID == active.UID {
		desired.Allocations = copyAllocations(snapshot.Allocations)
	} else {
		desired.Allocations.SiteUID = active.UID
		desired.Allocations.ResourceVersion = snapshot.Allocations.ResourceVersion
	}

	groups := []string{"skupper-router"}
	if active.Spec.HA {
		groups = append(groups, "skupper-router-2")
	}
	routerAccesses := deriveAccessComposition(snapshot, &desired, active, groups)
	reserved := reservedPorts(routerAccesses)
	existingServices := map[string]*corev1.Service{}
	for _, service := range snapshot.Services {
		existingServices[service.Name] = service
	}
	listenerServices := map[string]*corev1.Service{}
	servicePortOwners := map[string]types.UID{}
	servicePortConflicts := map[string]bool{}
	internalTrafficPolicy := corev1.ServiceInternalTrafficPolicyCluster
	listeners := make([]routercontrol.ServiceListener, 0, len(snapshot.Listeners)+len(snapshot.MultiKeyListeners))
	for _, listener := range sortedListeners(snapshot.Listeners) {
		if listener.Spec.ExposePodsByName {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: listener.UID, Reason: "Unsupported", Message: "exposePodsByName is not supported by namespace reconciliation"})
			continue
		}
		listenerProtocol, err := protocol(listener.Spec.Type)
		if err != nil {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: listener.UID, Reason: "UnsupportedProtocol", Message: err.Error()})
			continue
		}
		if listener.Spec.Port < 1 || listener.Spec.Port > 65535 {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: listener.UID, Reason: "InvalidPort", Message: fmt.Sprintf("Listener service port %d is outside 1-65535", listener.Spec.Port)})
			continue
		}
		port, err := allocatePort(desired.Allocations.Ports, reserved, string(listener.UID)+"/listener")
		if err != nil {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: listener.UID, Reason: "PortExhausted", Message: err.Error()})
			continue
		}
		listeners = append(listeners, routercontrol.ServiceListener{
			ID:          resourceID(listener.UID, "listener"),
			RoutingKeys: []string{listener.Spec.RoutingKey},
			Host:        "0.0.0.0",
			Port:        uint16(port),
			Protocol:    listenerProtocol,
			Observer:    listener.Spec.Observer,
			TLS:         serverTLSIntent(listener.Spec.TlsCredentials, false),
		})
		if listener.Spec.Host != "" {
			if current := existingServices[listener.Spec.Host]; current != nil && (current.Annotations[controlledAnnotation] != "true" || !ownedBy(current.OwnerReferences, active.UID)) {
				desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: listener.UID, Reason: "ForeignService", Message: fmt.Sprintf("Service %s/%s is not owned by the active Site", snapshot.Namespace.Name, listener.Spec.Host)})
				continue
			}
			service := listenerServices[listener.Spec.Host]
			if service == nil {
				controller, block := true, true
				service = &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: listener.Spec.Host, Namespace: snapshot.Namespace.Name, Labels: map[string]string{"internal.skupper.io/listener": "true"}, Annotations: map[string]string{"internal.skupper.io/controlled": "true"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: skupperv2alpha1.SchemeGroupVersion.String(), Kind: "Site", Name: active.Name, UID: active.UID, Controller: &controller, BlockOwnerDeletion: &block}}}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, SessionAffinity: corev1.ServiceAffinityNone, InternalTrafficPolicy: &internalTrafficPolicy, Selector: map[string]string{"skupper.io/component": "router"}}}
				listenerServices[listener.Spec.Host] = service
			}
			addListenerServicePort(&desired, service, listener.UID, corev1.Protocol(strings.ToUpper(string(listenerProtocol))), listener.Spec.Port, port, servicePortOwners, servicePortConflicts)
		}
	}
	for _, listener := range sortedMultiKeyListeners(snapshot.MultiKeyListeners) {
		if listener.Spec.Port < 1 || listener.Spec.Port > 65535 {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: listener.UID, Reason: "InvalidPort", Message: fmt.Sprintf("MultiKeyListener service port %d is outside 1-65535", listener.Spec.Port)})
			continue
		}
		keys, strategy, weights := multiKeyRouting(listener)
		invalidStrategy := ""
		if len(keys) == 0 {
			invalidStrategy = "MultiKeyListener strategy requires at least one routing key"
		}
		for _, key := range keys {
			if strings.TrimSpace(key) == "" {
				invalidStrategy = "MultiKeyListener routing keys must not be empty"
			}
		}
		if invalidStrategy != "" {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: listener.UID, Reason: "InvalidStrategy", Message: invalidStrategy})
			continue
		}
		port, err := allocatePort(desired.Allocations.Ports, reserved, string(listener.UID)+"/listener")
		if err != nil {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: listener.UID, Reason: "PortExhausted", Message: err.Error()})
			continue
		}
		listeners = append(listeners, routercontrol.ServiceListener{ID: resourceID(listener.UID, "listener"), RoutingKeys: keys, RoutingStrategy: strategy, RoutingKeyWeights: weights, Host: "0.0.0.0", Port: uint16(port), Protocol: routercontrol.ProtocolTCP, Observer: listener.Spec.Observer, TLS: serverTLSIntent(listener.Spec.TlsCredentials, listener.Spec.RequireClientCert)})
		if listener.Spec.Host != "" {
			if current := existingServices[listener.Spec.Host]; current != nil && (current.Annotations[controlledAnnotation] != "true" || !ownedBy(current.OwnerReferences, active.UID)) {
				desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: listener.UID, Reason: "ForeignService", Message: fmt.Sprintf("Service %s/%s is not owned by the active Site", snapshot.Namespace.Name, listener.Spec.Host)})
				continue
			}
			service := listenerServices[listener.Spec.Host]
			if service == nil {
				controller, block := true, true
				service = &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: listener.Spec.Host, Namespace: snapshot.Namespace.Name, Labels: map[string]string{"internal.skupper.io/listener": "true"}, Annotations: map[string]string{"internal.skupper.io/controlled": "true"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: skupperv2alpha1.SchemeGroupVersion.String(), Kind: "Site", Name: active.Name, UID: active.UID, Controller: &controller, BlockOwnerDeletion: &block}}}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, SessionAffinity: corev1.ServiceAffinityNone, InternalTrafficPolicy: &internalTrafficPolicy, Selector: map[string]string{"skupper.io/component": "router"}}}
				listenerServices[listener.Spec.Host] = service
			}
			addListenerServicePort(&desired, service, listener.UID, corev1.ProtocolTCP, listener.Spec.Port, port, servicePortOwners, servicePortConflicts)
		}
	}
	connectors := deriveConnectors(snapshot, &desired)
	connections := make([]routercontrol.RouterConnection, 0, len(snapshot.Links))
	for _, link := range sortedLinks(snapshot.Links) {
		role := "inter-router"
		if active.Spec.Edge {
			role = "edge"
		}
		endpoint, found := link.Spec.GetEndpointForRole(role)
		if !found || endpoint.Host == "" {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: link.UID, Reason: "MissingEndpoint", Message: fmt.Sprintf("Link has no valid %s endpoint", role)})
			continue
		}
		port, err := strconv.ParseUint(endpoint.Port, 10, 16)
		if err != nil || port == 0 {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: link.UID, Reason: "InvalidPort", Message: fmt.Sprintf("invalid %s endpoint port %q", role, endpoint.Port)})
			continue
		}
		connection := routercontrol.RouterConnection{ID: resourceID(link.UID, "link/"+endpoint.Name), Host: endpoint.Host, Port: uint16(port), Role: endpoint.Name, Cost: uint32(max(link.Spec.Cost, 0)), TLS: clientTLSIntent(link.Spec.TlsCredentials, true, false)}
		if proxy := link.Spec.GetProxyConfiguration(); proxy != "" {
			connection.ProxyCredentialBinding = routercontrol.ResourceID("proxy/" + proxy)
		}
		connections = append(connections, connection)
	}
	access := make([]routercontrol.RouterListener, 0)
	for _, routerAccess := range sortedRouterAccess(routerAccesses) {
		for _, role := range routerAccess.Spec.Roles {
			port := int(role.GetPort())
			if port < 1 || port > 65535 {
				desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: routerAccess.UID, Reason: "InvalidPort", Message: fmt.Sprintf("RouterAccess role %q port %d is outside 1-65535", role.Name, port)})
				continue
			}
			host := routerAccess.Spec.BindHost
			if host == "" {
				host = "0.0.0.0"
			}
			access = append(access, routercontrol.RouterListener{ID: resourceID(routerAccess.UID, "access/"+role.Name), Role: role.Name, Port: uint16(port), Host: host, TLS: serverTLSIntent(routerAccess.Spec.TlsCredentials, true)})
		}
	}
	for groupIndex, group := range groups {
		target := RouterTarget{NamespaceUID: string(snapshot.Namespace.UID), SiteUID: string(active.UID), RouterGroup: group}
		groupConnections := append([]routercontrol.RouterConnection(nil), connections...)
		if groupIndex > 0 {
			for _, routerAccess := range sortedRouterAccess(routerAccesses) {
				role := routerAccess.FindRole("inter-router")
				if role == nil {
					continue
				}
				port := int(role.GetPort())
				if port < 1 || port > 65535 {
					break
				}
				for _, previousGroup := range groups[:groupIndex] {
					groupConnections = append(groupConnections, routercontrol.RouterConnection{ID: resourceID(routerAccess.UID, "local/"+previousGroup+"/inter-router"), Host: previousGroup, Port: uint16(port), Role: "inter-router", Cost: 1, TLS: clientTLSIntent(routerAccess.Spec.TlsCredentials, true, false)})
				}
				break
			}
		}
		desired.Intents[target] = routercontrol.RouterIntent{SchemaVersion: routercontrol.SchemaVersion, Target: target, Settings: settings, ServiceListeners: copyListeners(listeners), ServiceConnectors: copyConnectors(connectors, target), RouterConnections: groupConnections, RouterListeners: append([]routercontrol.RouterListener(nil), access...), CredentialBindings: credentialBindings(listeners, connectors, groupConnections, access)}
	}
	for _, service := range listenerServices {
		if len(service.Spec.Ports) == 0 {
			continue
		}
		sort.Slice(service.Spec.Ports, func(i, j int) bool { return service.Spec.Ports[i].Name < service.Spec.Ports[j].Name })
		desired.ListenerServices = append(desired.ListenerServices, service)
	}
	sort.Slice(desired.ListenerServices, func(i, j int) bool { return desired.ListenerServices[i].Name < desired.ListenerServices[j].Name })
	deriveStatuses(snapshot, &desired)
	return desired
}

func diagnoseWorkloadOwnership(snapshot Snapshot, site *skupperv2alpha1.Site, desired *DesiredNamespace) {
	if current := snapshot.RouterControlCA; current != nil && !metav1.IsControlledBy(current, site) {
		desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: site.UID, Reason: "ForeignRouterControlCA", Message: fmt.Sprintf("router-control CA ConfigMap %s/%s is not controlled by Site UID %s", current.Namespace, current.Name, site.UID)})
	}
	desiredDeployments := map[string]bool{}
	for _, deployment := range desired.Deployments {
		desiredDeployments[deployment.Name] = true
	}
	for _, current := range snapshot.Deployments {
		if desiredDeployments[current.Name] && !metav1.IsControlledBy(current, site) {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: site.UID, Reason: "ForeignDeployment", Message: fmt.Sprintf("router Deployment %s/%s is not controlled by Site UID %s", current.Namespace, current.Name, site.UID)})
		}
	}
	if desired.LocalService != nil {
		for _, current := range snapshot.Services {
			if current.Name == desired.LocalService.Name && !ownedBy(current.OwnerReferences, site.UID) {
				desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: site.UID, Reason: "ForeignService", Message: fmt.Sprintf("router Service %s/%s is not owned by Site UID %s", current.Namespace, current.Name, site.UID)})
			}
		}
	}
}

func addListenerServicePort(desired *DesiredNamespace, service *corev1.Service, resource types.UID, protocol corev1.Protocol, servicePort, targetPort int, owners map[string]types.UID, conflicts map[string]bool) {
	key := service.Name + "/" + string(protocol) + "/" + strconv.Itoa(servicePort)
	if conflicts[key] {
		desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: resource, Reason: "ServicePortConflict", Message: fmt.Sprintf("Service %s port %d/%s is used by multiple listeners", service.Name, servicePort, protocol)})
		return
	}
	if owner, found := owners[key]; found {
		desired.Diagnostics = append(desired.Diagnostics,
			Diagnostic{Resource: owner, Reason: "ServicePortConflict", Message: fmt.Sprintf("Service %s port %d/%s is used by multiple listeners", service.Name, servicePort, protocol)},
			Diagnostic{Resource: resource, Reason: "ServicePortConflict", Message: fmt.Sprintf("Service %s port %d/%s is used by multiple listeners", service.Name, servicePort, protocol)},
		)
		conflicts[key] = true
		for index := range service.Spec.Ports {
			if service.Spec.Ports[index].Port == int32(servicePort) && service.Spec.Ports[index].Protocol == protocol {
				service.Spec.Ports = append(service.Spec.Ports[:index], service.Spec.Ports[index+1:]...)
				break
			}
		}
		return
	}
	owners[key] = resource
	service.Spec.Ports = append(service.Spec.Ports, corev1.ServicePort{Name: strings.ToLower(string(protocol)) + "-" + strconv.Itoa(targetPort), Port: int32(servicePort), TargetPort: intstr.FromInt(targetPort), Protocol: protocol})
}

func deriveRouterPrerequisites(site *skupperv2alpha1.Site, desired *DesiredNamespace) {
	if site.Spec.ServiceAccount != "" {
		return
	}
	controller, block := true, true
	owner := []metav1.OwnerReference{{APIVersion: skupperv2alpha1.SchemeGroupVersion.String(), Kind: "Site", Name: site.Name, UID: site.UID, Controller: &controller, BlockOwnerDeletion: &block}}
	desired.ServiceAccount = &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "skupper-router", Namespace: site.Namespace, OwnerReferences: owner}}
	desired.Role = &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "skupper-router", Namespace: site.Namespace, OwnerReferences: owner}, Rules: []rbacv1.PolicyRule{{Verbs: []string{"get", "list", "watch"}, APIGroups: []string{""}, Resources: []string{"secrets"}}}}
	desired.RoleBinding = &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "skupper-router", Namespace: site.Namespace, OwnerReferences: owner}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "skupper-router", Namespace: site.Namespace}}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "skupper-router"}}
}

func diagnoseRouterPrerequisites(snapshot Snapshot, site *skupperv2alpha1.Site, desired *DesiredNamespace) {
	if desired.ServiceAccount == nil {
		return
	}
	for _, current := range snapshot.ServiceAccounts {
		if current.Name == desired.ServiceAccount.Name && !metav1.IsControlledBy(current, site) {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: site.UID, Reason: "ForeignServiceAccount", Message: fmt.Sprintf("router ServiceAccount %s/%s is not controlled by Site UID %s", site.Namespace, current.Name, site.UID)})
		}
	}
	for _, current := range snapshot.Roles {
		if current.Name == desired.Role.Name && !metav1.IsControlledBy(current, site) {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: site.UID, Reason: "ForeignRole", Message: fmt.Sprintf("router Role %s/%s is not controlled by Site UID %s", site.Namespace, current.Name, site.UID)})
		}
	}
	for _, current := range snapshot.RoleBindings {
		if current.Name == desired.RoleBinding.Name && !metav1.IsControlledBy(current, site) {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: site.UID, Reason: "ForeignRoleBinding", Message: fmt.Sprintf("router RoleBinding %s/%s is not controlled by Site UID %s", site.Namespace, current.Name, site.UID)})
		}
	}
}

func activeSite(snapshot Snapshot, desired *DesiredNamespace) *skupperv2alpha1.Site {
	if len(snapshot.Sites) == 0 {
		return nil
	}
	if snapshot.Allocations.SiteUID != "" {
		for _, site := range snapshot.Sites {
			if site.UID == snapshot.Allocations.SiteUID {
				for _, other := range snapshot.Sites {
					if other.UID != site.UID {
						desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: other.UID, Reason: "SiteConflict", Message: fmt.Sprintf("Site %s/%s is already active in this namespace", site.Namespace, site.Name)})
					}
				}
				return site
			}
		}
	}
	if len(snapshot.Sites) != 1 {
		for _, site := range snapshot.Sites {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: site.UID, Reason: "SiteConflict", Message: "multiple Sites exist and no established Site UID identifies the active owner"})
		}
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
		connectorProtocol, protocolErr := protocol(connector.Spec.Type)
		if protocolErr != nil {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: connector.UID, Reason: "UnsupportedProtocol", Message: protocolErr.Error()})
			continue
		}
		endpoints, err := connectorEndpoints(connector.Namespace, connector.Spec.Host, connector.Spec.Selector, connector.Spec.Port, connector.Spec.IncludeNotReadyPods, snapshot.Pods)
		if err != nil {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: connector.UID, Reason: "InvalidSelector", Message: err.Error()})
			continue
		}
		if len(endpoints) == 0 {
			continue
		}
		result = append(result, routercontrol.ServiceConnector{ID: resourceID(connector.UID, "connector"), RoutingKey: connector.Spec.RoutingKey, Protocol: connectorProtocol, Endpoints: endpoints, TLS: clientTLSIntent(connector.Spec.TlsCredentials, connector.Spec.UseClientCert, connector.Spec.VerifyHostname)})
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
		if !attachedSourceControlled(snapshot, definition.Namespace) {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: binding.UID, Reason: "SourceNotControlled", Message: "AttachedConnector and AttachedConnectorBinding namespaces must be managed by the same controller"})
			continue
		}
		connectorProtocol, protocolErr := protocol(definition.Spec.Type)
		if protocolErr != nil {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: definition.UID, Reason: "UnsupportedProtocol", Message: protocolErr.Error()})
			continue
		}
		endpoints, err := connectorEndpoints(definition.Namespace, "", definition.Spec.Selector, definition.Spec.Port, definition.Spec.IncludeNotReadyPods, snapshot.Pods)
		if err != nil {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: definition.UID, Reason: "InvalidSelector", Message: err.Error()})
			continue
		}
		if len(endpoints) == 0 {
			continue
		}
		if desired.AttachedSources == nil {
			desired.AttachedSources = map[string]types.UID{}
		}
		desired.AttachedSources[definition.Namespace] = snapshot.SourceNamespaces[definition.Namespace]
		result = append(result, routercontrol.ServiceConnector{ID: resourceID(binding.UID, "attached-connector"), RoutingKey: binding.Spec.RoutingKey, Protocol: connectorProtocol, Endpoints: endpoints, TLS: clientTLSIntent(definition.Spec.TlsCredentials, definition.Spec.UseClientCert, false)})
	}
	return result
}

func attachedSourceControlled(snapshot Snapshot, namespace string) bool {
	assignment := snapshot.SourceAssignments[namespace]
	return snapshot.SourceNamespaces[namespace] != "" && assignment.Controlled && assignment.Controller == snapshot.Assignment.Controller
}

func connectorEndpoints(namespace, host, selector string, port int, includeNotReady bool, pods []*corev1.Pod) ([]routercontrol.Endpoint, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("port %d is outside 1-65535", port)
	}
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
		if pod.Namespace != namespace || !parsed.Matches(labels.Set(pod.Labels)) || pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" || (!includeNotReady && !podReady(pod)) {
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

func protocol(value string) (routercontrol.Protocol, error) {
	switch value {
	case "", "tcp":
		return routercontrol.ProtocolTCP, nil
	default:
		return "", fmt.Errorf("protocol %q is not supported", value)
	}
}

func clientTLSIntent(reference string, mutual, verifyHostname bool) routercontrol.TLSIntent {
	if reference == "" {
		return routercontrol.TLSIntent{Mode: routercontrol.TLSModeDisabled}
	}
	mode := routercontrol.TLSModeClient
	if mutual {
		mode = routercontrol.TLSModeMutual
	}
	return routercontrol.TLSIntent{Mode: mode, CredentialBinding: credentialID(reference), VerifyHostname: verifyHostname}
}

func serverTLSIntent(reference string, requireClientCert bool) routercontrol.TLSIntent {
	if reference == "" {
		return routercontrol.TLSIntent{Mode: routercontrol.TLSModeDisabled}
	}
	mode := routercontrol.TLSModeServer
	if requireClientCert {
		mode = routercontrol.TLSModeMutual
	}
	return routercontrol.TLSIntent{Mode: mode, CredentialBinding: credentialID(reference)}
}

func credentialID(reference string) routercontrol.ResourceID {
	return routercontrol.ResourceID("credential/" + reference)
}

func allocatePort(allocated map[string]int, reserved map[int]bool, key string) (int, error) {
	if port, ok := allocated[key]; ok {
		if port < firstDynamicPort || port > lastDynamicPort {
			return 0, fmt.Errorf("persisted port %d for %s is outside the valid range", port, key)
		}
		return port, nil
	}
	inUse := make(map[int]bool, len(allocated)+len(reserved)+4)
	for _, port := range allocated {
		inUse[port] = true
	}
	for port := range reserved {
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

func reservedPorts(accesses []*skupperv2alpha1.RouterAccess) map[int]bool {
	result := map[int]bool{45671: true, 55671: true, 5671: true, 5672: true, 9090: true}
	for _, access := range accesses {
		for _, role := range access.Spec.Roles {
			result[int(role.GetPort())] = true
		}
	}
	return result
}

func routingKeys(listener *skupperv2alpha1.MultiKeyListener) []string {
	keys, _, _ := multiKeyRouting(listener)
	return keys
}

func multiKeyRouting(listener *skupperv2alpha1.MultiKeyListener) ([]string, routercontrol.RoutingStrategy, map[string]uint) {
	if listener.Spec.Strategy.Priority != nil {
		// Preserve the existing wire representation: an omitted strategy with
		// multiple ordered keys is compiled as priority.
		return append([]string(nil), listener.Spec.Strategy.Priority.RoutingKeys...), "", nil
	}
	var result []string
	if listener.Spec.Strategy.Weighted != nil {
		for key := range listener.Spec.Strategy.Weighted.RoutingKeys {
			result = append(result, key)
		}
		sort.Strings(result)
		weights := make(map[string]uint, len(listener.Spec.Strategy.Weighted.RoutingKeys))
		for key, weight := range listener.Spec.Strategy.Weighted.RoutingKeys {
			weights[key] = weight
		}
		return result, routercontrol.RoutingStrategyWeighted, weights
	}
	return result, "", nil
}

func credentialBindings(listeners []routercontrol.ServiceListener, connectors []routercontrol.ServiceConnector, connections []routercontrol.RouterConnection, access []routercontrol.RouterListener) []routercontrol.CredentialBinding {
	type requirement struct {
		reference string
		usages    map[string]bool
	}
	ids := map[routercontrol.ResourceID]requirement{}
	add := func(id routercontrol.ResourceID, prefix, usage string) {
		if id == "" {
			return
		}
		requirement := ids[id]
		requirement.reference = strings.TrimPrefix(string(id), prefix)
		if requirement.usages == nil {
			requirement.usages = map[string]bool{}
		}
		requirement.usages[usage] = true
		ids[id] = requirement
	}
	addTLS := func(tls routercontrol.TLSIntent, inbound bool) {
		if tls.CredentialBinding == "" {
			return
		}
		if inbound {
			add(tls.CredentialBinding, "credential/", routercontrol.CredentialUsageServerAuth)
			if tls.Mode == routercontrol.TLSModeMutual {
				add(tls.CredentialBinding, "credential/", routercontrol.CredentialUsageTrust)
			}
			return
		}
		add(tls.CredentialBinding, "credential/", routercontrol.CredentialUsageTrust)
		if tls.Mode == routercontrol.TLSModeMutual {
			add(tls.CredentialBinding, "credential/", routercontrol.CredentialUsageClientAuth)
		}
	}
	for _, listener := range listeners {
		addTLS(listener.TLS, true)
	}
	for _, connector := range connectors {
		addTLS(connector.TLS, false)
	}
	for _, connection := range connections {
		addTLS(connection.TLS, false)
		add(connection.ProxyCredentialBinding, "proxy/", routercontrol.CredentialUsageProxy)
	}
	for _, listener := range access {
		addTLS(listener.TLS, true)
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, string(id))
	}
	sort.Strings(ordered)
	result := make([]routercontrol.CredentialBinding, 0, len(ordered))
	for _, value := range ordered {
		requirement := ids[routercontrol.ResourceID(value)]
		usages := make([]string, 0, len(requirement.usages))
		for usage := range requirement.usages {
			usages = append(usages, usage)
		}
		sort.Strings(usages)
		result = append(result, routercontrol.CredentialBinding{ID: routercontrol.ResourceID(value), Provider: routercontrol.CredentialProviderKubernetesSecret, Reference: requirement.reference, Usages: usages})
	}
	return result
}

func routerSettings(site *skupperv2alpha1.Site, desired *DesiredNamespace) (routercontrol.RouterSettings, bool) {
	settings := routercontrol.RouterSettings{Mode: map[bool]routercontrol.RoutingMode{true: routercontrol.RoutingModeEdge, false: routercontrol.RoutingModeInterior}[site.Spec.Edge]}
	if value := site.Spec.GetRouterDataConnectionCount(); value != "" {
		parsed, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: site.UID, Reason: "InvalidSetting", Message: fmt.Sprintf("invalid router-data-connection-count: %v", err)})
			return settings, false
		}
		settings.DataConnectionCount = uint32(parsed)
	}
	if value := site.Spec.GetRouterLogging(); value != "" {
		parsed, err := qdr.ParseRouterLogConfig(value)
		if err != nil {
			desired.Diagnostics = append(desired.Diagnostics, Diagnostic{Resource: site.UID, Reason: "InvalidSetting", Message: err.Error()})
			return settings, false
		}
		for _, entry := range parsed {
			settings.Logging = append(settings.Logging, routercontrol.RouterLogSetting{Module: entry.Module, Level: entry.Level})
		}
	}
	return settings, true
}

func copyListeners(in []routercontrol.ServiceListener) []routercontrol.ServiceListener {
	out := append([]routercontrol.ServiceListener(nil), in...)
	for i := range out {
		out[i].RoutingKeys = append([]string(nil), in[i].RoutingKeys...)
		if in[i].RoutingKeyWeights != nil {
			out[i].RoutingKeyWeights = make(map[string]uint, len(in[i].RoutingKeyWeights))
			for key, weight := range in[i].RoutingKeyWeights {
				out[i].RoutingKeyWeights[key] = weight
			}
		}
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
