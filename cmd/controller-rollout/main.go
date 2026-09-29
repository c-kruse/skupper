// controller-rollout replaces a two-replica, leader-only-ready controller after
// its StatefulSet template has been updated. It deliberately does not wait for
// both replicas to become Ready.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	var namespace, kubeconfig, name string
	var timeout time.Duration
	flag.StringVar(&namespace, "namespace", "", "Controller installation namespace")
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Kubeconfig (defaults to normal client-go loading rules)")
	flag.StringVar(&name, "name", "skupper-controller", "Controller StatefulSet and Lease name")
	flag.DurationVar(&timeout, "timeout", 5*time.Minute, "Maximum time to wait for the checked rollout")
	flag.Parse()
	loading := clientcmd.NewDefaultClientConfigLoadingRules()
	loading.ExplicitPath = kubeconfig
	config := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loading, &clientcmd.ConfigOverrides{})
	err := func() error {
		if namespace == "" {
			var err error
			namespace, _, err = config.Namespace()
			if err != nil {
				return err
			}
		}
		rest, err := config.ClientConfig()
		if err != nil {
			return err
		}
		client, err := kubernetes.NewForConfig(rest)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		return rollout(ctx, client, namespace, name)
	}()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func rollout(ctx context.Context, client kubernetes.Interface, namespace, name string) error {
	var lastWait string
	for {
		sts, err := client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		selector, err := metav1.LabelSelectorAsSelector(sts.Spec.Selector)
		if err != nil || selector.Empty() {
			return fmt.Errorf("controller StatefulSet must have a nonempty selector")
		}
		pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
		if err != nil {
			return err
		}
		lease, err := client.CoordinationV1().Leases(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		target, waitReason, err := replacement(sts, pods.Items, lease, time.Now())
		if err != nil {
			return err
		}
		if target == nil && waitReason == "" {
			fmt.Println("Controller rollout complete: both replicas updated and started; one Ready lease holder")
			return nil
		}
		if target != nil {
			// Informer-style multi-object reads are not a transaction. Revalidate
			// leadership and template, and condition deletion on the exact Pod.
			currentLease, err := client.CoordinationV1().Leases(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			currentSTS, err := client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if currentSTS.UID != sts.UID || currentSTS.Generation != sts.Generation || !sameHolder(lease, currentLease) {
				return fmt.Errorf("leadership or StatefulSet changed before replacement; rerun after it stabilizes")
			}
			fmt.Printf("Replacing %s/%s (UID %s)\n", namespace, target.Name, target.UID)
			err = client.CoreV1().Pods(namespace).Delete(ctx, target.Name, metav1.DeleteOptions{
				Preconditions: &metav1.Preconditions{UID: &target.UID, ResourceVersion: &target.ResourceVersion},
			})
			if err != nil && !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
				return err
			}
			waitReason = "waiting for replacement and leadership to stabilize"
		}
		if waitReason != lastWait {
			fmt.Println(waitReason)
			lastWait = waitReason
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %w", lastWait, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

func replacement(sts *appsv1.StatefulSet, pods []corev1.Pod, lease *coordinationv1.Lease, now time.Time) (*corev1.Pod, string, error) {
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 2 || sts.Spec.PodManagementPolicy != appsv1.ParallelPodManagement || sts.Spec.UpdateStrategy.Type != appsv1.OnDeleteStatefulSetStrategyType {
		return nil, "", fmt.Errorf("checked rollout requires a two-replica Parallel/OnDelete StatefulSet")
	}
	if sts.Status.ObservedGeneration < sts.Generation || sts.Status.UpdateRevision == "" {
		return nil, "waiting for StatefulSet to observe its new template", nil
	}
	if lease.Spec.HolderIdentity == nil || lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil || !now.Before(lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds)*time.Second)) {
		return nil, "waiting for a current lease holder", nil
	}
	var leader, standby *corev1.Pod
	owned := 0
	selector, err := metav1.LabelSelectorAsSelector(sts.Spec.Selector)
	if err != nil {
		return nil, "", err
	}
	for i := range pods {
		pod := &pods[i]
		owner := metav1.GetControllerOf(pod)
		if owner == nil || owner.UID != sts.UID || !selector.Matches(labels.Set(pod.Labels)) {
			continue
		}
		owned++
		if pod.DeletionTimestamp != nil {
			return nil, "waiting for terminating controller Pod", nil
		}
		isLeader := strings.HasPrefix(*lease.Spec.HolderIdentity, pod.Name+"/"+string(pod.UID)+"/")
		ready := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if ready != isLeader {
			return nil, "waiting for readiness to agree with the Lease", nil
		}
		if isLeader {
			leader = pod
		} else {
			standby = pod
		}
	}
	if owned != 2 || leader == nil || standby == nil {
		return nil, "waiting for one leader and one standby owned by the StatefulSet", nil
	}
	if standby.Labels[appsv1.StatefulSetRevisionLabel] != sts.Status.UpdateRevision {
		return standby, "", nil
	}
	started := false
	for _, status := range standby.Status.ContainerStatuses {
		if status.Name == "controller" && status.Started != nil && *status.Started && status.State.Running != nil {
			started = true
		}
	}
	if !started {
		return nil, "waiting for upgraded standby startup/cache synchronization", nil
	}
	if leader.Labels[appsv1.StatefulSetRevisionLabel] != sts.Status.UpdateRevision {
		return leader, "", nil
	}
	return nil, "", nil
}

func sameHolder(a, b *coordinationv1.Lease) bool {
	return a.UID == b.UID && a.Spec.HolderIdentity != nil && b.Spec.HolderIdentity != nil && *a.Spec.HolderIdentity == *b.Spec.HolderIdentity
}
