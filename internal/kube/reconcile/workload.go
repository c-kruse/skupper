package reconcile

import (
	"context"
	"sort"

	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

type SiteEnsurer interface {
	EnsureRouterPrerequisites(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, *corev1.ServiceAccount, *rbacv1.Role, *rbacv1.RoleBinding) error
	EnsureRouterControlCA(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, RouterControlBootstrap) error
	EnsureSite(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, []string, RouterControlBootstrap) error
	EnsureListenerServices(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, []*corev1.Service) error
}

type WorkloadPlanner struct {
	Next    Planner
	Ensurer SiteEnsurer
}

func (p WorkloadPlanner) Plan(snapshot Snapshot, desired DesiredNamespace) Plan {
	plan := p.Next.Plan(snapshot, desired)
	if desired.Site == nil {
		return plan
	}
	groups := make([]string, 0, len(desired.Intents))
	for target := range desired.Intents {
		groups = append(groups, target.RouterGroup)
	}
	sort.Strings(groups)
	site := desired.Site.DeepCopy()
	bootstrap := copyBootstrap(desired.Bootstrap)
	namespace := desired.Namespace
	const allocationID OperationID = "commit-allocations"
	const prerequisitesID OperationID = "ensure-router-prerequisites"
	const caID OperationID = "ensure-router-control-ca"
	const ensureID OperationID = "ensure-site-workloads"
	const listenersID OperationID = "ensure-listener-services"
	caDependencies := []OperationID(nil)
	for _, operation := range plan.Operations {
		if operation.ID == allocationID {
			caDependencies = append(caDependencies, allocationID)
			break
		}
	}
	var serviceAccount *corev1.ServiceAccount
	if desired.ServiceAccount != nil {
		serviceAccount = desired.ServiceAccount.DeepCopy()
	}
	var role *rbacv1.Role
	if desired.Role != nil {
		role = desired.Role.DeepCopy()
	}
	var roleBinding *rbacv1.RoleBinding
	if desired.RoleBinding != nil {
		roleBinding = desired.RoleBinding.DeepCopy()
	}
	plan.Operations = append(plan.Operations, Operation{ID: prerequisitesID, Kind: "EnsureRouterPrerequisites", Dependencies: caDependencies, Run: func(ctx context.Context) error {
		return p.Ensurer.EnsureRouterPrerequisites(ctx, namespace, site, serviceAccount, role, roleBinding)
	}})
	plan.Operations = append(plan.Operations, Operation{ID: caID, Kind: "EnsureRouterControlCA", Dependencies: []OperationID{prerequisitesID}, Run: func(ctx context.Context) error {
		return p.Ensurer.EnsureRouterControlCA(ctx, namespace, site, bootstrap)
	}})
	plan.Operations = append(plan.Operations, Operation{ID: ensureID, Kind: "EnsureSiteWorkloads", Dependencies: []OperationID{caID}, Run: func(ctx context.Context) error {
		return p.Ensurer.EnsureSite(ctx, namespace, site, groups, bootstrap)
	}})
	services := make([]*corev1.Service, 0, len(desired.ListenerServices))
	for _, service := range desired.ListenerServices {
		services = append(services, service.DeepCopy())
	}
	plan.Operations = append(plan.Operations, Operation{ID: listenersID, Kind: "EnsureListenerServices", Dependencies: []OperationID{ensureID}, Run: func(ctx context.Context) error {
		return p.Ensurer.EnsureListenerServices(ctx, namespace, site, services)
	}})
	for i := range plan.Operations {
		if plan.Operations[i].Kind == "PublishRouterIntent" {
			plan.Operations[i].Dependencies = append(plan.Operations[i].Dependencies, listenersID)
		}
	}
	return plan
}
