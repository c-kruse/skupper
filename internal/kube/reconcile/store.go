package reconcile

import (
	"context"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

// InputStore owns the mutable cache projection. Snapshot is its only read API;
// callers never receive informer-owned or store-owned objects.
type InputStore struct {
	mu           sync.RWMutex
	namespaces   map[string]NamespaceIdentity
	assignments  map[string]Assignment
	inputs       map[string]NamespaceInputs
	attached     map[string]*skupperv2alpha1.AttachedConnector
	bindings     map[string]*skupperv2alpha1.AttachedConnectorBinding
	connectors   map[string]*skupperv2alpha1.Connector
	pods         map[string]*corev1.Pod
	allocations  map[string]AllocationState
	observations map[string]map[RouterTarget]Observation
	now          func() time.Time
}

type NamespaceInputs struct {
	Sites             []*skupperv2alpha1.Site
	Listeners         []*skupperv2alpha1.Listener
	MultiKeyListeners []*skupperv2alpha1.MultiKeyListener
	Links             []*skupperv2alpha1.Link
	RouterAccesses    []*skupperv2alpha1.RouterAccess
	Certificates      []*skupperv2alpha1.Certificate
	SecuredAccesses   []*skupperv2alpha1.SecuredAccess
	Services          []*corev1.Service
	Secrets           []*corev1.Secret
}

func NewInputStore() *InputStore {
	return &InputStore{namespaces: map[string]NamespaceIdentity{}, assignments: map[string]Assignment{}, inputs: map[string]NamespaceInputs{}, attached: map[string]*skupperv2alpha1.AttachedConnector{}, bindings: map[string]*skupperv2alpha1.AttachedConnectorBinding{}, connectors: map[string]*skupperv2alpha1.Connector{}, pods: map[string]*corev1.Pod{}, allocations: map[string]AllocationState{}, observations: map[string]map[RouterTarget]Observation{}, now: time.Now}
}

func (s *InputStore) SetNamespace(namespace string, uid types.UID, assignment Assignment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.namespaces[namespace] = NamespaceIdentity{Name: namespace, UID: uid}
	s.assignments[namespace] = assignment
}

func (s *InputStore) SetInputs(namespace string, inputs NamespaceInputs) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inputs[namespace] = copyInputs(inputs)
}

func (s *InputStore) SetAllocations(namespace string, allocations AllocationState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allocations[namespace] = copyAllocations(allocations)
}

func (s *InputStore) SetObservations(namespace string, observations map[RouterTarget]Observation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observations[namespace] = copyObservations(observations)
}

// UpdateAttached returns the source and both old/new destination namespaces.
func (s *InputStore) UpdateAttached(key string, value *skupperv2alpha1.AttachedConnector) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	affected := map[string]bool{}
	if old := s.attached[key]; old != nil {
		affected[old.Namespace] = true
		affected[old.Spec.SiteNamespace] = true
	}
	if value == nil {
		delete(s.attached, key)
	} else {
		copy := value.DeepCopy()
		s.attached[key] = copy
		affected[copy.Namespace] = true
		affected[copy.Spec.SiteNamespace] = true
	}
	return orderedNamespaces(affected)
}

// UpdateBinding invalidates the Site namespace and both old/new source namespaces.
func (s *InputStore) UpdateBinding(key string, value *skupperv2alpha1.AttachedConnectorBinding) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	affected := map[string]bool{}
	if old := s.bindings[key]; old != nil {
		affected[old.Namespace] = true
		affected[old.Spec.ConnectorNamespace] = true
	}
	if value == nil {
		delete(s.bindings, key)
	} else {
		copy := value.DeepCopy()
		s.bindings[key] = copy
		affected[copy.Namespace] = true
		affected[copy.Spec.ConnectorNamespace] = true
	}
	return orderedNamespaces(affected)
}

func (s *InputStore) UpdateConnector(key string, value *skupperv2alpha1.Connector) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	affected := map[string]bool{}
	if old := s.connectors[key]; old != nil {
		affected[old.Namespace] = true
	}
	if value == nil {
		delete(s.connectors, key)
	} else {
		copy := value.DeepCopy()
		s.connectors[key] = copy
		affected[copy.Namespace] = true
	}
	return orderedNamespaces(affected)
}

// UpdatePod evaluates consumers against both the retained old Pod and new Pod.
// A nil value is a complete tombstone because the old object is owned here.
func (s *InputStore) UpdatePod(key string, value *corev1.Pod) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.pods[key]
	affected := map[string]bool{}
	if old != nil {
		s.addPodConsumers(affected, old)
	}
	if value == nil {
		delete(s.pods, key)
	} else {
		copy := value.DeepCopy()
		s.pods[key] = copy
		s.addPodConsumers(affected, copy)
	}
	return orderedNamespaces(affected)
}

func (s *InputStore) addPodConsumers(affected map[string]bool, pod *corev1.Pod) {
	affected[pod.Namespace] = true
	for _, connector := range s.connectors {
		if connector.Namespace == pod.Namespace && selectorMatches(connector.Spec.Selector, pod.Labels) {
			affected[connector.Namespace] = true
		}
	}
	for _, definition := range s.attached {
		if definition.Namespace != pod.Namespace || !selectorMatches(definition.Spec.Selector, pod.Labels) {
			continue
		}
		for _, binding := range s.bindings {
			if binding.Namespace == definition.Spec.SiteNamespace && binding.Name == definition.Name && binding.Spec.ConnectorNamespace == definition.Namespace {
				affected[binding.Namespace] = true
			}
		}
	}
}

func selectorMatches(selector string, podLabels map[string]string) bool {
	parsed, err := labels.Parse(selector)
	return err == nil && parsed.Matches(labels.Set(podLabels))
}

func (s *InputStore) Collect(ctx context.Context, namespace string) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	inputs := copyInputs(s.inputs[namespace])
	snapshot := Snapshot{Namespace: s.namespaces[namespace], Assignment: s.assignments[namespace], EvaluationTime: s.now(), Sites: inputs.Sites, Listeners: inputs.Listeners, MultiKeyListeners: inputs.MultiKeyListeners, Links: inputs.Links, RouterAccesses: inputs.RouterAccesses, Certificates: inputs.Certificates, SecuredAccesses: inputs.SecuredAccesses, Services: inputs.Services, Secrets: inputs.Secrets, Allocations: copyAllocations(s.allocations[namespace]), Observations: copyObservations(s.observations[namespace])}
	for _, connector := range s.connectors {
		if connector.Namespace == namespace {
			snapshot.Connectors = append(snapshot.Connectors, connector.DeepCopy())
		}
	}
	for _, binding := range s.bindings {
		if binding.Namespace == namespace {
			snapshot.Bindings = append(snapshot.Bindings, binding.DeepCopy())
		}
	}
	sources := map[string]bool{namespace: true}
	for _, definition := range s.attached {
		if definition.Namespace == namespace || definition.Spec.SiteNamespace == namespace {
			snapshot.Attached = append(snapshot.Attached, definition.DeepCopy())
		}
		if definition.Spec.SiteNamespace == namespace {
			sources[definition.Namespace] = true
		}
	}
	for _, pod := range s.pods {
		if sources[pod.Namespace] {
			snapshot.Pods = append(snapshot.Pods, pod.DeepCopy())
		}
	}
	sort.Slice(snapshot.Pods, func(i, j int) bool {
		return snapshot.Pods[i].Namespace+"/"+snapshot.Pods[i].Name < snapshot.Pods[j].Namespace+"/"+snapshot.Pods[j].Name
	})
	return snapshot, nil
}

func orderedNamespaces(values map[string]bool) []string {
	delete(values, "")
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
