package controllerruntime

import (
	"context"
	"errors"
	"testing"

	fakeclient "github.com/skupperproject/skupper/internal/kube/client/fake"
	"github.com/skupperproject/skupper/internal/kube/controller"
	auth "github.com/skupperproject/skupper/internal/kube/routercontrol"
	v2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
)

func TestAuthorizationUsesLiveAssignmentSiteAndServiceAccount(t *testing.T) {
	ctx := context.Background()
	assignment := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "skupper"}, Data: map[string]string{"controller": "control/skupper-controller"}}
	allocation := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "skupper-controller-allocations"}, Data: map[string]string{"namespaceUID": "namespace-uid", "siteUID": "site-uid"}}
	site := &v2alpha1.Site{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "west", UID: "site-uid"}, Spec: v2alpha1.SiteSpec{ServiceAccount: "custom-router", HA: true}}
	clients, err := fakeclient.NewFakeClient("control", []runtime.Object{assignment, allocation}, []runtime.Object{site}, "")
	if err != nil {
		t.Fatal(err)
	}
	config := &controller.Config{Namespace: "control", Name: "skupper-controller", RequireExplicitControl: true}
	authorize := authorizeAssignment(clients, config)
	identity := auth.Identity{Namespace: "tenant", NamespaceUID: "namespace-uid", SiteName: "west", SiteUID: "site-uid", ServiceAccount: "custom-router", Group: "skupper-router-2"}
	if err := authorize(ctx, identity); err != nil {
		t.Fatalf("valid HA/custom-SA assignment rejected: %v", err)
	}
	for _, test := range []struct {
		name   string
		change func(*auth.Identity)
	}{
		{"wrong SA", func(id *auth.Identity) { id.ServiceAccount = "skupper-router" }},
		{"old Site UID", func(id *auth.Identity) { id.SiteUID = "old-site" }},
		{"old namespace UID", func(id *auth.Identity) { id.NamespaceUID = "old-namespace" }},
		{"unexpected group", func(id *auth.Identity) { id.Group = "skupper-router-3" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := identity
			test.change(&invalid)
			if err := authorize(ctx, invalid); err == nil {
				t.Fatal("invalid workload assignment authorized")
			}
		})
	}
	// Mutate authoritative policy after the closure was built, without any
	// informer notification. A cached decision must not authorize this request.
	assignment.Data["controller"] = "other/skupper-controller"
	if _, err := clients.GetKubeClient().CoreV1().ConfigMaps("tenant").Update(ctx, assignment, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := authorize(ctx, identity); err == nil {
		t.Fatal("namespace reassignment did not revoke authorization")
	}
	assignment.Data["controller"] = "control/skupper-controller"
	if _, err := clients.GetKubeClient().CoreV1().ConfigMaps("tenant").Update(ctx, assignment, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	site.Spec.HA = false
	if _, err := clients.GetSkupperClient().SkupperV2alpha1().Sites("tenant").Update(ctx, site, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := authorize(ctx, identity); err == nil {
		t.Fatal("retired HA group remained authorized")
	}
	identity.Group = "skupper-router"
	config.WatchNamespace = "other"
	if err := authorize(ctx, identity); err == nil {
		t.Fatal("controller authorized outside its watch scope")
	}
	config.WatchNamespace = ""
	clients.GetKubeClient().(interface {
		PrependReactor(string, string, clienttesting.ReactionFunc)
	}).PrependReactor("get", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("API unavailable")
	})
	if err := authorize(ctx, identity); err == nil {
		t.Fatal("API uncertainty fell back to cached authorization")
	}
}
