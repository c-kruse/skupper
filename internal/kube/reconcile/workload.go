package reconcile

import (
	"context"
	"reflect"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/skupperproject/skupper/internal/kube/resource"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

type SiteEnsurer interface {
	EnsureRouterPrerequisites(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, *corev1.ServiceAccount, *rbacv1.Role, *rbacv1.RoleBinding, *corev1.ServiceAccount, *rbacv1.Role, *rbacv1.RoleBinding) error
	EnsureRouterControlCA(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, *corev1.ConfigMap, *corev1.ConfigMap) error
	EnsureDeployment(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, *appsv1.Deployment, *appsv1.Deployment) error
	EnsureLocalService(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, *corev1.Service, *corev1.Service) error
	RetireDeployment(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, *appsv1.Deployment) error
	EnsureListenerService(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, *corev1.Service, *corev1.Service) error
	RetireListenerService(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, *corev1.Service) error
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
	site := desired.Site.DeepCopy()
	namespace := desired.Namespace
	currentServiceAccount := serviceAccountNamed(snapshot.ServiceAccounts, "skupper-router")
	currentRole := roleNamed(snapshot.Roles, "skupper-router")
	currentRoleBinding := roleBindingNamed(snapshot.RoleBindings, "skupper-router")
	preparation := []OperationID{}
	allocationID := operationID(plan, "commit-allocations")

	if prerequisitesDiffer(site, desired, currentServiceAccount, currentRole, currentRoleBinding) {
		const id OperationID = "ensure-router-prerequisites"
		dependencies := existingIDs(allocationID)
		plan.Operations = append(plan.Operations, Operation{ID: id, Kind: "EnsureRouterPrerequisites", Dependencies: dependencies, Run: func(ctx context.Context) error {
			return p.Ensurer.EnsureRouterPrerequisites(ctx, namespace, site, copyServiceAccount(desired.ServiceAccount), copyRole(desired.Role), copyRoleBinding(desired.RoleBinding), copyServiceAccount(currentServiceAccount), copyRole(currentRole), copyRoleBinding(currentRoleBinding))
		}})
		preparation = append(preparation, id)
	}
	if configMapDiffer(site, snapshot.RouterControlCA, desired.RouterControlCA) {
		const id OperationID = "ensure-router-control-ca"
		dependencies := append([]OperationID(nil), preparation...)
		if len(dependencies) == 0 {
			dependencies = existingIDs(allocationID)
		}
		plan.Operations = append(plan.Operations, Operation{ID: id, Kind: "EnsureRouterControlCA", Dependencies: dependencies, Run: func(ctx context.Context) error {
			return p.Ensurer.EnsureRouterControlCA(ctx, namespace, site, desired.RouterControlCA.DeepCopy(), copyConfigMap(snapshot.RouterControlCA))
		}})
		preparation = []OperationID{id}
	}

	var publishIDs []OperationID
	for i := range plan.Operations {
		if plan.Operations[i].Kind != "PublishRouterIntent" {
			continue
		}
		plan.Operations[i].Dependencies = appendUnique(plan.Operations[i].Dependencies, preparation...)
		publishIDs = append(publishIDs, plan.Operations[i].ID)
	}
	rolloutDependencies := publishIDs
	if len(rolloutDependencies) == 0 {
		rolloutDependencies = preparation
	}

	afterRollout := rolloutDependencies
	if desired.WorkloadsKnown {
		currentDeployments := deploymentsByName(snapshot.Deployments)
		desiredDeployments := map[string]bool{}
		rolloutIDs := []OperationID{}
		for _, wanted := range desired.Deployments {
			wanted := wanted.DeepCopy()
			desiredDeployments[wanted.Name] = true
			current := currentDeployments[wanted.Name]
			if deploymentEqual(site, current, wanted) {
				continue
			}
			id := OperationID("ensure-deployment/" + wanted.Name)
			current = copyDeployment(current)
			plan.Operations = append(plan.Operations, Operation{ID: id, Kind: "EnsureSiteWorkload", Dependencies: append([]OperationID(nil), rolloutDependencies...), Run: func(ctx context.Context) error {
				return p.Ensurer.EnsureDeployment(ctx, namespace, site, wanted, current)
			}})
			rolloutIDs = append(rolloutIDs, id)
		}
		currentLocal := serviceNamed(snapshot.Services, "skupper-router-local")
		if desired.LocalService != nil && !localServiceEqual(site, currentLocal, desired.LocalService) {
			wanted := desired.LocalService.DeepCopy()
			current := copyService(currentLocal)
			const id OperationID = "ensure-local-service"
			plan.Operations = append(plan.Operations, Operation{ID: id, Kind: "EnsureSiteWorkload", Dependencies: append([]OperationID(nil), rolloutDependencies...), Run: func(ctx context.Context) error {
				return p.Ensurer.EnsureLocalService(ctx, namespace, site, wanted, current)
			}})
			rolloutIDs = append(rolloutIDs, id)
		}
		if len(rolloutIDs) > 0 {
			afterRollout = rolloutIDs
		}
		for _, current := range snapshot.Deployments {
			if desiredDeployments[current.Name] || current.Labels["application"] != "skupper-router" || !metav1.IsControlledBy(current, site) {
				continue
			}
			retiring := current.DeepCopy()
			id := OperationID("retire-deployment/" + current.Name)
			plan.Operations = append(plan.Operations, Operation{ID: id, Kind: "RetireSiteWorkload", Dependencies: append([]OperationID(nil), afterRollout...), Run: func(ctx context.Context) error {
				return p.Ensurer.RetireDeployment(ctx, namespace, site, retiring)
			}})
		}
	}

	desiredListenerNames := map[string]bool{}
	currentServices := servicesByName(snapshot.Services)
	for _, service := range desired.ListenerServices {
		service := service.DeepCopy()
		desiredListenerNames[service.Name] = true
		current := currentServices[service.Name]
		if listenerServiceEqual(site, current, service) {
			continue
		}
		id := OperationID("ensure-listener-service/" + service.Name)
		current = copyService(current)
		plan.Operations = append(plan.Operations, Operation{ID: id, Kind: "EnsureListenerService", Dependencies: append([]OperationID(nil), afterRollout...), Run: func(ctx context.Context) error {
			return p.Ensurer.EnsureListenerService(ctx, namespace, site, service, current)
		}})
	}
	for _, current := range snapshot.Services {
		if desiredListenerNames[current.Name] || current.Labels["internal.skupper.io/listener"] != "true" || current.Annotations["internal.skupper.io/controlled"] != "true" || !ownedBy(current.OwnerReferences, site.UID) {
			continue
		}
		retiring := current.DeepCopy()
		id := OperationID("retire-listener-service/" + current.Name)
		plan.Operations = append(plan.Operations, Operation{ID: id, Kind: "RetireListenerService", Dependencies: append([]OperationID(nil), afterRollout...), Run: func(ctx context.Context) error {
			return p.Ensurer.RetireListenerService(ctx, namespace, site, retiring)
		}})
	}
	return plan
}

func operationID(plan Plan, id OperationID) OperationID {
	for _, operation := range plan.Operations {
		if operation.ID == id {
			return id
		}
	}
	return ""
}

func existingIDs(ids ...OperationID) []OperationID {
	result := []OperationID{}
	for _, id := range ids {
		if id != "" {
			result = append(result, id)
		}
	}
	return result
}

func appendUnique(current []OperationID, values ...OperationID) []OperationID {
	seen := map[OperationID]bool{}
	for _, id := range current {
		seen[id] = true
	}
	for _, id := range values {
		if id != "" && !seen[id] {
			current = append(current, id)
			seen[id] = true
		}
	}
	return current
}

func prerequisitesDiffer(site *skupperv2alpha1.Site, desired DesiredNamespace, serviceAccount *corev1.ServiceAccount, role *rbacv1.Role, binding *rbacv1.RoleBinding) bool {
	if desired.ServiceAccount == nil {
		return controlledBy(site, serviceAccount) || controlledBy(site, role) || controlledBy(site, binding)
	}
	return serviceAccount == nil || role == nil || binding == nil || !metav1.IsControlledBy(serviceAccount, site) || !metav1.IsControlledBy(role, site) || !metav1.IsControlledBy(binding, site) ||
		!reflect.DeepEqual(serviceAccount.OwnerReferences, desired.ServiceAccount.OwnerReferences) || !reflect.DeepEqual(role.OwnerReferences, desired.Role.OwnerReferences) || !reflect.DeepEqual(role.Rules, desired.Role.Rules) || !reflect.DeepEqual(binding.OwnerReferences, desired.RoleBinding.OwnerReferences) || !reflect.DeepEqual(binding.Subjects, desired.RoleBinding.Subjects) || binding.RoleRef != desired.RoleBinding.RoleRef
}

func controlledBy(site *skupperv2alpha1.Site, object metav1.Object) bool {
	return object != nil && !reflect.ValueOf(object).IsNil() && metav1.IsControlledBy(object, site)
}

func configMapDiffer(site *skupperv2alpha1.Site, current, desired *corev1.ConfigMap) bool {
	return desired != nil && (current == nil || !metav1.IsControlledBy(current, site) || !reflect.DeepEqual(current.OwnerReferences, desired.OwnerReferences) || !reflect.DeepEqual(current.Data, desired.Data))
}

func deploymentEqual(site *skupperv2alpha1.Site, current, desired *appsv1.Deployment) bool {
	return current != nil && metav1.IsControlledBy(current, site) && resource.DeploymentApplyEqual(current, desired)
}

func localServiceEqual(site *skupperv2alpha1.Site, current, desired *corev1.Service) bool {
	return current != nil && ownedBy(current.OwnerReferences, site.UID) && resource.ServiceApplyEqual(current, desired)
}

func listenerServiceEqual(site *skupperv2alpha1.Site, current, desired *corev1.Service) bool {
	if current == nil || !ownedBy(current.OwnerReferences, site.UID) || current.Annotations["internal.skupper.io/controlled"] != "true" {
		return false
	}
	wanted := desired.DeepCopy()
	wanted.Spec.ClusterIP = current.Spec.ClusterIP
	wanted.Spec.ClusterIPs = append([]string(nil), current.Spec.ClusterIPs...)
	wanted.Spec.IPFamilies = append([]corev1.IPFamily(nil), current.Spec.IPFamilies...)
	wanted.Spec.IPFamilyPolicy = current.Spec.IPFamilyPolicy
	return reflect.DeepEqual(current.Labels, wanted.Labels) && reflect.DeepEqual(current.Annotations, wanted.Annotations) && reflect.DeepEqual(current.OwnerReferences, wanted.OwnerReferences) && reflect.DeepEqual(current.Spec, wanted.Spec)
}

func deploymentsByName(values []*appsv1.Deployment) map[string]*appsv1.Deployment {
	result := map[string]*appsv1.Deployment{}
	for _, value := range values {
		result[value.Name] = value
	}
	return result
}
func servicesByName(values []*corev1.Service) map[string]*corev1.Service {
	result := map[string]*corev1.Service{}
	for _, value := range values {
		result[value.Name] = value
	}
	return result
}
func serviceNamed(values []*corev1.Service, name string) *corev1.Service {
	return servicesByName(values)[name]
}
func serviceAccountNamed(values []*corev1.ServiceAccount, name string) *corev1.ServiceAccount {
	for _, value := range values {
		if value.Name == name {
			return value
		}
	}
	return nil
}
func roleNamed(values []*rbacv1.Role, name string) *rbacv1.Role {
	for _, value := range values {
		if value.Name == name {
			return value
		}
	}
	return nil
}
func roleBindingNamed(values []*rbacv1.RoleBinding, name string) *rbacv1.RoleBinding {
	for _, value := range values {
		if value.Name == name {
			return value
		}
	}
	return nil
}
func copyDeployment(value *appsv1.Deployment) *appsv1.Deployment {
	if value == nil {
		return nil
	}
	return value.DeepCopy()
}
func copyService(value *corev1.Service) *corev1.Service {
	if value == nil {
		return nil
	}
	return value.DeepCopy()
}
func copyServiceAccount(value *corev1.ServiceAccount) *corev1.ServiceAccount {
	if value == nil {
		return nil
	}
	return value.DeepCopy()
}
func copyRole(value *rbacv1.Role) *rbacv1.Role {
	if value == nil {
		return nil
	}
	return value.DeepCopy()
}
func copyRoleBinding(value *rbacv1.RoleBinding) *rbacv1.RoleBinding {
	if value == nil {
		return nil
	}
	return value.DeepCopy()
}
func copyConfigMap(value *corev1.ConfigMap) *corev1.ConfigMap {
	if value == nil {
		return nil
	}
	return value.DeepCopy()
}
