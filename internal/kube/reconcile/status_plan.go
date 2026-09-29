package reconcile

import "context"

import "k8s.io/apimachinery/pkg/types"

type StatusWriter interface {
	ApplyStatuses(context.Context, NamespaceIdentity, StatusProjection) error
}

type StatusPlanner struct {
	Next   Planner
	Writer StatusWriter
}

func (p StatusPlanner) Plan(snapshot Snapshot, desired DesiredNamespace) Plan {
	plan := p.Next.Plan(snapshot, desired)
	if statusProjectionEmpty(desired.Statuses) {
		return plan
	}
	dependencies := []OperationID{}
	for _, operation := range plan.Operations {
		if operation.Kind == "PublishRouterIntent" {
			dependencies = append(dependencies, operation.ID)
		}
	}
	projection := copyStatusProjection(desired.Statuses)
	plan.Operations = append(plan.Operations, Operation{ID: "apply-public-status", Kind: "ApplyPublicStatus", Dependencies: dependencies, Run: func(ctx context.Context) error {
		return p.Writer.ApplyStatuses(ctx, snapshot.Namespace, projection)
	}})
	return plan
}

func statusProjectionEmpty(value StatusProjection) bool {
	return len(value.Sites)+len(value.Listeners)+len(value.MultiKey)+len(value.Connectors)+len(value.Links)+len(value.Accesses)+len(value.SecuredAccesses)+len(value.Certificates)+len(value.Bindings)+len(value.Attached) == 0
}

func copyStatusProjection(in StatusProjection) StatusProjection {
	out := StatusProjection{}
	if in.Owner != nil {
		out.Owner = in.Owner.DeepCopy()
	}
	out.SourceNamespaces = make(map[string]types.UID, len(in.SourceNamespaces))
	for namespace, uid := range in.SourceNamespaces {
		out.SourceNamespaces[namespace] = uid
	}
	for _, value := range in.Sites {
		out.Sites = append(out.Sites, value.DeepCopy())
	}
	for _, value := range in.Listeners {
		out.Listeners = append(out.Listeners, value.DeepCopy())
	}
	for _, value := range in.MultiKey {
		out.MultiKey = append(out.MultiKey, value.DeepCopy())
	}
	for _, value := range in.Connectors {
		out.Connectors = append(out.Connectors, value.DeepCopy())
	}
	for _, value := range in.Links {
		out.Links = append(out.Links, value.DeepCopy())
	}
	for _, value := range in.Accesses {
		out.Accesses = append(out.Accesses, value.DeepCopy())
	}
	for _, value := range in.SecuredAccesses {
		out.SecuredAccesses = append(out.SecuredAccesses, value.DeepCopy())
	}
	for _, value := range in.Certificates {
		out.Certificates = append(out.Certificates, value.DeepCopy())
	}
	for _, value := range in.Bindings {
		out.Bindings = append(out.Bindings, value.DeepCopy())
	}
	for _, value := range in.Attached {
		out.Attached = append(out.Attached, value.DeepCopy())
	}
	return out
}
