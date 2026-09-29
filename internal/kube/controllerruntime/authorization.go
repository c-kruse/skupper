package controllerruntime

import (
	"context"
	"fmt"

	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	"github.com/skupperproject/skupper/internal/kube/controller"
	auth "github.com/skupperproject/skupper/internal/kube/routercontrol"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// authorizeAssignment deliberately reads live policy rather than trusting the
// informer snapshot that produced an earlier intent or a certificate's claims.
// The authenticator separately validates the complete workload ownership chain.
func authorizeAssignment(clients internalclient.Clients, config *controller.Config) auth.AssignmentAuthorizer {
	return func(ctx context.Context, identity auth.Identity) error {
		if config.WatchNamespace != "" && identity.Namespace != config.WatchNamespace {
			return fmt.Errorf("namespace is outside controller watch scope")
		}
		assignment, err := clients.GetKubeClient().CoreV1().ConfigMaps(identity.Namespace).Get(ctx, "skupper", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			assignment = nil
		} else if err != nil {
			return fmt.Errorf("read current namespace assignment: %w", err)
		}
		explicit := config.WatchNamespace != "" || config.RequireExplicitControl
		if !controller.ControlsNamespace(assignment, identity.Namespace, config.Namespace+"/"+config.Name, explicit) {
			return fmt.Errorf("namespace is not assigned to this controller")
		}
		site, err := clients.GetSkupperClient().SkupperV2alpha1().Sites(identity.Namespace).Get(ctx, identity.SiteName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read current Site: %w", err)
		}
		if site.UID != identity.SiteUID || site.DeletionTimestamp != nil {
			return fmt.Errorf("Site was replaced or is being deleted")
		}
		allocations, err := clients.GetKubeClient().CoreV1().ConfigMaps(identity.Namespace).Get(ctx, "skupper-controller-allocations", metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read established Site identity: %w", err)
		}
		if allocations.Data["namespaceUID"] != string(identity.NamespaceUID) || allocations.Data["siteUID"] != string(identity.SiteUID) {
			return fmt.Errorf("workload does not belong to the established Site")
		}
		if identity.ServiceAccount != site.Spec.GetServiceAccount() {
			return fmt.Errorf("workload ServiceAccount is not the Site ServiceAccount")
		}
		if identity.Group != "skupper-router" && !(site.Spec.HA && identity.Group == "skupper-router-2") {
			return fmt.Errorf("router group is not desired by the current Site")
		}
		return nil
	}
}
