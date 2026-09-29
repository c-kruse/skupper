//go:build integration

package controllerruntime

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/skupperproject/skupper/internal/certs"
	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	v2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/clientcmd"
)

// TestKindLinkedHA exercises a dynamic mutual-TLS edge Link and sends traffic
// through each HA router separately. The fixture's backend exists only in the
// edge Site, and the Link connects only to the primary HA router, so requests
// to the secondary must traverse its inter-router connection. It never execs
// into router containers and does not infer success from Pod readiness.
func TestKindLinkedHA(t *testing.T) {
	kubeconfig := os.Getenv("SKUPPER_CONTROL_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set SKUPPER_CONTROL_TEST_KUBECONFIG for the disposable kind fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	rest, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	clients, err := internalclient.NewClientFromRestConfig(rest, "reconciliation-smoke")
	if err != nil {
		t.Fatal(err)
	}
	kube, api := clients.GetKubeClient(), clients.GetSkupperClient().SkupperV2alpha1()
	const edgeNamespace, haNamespace = "reconciliation-smoke", "reconciliation-ha"
	name := "ha-live-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	routers, err := kube.CoreV1().Pods(haNamespace).List(ctx, metav1.ListOptions{LabelSelector: "skupper.io/component=router"})
	if err != nil || len(routers.Items) != 2 {
		t.Fatalf("need two stable HA fixture routers: %v", err)
	}
	groups := map[string]bool{}
	for _, router := range routers.Items {
		groups[router.Labels["skupper.io/group"]] = true
		if router.DeletionTimestamp != nil || router.Status.PodIP == "" {
			t.Fatal("HA fixture is not stable")
		}
	}
	if !groups["skupper-router"] || !groups["skupper-router-2"] {
		t.Fatal("fixture does not contain both HA groups")
	}
	ca, err := kube.CoreV1().Secrets(haNamespace).Get(ctx, "skupper-site-ca", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	secret, err := certs.GenerateSecret(name, name, nil, time.Hour, ca)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kube.CoreV1().Secrets(edgeNamespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	defer kube.CoreV1().Secrets(edgeNamespace).Delete(context.Background(), name, metav1.DeleteOptions{})
	links := api.Links(edgeNamespace)
	// Start with a refused port so an implementation that merely reports
	// successful apply as Operational cannot pass the test.
	if _, err := links.Create(ctx, &v2alpha1.Link{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v2alpha1.LinkSpec{TlsCredentials: name, Endpoints: []v2alpha1.Endpoint{{Name: "edge", Host: "skupper-router." + haNamespace, Port: "45672"}}}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	defer links.Delete(context.Background(), name, metav1.DeleteOptions{})
	waitLink := func(kind string, expected metav1.ConditionStatus) {
		t.Helper()
		var latest v2alpha1.LinkStatus
		err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
			current, err := links.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			latest = current.Status
			for _, condition := range latest.Conditions {
				if condition.Type == kind && condition.Status == expected && condition.ObservedGeneration == current.Generation {
					return true, nil
				}
			}
			return false, nil
		})
		if err != nil {
			t.Fatalf("Link did not become %s=%s: %v; status: %+v", kind, expected, err, latest)
		}
	}
	waitLink(v2alpha1.CONDITION_TYPE_CONFIGURED, metav1.ConditionTrue)
	waitLink(v2alpha1.CONDITION_TYPE_OPERATIONAL, metav1.ConditionFalse)
	link, err := links.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	link.Spec.Endpoints[0].Port = "45671"
	if _, err := links.Update(ctx, link, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitLink(v2alpha1.CONDITION_TYPE_READY, metav1.ConditionTrue)
	listeners := api.Listeners(haNamespace)
	if _, err := listeners.Create(ctx, &v2alpha1.Listener{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v2alpha1.ListenerSpec{RoutingKey: "smoke", Host: name, Port: 8080}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	defer listeners.Delete(context.Background(), name, metav1.DeleteOptions{})
	waitListener := func(kind string, expected metav1.ConditionStatus) {
		t.Helper()
		var latest v2alpha1.ListenerStatus
		err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
			current, err := listeners.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			latest = current.Status
			for _, condition := range latest.Conditions {
				if condition.Type == kind && condition.Status == expected && condition.ObservedGeneration == current.Generation {
					return true, nil
				}
			}
			return false, nil
		})
		if err != nil {
			t.Fatalf("HA Listener did not become %s=%s: %v; status: %+v", kind, expected, err, latest)
		}
	}
	waitListener(v2alpha1.CONDITION_TYPE_READY, metav1.ConditionTrue)
	service, err := kube.CoreV1().Services(haNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil || len(service.Spec.Ports) != 1 || service.Spec.Ports[0].TargetPort.IntVal == 0 {
		t.Fatalf("Listener did not allocate a concrete router port: %v", err)
	}
	for _, router := range routers.Items {
		url := fmt.Sprintf("http://%s:%d", router.Status.PodIP, service.Spec.Ports[0].TargetPort.IntVal)
		pods := kube.CoreV1().Pods(haNamespace)
		client, err := pods.Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{GenerateName: name + "-"}, Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "client", Image: "curlimages/curl:8.12.1", Args: []string{"-fsS", "--connect-timeout", "3", "--max-time", "10", url}}}}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer pods.Delete(context.Background(), client.Name, metav1.DeleteOptions{})
		if err := wait.PollUntilContextTimeout(ctx, time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
			current, err := pods.Get(ctx, client.Name, metav1.GetOptions{})
			return err == nil && (current.Status.Phase == corev1.PodSucceeded || current.Status.Phase == corev1.PodFailed), err
		}); err != nil {
			t.Fatal(err)
		}
		stream, err := pods.GetLogs(client.Name, &corev1.PodLogOptions{}).Stream(ctx)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(stream)
		stream.Close()
		if err != nil || strings.TrimSpace(string(body)) != "router-control-smoke" {
			t.Fatalf("traffic through HA group %s returned %q, %v", router.Labels["skupper.io/group"], body, err)
		}
	}
	if err := links.Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitListener(v2alpha1.CONDITION_TYPE_MATCHED, metav1.ConditionFalse)
	for _, original := range routers.Items {
		current, err := kube.CoreV1().Pods(haNamespace).Get(ctx, original.Name, metav1.GetOptions{})
		if err != nil || current.UID != original.UID {
			t.Fatalf("HA router replaced during dynamic link changes: %v", err)
		}
		for _, before := range original.Status.ContainerStatuses {
			for _, after := range current.Status.ContainerStatuses {
				if before.Name == after.Name && before.RestartCount != after.RestartCount {
					t.Fatalf("HA container %s restarted", after.Name)
				}
			}
		}
	}
	t.Log("Link Configured/Operational distinguished on a refused port; mutual TLS connected after update; exact payload passed through each HA group; deleting Link cleared Matched; HA router identities/restarts unchanged")
}
