package routercontrol

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

var testNow = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

func TestEnrollValidatesAudienceOwnershipCSRAndLifetime(t *testing.T) {
	client, installation, enroller := enrollmentFixture(t)
	csr := testCSR(t, pkix.Name{}, nil)
	token := testToken(testNow.Add(7 * time.Minute))

	enrollment, err := enroller.Enroll(context.Background(), token, csr)
	if err != nil {
		t.Fatal(err)
	}
	if !enrollment.NotAfter.Equal(testNow.Add(7 * time.Minute)) {
		t.Fatalf("certificate expiry = %v, want token expiry", enrollment.NotAfter)
	}
	leaf, err := x509.ParseCertificate(enrollment.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth || leaf.IsCA || leaf.Subject.String() != "" {
		t.Fatalf("issued escalated certificate: %#v", leaf)
	}
	identity, err := identityFromCertificate(leaf)
	if err != nil || identity.SiteUID != "site-uid" || identity.PodUID != "pod-uid" || identity.Installation != installation.Name {
		t.Fatalf("unexpected signed identity %#v: %v", identity, err)
	}

	client.PrependReactor("create", "tokenreviews", func(action ktesting.Action) (bool, runtime.Object, error) {
		review := action.(ktesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		if len(review.Spec.Audiences) != 1 || review.Spec.Audiences[0] != DefaultAudience {
			t.Fatalf("TokenReview audience = %v", review.Spec.Audiences)
		}
		return true, reviewedToken("another-audience"), nil
	})
	if _, err := enroller.Enroll(context.Background(), token, csr); err == nil || !strings.Contains(err.Error(), "enrollment audience") {
		t.Fatalf("wrong audience error = %v", err)
	}
}

func TestEnrollNormalizesTTLLimitedExpiryToCertificatePrecision(t *testing.T) {
	_, _, enroller := enrollmentFixture(t)
	now := testNow.Add(123456789 * time.Nanosecond)
	enroller.Now = func() time.Time { return now }
	enrollment, err := enroller.Enroll(context.Background(), testToken(now.Add(time.Hour)), testCSR(t, pkix.Name{}, nil))
	if err != nil {
		t.Fatal(err)
	}
	want := now.Add(DefaultCertTTL).UTC().Truncate(time.Second)
	leaf, err := x509.ParseCertificate(enrollment.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if !enrollment.NotAfter.Equal(want) || !leaf.NotAfter.Equal(want) {
		t.Fatalf("expiry metadata=%v leaf=%v, want %v", enrollment.NotAfter, leaf.NotAfter, want)
	}
}

func TestEnrollRejectsForgedCSRIdentityAndReplacedPod(t *testing.T) {
	client, _, enroller := enrollmentFixture(t)
	forged, _ := url.Parse("spiffe://attacker.invalid/admin")
	csr := testCSR(t, pkix.Name{CommonName: "admin"}, []*url.URL{forged})
	if _, err := enroller.Enroll(context.Background(), testToken(testNow.Add(time.Hour)), csr); err == nil || !strings.Contains(err.Error(), "must not request") {
		t.Fatalf("forged CSR error = %v", err)
	}
	pod, err := client.CoreV1().Pods("site-ns").Get(context.Background(), "router-pod", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod.UID = "replacement"
	if _, err = client.CoreV1().Pods("site-ns").Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := enroller.Enroll(context.Background(), testToken(testNow.Add(time.Hour)), testCSR(t, pkix.Name{}, nil)); err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("replaced Pod error = %v", err)
	}
}

func TestEnrollFailsClosedOnIdentityAPIError(t *testing.T) {
	client, _, enroller := enrollmentFixture(t)
	client.PrependReactor("get", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("API unavailable")
	})
	if _, err := enroller.Enroll(context.Background(), testToken(testNow.Add(time.Hour)), testCSR(t, pkix.Name{}, nil)); err == nil || !strings.Contains(err.Error(), "API unavailable") {
		t.Fatalf("API failure error = %v", err)
	}
}

func TestEnrollRechecksLeadershipAfterSlowAuthorization(t *testing.T) {
	_, _, enroller := enrollmentFixture(t)
	gate := &testGate{done: make(chan struct{})}
	authorizationStarted := make(chan struct{})
	releaseAuthorization := make(chan struct{})
	enroller.Gate = gate
	enroller.Authorize = func(context.Context, Identity) error {
		close(authorizationStarted)
		<-releaseAuthorization
		return nil
	}
	csr := testCSR(t, pkix.Name{}, nil)
	result := make(chan error, 1)
	go func() {
		_, err := enroller.Enroll(context.Background(), testToken(testNow.Add(time.Hour)), csr)
		result <- err
	}()
	<-authorizationStarted
	gate.err = ErrNotLeader
	close(gate.done)
	close(releaseAuthorization)
	if err := <-result; err == nil || !strings.Contains(err.Error(), "active leader") {
		t.Fatalf("leadership-loss enrollment error = %v", err)
	}
}

func TestEnrollRejectsDeploymentWithoutSiteControllerOwner(t *testing.T) {
	client, _, enroller := enrollmentFixture(t)
	deployment, err := client.AppsV1().Deployments("site-ns").Get(context.Background(), "skupper-router", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	deployment.OwnerReferences[0].Controller = nil
	if _, err := client.AppsV1().Deployments("site-ns").Update(context.Background(), deployment, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := enroller.Enroll(context.Background(), testToken(testNow.Add(time.Hour)), testCSR(t, pkix.Name{}, nil)); err == nil || !strings.Contains(err.Error(), "Site controller owner") {
		t.Fatalf("non-controller Site owner error = %v", err)
	}
}

func TestInstallationCAContinuity(t *testing.T) {
	client := fake.NewSimpleClientset()
	first, err := EnsureInstallation(context.Background(), client, "controller", "router-control", "controller/id", []string{"skupper-controller.controller.svc"}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EnsureInstallation(context.Background(), client, "controller", "router-control", "controller/id", []string{"skupper-controller.controller.svc"}, testNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if string(first.ClientCA.Raw) != string(second.ClientCA.Raw) || string(first.ServerCA.Raw) != string(second.ServerCA.Raw) {
		t.Fatal("installation CAs changed across loads")
	}
}

func enrollmentFixture(t *testing.T) (*fake.Clientset, *Installation, *Enroller) {
	t.Helper()
	controller := true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "router-pod", Namespace: "site-ns", UID: "pod-uid", Labels: map[string]string{GroupLabel: "skupper-router"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "router-rs", UID: "rs-uid", Controller: &controller}}}, Spec: corev1.PodSpec{ServiceAccountName: "router-sa"}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "router-rs", Namespace: "site-ns", UID: "rs-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "skupper-router", UID: "deployment-uid", Controller: &controller}}}}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "skupper-router", Namespace: "site-ns", UID: "deployment-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: siteAPIVer, Kind: "Site", Name: "west", UID: "site-uid", Controller: &controller}}}, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{GroupLabel: "skupper-router"}}}}}
	client := fake.NewSimpleClientset(pod, rs, deployment, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "router-sa", Namespace: "site-ns", UID: "sa-uid"}}, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "site-ns", UID: "namespace-uid"}})
	client.PrependReactor("create", "tokenreviews", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, reviewedToken(DefaultAudience), nil
	})
	installation, err := EnsureInstallation(context.Background(), client, "controller", "router-control", "controller/id", []string{"skupper-controller.controller.svc"}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	enroller := &Enroller{Kube: client, Installation: installation, Now: func() time.Time { return testNow }, Authorize: func(_ context.Context, identity Identity) error {
		if identity.SiteUID != types.UID("site-uid") {
			return fmt.Errorf("site is not assigned")
		}
		return nil
	}}
	return client, installation, enroller
}

func reviewedToken(audience string) *authenticationv1.TokenReview {
	return &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{Authenticated: true, Audiences: []string{audience}, User: authenticationv1.UserInfo{Username: "system:serviceaccount:site-ns:router-sa", UID: "sa-uid", Extra: map[string]authenticationv1.ExtraValue{podNameExtra: {"router-pod"}, podUIDExtra: {"pod-uid"}}}}}
}

func testCSR(t *testing.T, subject pkix.Name, uris []*url.URL) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: subject, URIs: uris}, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func testToken(expiry time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, expiry.Unix())))
	return "header." + payload + ".signature"
}
