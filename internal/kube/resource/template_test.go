package resource

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	applyappsv1 "k8s.io/client-go/applyconfigurations/apps/v1"
)

func TestDeploymentApplyEqualUsesOwnedFieldsAndDetectsRemoval(t *testing.T) {
	controller := true
	replicas := int32(1)
	current := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            "skupper-router",
			Namespace:       "site-ns",
			UID:             "deployment-uid",
			ResourceVersion: "7",
			Annotations:     map[string]string{"admission.example/injected": "true"},
			Labels:          map[string]string{"application": "skupper-router"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "skupper.io/v2alpha1", Kind: "Site", Name: "site", UID: "site-uid", Controller: &controller}},
			ManagedFields:   []metav1.ManagedFieldsEntry{{Manager: FieldManager, Operation: metav1.ManagedFieldsOperationApply, APIVersion: "apps/v1", FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:metadata":{"f:labels":{".":{},"f:application":{}},"f:ownerReferences":{".":{},"k:{\"uid\":\"site-uid\"}":{}}},"f:spec":{"f:replicas":{}}}`)}}},
		},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas},
	}
	desired := current.DeepCopy()
	desired.UID, desired.ResourceVersion, desired.ManagedFields = "", "", nil
	delete(desired.Annotations, "admission.example/injected")
	if !DeploymentApplyEqual(current, desired) {
		owned, _ := applyappsv1.ExtractDeployment(current, FieldManager)
		wanted := &applyappsv1.DeploymentApplyConfiguration{}
		decodeApply(desired, wanted)
		t.Fatalf("unowned admission metadata made an owned-field comparison unequal\nowned=%#v\nwanted=%#v", canonicalApply(owned, "Deployment"), canonicalApply(wanted, "Deployment"))
	}
	unmigrated := current.DeepCopy()
	unmigrated.ManagedFields = append(unmigrated.ManagedFields, metav1.ManagedFieldsEntry{Manager: FieldManager, Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "apps/v1"})
	if DeploymentApplyEqual(unmigrated, desired) {
		t.Fatal("an Apply entry hid creation-time ownership requiring migration")
	}
	unmigrated.ManagedFields[1].Subresource = "status"
	if !DeploymentApplyEqual(unmigrated, desired) {
		t.Fatal("status ownership triggered main-resource migration")
	}
	driftedMetadata := current.DeepCopy()
	driftedMetadata.Labels["application"] = "external-value"
	if DeploymentApplyEqual(driftedMetadata, desired) {
		t.Fatal("owned metadata drift was missed")
	}
	desired.Spec.Replicas = nil
	if DeploymentApplyEqual(current, desired) {
		t.Fatal("removal of a formerly-owned field was missed")
	}
	changed := int32(2)
	desired.Spec.Replicas = &changed
	if DeploymentApplyEqual(current, desired) {
		t.Fatal("owned field drift was missed")
	}
}

func TestDeploymentWithoutManagedFieldsConservativelyReapplies(t *testing.T) {
	replicas := int32(1)
	current := &appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: metav1.ObjectMeta{Name: "router", Namespace: "site"}, Spec: appsv1.DeploymentSpec{Replicas: &replicas}}
	if DeploymentApplyEqual(current, current.DeepCopy()) {
		t.Fatal("object without ownership evidence was treated as converged")
	}
}

func TestDeploymentEmptyDirPresenceIsComparedEvenWithoutOwnedLeafFields(t *testing.T) {
	current := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: "router", Namespace: "site", ManagedFields: []metav1.ManagedFieldsEntry{{Manager: FieldManager, Operation: metav1.ManagedFieldsOperationApply, APIVersion: "apps/v1", FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:template":{"f:spec":{"f:volumes":{"k:{\"name\":\"certs\"}":{".":{},"f:name":{},"f:emptyDir":{}}}}}}}`)}}}},
		Spec:       appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "certs", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}}}},
	}
	desired := current.DeepCopy()
	desired.ManagedFields = nil
	if !DeploymentApplyEqual(current, desired) {
		t.Fatal("emptyDir: {} never converged after SSA")
	}
	changedSource := current.DeepCopy()
	changedSource.Spec.Template.Spec.Volumes[0].EmptyDir = nil
	changedSource.Spec.Template.Spec.Volumes[0].Secret = &corev1.SecretVolumeSource{SecretName: "other"}
	if DeploymentApplyEqual(changedSource, desired) {
		t.Fatal("different volume source was treated as equivalent to emptyDir")
	}
	desired.Spec.Template.Spec.Volumes[0].EmptyDir.Medium = corev1.StorageMediumMemory
	if DeploymentApplyEqual(current, desired) {
		t.Fatal("change to explicit emptyDir settings was ignored")
	}
}
