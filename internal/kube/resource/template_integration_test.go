package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	applyappsv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// This test is intentionally opt-in: it validates apiserver defaulting and
// managedFields behavior that fake clients cannot model.
func TestRealAPIServerCreateOwnershipAndDefaultsConverge(t *testing.T) {
	kubeconfig := os.Getenv("SKUPPER_REAL_API_TEST")
	if kubeconfig == "" {
		t.Skip("SKUPPER_REAL_API_TEST is not set")
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	namespace := fmt.Sprintf("skupper-semantic-delta-%d", time.Now().UnixNano())
	_, err = client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.CoreV1().Namespaces().Delete(context.Background(), namespace, metav1.DeleteOptions{})

	parent, err := client.CoreV1().ConfigMaps(namespace).Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "test-owner"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	controller, block := true, true
	owner := []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: parent.Name, UID: parent.UID, Controller: &controller, BlockOwnerDeletion: &block}}
	replicas := int32(1)
	deployment := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: "router", Namespace: namespace, Labels: map[string]string{"application": "skupper-router"}, OwnerReferences: owner},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "router"}}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "router"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "router", Image: "busybox:1.36", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: apiresource.MustParse("100m"), corev1.ResourceMemory: apiresource.MustParse("64Mi")}}}}}}},
	}
	if _, err := client.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{FieldManager: FieldManager}); err != nil {
		t.Fatal(err)
	}
	currentDeployment, err := client.AppsV1().Deployments(namespace).Get(ctx, deployment.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if DeploymentApplyEqual(currentDeployment, deployment) {
		t.Fatal("Create was incorrectly treated as SSA ownership evidence")
	}
	deploymentObject, err := runtime.DefaultUnstructuredConverter.ToUnstructured(deployment)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 10; attempt++ {
		_, err = ApplyRendered(dynamicClient, ctx, DeploymentResource(), &unstructured.Unstructured{Object: deploymentObject}, currentDeployment)
		if err == nil {
			break
		}
		if !apierrors.IsConflict(err) {
			t.Fatal(err)
		}
		currentDeployment, err = client.AppsV1().Deployments(namespace).Get(ctx, deployment.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err != nil {
		t.Fatalf("Deployment SSA did not find a stable resourceVersion: %v", err)
	}
	currentDeployment, err = client.AppsV1().Deployments(namespace).Get(ctx, deployment.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !DeploymentApplyEqual(currentDeployment, deployment) {
		owned, extractErr := applyappsv1.ExtractDeployment(currentDeployment, FieldManager)
		wanted := &applyappsv1.DeploymentApplyConfiguration{}
		decodeApply(deployment, wanted)
		t.Fatalf("apiserver-defaulted Deployment did not converge after SSA established ownership: extract=%v\nowned=%#v\nwanted=%#v", extractErr, canonicalApply(owned, "Deployment"), canonicalApply(wanted, "Deployment"))
	}
	admissionPatch, err := json.Marshal(map[string]interface{}{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]interface{}{"name": deployment.Name, "namespace": namespace, "annotations": map[string]interface{}{"admission.example/injected": "true"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dynamicClient.Resource(DeploymentResource()).Namespace(namespace).Patch(ctx, deployment.Name, types.ApplyPatchType, admissionPatch, metav1.PatchOptions{FieldManager: "admission-test"}); err != nil {
		t.Fatal(err)
	}
	currentDeployment, err = client.AppsV1().Deployments(namespace).Get(ctx, deployment.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !DeploymentApplyEqual(currentDeployment, deployment) {
		t.Fatal("field owned by another manager caused a workload rewrite")
	}
	// A strategic-merge patch transfers ownership of the changed label, even
	// though the caller did not use SSA. Detect that drift but do not silently
	// force the field back from the new owner.
	currentDeployment, err = client.AppsV1().Deployments(namespace).Patch(ctx, deployment.Name, types.StrategicMergePatchType,
		[]byte(`{"metadata":{"labels":{"application":"external-value","external.example/label":"retained"}}}`), metav1.PatchOptions{FieldManager: "external-drift"})
	if err != nil {
		t.Fatal(err)
	}
	if DeploymentApplyEqual(currentDeployment, deployment) {
		t.Fatal("ownership transfer hid desired-label drift")
	}
	// Isolate ownership from unrelated Deployment status/RV changes here.
	_, err = ApplyRendered(dynamicClient, ctx, DeploymentResource(), &unstructured.Unstructured{Object: deploymentObject}, nil)
	if !apierrors.IsConflict(err) || !apierrors.HasStatusCause(err, metav1.CauseTypeFieldManagerConflict) {
		t.Fatalf("expected a field-ownership conflict, not forced adoption or an RV conflict: %v", err)
	}
	// Removing the conflicting value relinquishes ownership. Missing desired
	// metadata must then repair and converge without removing foreign fields.
	currentDeployment, err = client.AppsV1().Deployments(namespace).Patch(ctx, deployment.Name, types.StrategicMergePatchType,
		[]byte(`{"metadata":{"labels":{"application":null}}}`), metav1.PatchOptions{FieldManager: "external-drift"})
	if err != nil {
		t.Fatal(err)
	}
	if DeploymentApplyEqual(currentDeployment, deployment) {
		t.Fatal("missing desired label was treated as converged")
	}
	for attempt := 0; attempt < 10; attempt++ {
		_, err = ApplyRendered(dynamicClient, ctx, DeploymentResource(), &unstructured.Unstructured{Object: deploymentObject}, currentDeployment)
		if err == nil {
			break
		}
		if !apierrors.IsConflict(err) || apierrors.HasStatusCause(err, metav1.CauseTypeFieldManagerConflict) {
			t.Fatal(err)
		}
		currentDeployment, err = client.AppsV1().Deployments(namespace).Get(ctx, deployment.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err != nil {
		t.Fatalf("metadata repair did not find a stable resourceVersion: %v", err)
	}
	currentDeployment, err = client.AppsV1().Deployments(namespace).Get(ctx, deployment.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if currentDeployment.Labels["application"] != "skupper-router" || currentDeployment.Labels["external.example/label"] != "retained" || currentDeployment.Annotations["admission.example/injected"] != "true" || !DeploymentApplyEqual(currentDeployment, deployment) {
		t.Fatal("metadata repair did not preserve foreign fields and reach a zero-delta comparison")
	}
	t.Log("desired-label drift detected; external ownership conflicts without force; relinquished field repaired and reconverged with foreign metadata intact")
	withoutReplicas := deployment.DeepCopy()
	withoutReplicas.Spec.Replicas = nil
	if DeploymentApplyEqual(currentDeployment, withoutReplicas) {
		t.Fatal("real managedFields comparison missed removal of owned replicas")
	}
	withoutReplicasObject, err := runtime.DefaultUnstructuredConverter.ToUnstructured(withoutReplicas)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 10; attempt++ {
		_, err = ApplyRendered(dynamicClient, ctx, DeploymentResource(), &unstructured.Unstructured{Object: withoutReplicasObject}, currentDeployment)
		if err == nil {
			break
		}
		if !apierrors.IsConflict(err) {
			t.Fatal(err)
		}
		currentDeployment, err = client.AppsV1().Deployments(namespace).Get(ctx, deployment.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err != nil {
		t.Fatalf("Deployment removal SSA did not find a stable resourceVersion: %v", err)
	}
	currentDeployment, err = client.AppsV1().Deployments(namespace).Get(ctx, deployment.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !DeploymentApplyEqual(currentDeployment, withoutReplicas) {
		t.Fatal("owned-field removal did not converge after SSA and defaulting")
	}

	service := &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: "router-local", Namespace: namespace, OwnerReferences: owner},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Selector: map[string]string{"app": "router"}, Ports: []corev1.ServicePort{{Name: "amqps", Port: 5671, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(5671)}}},
	}
	if _, err := client.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{FieldManager: FieldManager}); err != nil {
		t.Fatal(err)
	}
	currentService, err := client.CoreV1().Services(namespace).Get(ctx, service.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	serviceObject, err := runtime.DefaultUnstructuredConverter.ToUnstructured(service)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyRendered(dynamicClient, ctx, schema.GroupVersionResource{Version: "v1", Resource: "services"}, &unstructured.Unstructured{Object: serviceObject}, currentService); err != nil {
		t.Fatal(err)
	}
	currentService, err = client.CoreV1().Services(namespace).Get(ctx, service.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !ServiceApplyEqual(currentService, service) {
		t.Fatal("apiserver-defaulted Service did not converge after SSA established ownership")
	}

	for _, fixture := range []struct {
		name     string
		resource schema.GroupVersionResource
		object   map[string]interface{}
		equal    func(*unstructured.Unstructured, *unstructured.Unstructured) bool
	}{
		{"deployment", DeploymentResource(), deploymentObject, func(current, desired *unstructured.Unstructured) bool {
			var a, b appsv1.Deployment
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(current.Object, &a); err != nil {
				t.Fatal(err)
			}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(desired.Object, &b); err != nil {
				t.Fatal(err)
			}
			return DeploymentApplyEqual(&a, &b)
		}},
		{"service", schema.GroupVersionResource{Version: "v1", Resource: "services"}, serviceObject, func(current, desired *unstructured.Unstructured) bool {
			var a, b corev1.Service
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(current.Object, &a); err != nil {
				t.Fatal(err)
			}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(desired.Object, &b); err != nil {
				t.Fatal(err)
			}
			return ServiceApplyEqual(&a, &b)
		}},
	} {
		for _, alreadyApplied := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/creation-field-removal/previous-apply=%t", fixture.name, alreadyApplied), func(t *testing.T) {
				desired := (&unstructured.Unstructured{Object: fixture.object}).DeepCopy()
				desired.SetName(fmt.Sprintf("migration-%s-%t", fixture.name, alreadyApplied))
				created := desired.DeepCopy()
				paths := [][]string{{"metadata", "labels"}, {"metadata", "annotations"}}
				if fixture.name == "deployment" {
					paths = append(paths, []string{"spec", "template", "metadata", "labels"}, []string{"spec", "template", "metadata", "annotations"})
				}
				for _, path := range paths {
					if err := unstructured.SetNestedField(created.Object, "remove-me", append(path, "creation.example/old")...); err != nil {
						t.Fatal(err)
					}
				}
				objects := dynamicClient.Resource(fixture.resource).Namespace(namespace)
				current, err := objects.Create(ctx, created, metav1.CreateOptions{FieldManager: FieldManager})
				if err != nil {
					t.Fatal(err)
				}
				if alreadyApplied {
					// Reproduce the old Create -> SSA path, including objects that
					// already look converged to an Apply-only field extraction.
					data, err := json.Marshal(desired)
					if err != nil {
						t.Fatal(err)
					}
					current, err = objects.Patch(ctx, desired.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{FieldManager: FieldManager})
					if err != nil {
						t.Fatal(err)
					}
				}
				if fixture.equal(current, desired) {
					t.Fatal("creation-time ownership was ignored when selecting migration/repair")
				}
				stale := current.DeepCopy()
				current, err = objects.Patch(ctx, desired.GetName(), types.MergePatchType, []byte(`{"metadata":{"labels":{"foreign.example/label":"retained"}}}`), metav1.PatchOptions{FieldManager: "foreign-owner"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := ApplyRendered(dynamicClient, ctx, fixture.resource, desired, stale); !apierrors.IsConflict(err) {
					t.Fatalf("stale ownership migration must conflict: %v", err)
				}
				for attempt := 0; attempt < 10; attempt++ {
					current, err = objects.Get(ctx, desired.GetName(), metav1.GetOptions{})
					if err != nil {
						t.Fatal(err)
					}
					current, err = ApplyRendered(dynamicClient, ctx, fixture.resource, desired, current)
					if !apierrors.IsConflict(err) {
						break
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				for _, path := range paths {
					if _, found, err := unstructured.NestedFieldNoCopy(current.Object, append(path, "creation.example/old")...); err != nil || found {
						t.Errorf("creation-time field survived desired removal at %v: %v", path, err)
					}
				}
				if current.GetUID() != stale.GetUID() || current.GetLabels()["foreign.example/label"] != "retained" || !fixture.equal(current, desired) {
					t.Fatal("migration failed to preserve identity/foreign metadata and converge")
				}
			})
		}
	}
}
