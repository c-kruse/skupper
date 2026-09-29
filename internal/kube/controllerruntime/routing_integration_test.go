//go:build integration

package controllerruntime

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	v2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	skupperclient "github.com/skupperproject/skupper/pkg/generated/client/clientset/versioned"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// TestKindDynamicRouting uses the same explicitly selected disposable fixture
// as TestKindEnrollment. Requests run in client Pods, never router containers.
func TestKindDynamicRouting(t *testing.T) {
	kubeconfig := os.Getenv("SKUPPER_CONTROL_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set SKUPPER_CONTROL_TEST_KUBECONFIG for the disposable kind fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	rest, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	kube, err := kubernetes.NewForConfig(rest)
	if err != nil {
		t.Fatal(err)
	}
	skupper, err := skupperclient.NewForConfig(rest)
	if err != nil {
		t.Fatal(err)
	}
	const namespace = "reconciliation-smoke"
	pods := kube.CoreV1().Pods(namespace)
	routers, err := pods.List(ctx, metav1.ListOptions{LabelSelector: "skupper.io/component=router"})
	if err != nil || len(routers.Items) != 1 {
		t.Fatalf("need one stable fixture router: %v", err)
	}
	router := routers.Items[0]
	backends, err := pods.List(ctx, metav1.ListOptions{LabelSelector: "app=backend"})
	if err != nil || len(backends.Items) != 1 || backends.Items[0].Status.PodIP == "" {
		t.Fatalf("need one fixture backend with an IP: %v", err)
	}
	name := "live-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	connectors := skupper.SkupperV2alpha1().Connectors(namespace)
	listeners := skupper.SkupperV2alpha1().Listeners(namespace)
	connector, err := connectors.Create(ctx, &v2alpha1.Connector{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v2alpha1.ConnectorSpec{RoutingKey: name, Host: backends.Items[0].Status.PodIP, Port: 8080}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer connectors.Delete(context.Background(), name, metav1.DeleteOptions{})
	if _, err := listeners.Create(ctx, &v2alpha1.Listener{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v2alpha1.ListenerSpec{RoutingKey: name, Host: name, Port: 8080}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	defer listeners.Delete(context.Background(), name, metav1.DeleteOptions{})
	waitCondition := func(kind string, expected metav1.ConditionStatus) {
		t.Helper()
		var latest *v2alpha1.Listener
		err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
			current, err := listeners.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			latest = current
			for _, condition := range latest.Status.Conditions {
				if condition.Type == kind && condition.Status == expected && condition.ObservedGeneration == latest.Generation {
					return true, nil
				}
			}
			return false, nil
		})
		if err != nil {
			t.Fatalf("Listener %s did not become %s=%s: %v; last object: %+v", name, kind, expected, err, latest)
		}
	}
	waitCondition(v2alpha1.CONDITION_TYPE_READY, metav1.ConditionTrue)
	currentConnector, err := connectors.Get(ctx, name, metav1.GetOptions{})
	if err != nil || currentConnector.Status.StatusType != v2alpha1.StatusReady || currentConnector.Status.HasMatchingListener {
		t.Fatalf("host Connector not configured independently of retired matching: %+v, %v", currentConnector, err)
	}
	client, err := pods.Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{GenerateName: name + "-"}, Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "client", Image: "curlimages/curl:8.12.1", Args: []string{"-fsS", "--connect-timeout", "3", "--max-time", "10", "http://" + name + ":8080"}}}}}, metav1.CreateOptions{})
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
		t.Fatalf("dynamic route returned %q, %v", body, err)
	}
	connector, err = connectors.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	connector.Spec.RoutingKey = name + "-unmatched"
	if _, err := connectors.Update(ctx, connector, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitCondition(v2alpha1.CONDITION_TYPE_MATCHED, metav1.ConditionFalse)
	connector, err = connectors.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	connector.Spec.RoutingKey = name
	if _, err := connectors.Update(ctx, connector, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitCondition(v2alpha1.CONDITION_TYPE_READY, metav1.ConditionTrue)
	if err := listeners.Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := wait.PollUntilContextTimeout(ctx, time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		_, err := kube.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}); err != nil {
		t.Fatalf("managed Service did not retire: %v", err)
	}
	currentRouter, err := pods.Get(ctx, router.Name, metav1.GetOptions{})
	if err != nil || currentRouter.UID != router.UID {
		t.Fatalf("router was replaced during dynamic routing: %v", err)
	}
	for i, container := range currentRouter.Status.ContainerStatuses {
		if container.RestartCount != router.Status.ContainerStatuses[i].RestartCount {
			t.Fatalf("container %s restarted during dynamic routing", container.Name)
		}
	}
	t.Log("dynamic host Connector + Listener applied; payload verified; exact local matching disappeared/recovered; Service retired; router UID and restart counts unchanged")
}
