package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/skupperproject/skupper/internal/routercontrol"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	"k8s.io/apimachinery/pkg/types"
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
func (p *recordingPublisher) PublishedIntents(string) map[routercontrol.TargetIdentity]routercontrol.Publication {
	return nil
}

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

func TestUnchangedPublicationOmitsValidationAndEffects(t *testing.T) {
	target := RouterTarget{NamespaceUID: "namespace", SiteUID: "site", RouterGroup: "skupper-router"}
	intent := routercontrol.RouterIntent{Target: target, Settings: routercontrol.RouterSettings{Mode: routercontrol.RoutingModeInterior, OwnedAddressKeys: []string{"b", "a"}}}
	publisher := routercontrol.NewPublisher()
	if _, err := publisher.Publish(intent); err != nil {
		t.Fatal(err)
	}
	allocations := AllocationState{SiteUID: "site", Ports: map[string]int{"listener": 1024}}
	snapshot := Snapshot{Namespace: NamespaceIdentity{Name: "ns", UID: "namespace"}, Allocations: allocations, PublishedIntents: publisher.PublishedIntents("namespace")}
	// Set ordering is not a desired-state difference; raw object equality would
	// schedule unnecessary publication and live validation here.
	intent.Settings.OwnedAddressKeys = []string{"a", "b"}
	desired := DesiredNamespace{Namespace: snapshot.Namespace, SiteUID: "site", Allocations: copyAllocations(allocations), Intents: map[RouterTarget]routercontrol.RouterIntent{target: intent}}
	planner := PublicationPlanner{Validator: func(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, map[string]types.UID) error {
		t.Fatal("unchanged publication made a live validation call")
		return nil
	}}
	plan := planner.Plan(snapshot, desired)
	if len(plan.Operations) != 0 {
		t.Fatalf("unchanged publication scheduled effects: %#v", plan.Operations)
	}
	if report := (Executor{}).Execute(context.Background(), plan); report.NeedsRetry() {
		t.Fatalf("empty plan failed: %#v", report)
	}
	desired.Allocations.Ports["listener"] = 1025
	plan = planner.Plan(snapshot, desired)
	if len(plan.Operations) != 1 || plan.Operations[0].Kind != "CommitAllocations" {
		t.Fatalf("unchanged publication hid allocation drift or republished: %#v", plan.Operations)
	}
}

func TestRequiredPublicationStillValidatesBeforePublishing(t *testing.T) {
	for _, state := range []string{"missing", "unavailable", "changed", "invalid"} {
		t.Run(state, func(t *testing.T) {
			target := RouterTarget{NamespaceUID: "namespace", SiteUID: "site", RouterGroup: "skupper-router"}
			intent := routercontrol.RouterIntent{Target: target, Settings: routercontrol.RouterSettings{Mode: routercontrol.RoutingModeInterior, DataConnectionCount: 1}}
			publisher := routercontrol.NewPublisher()
			digest, err := publisher.Publish(intent)
			if err != nil {
				t.Fatal(err)
			}
			switch state {
			case "missing":
				publisher = routercontrol.NewPublisher()
			case "unavailable":
				publisher.SetUnavailable(target)
			case "changed":
				intent.Settings.DataConnectionCount = 3
			case "invalid":
				intent.Settings.Mode = "invalid"
			}
			allocations := AllocationState{SiteUID: "site"}
			before := publisher.PublishedIntents("namespace")[target]
			snapshot := Snapshot{Namespace: NamespaceIdentity{Name: "ns", UID: "namespace"}, Allocations: allocations, PublishedIntents: publisher.PublishedIntents("namespace"), Observations: map[RouterTarget][]Observation{target: {{AcceptedDigest: digest}}}}
			desired := DesiredNamespace{Namespace: snapshot.Namespace, SiteUID: "site", Allocations: allocations, Intents: map[RouterTarget]routercontrol.RouterIntent{target: intent}, AttachedSources: map[string]types.UID{"attached-ns": "attached-uid"}}
			calls := 0
			validationError := errors.New("source reassigned")
			planner := PublicationPlanner{Publisher: publisher, Validator: func(_ context.Context, _ NamespaceIdentity, _ *skupperv2alpha1.Site, sources map[string]types.UID) error {
				calls++
				if sources["attached-ns"] != "attached-uid" {
					t.Fatal("changed publication lost cross-namespace validation sources")
				}
				return validationError
			}}
			plan := planner.Plan(snapshot, desired)
			if len(plan.Operations) != 1 || plan.Operations[0].Kind != "PublishRouterIntent" {
				t.Fatalf("required publication omitted: %#v", plan.Operations)
			}
			if report := (Executor{}).Execute(context.Background(), plan); !report.NeedsRetry() || calls != 1 {
				t.Fatalf("validation was bypassed: calls=%d report=%#v", calls, report)
			}
			if after := publisher.PublishedIntents("namespace")[target]; after != before {
				t.Fatal("failed validation changed publication")
			}
			validationError = nil
			report := (Executor{}).Execute(context.Background(), planner.Plan(snapshot, desired))
			if state == "invalid" {
				if !report.NeedsRetry() || publisher.PublishedIntents("namespace")[target] != before {
					t.Fatal("invalid desired intent was hidden or published")
				}
			} else if report.NeedsRetry() || !publisher.PublishedIntents("namespace")[target].Available {
				t.Fatalf("validated required publication failed: %#v", report)
			}
		})
	}
}

func TestObsoletePublicationsRetireWithoutConnectedObservations(t *testing.T) {
	publisher := routercontrol.NewPublisher()
	for _, site := range []string{"old-site", "replacement-site"} {
		target := RouterTarget{NamespaceUID: "namespace", SiteUID: site, RouterGroup: "skupper-router"}
		if _, err := publisher.Publish(routercontrol.RouterIntent{Target: target, Settings: routercontrol.RouterSettings{Mode: routercontrol.RoutingModeInterior}}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := Snapshot{Namespace: NamespaceIdentity{Name: "ns", UID: "namespace"}, PublishedIntents: publisher.PublishedIntents("namespace")}
	planner := PublicationPlanner{Publisher: publisher}
	plan := planner.Plan(snapshot, DesiredNamespace{})
	if len(plan.Operations) != 2 {
		t.Fatalf("orphan publications without observations were ignored: %#v", plan.Operations)
	}
	if report := (Executor{}).Execute(context.Background(), plan); report.NeedsRetry() {
		t.Fatalf("same-group distinct targets collided: %#v", report)
	}
	snapshot.PublishedIntents = publisher.PublishedIntents("namespace")
	for _, publication := range snapshot.PublishedIntents {
		if publication.Available {
			t.Fatal("obsolete intent remained available")
		}
	}
	if plan := planner.Plan(snapshot, DesiredNamespace{}); len(plan.Operations) != 0 {
		t.Fatal("already-unavailable targets scheduled effects")
	}
}
