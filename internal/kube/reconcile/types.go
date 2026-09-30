// Package reconcile implements namespace-scoped snapshot, derivation, planning,
// and execution.  It deliberately has no Kubernetes clients: acquisition and
// effects are supplied by the controller at the package boundary.
package reconcile

import (
	"context"
	"fmt"
	"time"

	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/skupperproject/skupper/internal/routercontrol"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

type NamespaceIdentity struct {
	Name string
	UID  types.UID
}

type Assignment struct {
	Controller        string
	ControllerVersion string
	Controlled        bool
}

// Completeness distinguishes absence proved by a complete query from absence of
// evidence. Unknown and Partial observations must never authorize retirement.
type Completeness uint8

const (
	Unknown Completeness = iota
	Partial
	Complete
)

type RouterTarget = routercontrol.TargetIdentity

type Observation struct {
	Key            routercontrol.SessionKey
	SessionID      string
	AcceptedDigest routercontrol.Digest
	Application    *routercontrol.ApplicationReport
	Scopes         map[string]ObservationScope
}

type ObservationScope struct {
	Fresh    bool
	Snapshot routercontrol.ObservationSnapshot
}

func (o Observation) KnownEmpty(scope string) bool {
	value, ok := o.Scopes[scope]
	if !ok || !value.Fresh || value.Snapshot.Knowledge != routercontrol.KnowledgeComplete {
		return false
	}
	return len(value.Snapshot.Resources) == 0 && len(value.Snapshot.Addresses) == 0
}

// Snapshot is an immutable-by-contract deep copy of all inputs used by one
// namespace attempt. EvaluationTime is captured once by the effectful collector.
type Snapshot struct {
	Namespace         NamespaceIdentity
	Assignment        Assignment
	EvaluationTime    time.Time
	Sites             []*skupperv2alpha1.Site
	Listeners         []*skupperv2alpha1.Listener
	MultiKeyListeners []*skupperv2alpha1.MultiKeyListener
	Connectors        []*skupperv2alpha1.Connector
	Links             []*skupperv2alpha1.Link
	RouterAccesses    []*skupperv2alpha1.RouterAccess
	Certificates      []*skupperv2alpha1.Certificate
	SecuredAccesses   []*skupperv2alpha1.SecuredAccess
	Attached          []*skupperv2alpha1.AttachedConnector
	Bindings          []*skupperv2alpha1.AttachedConnectorBinding
	Pods              []*corev1.Pod
	Services          []*corev1.Service
	ServiceAccounts   []*corev1.ServiceAccount
	Roles             []*rbacv1.Role
	RoleBindings      []*rbacv1.RoleBinding
	Routes            []*routev1.Route
	Ingresses         []*networkingv1.Ingress
	HTTPProxies       []*unstructured.Unstructured
	TLSRoutes         []*unstructured.Unstructured
	Gateway           *unstructured.Unstructured
	Secrets           []*corev1.Secret
	Observations      map[RouterTarget][]Observation
	PublishedIntents  map[RouterTarget]routercontrol.Publication
	Allocations       AllocationState
	Bootstrap         RouterControlBootstrap
	DefaultAccessType string
	ClusterHost       string
	AccessConfig      AccessConfig
	SourceNamespaces  map[string]types.UID
	SourceAssignments map[string]Assignment
}

type AccessConfig struct {
	EnabledTypes        []string
	DefaultType         string
	ClusterHost         string
	IngressDomain       string
	IngressClassName    string
	HTTPProxyDomain     string
	GatewayPort         int
	GatewayClass        string
	GatewayDomain       string
	ControllerNamespace string
	GatewayOwner        *metav1.OwnerReference
}

// RouterControlBootstrap contains only public endpoint/trust and projected-token
// metadata used to derive router workloads. It never contains issuer or client
// private keys, bearer-token contents, or traffic credentials.
type RouterControlBootstrap struct {
	EnrollmentURL     string
	ControlAddress    string
	TLSServerName     string
	TokenAudience     string
	TokenPath         string
	CABundleConfigMap string
	CABundleKey       string
	CABundlePath      string
	PublicCA          []byte
}

func DefaultRouterControlBootstrap(controllerNamespace string) RouterControlBootstrap {
	dnsName := "skupper-controller." + controllerNamespace + ".svc"
	return RouterControlBootstrap{EnrollmentURL: "https://" + dnsName + ":8443", ControlAddress: dnsName + ":8444", TLSServerName: dnsName, TokenAudience: "skupper-controller-enrollment", TokenPath: "/var/run/secrets/skupper-controller/enrollment-token", CABundleConfigMap: "skupper-controller-ca", CABundleKey: "ca.crt", CABundlePath: "/etc/skupper-controller/ca.crt"}
}

func (b RouterControlBootstrap) Validate() error {
	if b.EnrollmentURL == "" || b.ControlAddress == "" || b.TLSServerName == "" || b.TokenAudience == "" || b.TokenPath == "" || b.CABundleConfigMap == "" || b.CABundleKey == "" || b.CABundlePath == "" {
		return fmt.Errorf("router-control bootstrap fields must all be set")
	}
	return nil
}

type AllocationState struct {
	SiteUID         types.UID
	Ports           map[string]int
	ResourceVersion string
}

type DesiredNamespace struct {
	Namespace         NamespaceIdentity
	SiteUID           types.UID
	Intents           map[RouterTarget]routercontrol.RouterIntent
	Allocations       AllocationState
	Diagnostics       []Diagnostic
	AttachedSources   map[string]types.UID
	Bootstrap         RouterControlBootstrap
	Site              *skupperv2alpha1.Site
	ListenerServices  []*corev1.Service
	ServiceAccount    *corev1.ServiceAccount
	Role              *rbacv1.Role
	RoleBinding       *rbacv1.RoleBinding
	GeneratedAccess   *skupperv2alpha1.RouterAccess
	SecuredAccesses   []*skupperv2alpha1.SecuredAccess
	Certificates      []*skupperv2alpha1.Certificate
	AccessServices    []*corev1.Service
	AccessRoutes      []*routev1.Route
	AccessIngresses   []*networkingv1.Ingress
	AccessHTTPProxies []*unstructured.Unstructured
	AccessTLSRoutes   []*unstructured.Unstructured
	AccessGateway     *unstructured.Unstructured
	Statuses          StatusProjection
}

type StatusProjection struct {
	Owner            *skupperv2alpha1.Site
	SourceNamespaces map[string]types.UID
	Sites            []*skupperv2alpha1.Site
	Listeners        []*skupperv2alpha1.Listener
	MultiKey         []*skupperv2alpha1.MultiKeyListener
	Connectors       []*skupperv2alpha1.Connector
	Links            []*skupperv2alpha1.Link
	Accesses         []*skupperv2alpha1.RouterAccess
	SecuredAccesses  []*skupperv2alpha1.SecuredAccess
	Certificates     []*skupperv2alpha1.Certificate
	Bindings         []*skupperv2alpha1.AttachedConnectorBinding
	Attached         []*skupperv2alpha1.AttachedConnector
}

func copyBootstrap(in RouterControlBootstrap) RouterControlBootstrap {
	out := in
	out.PublicCA = append([]byte(nil), in.PublicCA...)
	return out
}

type Diagnostic struct {
	Resource types.UID
	Reason   string
	Message  string
}

type Collector interface {
	Collect(context.Context, string) (Snapshot, error)
}

type Deriver interface {
	Derive(Snapshot) DesiredNamespace
}

type Planner interface {
	Plan(Snapshot, DesiredNamespace) Plan
}

type PlanExecutor interface {
	Execute(context.Context, Plan) ExecutionReport
}

type Publisher = routercontrol.IntentPublisher
