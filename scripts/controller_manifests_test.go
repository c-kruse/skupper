package scripts_test

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestControllerManifests(t *testing.T) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("manifest generation requires kubectl kustomize")
	}
	for _, scope := range []string{"cluster", "namespace"} {
		t.Run(scope, func(t *testing.T) {
			cmd := exec.Command("bash", "skupper-deployment-generator.sh", scope, "test-controller", "test-router", "false")
			cmd.Env = append(os.Environ(), "SKUPPER_CONTROLLER_NAMESPACE=tenant-blue", "SKUPPER_TESTING=true")
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("generator: %v\n%s", err, output)
			}
			objects := map[string]*unstructured.Unstructured{}
			decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
			for {
				obj := &unstructured.Unstructured{}
				err := decoder.Decode(obj)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				objects[obj.GetKind()+"/"+obj.GetName()] = obj
			}
			get := func(kind, name string, target interface{}) {
				t.Helper()
				obj := objects[kind+"/"+name]
				if obj == nil {
					t.Fatalf("missing %s/%s", kind, name)
				}
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, target); err != nil {
					t.Fatal(err)
				}
			}
			var sts appsv1.StatefulSet
			get("StatefulSet", "skupper-controller", &sts)
			if sts.Namespace != "tenant-blue" || sts.Spec.Replicas == nil || *sts.Spec.Replicas != 2 || sts.Spec.PodManagementPolicy != appsv1.ParallelPodManagement || sts.Spec.UpdateStrategy.Type != appsv1.OnDeleteStatefulSetStrategyType {
				t.Fatalf("wrong active/standby workload: %#v", sts.Spec)
			}
			if objects["Deployment/skupper-controller"] != nil || len(sts.Spec.VolumeClaimTemplates) != 0 {
				t.Fatal("controller must not use a Ready-gated Deployment or require persistent volumes")
			}
			container := sts.Spec.Template.Spec.Containers[0]
			if container.StartupProbe == nil || container.StartupProbe.HTTPGet.Path != "/startupz" || container.ReadinessProbe == nil || container.ReadinessProbe.HTTPGet.Path != "/readyz" || container.LivenessProbe == nil || container.LivenessProbe.HTTPGet.Path != "/livez" {
				t.Fatal("standby startup, process liveness, and leader readiness must have distinct probes")
			}
			if container.ImagePullPolicy != corev1.PullNever {
				t.Fatal("testing image pull patch did not target the StatefulSet")
			}
			var service corev1.Service
			get("Service", "skupper-controller", &service)
			if service.Namespace != sts.Namespace || service.Spec.PublishNotReadyAddresses || len(service.Spec.Ports) != 2 || service.Spec.Ports[0].Port != 8443 || service.Spec.Ports[1].Port != 8444 {
				t.Fatalf("incorrect leader-only RPC Service: %#v", service.Spec)
			}
			for k, v := range service.Spec.Selector {
				if sts.Spec.Template.Labels[k] != v {
					t.Fatalf("Service selector %s=%s misses controller Pods", k, v)
				}
			}
			var headless corev1.Service
			get("Service", sts.Spec.ServiceName, &headless)
			if headless.Spec.ClusterIP != corev1.ClusterIPNone {
				t.Fatal("missing headless governing Service")
			}
			var rules []rbacv1.PolicyRule
			var binding rbacv1.ClusterRoleBinding
			if scope == "cluster" {
				var role rbacv1.ClusterRole
				get("ClusterRole", "skupper-controller", &role)
				rules = role.Rules
				get("ClusterRoleBinding", "skupper-controller", &binding)
			} else {
				var role rbacv1.Role
				get("Role", "skupper-controller", &role)
				rules = role.Rules
				var enrollment rbacv1.ClusterRole
				get("ClusterRole", "skupper-controller-enrollment", &enrollment)
				rules = append(rules, enrollment.Rules...)
				get("ClusterRoleBinding", "skupper-controller-enrollment-tenant-blue", &binding)
			}
			if len(binding.Subjects) != 1 || binding.Subjects[0].Namespace != "tenant-blue" {
				t.Fatalf("TokenReview grant points at wrong namespace: %#v", binding.Subjects)
			}
			tokenReview := false
			for _, rule := range rules {
				for _, resource := range rule.Resources {
					if resource == "pods/exec" {
						t.Fatal("controller must not have router exec permission")
					}
					if resource == "tokenreviews" && len(rule.Verbs) == 1 && rule.Verbs[0] == "create" {
						tokenReview = true
					}
				}
			}
			if !tokenReview {
				t.Fatal("missing cluster-scoped TokenReview create grant")
			}
		})
	}
}
