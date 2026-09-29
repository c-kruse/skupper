//go:build integration

package controllerruntime

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	auth "github.com/skupperproject/skupper/internal/kube/routercontrol"
	protocol "github.com/skupperproject/skupper/internal/routercontrol"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
)

// TestKindEnrollment requires an explicitly selected disposable cluster with a
// running router in reconciliation-smoke and forwards of the controller Service
// at 127.0.0.1:18443/18444. It never executes a command in a router container.
// Tokens are written only to a private test directory and are never logged.
func TestKindEnrollment(t *testing.T) {
	kubeconfig := os.Getenv("SKUPPER_CONTROL_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set SKUPPER_CONTROL_TEST_KUBECONFIG for the disposable kind fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rest, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	kube, err := kubernetes.NewForConfig(rest)
	if err != nil {
		t.Fatal(err)
	}
	const namespace = "reconciliation-smoke"
	pods, err := kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "skupper.io/component=router"})
	if err != nil {
		t.Fatal(err)
	}
	var router *corev1.Pod
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp == nil && pods.Items[i].Status.Phase == corev1.PodRunning {
			router = &pods.Items[i]
			break
		}
	}
	if router == nil {
		t.Fatal("fixture has no current running router Pod")
	}
	ca, err := kube.CoreV1().ConfigMaps(namespace).Get(ctx, "skupper-controller-ca", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	caPath, tokenPath := filepath.Join(directory, "ca.crt"), filepath.Join(directory, "token")
	if err := os.WriteFile(caPath, []byte(ca.Data["ca.crt"]), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := auth.NewEnrollmentClient("https://127.0.0.1:18443", tokenPath, caPath, "skupper-controller.skupper.svc")
	if err != nil {
		t.Fatal(err)
	}
	writeToken := func(pod *corev1.Pod, audience string, bound bool) {
		t.Helper()
		request := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{Audiences: []string{audience}, ExpirationSeconds: ptr.To(int64(600))}}
		if bound {
			request.Spec.BoundObjectRef = &authenticationv1.BoundObjectReference{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID}
		}
		result, err := kube.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, pod.Spec.ServiceAccountName, request, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tokenPath, []byte(result.Status.Token), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeToken(router, auth.DefaultAudience, true)
	credential, err := client.Enroll(ctx)
	if err != nil {
		t.Fatalf("live bound router token enrollment: %v", err)
	}
	if credential.Identity.PodUID != router.UID || credential.Identity.ServiceAccount != router.Spec.ServiceAccountName || credential.Leaf.IsCA || credential.Leaf.Subject.String() != "" || len(credential.Leaf.DNSNames) != 0 || len(credential.Leaf.URIs) != 1 {
		t.Fatal("enrollment did not return the server-built identity and client-only leaf")
	}
	if remaining := time.Until(credential.Leaf.NotAfter); remaining <= 0 || remaining > 10*time.Minute {
		t.Fatalf("certificate did not respect bound token lifetime: %s", remaining)
	}
	second, err := client.Enroll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(credential.Leaf.RawSubjectPublicKeyInfo, second.Leaf.RawSubjectPublicKeyInfo) || credential.Leaf.SerialNumber.Cmp(second.Leaf.SerialNumber) == 0 {
		t.Fatal("renewal reused the in-process key or leaf")
	}
	t.Log("valid Pod-bound TokenReview enrollment, server-built identity, token-bounded expiry, and fresh-key renewal passed")

	t.Run("wrong audience is rejected after token file rotation", func(t *testing.T) {
		writeToken(router, "not-the-enrollment-audience", true)
		if _, err := client.Enroll(ctx); err == nil || !strings.Contains(err.Error(), "HTTP status 401") {
			t.Fatalf("expected wrong-audience denial after token rotation, got %v", err)
		}
	})
	t.Run("unbound token is rejected", func(t *testing.T) {
		writeToken(router, auth.DefaultAudience, false)
		if _, err := client.Enroll(ctx); err == nil || !strings.Contains(err.Error(), "HTTP status 401") {
			t.Fatalf("expected unbound token denial, got %v", err)
		}
	})
	t.Run("same ServiceAccount without workload ownership is rejected", func(t *testing.T) {
		pod, err := kube.CoreV1().Pods(namespace).Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "unauthorized-enrollment-", Namespace: namespace},
			Spec:       corev1.PodSpec{ServiceAccountName: router.Spec.ServiceAccountName, Containers: []corev1.Container{{Name: "pause", Image: "registry.k8s.io/pause:3.10"}}},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer kube.CoreV1().Pods(namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
		writeToken(pod, auth.DefaultAudience, true)
		if _, err := client.Enroll(ctx); err == nil || !strings.Contains(err.Error(), "HTTP status 401") {
			t.Fatalf("expected workload ownership denial, got %v", err)
		}
	})
	t.Run("TLS DNS mismatch is rejected", func(t *testing.T) {
		writeToken(router, auth.DefaultAudience, true)
		wrongDNS, err := auth.NewEnrollmentClient(client.URL, tokenPath, caPath, "wrong-controller.skupper.svc")
		if err != nil {
			t.Fatal(err)
		}
		var mismatch x509.HostnameError
		if _, err := wrongDNS.Enroll(ctx); !errors.As(err, &mismatch) {
			t.Fatalf("expected TLS DNS validation error, got %v", err)
		}
	})
	t.Run("mTLS certificate cannot substitute another target", func(t *testing.T) {
		roots := x509.NewCertPool()
		roots.AppendCertsFromPEM([]byte(ca.Data["ca.crt"]))
		connection, err := grpc.NewClient("127.0.0.1:18444", grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "skupper-controller.skupper.svc", Certificates: []tls.Certificate{credential.TLSCertificate()},
		})))
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		streamCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, err = protocol.OpenClientSession(streamCtx, connection, protocol.Hello{
			AdaptorInstanceID: "negative-auth-test", RouterIncarnation: "test", Target: protocol.TargetIdentity{
				NamespaceUID: string(credential.Identity.NamespaceUID), SiteUID: "another-site", RouterGroup: credential.Identity.Group,
			},
		}, protocol.DefaultLimits())
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("expected authenticated target denial, got %v", err)
		}
	})
}
