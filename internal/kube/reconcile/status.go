package reconcile

import (
	"fmt"
	"reflect"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/skupperproject/skupper/internal/routercontrol"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

type targetEvidence struct {
	observations []Observation
	digest       routercontrol.Digest
	expectedPods int
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
		observations, expectedPods := currentTargetObservations(snapshot, target)
		evidence[target] = targetEvidence{observations: observations, digest: digest, expectedPods: expectedPods}
	}
	evaluationTime := snapshot.EvaluationTime
	if evaluationTime.IsZero() {
		evaluationTime = time.Unix(1, 0).UTC()
	}
	now := metav1.NewTime(evaluationTime)

	site := desired.Site.DeepCopy()
	beforeSite := site.Status.DeepCopy()
	site.ClearLegacyNetworkStatus()
	configured := skupperv2alpha1.ReadyCondition()
	if len(desired.Diagnostics) > 0 {
		configured = skupperv2alpha1.ErrorCondition(fmt.Errorf("%s", desired.Diagnostics[0].Message))
	}
	setStatusCondition(&site.Status.Status, skupperv2alpha1.CONDITION_TYPE_CONFIGURED, configured, site.Generation, now)
	setStatusCondition(&site.Status.Status, skupperv2alpha1.CONDITION_TYPE_RUNNING, allTargetsApplied(evidence), site.Generation, now)
	required := []string{skupperv2alpha1.CONDITION_TYPE_CONFIGURED, skupperv2alpha1.CONDITION_TYPE_RUNNING}
	if site.Spec.LinkAccess != "" && site.Spec.LinkAccess != "none" {
		required = append(required, skupperv2alpha1.CONDITION_TYPE_RESOLVED)
	}
	aggregateStatus(&site.Status.Status, site.Generation, now, required...)
	if !reflect.DeepEqual(*beforeSite, site.Status) {
		desired.Statuses.Sites = append(desired.Statuses.Sites, site)
	}

	for _, current := range sortedListeners(snapshot.Listeners) {
		updated := current.DeepCopy()
		before := updated.Status.DeepCopy()
		id := resourceID(updated.UID, "listener")
		setStatusCondition(&updated.Status.Status, skupperv2alpha1.CONDITION_TYPE_CONFIGURED, resourceApplied(evidence, id), updated.Generation, now)
		matched := routingKeysReachable(evidence, []string{updated.Spec.RoutingKey})
		updated.Status.HasMatchingConnector = matched.Status == metav1.ConditionTrue
		setStatusCondition(&updated.Status.Status, skupperv2alpha1.CONDITION_TYPE_MATCHED, matched, updated.Generation, now)
		aggregateStatus(&updated.Status.Status, updated.Generation, now, skupperv2alpha1.CONDITION_TYPE_CONFIGURED, skupperv2alpha1.CONDITION_TYPE_MATCHED)
		if !reflect.DeepEqual(*before, updated.Status) {
			desired.Statuses.Listeners = append(desired.Statuses.Listeners, updated)
		}
	}
	for _, current := range sortedMultiKeyListeners(snapshot.MultiKeyListeners) {
		updated := current.DeepCopy()
		before := updated.Status.DeepCopy()
		configured := resourceApplied(evidence, resourceID(updated.UID, "listener"))
		matched := routingKeysReachable(evidence, routingKeys(updated))
		setMultiKeyCondition(&updated.Status, skupperv2alpha1.CONDITION_TYPE_CONFIGURED, configured, updated.Generation, now)
		setMultiKeyCondition(&updated.Status, skupperv2alpha1.CONDITION_TYPE_OPERATIONAL, matched, updated.Generation, now)
		updated.Status.HasDestination = matched.Status == metav1.ConditionTrue
		aggregateMultiKey(&updated.Status, updated.Generation, now)
		if !reflect.DeepEqual(*before, updated.Status) {
			desired.Statuses.MultiKey = append(desired.Statuses.MultiKey, updated)
		}
	}
	for _, current := range sortedConnectors(snapshot.Connectors) {
		updated := current.DeepCopy()
		before := updated.Status.DeepCopy()
		state := resourceApplied(evidence, resourceID(updated.UID, "connector"))
		selected := selectedPodsForConnector(snapshot, updated)
		if len(selected) == 0 {
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
		state := resourceApplied(evidence, resourceID(updated.UID, "attached-connector"))
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
			state = resourceApplied(evidence, resourceID(binding.UID, "attached-connector"))
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
		setStatusCondition(&updated.Status.Status, skupperv2alpha1.CONDITION_TYPE_CONFIGURED, resourcesApplied(evidence, ids), updated.Generation, now)
		setStatusCondition(&updated.Status.Status, skupperv2alpha1.CONDITION_TYPE_OPERATIONAL, resourcesOperational(evidence, ids), updated.Generation, now)
		aggregateStatus(&updated.Status.Status, updated.Generation, now, skupperv2alpha1.CONDITION_TYPE_CONFIGURED, skupperv2alpha1.CONDITION_TYPE_OPERATIONAL)
		if !reflect.DeepEqual(*before, updated.Status) {
			desired.Statuses.Links = append(desired.Statuses.Links, updated)
		}
	}
	for _, current := range sortedRouterAccess(snapshot.RouterAccesses) {
		updated := current.DeepCopy()
		before := updated.Status.DeepCopy()
		ids := make([]routercontrol.ResourceID, 0, len(updated.Spec.Roles))
		for _, role := range updated.Spec.Roles {
			ids = append(ids, resourceID(updated.UID, "access/"+role.Name))
		}
		setStatusCondition(&updated.Status.Status, skupperv2alpha1.CONDITION_TYPE_CONFIGURED, resourcesApplied(evidence, ids), updated.Generation, now)
		aggregateStatus(&updated.Status.Status, updated.Generation, now, skupperv2alpha1.CONDITION_TYPE_CONFIGURED, skupperv2alpha1.CONDITION_TYPE_RESOLVED)
		if !reflect.DeepEqual(*before, updated.Status) {
			desired.Statuses.Accesses = append(desired.Statuses.Accesses, updated)
		}
	}
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
	return result, len(pods)
}

func allTargetsApplied(evidence map[RouterTarget]targetEvidence) skupperv2alpha1.ConditionState {
	if len(evidence) == 0 {
		return unknownState("No desired router targets")
	}
	for _, target := range evidence {
		if target.expectedPods == 0 || len(target.observations) != target.expectedPods {
			return unknownState("No authenticated observation from a current router Pod")
		}
		for _, observation := range target.observations {
			if state := applicationState(observation, target.digest); state.Status != metav1.ConditionTrue {
				return state
			}
			scope, ok := observation.Scopes[routercontrol.ObservationScopeResources]
			if !ok || !scope.Fresh || scope.Snapshot.SessionID != observation.SessionID || scope.Snapshot.Knowledge == routercontrol.KnowledgeUnknown {
				return unknownState("Local router observation is missing, stale, or unknown")
			}
		}
	}
	return skupperv2alpha1.ReadyCondition()
}

func applicationState(observation Observation, digest routercontrol.Digest) skupperv2alpha1.ConditionState {
	application := observation.Application
	if application == nil || application.SessionID != observation.SessionID || observation.AcceptedDigest != digest || application.IntentDigest != digest {
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
		return skupperv2alpha1.ReadyCondition()
	}
	for _, target := range evidence {
		if target.expectedPods == 0 || len(target.observations) != target.expectedPods {
			return unknownState("No authenticated observation from a current router Pod")
		}
		for _, observation := range target.observations {
			if state := applicationState(observation, target.digest); state.Status != metav1.ConditionTrue {
				return state
			}
			for _, id := range ids {
				found := false
				for _, resource := range observation.Application.Resources {
					if resource.ResourceID != id {
						continue
					}
					found = true
					if resource.State == routercontrol.ApplicationFailed {
						return skupperv2alpha1.ErrorCondition(fmt.Errorf("router resource %s failed: %s", id, resource.Reason))
					}
					if resource.State != routercontrol.ApplicationApplied {
						return pendingState("Router resource application pending")
					}
				}
				if !found {
					return unknownState("Applied report does not contain the desired resource")
				}
			}
		}
	}
	return skupperv2alpha1.ReadyCondition()
}

func resourcesOperational(evidence map[RouterTarget]targetEvidence, ids []routercontrol.ResourceID) skupperv2alpha1.ConditionState {
	if len(ids) == 0 {
		return skupperv2alpha1.ReadyCondition()
	}
	for _, target := range evidence {
		if target.expectedPods == 0 || len(target.observations) != target.expectedPods {
			return unknownState("No authenticated local router observation")
		}
		for _, observation := range target.observations {
			scope, ok := observation.Scopes[routercontrol.ObservationScopeResources]
			if !ok || !scope.Fresh || scope.Snapshot.SessionID != observation.SessionID || scope.Snapshot.Knowledge == routercontrol.KnowledgeUnknown {
				return unknownState("Local resource observation is missing, stale, or unknown")
			}
			for _, id := range ids {
				found := false
				for _, resource := range scope.Snapshot.Resources {
					if resource.ResourceID == id {
						found = true
						if resource.Operational == routercontrol.OperationalDown {
							return pendingState("Router resource is not operational")
						}
						if resource.Operational != routercontrol.OperationalUp {
							return unknownState("Router resource operational state is unknown")
						}
					}
				}
				if !found {
					return unknownState("Local observation does not contain the desired resource")
				}
			}
		}
	}
	return skupperv2alpha1.ReadyCondition()
}

func routingKeysReachable(evidence map[RouterTarget]targetEvidence, keys []string) skupperv2alpha1.ConditionState {
	known := false
	for _, target := range evidence {
		if target.expectedPods == 0 || len(target.observations) != target.expectedPods {
			return unknownState("Local address observation is missing, stale, or unknown")
		}
		for _, observation := range target.observations {
			scope, ok := observation.Scopes[routercontrol.ObservationScopeAddresses]
			if !ok || !scope.Fresh || scope.Snapshot.SessionID != observation.SessionID || scope.Snapshot.Knowledge == routercontrol.KnowledgeUnknown {
				return unknownState("Local address observation is missing, stale, or unknown")
			}
			known = true
			for _, address := range scope.Snapshot.Addresses {
				for _, key := range keys {
					if address.RoutingKey == key && address.Reachable {
						return skupperv2alpha1.ReadyCondition()
					}
				}
			}
		}
	}
	if !known {
		return unknownState("Local address observation is missing, stale, or unknown")
	}
	return pendingState("No matching local routing address")
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
