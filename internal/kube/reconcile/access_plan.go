package reconcile

import (
	"context"
	"reflect"
	"time"

	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/skupperproject/skupper/internal/kube/certificates"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

// AccessChanges contains only cache-proven semantic deltas. Retirement values
// are the exact observed identities to delete; Desired* values are the full
// desired objects selected for creation or repair.
type AccessChanges struct {
	Generated          *skupperv2alpha1.RouterAccess
	RetireGenerated    *skupperv2alpha1.RouterAccess
	Secured            []*skupperv2alpha1.SecuredAccess
	RetireSecured      []*skupperv2alpha1.SecuredAccess
	Certificates       []*skupperv2alpha1.Certificate
	CertificateSecrets []*skupperv2alpha1.Certificate
	Services           []*corev1.Service
	RetireServices     []*corev1.Service
	Routes             []*routev1.Route
	RetireRoutes       []*routev1.Route
	Ingresses          []*networkingv1.Ingress
	RetireIngresses    []*networkingv1.Ingress
	HTTPProxies        []*unstructured.Unstructured
	RetireHTTPProxies  []*unstructured.Unstructured
	TLSRoutes          []*unstructured.Unstructured
	RetireTLSRoutes    []*unstructured.Unstructured
	Gateway            *unstructured.Unstructured
	RouterAccesses     []*skupperv2alpha1.RouterAccess
	SecuredAccesses    []*skupperv2alpha1.SecuredAccess
	EvaluationTime     time.Time
}

func (c AccessChanges) Empty() bool {
	return c.Generated == nil && c.RetireGenerated == nil && len(c.Secured) == 0 && len(c.RetireSecured) == 0 &&
		len(c.Certificates) == 0 && len(c.CertificateSecrets) == 0 && len(c.Services) == 0 && len(c.RetireServices) == 0 &&
		len(c.Routes) == 0 && len(c.RetireRoutes) == 0 && len(c.Ingresses) == 0 && len(c.RetireIngresses) == 0 &&
		len(c.HTTPProxies) == 0 && len(c.RetireHTTPProxies) == 0 && len(c.TLSRoutes) == 0 && len(c.RetireTLSRoutes) == 0 && c.Gateway == nil
}

type AccessEnsurer interface {
	EnsureAccessComposition(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, AccessChanges) error
}

type AccessPlanner struct {
	Next    Planner
	Ensurer AccessEnsurer
}

func (p AccessPlanner) Plan(snapshot Snapshot, desired DesiredNamespace) Plan {
	plan := p.Next.Plan(snapshot, desired)
	if !snapshot.Assignment.Controlled {
		return plan
	}
	changes, deadline := planAccessChanges(snapshot, desired)
	if !deadline.IsZero() {
		after := deadline.Sub(snapshot.EvaluationTime)
		if after > 0 && (plan.NextReevaluation == 0 || after < plan.NextReevaluation) {
			plan.NextReevaluation = after
		}
	}
	if changes.Empty() {
		return plan
	}
	namespace := desired.Namespace
	var site *skupperv2alpha1.Site
	if desired.Site != nil {
		site = desired.Site.DeepCopy()
	}
	const operationID OperationID = "ensure-access-composition"
	plan.Operations = append(plan.Operations, Operation{ID: operationID, Kind: "EnsureAccessComposition", Run: func(ctx context.Context) error {
		return p.Ensurer.EnsureAccessComposition(ctx, namespace, site, changes)
	}})
	return plan
}

func planAccessChanges(snapshot Snapshot, desired DesiredNamespace) (AccessChanges, time.Time) {
	changes := AccessChanges{
		RouterAccesses: copyRouterAccesses(snapshot.RouterAccesses), SecuredAccesses: copySecuredAccesses(snapshot.SecuredAccesses),
		EvaluationTime: snapshot.EvaluationTime,
	}
	generated := findRouterAccess(snapshot.RouterAccesses, "skupper-router")
	if desired.GeneratedAccess != nil {
		if generated == nil || !routerAccessCorrect(generated, desired.GeneratedAccess) {
			changes.Generated = desired.GeneratedAccess.DeepCopy()
		}
	} else if generated != nil && controlled(generated.Annotations) && desired.Site != nil && hasOwnerIdentity(generated.OwnerReferences, "Site", desired.Site.UID) {
		changes.RetireGenerated = generated.DeepCopy()
	}

	changes.Secured, changes.RetireSecured = diffSecured(desired.SecuredAccesses, snapshot.SecuredAccesses, snapshot.RouterAccesses)
	changes.Services, changes.RetireServices = diffServices(desired.AccessServices, snapshot.Services, snapshot.SecuredAccesses)
	changes.Routes, changes.RetireRoutes = diffRoutes(desired.AccessRoutes, snapshot.Routes, snapshot.SecuredAccesses, snapshot.ObservedAccess.Routes)
	changes.Ingresses, changes.RetireIngresses = diffIngresses(desired.AccessIngresses, snapshot.Ingresses, snapshot.SecuredAccesses)
	changes.HTTPProxies, changes.RetireHTTPProxies = diffDynamic(desired.AccessHTTPProxies, snapshot.HTTPProxies, snapshot.SecuredAccesses, snapshot.ObservedAccess.HTTPProxies)
	changes.TLSRoutes, changes.RetireTLSRoutes = diffDynamic(desired.AccessTLSRoutes, snapshot.TLSRoutes, snapshot.SecuredAccesses, snapshot.ObservedAccess.TLSRoutes)
	if desired.AccessGateway != nil && (snapshot.Gateway == nil || !dynamicCorrect(snapshot.Gateway, desired.AccessGateway)) {
		changes.Gateway = desired.AccessGateway.DeepCopy()
	}

	actualCertificates := byCertificateName(snapshot.Certificates)
	actualSecrets := bySecretName(snapshot.Secrets)
	mutatingCertificates := map[string]bool{}
	for _, wanted := range desired.Certificates {
		actual := actualCertificates[wanted.Name]
		if actual == nil || !certificateCorrect(actual, wanted) {
			changes.Certificates = append(changes.Certificates, wanted.DeepCopy())
			mutatingCertificates[wanted.Name] = true
		}
	}
	var deadline time.Time
	for _, actual := range snapshot.Certificates {
		if mutatingCertificates[actual.Name] {
			continue // wait for informer identity/resourceVersion catch-up
		}
		secret := actualSecrets[actual.Name]
		if !certificateSecretCorrect(actual, secret, snapshot.EvaluationTime) {
			changes.CertificateSecrets = append(changes.CertificateSecrets, actual.DeepCopy())
			continue
		}
		if expiry, ok := certificates.SecretExpiry(secret); ok && (deadline.IsZero() || expiry.Before(deadline)) {
			deadline = expiry
		}
	}
	return changes, deadline
}

func routerAccessCorrect(actual, desired *skupperv2alpha1.RouterAccess) bool {
	return controlled(actual.Annotations) && reflect.DeepEqual(actual.Spec, desired.Spec) && reflect.DeepEqual(actual.Labels, desired.Labels) && reflect.DeepEqual(actual.OwnerReferences, desired.OwnerReferences) && reflect.DeepEqual(actual.Annotations, desired.Annotations)
}

func securedCorrect(actual, desired *skupperv2alpha1.SecuredAccess) bool {
	owner := metav1.GetControllerOf(desired)
	return owner != nil && controlled(actual.Annotations) && hasOwnerIdentity(actual.OwnerReferences, "RouterAccess", owner.UID) && reflect.DeepEqual(actual.Spec, desired.Spec) && reflect.DeepEqual(actual.Labels, desired.Labels) && reflect.DeepEqual(actual.OwnerReferences, desired.OwnerReferences) && reflect.DeepEqual(actual.Annotations, desired.Annotations)
}

func serviceCorrect(actual, desired *corev1.Service) bool {
	owner := metav1.GetControllerOf(desired)
	if owner == nil || !controlled(actual.Annotations) || !hasOwnerIdentity(actual.OwnerReferences, "SecuredAccess", owner.UID) {
		return false
	}
	wanted := desired.DeepCopy()
	preserveServiceFields(wanted, actual)
	return reflect.DeepEqual(actual.Spec, wanted.Spec) && reflect.DeepEqual(actual.Labels, wanted.Labels) && reflect.DeepEqual(actual.Annotations, wanted.Annotations) && reflect.DeepEqual(actual.OwnerReferences, wanted.OwnerReferences)
}

func preserveServiceFields(desired, actual *corev1.Service) {
	desired.Spec.ClusterIP = actual.Spec.ClusterIP
	desired.Spec.ClusterIPs = append([]string(nil), actual.Spec.ClusterIPs...)
	desired.Spec.IPFamilies = append([]corev1.IPFamily(nil), actual.Spec.IPFamilies...)
	desired.Spec.IPFamilyPolicy = actual.Spec.IPFamilyPolicy
	desired.Spec.HealthCheckNodePort = actual.Spec.HealthCheckNodePort
	desired.Spec.ExternalTrafficPolicy = actual.Spec.ExternalTrafficPolicy
	desired.Spec.AllocateLoadBalancerNodePorts = actual.Spec.AllocateLoadBalancerNodePorts
	desired.Spec.LoadBalancerClass = actual.Spec.LoadBalancerClass
	desired.Spec.SessionAffinityConfig = actual.Spec.SessionAffinityConfig
	desired.Spec.TrafficDistribution = actual.Spec.TrafficDistribution
	for index := range desired.Spec.Ports {
		for _, port := range actual.Spec.Ports {
			if port.Name == desired.Spec.Ports[index].Name && desired.Spec.Ports[index].NodePort == 0 {
				desired.Spec.Ports[index].NodePort = port.NodePort
			}
		}
	}
}

func routeCorrect(actual, desired *routev1.Route) bool {
	owner := metav1.GetControllerOf(desired)
	if owner == nil || !controlled(actual.Annotations) || !hasOwnerIdentity(actual.OwnerReferences, "SecuredAccess", owner.UID) {
		return false
	}
	wanted := desired.DeepCopy()
	if wanted.Spec.Host == "" {
		wanted.Spec.Host = actual.Spec.Host
	}
	return reflect.DeepEqual(actual.Spec, wanted.Spec) && reflect.DeepEqual(actual.Labels, wanted.Labels) && reflect.DeepEqual(actual.Annotations, wanted.Annotations) && reflect.DeepEqual(actual.OwnerReferences, wanted.OwnerReferences)
}

func ingressCorrect(actual, desired *networkingv1.Ingress) bool {
	owner := metav1.GetControllerOf(desired)
	return owner != nil && controlled(actual.Annotations) && hasOwnerIdentity(actual.OwnerReferences, "SecuredAccess", owner.UID) && reflect.DeepEqual(actual.Spec, desired.Spec) && reflect.DeepEqual(actual.Labels, desired.Labels) && reflect.DeepEqual(actual.Annotations, desired.Annotations) && reflect.DeepEqual(actual.OwnerReferences, desired.OwnerReferences)
}

func dynamicCorrect(actual, desired *unstructured.Unstructured) bool {
	owner := metav1.GetControllerOf(desired)
	actualOwner := metav1.GetControllerOf(actual)
	return owner != nil && actualOwner != nil && controlled(actual.GetAnnotations()) && actual.GroupVersionKind() == desired.GroupVersionKind() && actualOwner.UID == owner.UID &&
		reflect.DeepEqual(actual.Object["spec"], MergeDesiredJSON(actual.Object["spec"], desired.Object["spec"])) && reflect.DeepEqual(actual.GetLabels(), desired.GetLabels()) && reflect.DeepEqual(actual.GetAnnotations(), desired.GetAnnotations()) && reflect.DeepEqual(actual.GetOwnerReferences(), desired.GetOwnerReferences())
}

// MergeDesiredJSON applies controller-owned desired fields while retaining
// unknown/defaulted fields returned by extension APIs.
func MergeDesiredJSON(current, desired interface{}) interface{} {
	currentMap, currentOK := current.(map[string]interface{})
	desiredMap, desiredOK := desired.(map[string]interface{})
	if !currentOK || !desiredOK {
		return runtime.DeepCopyJSONValue(desired)
	}
	result := runtime.DeepCopyJSONValue(currentMap).(map[string]interface{})
	for key, value := range desiredMap {
		result[key] = MergeDesiredJSON(currentMap[key], value)
	}
	return result
}

func certificateCorrect(actual, desired *skupperv2alpha1.Certificate) bool {
	return (controlled(actual.Annotations) || actual.Labels["internal.skupper.io/certificate"] == "true") && ownerSetsOverlapPure(actual.OwnerReferences, desired.OwnerReferences) && reflect.DeepEqual(actual.Spec, desired.Spec) && reflect.DeepEqual(actual.Labels, desired.Labels) && reflect.DeepEqual(actual.OwnerReferences, desired.OwnerReferences) && reflect.DeepEqual(actual.Annotations, desired.Annotations)
}

func certificateSecretCorrect(certificate *skupperv2alpha1.Certificate, secret *corev1.Secret, now time.Time) bool {
	return secret != nil && certificates.SecretControlled(secret) && hasOwnerIdentity(secret.OwnerReferences, "Certificate", certificate.UID) && certificates.SecretCorrectAt(certificate, secret, now)
}

func diffSecured(desired, actual []*skupperv2alpha1.SecuredAccess, parents []*skupperv2alpha1.RouterAccess) (selected, retired []*skupperv2alpha1.SecuredAccess) {
	names := map[string]bool{}
	byName := map[string]*skupperv2alpha1.SecuredAccess{}
	for _, value := range actual {
		byName[value.Name] = value
	}
	for _, value := range desired {
		names[value.Name] = true
		if byName[value.Name] == nil || !securedCorrect(byName[value.Name], value) {
			selected = append(selected, value.DeepCopy())
		}
	}
	for _, value := range actual {
		owner := metav1.GetControllerOf(value)
		if !names[value.Name] && controlled(value.Annotations) && value.Annotations["internal.skupper.io/routeraccess"] != "" && owner != nil && owner.Kind == "RouterAccess" && routerIdentityExists(parents, owner) {
			retired = append(retired, value.DeepCopy())
		}
	}
	return
}

func diffServices(desired, actual []*corev1.Service, parents []*skupperv2alpha1.SecuredAccess) (selected, retired []*corev1.Service) {
	names := map[string]bool{}
	byName := map[string]*corev1.Service{}
	for _, value := range actual {
		byName[value.Name] = value
	}
	for _, value := range desired {
		names[value.Name] = true
		if byName[value.Name] == nil || !serviceCorrect(byName[value.Name], value) {
			selected = append(selected, value.DeepCopy())
		}
	}
	for _, value := range actual {
		if !names[value.Name] && retireChild(value.Annotations, value.OwnerReferences, parents) {
			retired = append(retired, value.DeepCopy())
		}
	}
	return
}

func diffRoutes(desired, actual []*routev1.Route, parents []*skupperv2alpha1.SecuredAccess, complete bool) (selected, retired []*routev1.Route) {
	names := map[string]bool{}
	byName := map[string]*routev1.Route{}
	for _, value := range actual {
		byName[value.Name] = value
	}
	for _, value := range desired {
		names[value.Name] = true
		if byName[value.Name] == nil || !routeCorrect(byName[value.Name], value) {
			selected = append(selected, value.DeepCopy())
		}
	}
	if complete {
		for _, value := range actual {
			if !names[value.Name] && retireChild(value.Annotations, value.OwnerReferences, parents) {
				retired = append(retired, value.DeepCopy())
			}
		}
	}
	return
}

func diffIngresses(desired, actual []*networkingv1.Ingress, parents []*skupperv2alpha1.SecuredAccess) (selected, retired []*networkingv1.Ingress) {
	names := map[string]bool{}
	byName := map[string]*networkingv1.Ingress{}
	for _, value := range actual {
		byName[value.Name] = value
	}
	for _, value := range desired {
		names[value.Name] = true
		if byName[value.Name] == nil || !ingressCorrect(byName[value.Name], value) {
			selected = append(selected, value.DeepCopy())
		}
	}
	for _, value := range actual {
		if !names[value.Name] && retireChild(value.Annotations, value.OwnerReferences, parents) {
			retired = append(retired, value.DeepCopy())
		}
	}
	return
}

func diffDynamic(desired, actual []*unstructured.Unstructured, parents []*skupperv2alpha1.SecuredAccess, complete bool) (selected, retired []*unstructured.Unstructured) {
	names := map[string]bool{}
	byName := map[string]*unstructured.Unstructured{}
	for _, value := range actual {
		byName[value.GetName()] = value
	}
	for _, value := range desired {
		names[value.GetName()] = true
		if byName[value.GetName()] == nil || !dynamicCorrect(byName[value.GetName()], value) {
			selected = append(selected, value.DeepCopy())
		}
	}
	if complete {
		for _, value := range actual {
			if !names[value.GetName()] && retireChild(value.GetAnnotations(), value.GetOwnerReferences(), parents) {
				retired = append(retired, value.DeepCopy())
			}
		}
	}
	return
}

func retireChild(annotations map[string]string, owners []metav1.OwnerReference, parents []*skupperv2alpha1.SecuredAccess) bool {
	owner := metav1.GetControllerOf(&metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{OwnerReferences: owners}})
	return controlled(annotations) && owner != nil && owner.Kind == "SecuredAccess" && securedIdentityExists(parents, owner)
}

func controlled(annotations map[string]string) bool {
	return annotations["internal.skupper.io/controlled"] == "true"
}
func hasOwnerIdentity(owners []metav1.OwnerReference, kind string, uid types.UID) bool {
	for _, owner := range owners {
		if owner.Kind == kind && owner.UID == uid {
			return true
		}
	}
	return false
}
func routerIdentityExists(values []*skupperv2alpha1.RouterAccess, owner *metav1.OwnerReference) bool {
	for _, value := range values {
		if value.Name == owner.Name && value.UID == owner.UID {
			return true
		}
	}
	return false
}
func securedIdentityExists(values []*skupperv2alpha1.SecuredAccess, owner *metav1.OwnerReference) bool {
	for _, value := range values {
		if value.Name == owner.Name && value.UID == owner.UID {
			return true
		}
	}
	return false
}
func ownerSetsOverlapPure(first, second []metav1.OwnerReference) bool {
	for _, left := range first {
		for _, right := range second {
			if left.UID != "" && left.UID == right.UID && left.Kind == right.Kind && left.APIVersion == right.APIVersion {
				return true
			}
		}
	}
	return false
}
func findRouterAccess(values []*skupperv2alpha1.RouterAccess, name string) *skupperv2alpha1.RouterAccess {
	for _, value := range values {
		if value.Name == name {
			return value
		}
	}
	return nil
}
func byCertificateName(values []*skupperv2alpha1.Certificate) map[string]*skupperv2alpha1.Certificate {
	result := map[string]*skupperv2alpha1.Certificate{}
	for _, value := range values {
		result[value.Name] = value
	}
	return result
}
func bySecretName(values []*corev1.Secret) map[string]*corev1.Secret {
	result := map[string]*corev1.Secret{}
	for _, value := range values {
		result[value.Name] = value
	}
	return result
}

func copyRouterAccesses(values []*skupperv2alpha1.RouterAccess) []*skupperv2alpha1.RouterAccess {
	result := make([]*skupperv2alpha1.RouterAccess, 0, len(values))
	for _, value := range values {
		result = append(result, value.DeepCopy())
	}
	return result
}
func copySecuredAccesses(values []*skupperv2alpha1.SecuredAccess) []*skupperv2alpha1.SecuredAccess {
	result := make([]*skupperv2alpha1.SecuredAccess, 0, len(values))
	for _, value := range values {
		result = append(result, value.DeepCopy())
	}
	return result
}
