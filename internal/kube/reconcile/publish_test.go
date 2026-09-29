package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/skupperproject/skupper/internal/routercontrol"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

type recordingCommitter struct{ err error }

func (c *recordingCommitter) CommitAllocations(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, AllocationState) error {
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

func TestUnchangedAllocationsDoNotCommitBecauseResourceVersionIsPresent(t *testing.T) {
	target := routercontrol.TargetIdentity{NamespaceUID: "namespace", SiteUID: "site", RouterGroup: "skupper-router"}
	allocations := AllocationState{SiteUID: "site", Ports: map[string]int{"listener": 1024}, ResourceVersion: "27"}
	desired := DesiredNamespace{Namespace: NamespaceIdentity{Name: "ns", UID: "namespace"}, SiteUID: "site", Allocations: copyAllocations(allocations), Intents: map[RouterTarget]routercontrol.RouterIntent{target: {Target: target}}}
	planner := PublicationPlanner{Allocations: &recordingCommitter{}, Publisher: &recordingPublisher{}}
	for _, operation := range planner.Plan(Snapshot{Namespace: desired.Namespace, Allocations: allocations}, desired).Operations {
		if operation.Kind == "CommitAllocations" {
			t.Fatal("unchanged logical allocation state scheduled a commit")
		}
	}
}
