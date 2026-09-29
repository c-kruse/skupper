package reconcile

import (
	"context"
	"maps"
	"reflect"
	"sort"

	"k8s.io/apimachinery/pkg/types"

	"github.com/skupperproject/skupper/internal/routercontrol"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

type AllocationCommitter interface {
	CommitAllocations(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, AllocationState) error
}

type PublicationPlanner struct {
	Allocations AllocationCommitter
	Publisher   routercontrol.IntentPublisher
	Validator   func(context.Context, NamespaceIdentity, *skupperv2alpha1.Site, map[string]types.UID) error
}

func (p PublicationPlanner) Plan(snapshot Snapshot, desired DesiredNamespace) Plan {
	plan := Plan{Namespace: snapshot.Namespace}
	sources := maps.Clone(desired.AttachedSources)
	allocationID := OperationID("commit-allocations")
	allocationChanged := snapshot.Allocations.SiteUID != desired.Allocations.SiteUID || !reflect.DeepEqual(snapshot.Allocations.Ports, desired.Allocations.Ports)
	if allocationChanged && desired.SiteUID != "" {
		allocations := copyAllocations(desired.Allocations)
		plan.Operations = append(plan.Operations, Operation{ID: allocationID, Kind: "CommitAllocations", Run: func(ctx context.Context) error {
			return p.Allocations.CommitAllocations(ctx, snapshot.Namespace, desired.Site, allocations)
		}})
	}
	targets := make([]routercontrol.TargetIdentity, 0, len(desired.Intents))
	for target := range desired.Intents {
		targets = append(targets, target)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].RouterGroup < targets[j].RouterGroup })
	for _, target := range targets {
		intent := desired.Intents[target]
		dependencies := []OperationID(nil)
		if allocationChanged {
			dependencies = []OperationID{allocationID}
		}
		plan.Operations = append(plan.Operations, Operation{ID: OperationID("publish/" + target.RouterGroup), Kind: "PublishRouterIntent", Dependencies: dependencies, Run: func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if p.Validator != nil {
				if err := p.Validator(ctx, snapshot.Namespace, desired.Site, sources); err != nil {
					return err
				}
			}
			_, err := p.Publisher.Publish(intent)
			return err
		}})
	}
	for target := range snapshot.Observations {
		if _, stillDesired := desired.Intents[target]; stillDesired {
			continue
		}
		target := target
		plan.Operations = append(plan.Operations, Operation{ID: OperationID("unavailable/" + target.RouterGroup), Kind: "SetIntentUnavailable", Run: func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			p.Publisher.SetUnavailable(target)
			return nil
		}})
	}
	return plan
}
