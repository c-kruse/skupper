package main

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

func TestReplacement(t *testing.T) {
	now := time.Now()
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "controller", UID: "workload-uid", Generation: 2},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To(int32(2)), PodManagementPolicy: appsv1.ParallelPodManagement,
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType},
			Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "controller"}},
		},
		Status: appsv1.StatefulSetStatus{ObservedGeneration: 2, UpdateRevision: "revision-new"},
	}
	pod := func(name, uid string, ready bool) corev1.Pod {
		status := corev1.ConditionFalse
		if ready {
			status = corev1.ConditionTrue
		}
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, UID: types.UID(uid),
				Labels:          map[string]string{"app": "controller", appsv1.StatefulSetRevisionLabel: "revision-old"},
				OwnerReferences: []metav1.OwnerReference{{UID: sts.UID, Controller: ptr.To(true)}},
			},
			Status: corev1.PodStatus{
				Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
				ContainerStatuses: []corev1.ContainerStatus{{Name: "controller", Started: ptr.To(true), State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
			},
		}
	}
	leader := pod("controller-1", "leader-uid", true)
	standby := pod("controller-0", "standby-uid", false)
	lease := &coordinationv1.Lease{Spec: coordinationv1.LeaseSpec{
		HolderIdentity: ptr.To("controller/controller-1/leader-uid"),
		RenewTime:      &metav1.MicroTime{Time: now}, LeaseDurationSeconds: ptr.To(int32(30)),
	}}
	tests := []struct {
		name   string
		mutate func(*corev1.Pod, *corev1.Pod, *coordinationv1.Lease)
		want   string
		wait   bool
	}{
		{name: "replace standby first", want: "controller-0"},
		{name: "handoff to started upgraded standby", want: "controller-1", mutate: func(l, s *corev1.Pod, _ *coordinationv1.Lease) {
			s.Labels[appsv1.StatefulSetRevisionLabel] = "revision-new"
		}},
		{name: "unstarted replacement cannot take over", wait: true, mutate: func(l, s *corev1.Pod, _ *coordinationv1.Lease) {
			s.Labels[appsv1.StatefulSetRevisionLabel] = "revision-new"
			s.Status.ContainerStatuses[0].Started = ptr.To(false)
		}},
		{name: "complete with only leader ready", mutate: func(l, s *corev1.Pod, _ *coordinationv1.Lease) {
			l.Labels[appsv1.StatefulSetRevisionLabel] = "revision-new"
			s.Labels[appsv1.StatefulSetRevisionLabel] = "revision-new"
		}},
		{name: "replaced pod cannot inherit old lease", wait: true, mutate: func(l, s *corev1.Pod, _ *coordinationv1.Lease) {
			l.UID = "replacement-uid"
		}},
		{name: "different logical controller cannot own lease", wait: true, mutate: func(l, s *corev1.Pod, lease *coordinationv1.Lease) {
			lease.Spec.HolderIdentity = ptr.To("other/controller-1/leader-uid")
		}},
		{name: "stale lease prevents deletion", wait: true, mutate: func(l, s *corev1.Pod, lease *coordinationv1.Lease) {
			lease.Spec.RenewTime.Time = now.Add(-31 * time.Second)
		}},
		{name: "two ready replicas prevent deletion", wait: true, mutate: func(l, s *corev1.Pod, _ *coordinationv1.Lease) {
			s.Status.Conditions[0].Status = corev1.ConditionTrue
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			l, s, lease := leader.DeepCopy(), standby.DeepCopy(), lease.DeepCopy()
			if test.mutate != nil {
				test.mutate(l, s, lease)
			}
			got, wait, err := replacement(sts, []corev1.Pod{*l, *s}, lease, now)
			if err != nil || (wait != "") != test.wait {
				t.Fatalf("wait=%q err=%v", wait, err)
			}
			name := ""
			if got != nil {
				name = got.Name
			}
			if name != test.want {
				t.Fatalf("replacement=%q, want %q", name, test.want)
			}
		})
	}
}
