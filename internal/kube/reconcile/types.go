// Package reconcile implements namespace-scoped snapshot, derivation, planning,
// and execution.  It deliberately has no Kubernetes clients: acquisition and
// effects are supplied by the controller at the package boundary.
package reconcile

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/skupperproject/skupper/internal/routercontrol"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

type NamespaceIdentity struct {
	Name string
	UID  types.UID
}

type Assignment struct {
	Controller string
	Controlled bool
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
	Target            RouterTarget
	Completeness      Completeness
	Fresh             bool
	SessionID         string
	RouterIncarnation string
	IntentDigest      routercontrol.Digest
	RealizationID     string
	Accepted          bool
	Applied           bool
	Resources         map[routercontrol.ResourceID]routercontrol.LocalResourceObservation
}

func (o Observation) KnownEmpty() bool {
	return o.Fresh && o.Completeness == Complete && len(o.Resources) == 0
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
	Secrets           []*corev1.Secret
	Observations      map[RouterTarget]Observation
	Allocations       AllocationState
}

type AllocationState struct {
	SiteUID types.UID
	Ports   map[string]int
}

type DesiredNamespace struct {
	Namespace   NamespaceIdentity
	SiteUID     types.UID
	Intents     map[RouterTarget]routercontrol.RouterIntent
	Allocations AllocationState
	Diagnostics []Diagnostic
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
