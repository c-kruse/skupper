package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/skupperproject/skupper/internal/routercontrol"
)

type recordingCommitter struct{ err error }

func (c *recordingCommitter) CommitAllocations(context.Context, NamespaceIdentity, AllocationState) error {
	return c.err
}

type recordingPublisher struct{ published []routercontrol.RouterIntent }

func (p *recordingPublisher) Publish(intent routercontrol.RouterIntent) (routercontrol.Digest, error) {
	p.published = append(p.published, intent)
	return "digest", nil
}
func (p *recordingPublisher) SetUnavailable(routercontrol.TargetIdentity) {}

func TestPublicationWaitsForAllocationCommit(t *testing.T) {
	target := routercontrol.TargetIdentity{NamespaceUID: "namespace", SiteUID: "site", RouterGroup: "skupper-router"}
	desired := DesiredNamespace{Namespace: NamespaceIdentity{Name: "ns", UID: "namespace"}, SiteUID: "site", Allocations: AllocationState{SiteUID: "site", Ports: map[string]int{"listener": 1024}}, Intents: map[RouterTarget]routercontrol.RouterIntent{target: {Target: target}}}
	publisher := &recordingPublisher{}
	planner := PublicationPlanner{Allocations: &recordingCommitter{err: errors.New("conflict")}, Publisher: publisher}
	report := (Executor{}).Execute(context.Background(), planner.Plan(Snapshot{Namespace: desired.Namespace, Allocations: AllocationState{Ports: map[string]int{}}}, desired))
	if len(publisher.published) != 0 {
		t.Fatal("intent published before its allocation was committed")
	}
	if !report.NeedsRetry() {
		t.Fatal("failed allocation and skipped publication must be retried")
	}
}
