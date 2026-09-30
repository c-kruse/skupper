package routercontrol

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const (
	podNameExtra = "authentication.kubernetes.io/pod-name"
	podUIDExtra  = "authentication.kubernetes.io/pod-uid"
	siteAPIVer   = "skupper.io/v2alpha1"
)

type Enroller struct {
	Kube         kubernetes.Interface
	Installation *Installation
	Audience     string
	Authorize    AssignmentAuthorizer
	Gate         LeaderGate
	Now          func() time.Time
	TTL          time.Duration
}

func (e *Enroller) Enroll(ctx context.Context, token string, csrDER []byte) (*Enrollment, error) {
	if err := e.checkGate(); err != nil {
		return nil, err
	}
	if e.Kube == nil || e.Installation == nil || e.Authorize == nil {
		return nil, fmt.Errorf("router-control enroller is not configured")
	}
	audience := e.Audience
	if audience == "" {
		audience = DefaultAudience
	}
	review, err := e.Kube.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: []string{audience}}}, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("TokenReview failed: %w", err)
	}
	if !review.Status.Authenticated || !contains(review.Status.Audiences, audience) {
		return nil, fmt.Errorf("%w: token was not authenticated for the enrollment audience", ErrUnauthenticated)
	}
	namespace, serviceAccount, err := serviceAccountName(review.Status.User.Username)
	if err != nil {
		return nil, err
	}
	podName, err := oneExtra(review.Status.User.Extra, podNameExtra)
	if err != nil {
		return nil, err
	}
	podUID, err := oneExtra(review.Status.User.Extra, podUIDExtra)
	if err != nil {
		return nil, err
	}
	identity, err := e.liveIdentity(ctx, namespace, serviceAccount, types.UID(review.Status.User.UID), podName, types.UID(podUID))
	if err != nil {
		return nil, err
	}
	if err := e.Authorize(ctx, identity); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	if err := e.checkGate(); err != nil {
		return nil, err
	}
	csr, err := validateCSR(csrDER)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if e.Now != nil {
		now = e.Now()
	}
	ttl := e.TTL
	if ttl <= 0 || ttl > DefaultCertTTL {
		ttl = DefaultCertTTL
	}
	// X.509 DER encodes certificate validity at whole-second precision. Use the
	// signed precision for both the leaf and response metadata so clients can
	// compare them exactly without rejecting a legitimate TTL-bounded issuance.
	notAfter := minTime(now.Add(ttl), e.Installation.ClientCA.NotAfter).UTC().Truncate(time.Second)
	if !notAfter.After(now.Add(time.Minute)) {
		return nil, fmt.Errorf("client issuer expires too soon")
	}
	uri, err := identityURI(identity)
	if err != nil {
		return nil, err
	}
	serialNumber, err := serial()
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial: %w", err)
	}
	template := &x509.Certificate{SerialNumber: serialNumber, NotBefore: now.Add(-30 * time.Second), NotAfter: notAfter, URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, e.Installation.ClientCA, csr.PublicKey, e.Installation.ClientCAKey)
	if err != nil {
		return nil, fmt.Errorf("issue router-control client certificate: %w", err)
	}
	if err := e.checkGate(); err != nil {
		return nil, err
	}
	return &Enrollment{Identity: identity, Certificate: [][]byte{der, e.Installation.ClientCA.Raw}, NotAfter: notAfter}, nil
}

func (e *Enroller) checkGate() error {
	if e.Gate != nil {
		if err := e.Gate.Check(); err != nil {
			return fmt.Errorf("%w: %v", ErrNotLeader, err)
		}
	}
	return nil
}

func (e *Enroller) liveIdentity(ctx context.Context, namespace, serviceAccount string, serviceAccountUID types.UID, podName string, podUID types.UID) (Identity, error) {
	pod, err := e.Kube.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return Identity{}, authorizationReadFailure("bound Pod", err)
	}
	if pod.UID != podUID || pod.DeletionTimestamp != nil || pod.Spec.ServiceAccountName != serviceAccount {
		return Identity{}, fmt.Errorf("%w: bound Pod was replaced, deleted, or uses another ServiceAccount", ErrUnauthenticated)
	}
	sa, err := e.Kube.CoreV1().ServiceAccounts(namespace).Get(ctx, serviceAccount, metav1.GetOptions{})
	if err != nil {
		return Identity{}, authorizationReadFailure("bound ServiceAccount", err)
	}
	if sa.UID != serviceAccountUID || sa.DeletionTimestamp != nil {
		return Identity{}, fmt.Errorf("%w: bound ServiceAccount was replaced or deleted", ErrUnauthenticated)
	}
	ns, err := e.Kube.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		return Identity{}, authorizationReadFailure("namespace identity", err)
	}
	if ns.DeletionTimestamp != nil {
		return Identity{}, fmt.Errorf("%w: namespace is being deleted", ErrUnauthorized)
	}
	rsOwner, err := controllerOwner(pod.OwnerReferences, "apps/v1", "ReplicaSet")
	if err != nil {
		return Identity{}, fmt.Errorf("Pod ownership: %w", err)
	}
	rs, err := e.Kube.AppsV1().ReplicaSets(namespace).Get(ctx, rsOwner.Name, metav1.GetOptions{})
	if err != nil {
		return Identity{}, authorizationReadFailure("owning ReplicaSet", err)
	}
	if rs.UID != rsOwner.UID || rs.DeletionTimestamp != nil {
		return Identity{}, fmt.Errorf("%w: owning ReplicaSet was replaced or deleted", ErrUnauthorized)
	}
	deploymentOwner, err := controllerOwner(rs.OwnerReferences, "apps/v1", "Deployment")
	if err != nil {
		return Identity{}, fmt.Errorf("ReplicaSet ownership: %w", err)
	}
	deployment, err := e.Kube.AppsV1().Deployments(namespace).Get(ctx, deploymentOwner.Name, metav1.GetOptions{})
	if err != nil {
		return Identity{}, authorizationReadFailure("owning Deployment", err)
	}
	if deployment.UID != deploymentOwner.UID || deployment.DeletionTimestamp != nil {
		return Identity{}, fmt.Errorf("%w: owning Deployment was replaced or deleted", ErrUnauthorized)
	}
	siteOwner, err := controllerOwner(deployment.OwnerReferences, siteAPIVer, "Site")
	if err != nil {
		return Identity{}, fmt.Errorf("Deployment ownership: %w", err)
	}
	group := pod.Labels[GroupLabel]
	if group == "" || group != deployment.Spec.Template.Labels[GroupLabel] || group != deployment.Name {
		return Identity{}, fmt.Errorf("%w: router group metadata is missing or inconsistent", ErrUnauthorized)
	}
	return Identity{Installation: e.Installation.Name, Namespace: namespace, NamespaceUID: ns.UID, SiteName: siteOwner.Name, SiteUID: siteOwner.UID, Group: group, PodName: pod.Name, PodUID: pod.UID, ServiceAccount: serviceAccount, ServiceAccountUID: sa.UID}, nil
}

func authorizationReadFailure(resource string, err error) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: %s no longer exists", ErrUnauthorized, resource)
	}
	return AuthorizationUnavailable(fmt.Errorf("read %s: %w", resource, err))
}

func validateCSR(der []byte) (*x509.CertificateRequest, error) {
	if len(der) == 0 || len(der) > 16*1024 {
		return nil, fmt.Errorf("invalid CSR size")
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		return nil, fmt.Errorf("parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("verify CSR signature: %w", err)
	}
	if csr.Subject.String() != "" || len(csr.Extensions) != 0 || len(csr.ExtraExtensions) != 0 || len(csr.DNSNames) != 0 || len(csr.EmailAddresses) != 0 || len(csr.IPAddresses) != 0 || len(csr.URIs) != 0 {
		return nil, fmt.Errorf("CSR must not request a subject, SAN, extensions, or usages")
	}
	switch key := csr.PublicKey.(type) {
	case *rsa.PublicKey:
		if key.N.BitLen() < 2048 {
			return nil, fmt.Errorf("CSR RSA key is smaller than 2048 bits")
		}
	case *ecdsa.PublicKey:
		if key.Curve != elliptic.P256() && key.Curve != elliptic.P384() {
			return nil, fmt.Errorf("CSR ECDSA curve is not supported")
		}
	case ed25519.PublicKey:
	default:
		return nil, fmt.Errorf("CSR public key type is not supported")
	}
	return csr, nil
}

func serviceAccountName(username string) (string, string, error) {
	parts := strings.Split(username, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" || parts[2] == "" || parts[3] == "" {
		return "", "", fmt.Errorf("%w: TokenReview user is not a ServiceAccount", ErrUnauthenticated)
	}
	return parts[2], parts[3], nil
}

func oneExtra(extras map[string]authenticationv1.ExtraValue, name string) (string, error) {
	values := extras[name]
	if len(values) != 1 || values[0] == "" {
		return "", fmt.Errorf("%w: TokenReview lacks a unique %s", ErrUnauthenticated, name)
	}
	return values[0], nil
}

func controllerOwner(refs []metav1.OwnerReference, apiVersion, kind string) (metav1.OwnerReference, error) {
	var found *metav1.OwnerReference
	for i := range refs {
		ref := &refs[i]
		if ref.Controller != nil && *ref.Controller {
			if found != nil {
				return metav1.OwnerReference{}, fmt.Errorf("multiple controller owners")
			}
			found = ref
		}
	}
	if found == nil || found.APIVersion != apiVersion || found.Kind != kind || found.Name == "" || found.UID == "" {
		return metav1.OwnerReference{}, fmt.Errorf("expected %s %s controller owner", apiVersion, kind)
	}
	return *found, nil
}

func identityURI(identity Identity) (*url.URL, error) {
	data, err := json.Marshal(identity)
	if err != nil {
		return nil, err
	}
	return &url.URL{Scheme: "skupper-router-control", Host: "identity", Path: "/" + base64.RawURLEncoding.EncodeToString(data)}, nil
}

func identityFromCertificate(cert *x509.Certificate) (Identity, error) {
	if len(cert.URIs) != 1 || cert.URIs[0].Scheme != "skupper-router-control" || cert.URIs[0].Host != "identity" {
		return Identity{}, fmt.Errorf("%w: certificate has no router-control identity", ErrUnauthenticated)
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(cert.URIs[0].Path, "/"))
	if err != nil {
		return Identity{}, fmt.Errorf("%w: malformed certificate identity", ErrUnauthenticated)
	}
	var identity Identity
	if err := json.Unmarshal(data, &identity); err != nil {
		return Identity{}, fmt.Errorf("%w: malformed certificate identity", ErrUnauthenticated)
	}
	if identity.Installation == "" || identity.Namespace == "" || identity.NamespaceUID == "" || identity.SiteUID == "" || identity.Group == "" || identity.PodUID == "" || identity.ServiceAccountUID == "" {
		return Identity{}, fmt.Errorf("%w: incomplete certificate identity", ErrUnauthenticated)
	}
	return identity, nil
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
