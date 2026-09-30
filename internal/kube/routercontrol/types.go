package routercontrol

import (
	"context"
	"crypto"
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

const (
	DefaultAudience = "skupper-controller-enrollment"
	DefaultCertTTL  = 15 * time.Minute
	GroupLabel      = "skupper.io/group"
)

var (
	ErrUnauthenticated          = errors.New("router adaptor is not authenticated")
	ErrUnauthorized             = errors.New("router adaptor is not authorized")
	ErrAuthorizationUnavailable = errors.New("router adaptor authorization could not be confirmed")
	ErrNotLeader                = errors.New("controller is not the active leader")
)

func AuthorizationUnavailable(err error) error {
	return fmt.Errorf("%w: %v", ErrAuthorizationUnavailable, err)
}

// Identity is derived exclusively from TokenReview and live Kubernetes objects.
// Values supplied in an enrollment request or CSR are never copied into it.
type Identity struct {
	Installation      string    `json:"installation"`
	Namespace         string    `json:"namespace"`
	NamespaceUID      types.UID `json:"namespaceUID"`
	SiteName          string    `json:"siteName"`
	SiteUID           types.UID `json:"siteUID"`
	Group             string    `json:"group"`
	PodName           string    `json:"podName"`
	PodUID            types.UID `json:"podUID"`
	ServiceAccount    string    `json:"serviceAccount"`
	ServiceAccountUID types.UID `json:"serviceAccountUID"`
}

// AssignmentAuthorizer owns the policy that is external to workload identity:
// current Site existence/UID, expected ServiceAccount, and namespace assignment.
// It must use current authoritative state and fail closed on API uncertainty.
type AssignmentAuthorizer func(context.Context, Identity) error

// LeaderGate is implemented by leadership.Fence without coupling this package to
// the controller lifecycle package.
type LeaderGate interface {
	Check() error
	Done() <-chan struct{}
}

type Enrollment struct {
	Identity    Identity
	Certificate [][]byte
	NotAfter    time.Time
}

type ClientCredential struct {
	Identity    Identity
	Certificate [][]byte
	PrivateKey  crypto.Signer
	Leaf        *x509.Certificate
}
