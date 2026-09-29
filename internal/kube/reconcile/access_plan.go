package reconcile

import (
	"context"
	"time"

	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"

	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

type AccessEnsurer interface {
	EnsureAccessComposition(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, *skupperv2alpha1.RouterAccess, []*skupperv2alpha1.SecuredAccess, []*skupperv2alpha1.Certificate, []*corev1.Service, []*routev1.Route, []*skupperv2alpha1.RouterAccess, []*skupperv2alpha1.SecuredAccess, []*skupperv2alpha1.Certificate, []*corev1.Secret, time.Time) error
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
	namespace := desired.Namespace
	var site *skupperv2alpha1.Site
	if desired.Site != nil {
		site = desired.Site.DeepCopy()
	}
	var generated *skupperv2alpha1.RouterAccess
	if desired.GeneratedAccess != nil {
		generated = desired.GeneratedAccess.DeepCopy()
	}
	secured := copySecuredAccesses(desired.SecuredAccesses)
	certificates := copyCertificates(desired.Certificates)
	services := copyServices(desired.AccessServices)
	routes := copyRoutes(desired.AccessRoutes)
	currentCertificates := copyCertificates(snapshot.Certificates)
	currentRouterAccesses := copyRouterAccesses(snapshot.RouterAccesses)
	currentSecuredAccesses := copySecuredAccesses(snapshot.SecuredAccesses)
	secrets := copySecrets(snapshot.Secrets)
	evaluationTime := snapshot.EvaluationTime
	const operationID OperationID = "ensure-access-composition"
	plan.Operations = append(plan.Operations, Operation{ID: operationID, Kind: "EnsureAccessComposition", Run: func(ctx context.Context) error {
		return p.Ensurer.EnsureAccessComposition(ctx, namespace, site, generated, secured, certificates, services, routes, currentRouterAccesses, currentSecuredAccesses, currentCertificates, secrets, evaluationTime)
	}})
	return plan
}

func copyRouterAccesses(values []*skupperv2alpha1.RouterAccess) []*skupperv2alpha1.RouterAccess {
	result := make([]*skupperv2alpha1.RouterAccess, 0, len(values))
	for _, value := range values {
		result = append(result, value.DeepCopy())
	}
	return result
}

func copyRoutes(values []*routev1.Route) []*routev1.Route {
	result := make([]*routev1.Route, 0, len(values))
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

func copyCertificates(values []*skupperv2alpha1.Certificate) []*skupperv2alpha1.Certificate {
	result := make([]*skupperv2alpha1.Certificate, 0, len(values))
	for _, value := range values {
		result = append(result, value.DeepCopy())
	}
	return result
}

func copyServices(values []*corev1.Service) []*corev1.Service {
	result := make([]*corev1.Service, 0, len(values))
	for _, value := range values {
		result = append(result, value.DeepCopy())
	}
	return result
}

func copySecrets(values []*corev1.Secret) []*corev1.Secret {
	result := make([]*corev1.Secret, 0, len(values))
	for _, value := range values {
		result = append(result, value.DeepCopy())
	}
	return result
}
