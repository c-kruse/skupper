package resources

import (
	"bytes"
	"testing"
	"text/template"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/skupperproject/skupper/internal/kube/site/sizing"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

func TestRouterControlWorkloadHasAuthenticatedBootstrapAndControllerOwner(t *testing.T) {
	site := &skupperv2alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site", Namespace: "site-ns", UID: "site-uid"}}
	control := RouterControlConfig{NamespaceUID: "namespace-uid", SiteUID: "site-uid", EnrollmentURL: "https://skupper-controller.controller.svc:8443", ControlAddress: "skupper-controller.controller.svc:8444", TLSServerName: "skupper-controller.controller.svc", TokenAudience: "skupper-controller-enrollment", TokenPath: "/var/run/secrets/skupper-controller/enrollment-token", CABundleConfigMap: "skupper-controller-ca", CABundleKey: "ca.crt", CABundlePath: "/etc/skupper-controller/ca.crt"}
	params := getCoreParams(site, "skupper-router", sizing.Sizing{}, false, &control)
	tmpl, err := template.New("deployment").Parse(routerDeploymentTemplate)
	if err != nil {
		t.Fatal(err)
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, params); err != nil {
		t.Fatal(err)
	}
	deployment := &appsv1.Deployment{}
	if err := yaml.Unmarshal(rendered.Bytes(), deployment); err != nil {
		t.Fatalf("invalid rendered Deployment: %v\n%s", err, rendered.String())
	}
	owners := deployment.OwnerReferences
	if len(owners) != 1 || owners[0].Controller == nil || !*owners[0].Controller || owners[0].BlockOwnerDeletion == nil || !*owners[0].BlockOwnerDeletion {
		t.Fatalf("Site is not the Deployment controller owner: %#v", owners)
	}
	if deployment.Spec.Template.Labels["skupper.io/group"] != "skupper-router" {
		t.Fatalf("router group label missing: %#v", deployment.Spec.Template.Labels)
	}
	if deployment.Spec.Template.Spec.ServiceAccountName != "skupper-router" {
		t.Fatalf("unexpected service account %q", deployment.Spec.Template.Spec.ServiceAccountName)
	}
	if findEnv(deployment.Spec.Template.Spec.Containers[1].Env, "SKUPPER_CONTROLLER_ENROLLMENT_URL") != control.EnrollmentURL {
		t.Fatal("adaptor enrollment URL missing")
	}
	if findEnv(deployment.Spec.Template.Spec.InitContainers[0].Env, "SKUPPER_CONTROLLER_ENROLLMENT_TOKEN") != control.TokenPath {
		t.Fatal("config-init token path missing")
	}
	for _, container := range []corev1.Container{deployment.Spec.Template.Spec.Containers[1], deployment.Spec.Template.Spec.InitContainers[0]} {
		want := map[string]string{"SKUPPER_NAMESPACE_UID": control.NamespaceUID, "SKUPPER_SITE_UID": control.SiteUID, "SKUPPER_ROUTER_GROUP": "skupper-router", "SKUPPER_CONFIG_DIR": "/etc/skupper-router-certs", "SKUPPER_CONTROLLER_ENROLLMENT_URL": control.EnrollmentURL, "SKUPPER_CONTROLLER_CONTROL_ADDRESS": control.ControlAddress, "SKUPPER_CONTROLLER_SERVER_NAME": control.TLSServerName, "SKUPPER_CONTROLLER_ENROLLMENT_TOKEN": control.TokenPath, "SKUPPER_CONTROLLER_CA": control.CABundlePath}
		for name, value := range want {
			if got := findEnv(container.Env, name); got != value {
				t.Fatalf("%s env %s=%q, want %q", container.Name, name, got, value)
			}
		}
	}
	for _, env := range deployment.Spec.Template.Spec.Containers[1].Env {
		if env.Name == "SKUPPER_ROUTER_CONFIG" {
			t.Fatal("router-control workload still references router-config")
		}
	}
	if len(deployment.Spec.Template.Spec.Volumes) != 4 {
		t.Fatalf("expected traffic, proxy, projected token, and public CA volumes: %#v", deployment.Spec.Template.Spec.Volumes)
	}
}

func findEnv(values []corev1.EnvVar, name string) string {
	for _, value := range values {
		if value.Name == name {
			return value.Value
		}
	}
	return ""
}
