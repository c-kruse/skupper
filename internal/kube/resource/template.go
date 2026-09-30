package resource

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"text/template"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"k8s.io/apimachinery/pkg/types"
	applyappsv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	applycorev1 "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/dynamic"
)

const FieldManager = "skupper-controller"

type Template struct {
	Name       string
	Template   string
	Parameters interface{}
	Resource   schema.GroupVersionResource
}

func (t Template) getYaml() ([]byte, error) {
	tmpl, err := template.New(t.Name).Parse(t.Template)
	if err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	err = tmpl.Execute(&buffer, t.Parameters)
	if err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

var decoder = yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)

// Render expands a resource template without requiring a Kubernetes client.
func (t Template) Render(namespace string) (*unstructured.Unstructured, error) {
	raw, err := t.getYaml()
	if err != nil {
		return nil, err
	}
	obj := &unstructured.Unstructured{}
	_, _, err = decoder.Decode(raw, nil, obj)
	if err != nil {
		return nil, err
	}
	obj.SetNamespace(namespace)
	return obj, nil
}

func (t Template) Apply(client dynamic.Interface, ctx context.Context, namespace string) (*unstructured.Unstructured, error) {
	obj, err := t.Render(namespace)
	if err != nil {
		return nil, err
	}
	return ApplyRendered(client, ctx, t.Resource, obj, "")
}

// ApplyRendered performs the same no-force server-side apply as Template.Apply.
// resourceVersion is an update precondition; an empty value retains SSA create
// behavior for legacy callers.
func ApplyRendered(client dynamic.Interface, ctx context.Context, resource schema.GroupVersionResource, obj *unstructured.Unstructured, resourceVersion string) (*unstructured.Unstructured, error) {
	obj = obj.DeepCopy()
	obj.SetResourceVersion(resourceVersion)
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return client.Resource(resource).Namespace(obj.GetNamespace()).Patch(ctx, obj.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{
		FieldManager: FieldManager,
	})
}

// DeploymentApplyEqual compares the desired apply document with exactly the
// fields currently owned by Skupper's SSA manager. Fields owned by admission or
// another manager do not trigger work; loss or removal of a formerly-owned
// field does. Objects without usable managedFields conservatively compare
// unequal because their ownership cannot be established.
func DeploymentApplyEqual(current, desired *appsv1.Deployment) bool {
	owned, err := applyappsv1.ExtractDeployment(current, FieldManager)
	if err != nil || !hasManagedFields(current.ManagedFields) {
		return false
	}
	wanted := &applyappsv1.DeploymentApplyConfiguration{}
	if !decodeApply(desired, wanted) {
		return false
	}
	return applyEqual(owned, wanted, "Deployment")
}

// ServiceApplyEqual is the Service equivalent of DeploymentApplyEqual.
func ServiceApplyEqual(current, desired *corev1.Service) bool {
	owned, err := applycorev1.ExtractService(current, FieldManager)
	if err != nil || !hasManagedFields(current.ManagedFields) {
		return false
	}
	wanted := &applycorev1.ServiceApplyConfiguration{}
	if !decodeApply(desired, wanted) {
		return false
	}
	return applyEqual(owned, wanted, "Service")
}

func hasManagedFields(entries []metav1.ManagedFieldsEntry) bool {
	for _, entry := range entries {
		if entry.Manager == FieldManager && entry.Operation == metav1.ManagedFieldsOperationApply && entry.Subresource == "" {
			return true
		}
	}
	return false
}

func decodeApply(object, configuration interface{}) bool {
	data, err := json.Marshal(object)
	if err != nil {
		return false
	}
	return json.Unmarshal(data, configuration) == nil
}

func applyEqual(current, desired interface{}, kind string) bool {
	a := canonicalApply(current, kind)
	b := canonicalApply(desired, kind)
	return reflect.DeepEqual(a, b)
}

func canonicalApply(value interface{}, kind string) map[string]interface{} {
	data, _ := json.Marshal(value)
	result := map[string]interface{}{}
	_ = json.Unmarshal(data, &result)
	if metadata, ok := result["metadata"].(map[string]interface{}); ok {
		delete(metadata, "resourceVersion")
		delete(metadata, "uid")
		delete(metadata, "creationTimestamp")
		delete(metadata, "generation")
	}
	delete(result, "status")
	if spec, ok := result["spec"].(map[string]interface{}); ok {
		if kind == "Deployment" {
			deleteIfEmpty(spec, "strategy")
			deleteIfEmpty(spec, "template")
		}
		if kind == "Service" {
			for _, field := range []string{"clusterIP", "clusterIPs", "ipFamilies", "ipFamilyPolicy", "healthCheckNodePort"} {
				delete(spec, field)
			}
			if ports, ok := spec["ports"].([]interface{}); ok {
				for _, item := range ports {
					if port, ok := item.(map[string]interface{}); ok {
						delete(port, "nodePort")
					}
				}
			}
		}
		canonicalizeLists(spec)
		if len(spec) == 0 {
			delete(result, "spec")
		}
	}
	return result
}

func deleteIfEmpty(object map[string]interface{}, field string) {
	value, ok := object[field].(map[string]interface{})
	if !ok {
		return
	}
	if metadata, ok := value["metadata"].(map[string]interface{}); ok && len(metadata) == 0 {
		delete(value, "metadata")
	}
	if spec, ok := value["spec"].(map[string]interface{}); ok && len(spec) == 0 {
		delete(value, "spec")
	}
	if len(value) == 0 {
		delete(object, field)
	}
}

func canonicalizeLists(value interface{}) {
	// Only normalize associative list shapes used by the router templates. An
	// unknown shape retains its order and may conservatively cause an apply.
	switch value := value.(type) {
	case map[string]interface{}:
		for key, child := range value {
			canonicalizeLists(child)
			if key == "resources" {
				if object, ok := child.(map[string]interface{}); ok && len(object) == 0 {
					delete(value, key)
				}
			}
		}
	case []interface{}:
		for _, child := range value {
			canonicalizeLists(child)
		}
		if len(value) > 1 {
			if _, ok := value[0].(map[string]interface{}); ok {
				known := true
				for index := range value {
					_, known = listKey(value[index])
					if !known {
						break
					}
				}
				if known {
					sort.SliceStable(value, func(i, j int) bool {
						left, _ := listKey(value[i])
						right, _ := listKey(value[j])
						return left < right
					})
				}
			}
		}
	}
}

func listKey(value interface{}) (string, bool) {
	item, _ := value.(map[string]interface{})
	for _, fields := range [][]string{{"name"}, {"uid"}, {"containerPort", "protocol"}, {"port", "protocol"}, {"mountPath"}} {
		parts := make([]interface{}, 0, len(fields))
		found := true
		for _, field := range fields {
			part, ok := item[field]
			if !ok {
				found = false
				break
			}
			parts = append(parts, part)
		}
		if found {
			data, _ := json.Marshal(parts)
			return string(data), true
		}
	}
	return "", false
}
