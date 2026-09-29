package controllerruntime

import (
	"context"
	"testing"

	fakeclient "github.com/skupperproject/skupper/internal/kube/client/fake"
	"github.com/skupperproject/skupper/internal/kube/controller"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
)

func TestControllerGatewayOwnerUsesStableWorkloadIdentity(t *testing.T) {
	for _, test := range []string{"valid", "replaced Pod", "deleting Pod", "no owner", "noncontrolling owner", "wrong kind", "missing UID"} {
		t.Run(test, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "controller-1", Namespace: "control", UID: "pod-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "controller", UID: "set-uid", Controller: ptr.To(true)}}}}
			switch test {
			case "replaced Pod":
				pod.UID = "new-pod"
			case "deleting Pod":
				now := metav1.Now()
				pod.DeletionTimestamp = &now
			case "no owner":
				pod.OwnerReferences = nil
			case "noncontrolling owner":
				pod.OwnerReferences[0].Controller = ptr.To(false)
			case "wrong kind":
				pod.OwnerReferences[0].Kind = "ReplicaSet"
			case "missing UID":
				pod.OwnerReferences[0].UID = ""
			}
			clients, err := fakeclient.NewFakeClient("control", []runtime.Object{pod}, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			owner, err := controllerOwner(context.Background(), clients, &controller.Config{Namespace: "control", PodName: pod.Name, PodUID: "pod-uid"})
			if test == "valid" {
				if err != nil || owner == nil || owner.Kind != "StatefulSet" || owner.UID != "set-uid" || owner.Name != "controller" {
					t.Fatalf("Gateway not assigned the stable StatefulSet owner: %v, %v", owner, err)
				}
			} else if err == nil || owner != nil {
				t.Fatalf("unsafe Gateway ownership accepted: %v, %v", owner, err)
			}
		})
	}
}
