package reconcile

import (
	"context"
	"reflect"
	"sort"

	"github.com/skupperproject/skupper/internal/routercontrol"
)

type AllocationCommitter interface {
	CommitAllocations(context.Context, NamespaceIdentity, AllocationState) error
}

type PublicationPlanner struct {
	Allocations AllocationCommitter
	Publisher   routercontrol.IntentPublisher
}

func (p PublicationPlanner) Plan(snapshot Snapshot, desired DesiredNamespace) Plan {
	plan := Plan{Namespace: snapshot.Namespace}
	allocationID := OperationID("commit-allocations")
	allocationChanged := !reflect.DeepEqual(snapshot.Allocations, desired.Allocations)
	if allocationChanged && desired.SiteUID != "" {
		allocations := copyAllocations(desired.Allocations)
		plan.Operations = append(plan.Operations, Operation{ID: allocationID, Kind: "CommitAllocations", Run: func(ctx context.Context) error {
			return p.Allocations.CommitAllocations(ctx, snapshot.Namespace, allocations)
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
