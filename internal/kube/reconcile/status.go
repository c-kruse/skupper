package reconcile

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/skupperproject/skupper/internal/certs"
	"github.com/skupperproject/skupper/internal/kube/certificates"
	"github.com/skupperproject/skupper/internal/routercontrol"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

type targetEvidence struct {
	observations []Observation
	digest       routercontrol.Digest
	currentPods  int
}

func deriveStatuses(snapshot Snapshot, desired *DesiredNamespace) {
	if desired.Site == nil {
		return
	}
	desired.Statuses.Owner = desired.Site.DeepCopy()
	desired.Statuses.SourceNamespaces = make(map[string]types.UID, len(snapshot.SourceNamespaces))
	for namespace, uid := range snapshot.SourceNamespaces {
		desired.Statuses.SourceNamespaces[namespace] = uid
	}
	evidence := map[RouterTarget]targetEvidence{}
	for target, intent := range desired.Intents {
		_, digest, err := routercontrol.CanonicalIntent(intent)
		if err != nil {
			continue
		}
		observations, currentPods := currentTargetObservations(snapshot, target)
		evidence[target] = targetEvidence{observations: observations, digest: digest, currentPods: currentPods}
	}
	evaluationTime := snapshot.EvaluationTime
	if evaluationTime.IsZero() {
		evaluationTime = time.Unix(1, 0).UTC()
	}
	now := metav1.NewTime(evaluationTime)

	for _, current := range sortedListeners(snapshot.Listeners) {
		updated := current.DeepCopy()
		before := updated.Status.DeepCopy()
		id := resourceID(updated.UID, "listener")
		configured := combineStates(configuredState(desired.Diagnostics, updated.UID), resourceApplied(evidence, id))
		operational := resourceOperational(evidence, id)
		matched, _ := routingKeysReachable(evidence, []string{updated.Spec.RoutingKey})
		updated.Status.HasMatchingConnector = matched.Status == metav1.ConditionTrue
		setStatusCondition(&updated.Status.Status, skupperv2alpha1.CONDITION_TYPE_CONFIGURED, configured, updated.Generation, now)
		setStatusCondition(&updated.Status.Status, skupperv2alpha1.CONDITION_TYPE_OPERATIONAL, operational, updated.Generation, now)
		setStatusCondition(&updated.Status.Status, skupperv2alpha1.CONDITION_TYPE_MATCHED, matched, updated.Generation, now)
		aggregateStatus(&updated.Status.Status, updated.Generation, now, skupperv2alpha1.CONDITION_TYPE_CONFIGURED, skupperv2alpha1.CONDITION_TYPE_OPERATIONAL, skupperv2alpha1.CONDITION_TYPE_MATCHED)
		if !reflect.DeepEqual(*before, updated.Status) {
			desired.Statuses.Listeners = append(desired.Statuses.Listeners, updated)
		}
	}
	for _, current := range sortedMultiKeyListeners(snapshot.MultiKeyListeners) {
		updated := current.DeepCopy()
		before := updated.Status.DeepCopy()
		configured := combineStates(configuredState(desired.Diagnostics, updated.UID), resourceApplied(evidence, resourceID(updated.UID, "listener")))
		socket := resourceOperational(evidence, resourceID(updated.UID, "listener"))
		matched, reachable := routingKeysReachable(evidence, routingKeys(updated))
		setMultiKeyCondition(&updated.Status, skupperv2alpha1.CONDITION_TYPE_CONFIGURED, configured, updated.Generation, now)
		setMultiKeyCondition(&updated.Status, skupperv2alpha1.CONDITION_TYPE_OPERATIONAL, combineStates(socket, matched), updated.Generation, now)
		updated.Status.HasDestination = matched.Status == metav1.ConditionTrue
		updated.SetRoutingKeysReachable(reachable)
		aggregateMultiKey(&updated.Status, updated.Generation, now)
		if !reflect.DeepEqual(*before, updated.Status) {
			desired.Statuses.MultiKey = append(desired.Statuses.MultiKey, updated)
		}
	}
	for _, current := range sortedConnectors(snapshot.Connectors) {
		updated := current.DeepCopy()
		before := updated.Status.DeepCopy()
		state := combineStates(configuredState(desired.Diagnostics, updated.UID), resourceApplied(evidence, resourceID(updated.UID, "connector")))
		selected := selectedPodsForConnector(snapshot, updated)
		if updated.Spec.Selector != "" && len(selected) == 0 && state.Reason != skupperv2alpha1.StatusError {
			state = pendingState("No selected endpoints")
		}
		updated.Status.SelectedPods = selected
		updated.Status.HasMatchingListener = false
		removeCondition(&updated.Status.Status, skupperv2alpha1.CONDITION_TYPE_MATCHED)
		setStatusCondition(&updated.Status.Status, skupperv2alpha1.CONDITION_TYPE_CONFIGURED, state, updated.Generation, now)
		aggregateStatus(&updated.Status.Status, updated.Generation, now, skupperv2alpha1.CONDITION_TYPE_CONFIGURED)
		if !reflect.DeepEqual(*before, updated.Status) {
			desired.Statuses.Connectors = append(desired.Statuses.Connectors, updated)
		}
	}
	for _, current := range sortedBindings(snapshot.Bindings) {
		updated := current.DeepCopy()
		before := updated.Status.DeepCopy()
		state := combineStates(configuredState(desired.Diagnostics, updated.UID), resourceApplied(evidence, resourceID(updated.UID, "attached-connector")))
		updated.Status.HasMatchingListener = false
		removeCondition(&updated.Status.Status, skupperv2alpha1.CONDITION_TYPE_MATCHED)
		setStatusCondition(&updated.Status.Status, skupperv2alpha1.CONDITION_TYPE_CONFIGURED, state, updated.Generation, now)
		aggregateStatus(&updated.Status.Status, updated.Generation, now, skupperv2alpha1.CONDITION_TYPE_CONFIGURED)
		if !reflect.DeepEqual(*before, updated.Status) {
			desired.Statuses.Bindings = append(desired.Statuses.Bindings, updated)
		}
	}
	for _, current := range snapshot.Attached {
		if current.Spec.SiteNamespace != snapshot.Namespace.Name {
			continue
		}
		updated := current.DeepCopy()
		before := updated.Status.DeepCopy()
		var binding *skupperv2alpha1.AttachedConnectorBinding
		for _, candidate := range snapshot.Bindings {
			if candidate.Spec.ConnectorNamespace == current.Namespace && candidate.Name == current.Name {
				binding = candidate
				break
			}
		}
		state := pendingState("No authorizing AttachedConnectorBinding")
		if binding != nil {
			state = combineStates(configuredState(desired.Diagnostics, updated.UID), resourceApplied(evidence, resourceID(binding.UID, "attached-connector")))
		}
		selected := selectedPodsForAttached(snapshot, updated)
		if len(selected) == 0 {
			state = pendingState("No selected endpoints")
		}
		updated.Status.SelectedPods = selected
		setStatusCondition(&updated.Status.Status, skupperv2alpha1.CONDITION_TYPE_CONFIGURED, state, updated.Generation, now)
		aggregateStatus(&updated.Status.Status, updated.Generation, now, skupperv2alpha1.CONDITION_TYPE_CONFIGURED)
		if !reflect.DeepEqual(*before, updated.Status) {
			desired.Statuses.Attached = append(desired.Statuses.Attached, updated)
		}
	}
	for _, current := range sortedLinks(snapshot.Links) {
		updated := current.DeepCopy()
		before := updated.Status.DeepCopy()
		ids := make([]routercontrol.ResourceID, 0, len(updated.Spec.Endpoints))
		for _, endpoint := range updated.Spec.Endpoints {
			ids = append(ids, resourceID(updated.UID, "link/"+endpoint.Name))
		}
		setStatusCondition(&updated.Status.Status, skupperv2alpha1.CONDITION_TYPE_CONFIGURED, combineStates(configuredState(desired.Diagnostics, updated.UID), resourcesApplied(evidence, ids)), updated.Generation, now)
		setStatusCondition(&updated.Status.Status, skupperv2alpha1.CONDITION_TYPE_OPERATIONAL, resourcesOperational(evidence, ids), updated.Generation, now)
		aggregateStatus(&updated.Status.Status, updated.Generation, now, skupperv2alpha1.CONDITION_TYPE_CONFIGURED, skupperv2alpha1.CONDITION_TYPE_OPERATIONAL)
		if !reflect.DeepEqual(*before, updated.Status) {
			desired.Statuses.Links = append(desired.Statuses.Links, updated)
		}
	}
	serviceByName := map[string]*corev1.Service{}
	for _, service := range snapshot.Services {
		serviceByName[service.Name] = service
	}
	routeByName := map[string]string{}
	for _, route := range snapshot.Routes {
		for _, ingress := range route.Status.Ingress {
			if ingress.Host != "" {
				routeByName[route.Name] = ingress.Host
				break
			}
		}
	}
	secretByName := map[string]*corev1.Secret{}
	for _, secret := range snapshot.Secrets {
		secretByName[secret.Name] = secret
	}
	secured := append([]*skupperv2alpha1.SecuredAccess(nil), snapshot.SecuredAccesses...)
	sort.Slice(secured, func(i, j int) bool { return secured[i].Name < secured[j].Name })
	securedEndpoints := map[string][]skupperv2alpha1.Endpoint{}
	for _, current := range secured {
		updated := current.DeepCopy()
		before := updated.Status.DeepCopy()
		service := serviceByName[updated.Name]
		configured := pendingState("Exposure Service has not been realized")
		resolved := pendingState("No external endpoint has been resolved")
		var endpoints []skupperv2alpha1.Endpoint
		if service != nil && ownedBy(service.OwnerReferences, updated.UID) {
			configured = skupperv2alpha1.ReadyCondition()
			accessType := updated.Spec.AccessType
			if accessType == "" {
				accessType = snapshot.DefaultAccessType
			}
			switch accessType {
			case "local":
				for _, port := range updated.Spec.Ports {
					endpoints = append(endpoints, skupperv2alpha1.Endpoint{Name: port.Name, Host: updated.Name + "." + updated.Namespace, Port: strconv.Itoa(port.Port)})
				}
			case "loadbalancer":
				for _, ingress := range service.Status.LoadBalancer.Ingress {
					host := ingress.IP
					if host == "" {
						host = ingress.Hostname
					}
					if host == "" {
						continue
					}
					for _, port := range service.Spec.Ports {
						endpoints = append(endpoints, skupperv2alpha1.Endpoint{Name: port.Name, Host: host, Port: strconv.Itoa(int(port.Port))})
					}
				}
			case "route":
				for _, port := range updated.Spec.Ports {
					if host := routeByName[updated.Name+"-"+port.Name]; host != "" {
						endpoints = append(endpoints, skupperv2alpha1.Endpoint{Name: port.Name, Host: host, Port: "443"})
					}
				}
			case "ingress", "ingress-nginx":
				endpoints = ingressEndpoints(snapshot.Ingresses, updated)
			case "nodeport":
				if snapshot.ClusterHost == "" {
					resolved = unknownState("Cluster host is not configured for nodeport access")
					break
				}
				for _, port := range service.Spec.Ports {
					if port.NodePort != 0 {
						endpoints = append(endpoints, skupperv2alpha1.Endpoint{Name: port.Name, Host: snapshot.ClusterHost, Port: strconv.Itoa(int(port.NodePort))})
					}
				}
			default:
				resolved = unknownState("Endpoint observation for this access type is unavailable")
			}
			if len(endpoints) > 0 {
				resolved = skupperv2alpha1.ReadyCondition()
			}
		}
		sort.Slice(endpoints, func(i, j int) bool {
			if endpoints[i].Name != endpoints[j].Name {
				return endpoints[i].Name < endpoints[j].Name
			}
			if endpoints[i].Host != endpoints[j].Host {
				return endpoints[i].Host < endpoints[j].Host
			}
			return endpoints[i].Port < endpoints[j].Port
		})
		updated.Status.Endpoints = endpoints
		securedEndpoints[updated.Name] = endpoints
		setStatusCondition(&updated.Status.Status, skupperv2alpha1.CONDITION_TYPE_CONFIGURED, configured, updated.Generation, now)
		setStatusCondition(&updated.Status.Status, skupperv2alpha1.CONDITION_TYPE_RESOLVED, resolved, updated.Generation, now)
		aggregateStatus(&updated.Status.Status, updated.Generation, now, skupperv2alpha1.CONDITION_TYPE_CONFIGURED, skupperv2alpha1.CONDITION_TYPE_RESOLVED)
		if !reflect.DeepEqual(*before, updated.Status) {
			desired.Statuses.SecuredAccesses = append(desired.Statuses.SecuredAccesses, updated)
		}
	}
	certificatesInOrder := append([]*skupperv2alpha1.Certificate(nil), snapshot.Certificates...)
	sort.Slice(certificatesInOrder, func(i, j int) bool { return certificatesInOrder[i].Name < certificatesInOrder[j].Name })
	for _, current := range certificatesInOrder {
		updated := current.DeepCopy()
		before := updated.Status.DeepCopy()
		state, expiration := certificateState(updated, secretByName[updated.Name], evaluationTime)
		updated.Status.Expiration = expiration
		setStatusCondition(&updated.Status.Status, skupperv2alpha1.CONDITION_TYPE_READY, state, updated.Generation, now)
		aggregateStatus(&updated.Status.Status, updated.Generation, now, skupperv2alpha1.CONDITION_TYPE_READY)
		if !reflect.DeepEqual(*before, updated.Status) {
			desired.Statuses.Certificates = append(desired.Statuses.Certificates, updated)
		}
	}
	routerEndpoints := map[types.UID][]skupperv2alpha1.Endpoint{}
	for _, current := range sortedRouterAccess(snapshot.RouterAccesses) {
		updated := current.DeepCopy()
		before := updated.Status.DeepCopy()
		ids := make([]routercontrol.ResourceID, 0, len(updated.Spec.Roles))
		for _, role := range updated.Spec.Roles {
			ids = append(ids, resourceID(updated.UID, "access/"+role.Name))
		}
		setStatusCondition(&updated.Status.Status, skupperv2alpha1.CONDITION_TYPE_CONFIGURED, combineStates(configuredState(desired.Diagnostics, updated.UID), resourcesApplied(evidence, ids)), updated.Generation, now)
		var endpoints []skupperv2alpha1.Endpoint
		for _, access := range secured {
			if access.Annotations["internal.skupper.io/routeraccess"] != updated.Name || !ownedBy(access.OwnerReferences, updated.UID) {
				continue
			}
			group := access.Spec.Selector["skupper.io/group"]
			for _, endpoint := range securedEndpoints[access.Name] {
				endpoint.Group = group
				endpoints = append(endpoints, endpoint)
			}
		}
		sort.Slice(endpoints, func(i, j int) bool {
			if endpoints[i].Group != endpoints[j].Group {
				return endpoints[i].Group < endpoints[j].Group
			}
			if endpoints[i].Name != endpoints[j].Name {
				return endpoints[i].Name < endpoints[j].Name
			}
			if endpoints[i].Host != endpoints[j].Host {
				return endpoints[i].Host < endpoints[j].Host
			}
			return endpoints[i].Port < endpoints[j].Port
		})
		updated.Status.Endpoints = endpoints
		routerEndpoints[updated.UID] = append([]skupperv2alpha1.Endpoint(nil), endpoints...)
		resolved := pendingState("No external endpoint has been resolved")
		if len(endpoints) > 0 {
			resolved = skupperv2alpha1.ReadyCondition()
		}
		setStatusCondition(&updated.Status.Status, skupperv2alpha1.CONDITION_TYPE_RESOLVED, resolved, updated.Generation, now)
		aggregateStatus(&updated.Status.Status, updated.Generation, now, skupperv2alpha1.CONDITION_TYPE_CONFIGURED, skupperv2alpha1.CONDITION_TYPE_RESOLVED)
		if !reflect.DeepEqual(*before, updated.Status) {
			desired.Statuses.Accesses = append(desired.Statuses.Accesses, updated)
		}
	}

	site := desired.Site.DeepCopy()
	beforeSite := site.Status.DeepCopy()
	site.ClearLegacyNetworkStatus()
	site.Status.Endpoints = nil
	for _, access := range snapshot.RouterAccesses {
		if access.Name == "skupper-router" && access.Annotations[controlledAnnotation] == "true" && ownedBy(access.OwnerReferences, site.UID) {
			site.Status.Endpoints = append([]skupperv2alpha1.Endpoint(nil), routerEndpoints[access.UID]...)
			break
		}
	}
	setStatusCondition(&site.Status.Status, skupperv2alpha1.CONDITION_TYPE_CONFIGURED, configuredState(desired.Diagnostics, site.UID), site.Generation, now)
	setStatusCondition(&site.Status.Status, skupperv2alpha1.CONDITION_TYPE_RUNNING, allTargetsApplied(evidence), site.Generation, now)
	required := []string{skupperv2alpha1.CONDITION_TYPE_CONFIGURED, skupperv2alpha1.CONDITION_TYPE_RUNNING}
	if site.Spec.LinkAccess != "" && site.Spec.LinkAccess != "none" {
		required = append(required, skupperv2alpha1.CONDITION_TYPE_RESOLVED)
		resolvedGroups := map[string]bool{}
		for _, access := range secured {
			if access.Annotations["internal.skupper.io/routeraccess"] == "skupper-router" && len(securedEndpoints[access.Name]) > 0 {
				resolvedGroups[access.Spec.Selector["skupper.io/group"]] = true
			}
		}
		resolved := len(resolvedGroups) > 0
		if site.Spec.HA {
			resolved = resolvedGroups["skupper-router"] && resolvedGroups["skupper-router-2"]
		}
		state := pendingState("Router access endpoint has not been resolved")
		if resolved {
			state = skupperv2alpha1.ReadyCondition()
		}
		setStatusCondition(&site.Status.Status, skupperv2alpha1.CONDITION_TYPE_RESOLVED, state, site.Generation, now)
	}
	aggregateStatus(&site.Status.Status, site.Generation, now, required...)
	if !reflect.DeepEqual(*beforeSite, site.Status) {
		desired.Statuses.Sites = append(desired.Statuses.Sites, site)
	}
}

func ingressEndpoints(ingresses []*networkingv1.Ingress, access *skupperv2alpha1.SecuredAccess) []skupperv2alpha1.Endpoint {
	for _, ingress := range ingresses {
		if ingress.Name != access.Name || !ownedBy(ingress.OwnerReferences, access.UID) {
			continue
		}
		result := make([]skupperv2alpha1.Endpoint, 0, len(ingress.Spec.Rules))
		for _, rule := range ingress.Spec.Rules {
			if rule.Host == "" {
				continue
			}
			name := strings.SplitN(rule.Host, ".", 2)[0]
			result = append(result, skupperv2alpha1.Endpoint{Name: name, Host: rule.Host, Port: "443"})
		}
		return result
	}
	return nil
}

func certificateState(certificate *skupperv2alpha1.Certificate, secret *corev1.Secret, evaluationTime time.Time) (skupperv2alpha1.ConditionState, string) {
	state := pendingState("Certificate Secret has not been realized")
	if secret == nil {
		return state, ""
	}
	expiration := ""
	if decoded, err := certs.DecodeCertificate(secret.Data[corev1.TLSCertKey]); err == nil {
		expiration = decoded.NotAfter.UTC().Format(time.RFC3339)
	}
	if certificates.SecretCorrectAt(certificate, secret, evaluationTime) {
		state = skupperv2alpha1.ReadyCondition()
	}
	return state, expiration
}

func currentTargetObservations(snapshot Snapshot, target RouterTarget) ([]Observation, int) {
	pods := map[string]bool{}
	for _, pod := range snapshot.Pods {
		if pod.Namespace == snapshot.Namespace.Name && pod.DeletionTimestamp == nil && pod.Labels["skupper.io/component"] == "router" && pod.Labels["skupper.io/group"] == target.RouterGroup {
			pods[string(pod.UID)] = true
		}
	}
	byPod := map[string]Observation{}
	duplicate := map[string]bool{}
	for _, observation := range snapshot.Observations[target] {
		podUID := observation.Key.Identity.PodUID
		if observation.Key.Target != target || !pods[podUID] || observation.SessionID == "" {
			continue
		}
		if _, found := byPod[podUID]; found {
			duplicate[podUID] = true
		}
		byPod[podUID] = observation
	}
	result := []Observation{}
	for podUID, observation := range byPod {
		if !duplicate[podUID] {
			result = append(result, observation)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Key.Identity.PodUID != result[j].Key.Identity.PodUID {
			return result[i].Key.Identity.PodUID < result[j].Key.Identity.PodUID
		}
		return result[i].SessionID < result[j].SessionID
	})
	return result, len(pods)
}

func allTargetsApplied(evidence map[RouterTarget]targetEvidence) skupperv2alpha1.ConditionState {
	return evaluateTargets(evidence, func(observation Observation, digest routercontrol.Digest) skupperv2alpha1.ConditionState {
		if state := applicationState(observation, digest); state.Status != metav1.ConditionTrue {
			return state
		}
		_, state := freshScope(observation, routercontrol.ObservationScopeResources)
		return state
	})
}

func applicationState(observation Observation, digest routercontrol.Digest) skupperv2alpha1.ConditionState {
	application := observation.Application
	if application == nil || application.SessionID != observation.SessionID || observation.AcceptedDigest != digest || application.IntentDigest != digest || application.RealizationID == "" {
		return unknownState("Desired intent has no fresh applied report for this session")
	}
	switch application.State {
	case routercontrol.ApplicationApplied:
		return skupperv2alpha1.ReadyCondition()
	case routercontrol.ApplicationFailed:
		return skupperv2alpha1.ErrorCondition(fmt.Errorf("router application failed"))
	default:
		return pendingState("Router application pending")
	}
}

func resourceApplied(evidence map[RouterTarget]targetEvidence, id routercontrol.ResourceID) skupperv2alpha1.ConditionState {
	return resourcesApplied(evidence, []routercontrol.ResourceID{id})
}

func resourcesApplied(evidence map[RouterTarget]targetEvidence, ids []routercontrol.ResourceID) skupperv2alpha1.ConditionState {
	if len(ids) == 0 {
		return unknownState("No desired router resources")
	}
	return evaluateTargets(evidence, func(observation Observation, digest routercontrol.Digest) skupperv2alpha1.ConditionState {
		return observedResourcesState(observation, digest, ids, false)
	})
}

func resourcesOperational(evidence map[RouterTarget]targetEvidence, ids []routercontrol.ResourceID) skupperv2alpha1.ConditionState {
	if len(ids) == 0 {
		return unknownState("No desired router resources")
	}
	return evaluateTargets(evidence, func(observation Observation, digest routercontrol.Digest) skupperv2alpha1.ConditionState {
		return observedResourcesState(observation, digest, ids, true)
	})
}

func resourceOperational(evidence map[RouterTarget]targetEvidence, id routercontrol.ResourceID) skupperv2alpha1.ConditionState {
	return resourcesOperational(evidence, []routercontrol.ResourceID{id})
}

func evaluateTargets(evidence map[RouterTarget]targetEvidence, evaluate func(Observation, routercontrol.Digest) skupperv2alpha1.ConditionState) skupperv2alpha1.ConditionState {
	if len(evidence) == 0 {
		return unknownState("No desired router targets")
	}
	for _, key := range sortedEvidenceTargets(evidence) {
		target := evidence[key]
		if target.currentPods == 0 || len(target.observations) == 0 {
			return unknownState("No authenticated observation from a current router Pod")
		}
		state := unknownState("No verified serving realization")
		for _, observation := range target.observations {
			candidate := evaluate(observation, target.digest)
			if candidate.Status == metav1.ConditionTrue {
				state = candidate
				break
			}
			state = preferState(state, candidate)
		}
		if state.Status != metav1.ConditionTrue {
			return state
		}
	}
	return skupperv2alpha1.ReadyCondition()
}

func observedResourcesState(observation Observation, digest routercontrol.Digest, ids []routercontrol.ResourceID, operational bool) skupperv2alpha1.ConditionState {
	if state := applicationState(observation, digest); state.Status != metav1.ConditionTrue {
		return state
	}
	snapshot, state := freshScope(observation, routercontrol.ObservationScopeResources)
	if state.Status != metav1.ConditionTrue {
		return state
	}
	for _, id := range ids {
		var applied *routercontrol.ResourceApplication
		for index := range observation.Application.Resources {
			if observation.Application.Resources[index].ResourceID == id {
				applied = &observation.Application.Resources[index]
				break
			}
		}
		if applied == nil {
			return unknownState("Applied report does not contain the desired resource")
		}
		if applied.State == routercontrol.ApplicationFailed {
			return skupperv2alpha1.ErrorCondition(fmt.Errorf("router resource %s failed: %s", id, applied.Reason))
		}
		if applied.State != routercontrol.ApplicationApplied {
			return pendingState("Router resource application pending")
		}
		if applied.RealizationID == "" {
			return unknownState("Applied resource has no realization identity")
		}
		var observed *routercontrol.LocalResourceObservation
		for index := range snapshot.Resources {
			if snapshot.Resources[index].ResourceID == id {
				observed = &snapshot.Resources[index]
				break
			}
		}
		if observed == nil || observed.RealizationID == "" || observed.RealizationID != applied.RealizationID {
			return unknownState("Local observation does not match the applied resource realization")
		}
		if !operational {
			continue
		}
		switch observed.Operational {
		case routercontrol.OperationalUp:
		case routercontrol.OperationalDown:
			return pendingState("Router resource is not operational")
		default:
			return unknownState("Router resource operational state is unknown")
		}
	}
	return skupperv2alpha1.ReadyCondition()
}

func freshScope(observation Observation, name string) (routercontrol.ObservationSnapshot, skupperv2alpha1.ConditionState) {
	scope, ok := observation.Scopes[name]
	if !ok || !scope.Fresh || scope.Snapshot.SessionID != observation.SessionID || scope.Snapshot.Knowledge == routercontrol.KnowledgeUnknown {
		return routercontrol.ObservationSnapshot{}, unknownState("Local router observation is missing, stale, or unknown")
	}
	return scope.Snapshot, skupperv2alpha1.ReadyCondition()
}

func routingKeysReachable(evidence map[RouterTarget]targetEvidence, keys []string) (skupperv2alpha1.ConditionState, []string) {
	if len(evidence) == 0 || len(keys) == 0 {
		return unknownState("Local address observation is missing, stale, or unknown"), nil
	}
	var reachableAcrossTargets map[string]bool
	for _, targetKey := range sortedEvidenceTargets(evidence) {
		target := evidence[targetKey]
		if target.currentPods == 0 || len(target.observations) == 0 {
			return unknownState("Local address observation is missing, stale, or unknown"), nil
		}
		targetReachable := map[string]bool{}
		known := false
		for _, observation := range target.observations {
			if applicationState(observation, target.digest).Status != metav1.ConditionTrue {
				continue
			}
			if _, state := freshScope(observation, routercontrol.ObservationScopeResources); state.Status != metav1.ConditionTrue {
				continue
			}
			snapshot, state := freshScope(observation, routercontrol.ObservationScopeAddresses)
			if state.Status != metav1.ConditionTrue {
				continue
			}
			seen := map[string]bool{}
			for _, address := range snapshot.Addresses {
				for _, key := range keys {
					if address.RoutingKey == key {
						seen[key] = true
						if address.Reachable {
							targetReachable[key] = true
						}
					}
				}
			}
			if len(targetReachable) > 0 || snapshot.Knowledge == routercontrol.KnowledgeComplete || containsAllKeys(seen, keys) {
				known = true
			}
		}
		if !known {
			return unknownState("Local address observation is missing, stale, partial, or unknown"), nil
		}
		if len(targetReachable) == 0 {
			return pendingState("No matching local routing address"), nil
		}
		if reachableAcrossTargets == nil {
			reachableAcrossTargets = targetReachable
		} else {
			for key := range reachableAcrossTargets {
				if !targetReachable[key] {
					delete(reachableAcrossTargets, key)
				}
			}
		}
	}
	if len(reachableAcrossTargets) == 0 {
		return pendingState("No routing key is reachable on every router group"), nil
	}
	reachable := make([]string, 0, len(reachableAcrossTargets))
	for _, key := range keys {
		if reachableAcrossTargets[key] {
			reachable = append(reachable, key)
		}
	}
	return skupperv2alpha1.ReadyCondition(), reachable
}

func selectedPodsForConnector(snapshot Snapshot, connector *skupperv2alpha1.Connector) []skupperv2alpha1.PodDetails {
	endpoints, err := connectorEndpoints(connector.Namespace, connector.Spec.Host, connector.Spec.Selector, connector.Spec.Port, connector.Spec.IncludeNotReadyPods, snapshot.Pods)
	if err != nil {
		return nil
	}
	result := make([]skupperv2alpha1.PodDetails, 0, len(endpoints))
	byUID := map[string]*corev1.Pod{}
	for _, pod := range snapshot.Pods {
		byUID[string(pod.UID)] = pod
	}
	for _, endpoint := range endpoints {
		if pod := byUID[endpoint.ID]; pod != nil {
			result = append(result, skupperv2alpha1.PodDetails{UID: string(pod.UID), Name: pod.Name, IP: pod.Status.PodIP})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UID < result[j].UID })
	return result
}

func selectedPodsForAttached(snapshot Snapshot, attached *skupperv2alpha1.AttachedConnector) []skupperv2alpha1.PodDetails {
	endpoints, err := connectorEndpoints(attached.Namespace, "", attached.Spec.Selector, attached.Spec.Port, attached.Spec.IncludeNotReadyPods, snapshot.Pods)
	if err != nil {
		return nil
	}
	result := make([]skupperv2alpha1.PodDetails, 0, len(endpoints))
	byUID := map[string]*corev1.Pod{}
	for _, pod := range snapshot.Pods {
		byUID[string(pod.UID)] = pod
	}
	for _, endpoint := range endpoints {
		if pod := byUID[endpoint.ID]; pod != nil {
			result = append(result, skupperv2alpha1.PodDetails{UID: string(pod.UID), Name: pod.Name, IP: pod.Status.PodIP})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UID < result[j].UID })
	return result
}

func sortedEvidenceTargets(evidence map[RouterTarget]targetEvidence) []RouterTarget {
	result := make([]RouterTarget, 0, len(evidence))
	for target := range evidence {
		result = append(result, target)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].NamespaceUID != result[j].NamespaceUID {
			return result[i].NamespaceUID < result[j].NamespaceUID
		}
		if result[i].SiteUID != result[j].SiteUID {
			return result[i].SiteUID < result[j].SiteUID
		}
		return result[i].RouterGroup < result[j].RouterGroup
	})
	return result
}

func containsAllKeys(seen map[string]bool, keys []string) bool {
	for _, key := range keys {
		if !seen[key] {
			return false
		}
	}
	return true
}

func configuredState(diagnostics []Diagnostic, resource types.UID) skupperv2alpha1.ConditionState {
	for _, diagnostic := range diagnostics {
		if diagnostic.Resource == resource || (resource == "" && diagnostic.Resource == "") {
			return skupperv2alpha1.ErrorCondition(fmt.Errorf("%s", diagnostic.Message))
		}
	}
	return skupperv2alpha1.ReadyCondition()
}

func combineStates(first, second skupperv2alpha1.ConditionState) skupperv2alpha1.ConditionState {
	if first.Status != metav1.ConditionTrue {
		return first
	}
	return second
}

func preferState(current, candidate skupperv2alpha1.ConditionState) skupperv2alpha1.ConditionState {
	priority := func(state skupperv2alpha1.ConditionState) int {
		if state.Status == metav1.ConditionTrue {
			return 4
		}
		if state.Reason == skupperv2alpha1.StatusError {
			return 3
		}
		if state.Status == metav1.ConditionFalse {
			return 2
		}
		return 1
	}
	if priority(candidate) > priority(current) {
		return candidate
	}
	return current
}

func unknownState(message string) skupperv2alpha1.ConditionState {
	return skupperv2alpha1.ConditionState{Status: metav1.ConditionUnknown, Reason: skupperv2alpha1.StatusPending, Message: message}
}

func pendingState(message string) skupperv2alpha1.ConditionState {
	return skupperv2alpha1.ConditionState{Status: metav1.ConditionFalse, Reason: skupperv2alpha1.StatusPending, Message: message}
}

func setStatusCondition(status *skupperv2alpha1.Status, conditionType string, state skupperv2alpha1.ConditionState, generation int64, now metav1.Time) {
	condition := meta.FindStatusCondition(status.Conditions, conditionType)
	transition := now
	if condition != nil && condition.Status == state.Status {
		transition = condition.LastTransitionTime
	}
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: conditionType, Status: state.Status, Reason: string(state.Reason), Message: state.Message, ObservedGeneration: generation, LastTransitionTime: transition})
}

func removeCondition(status *skupperv2alpha1.Status, conditionType string) {
	meta.RemoveStatusCondition(&status.Conditions, conditionType)
}

func aggregateStatus(status *skupperv2alpha1.Status, generation int64, now metav1.Time, required ...string) {
	state := skupperv2alpha1.ReadyCondition()
	for _, name := range required {
		condition := meta.FindStatusCondition(status.Conditions, name)
		if condition == nil {
			state = pendingState("Not " + name)
			break
		}
		if condition.Status != metav1.ConditionTrue {
			state = skupperv2alpha1.ConditionState{Status: condition.Status, Reason: skupperv2alpha1.StatusType(condition.Reason), Message: condition.Message}
			break
		}
	}
	status.StatusType, status.Message = state.Reason, state.Message
	setStatusCondition(status, skupperv2alpha1.CONDITION_TYPE_READY, state, generation, now)
}

func setMultiKeyCondition(status *skupperv2alpha1.MultiKeyListenerStatus, conditionType string, state skupperv2alpha1.ConditionState, generation int64, now metav1.Time) {
	common := skupperv2alpha1.Status{Conditions: status.Conditions, StatusType: status.StatusType, Message: status.Message}
	setStatusCondition(&common, conditionType, state, generation, now)
	status.Conditions, status.StatusType, status.Message = common.Conditions, common.StatusType, common.Message
}

func aggregateMultiKey(status *skupperv2alpha1.MultiKeyListenerStatus, generation int64, now metav1.Time) {
	common := skupperv2alpha1.Status{Conditions: status.Conditions, StatusType: status.StatusType, Message: status.Message}
	aggregateStatus(&common, generation, now, skupperv2alpha1.CONDITION_TYPE_CONFIGURED, skupperv2alpha1.CONDITION_TYPE_OPERATIONAL)
	status.Conditions, status.StatusType, status.Message = common.Conditions, common.StatusType, common.Message
}
