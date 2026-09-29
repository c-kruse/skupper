package adaptor

import (
	"context"
	"fmt"
	"reflect"
	"sort"

	"github.com/skupperproject/skupper/internal/qdr"
	"github.com/skupperproject/skupper/internal/routercontrol"
)

type Engine struct {
	Router            LocalRouter
	Credentials       TrafficCredentialResolver
	RouterIncarnation string // fallback used only before local management is available
	StartupConfig     *qdr.RouterConfig
}

type routerRealizationEvidence interface {
	CurrentRouterIncarnation() (string, bool)
	MarkRouterVerified() string
}

// Realize resolves current dependency revisions and reconciles the accepted
// intent. It is safe to call again with an unchanged digest after credential
// rotation or an ambiguous/partial management operation.
func (e *Engine) Realize(ctx context.Context, sessionID string, sequence uint64, intent routercontrol.RouterIntent, digest routercontrol.Digest) routercontrol.ApplicationReport {
	report, _ := e.RealizeDetailed(ctx, sessionID, sequence, intent, digest)
	return report
}

func (e *Engine) RealizeDetailed(ctx context.Context, sessionID string, sequence uint64, intent routercontrol.RouterIntent, digest routercontrol.Digest) (routercontrol.ApplicationReport, CompiledIntent) {
	report := routercontrol.ApplicationReport{SessionID: sessionID, Sequence: sequence, IntentDigest: digest, RouterIncarnation: e.RouterIncarnation, RealizationID: string(digest), State: routercontrol.ApplicationPending}
	credentials := map[routercontrol.ResourceID]CredentialRealization{}
	for _, binding := range intent.CredentialBindings {
		realization, err := e.Credentials.Resolve(ctx, binding)
		if err != nil {
			report.State = routercontrol.ApplicationFailed
			report.Resources = []routercontrol.ResourceApplication{{ResourceID: binding.ID, State: routercontrol.ApplicationFailed, Reason: err.Error()}}
			return report, CompiledIntent{}
		}
		credentials[binding.ID] = realization
	}
	compiled, err := CompileIntent(intent, credentials)
	if err != nil {
		report.State = routercontrol.ApplicationFailed
		report.Resources = []routercontrol.ResourceApplication{{State: routercontrol.ApplicationFailed, Reason: err.Error()}}
		return report, CompiledIntent{}
	}
	result := Reconcile(e.Router, compiled)
	restartReason := e.restartRequired(compiled.Config)
	if result.Applied {
		if retiree, ok := e.Credentials.(interface {
			Retire(map[string]struct{}) error
		}); ok {
			live := make(map[string]struct{}, len(compiled.CredentialIDs))
			for _, revision := range compiled.CredentialIDs {
				live[revision] = struct{}{}
			}
			if err := retiree.Retire(live); err != nil {
				result.Applied = false
				result.Err = fmt.Errorf("retire unused credential material: %w", err)
			}
		}
	}
	state := routercontrol.ApplicationApplied
	reason := ""
	incarnation := e.RouterIncarnation
	if !result.Applied {
		state = routercontrol.ApplicationPending
		if result.Err != nil {
			reason = result.Err.Error()
		}
	}
	if result.Applied && restartReason != "" {
		state = routercontrol.ApplicationPending
		reason = restartReason
	}
	if evidence, ok := e.Router.(routerRealizationEvidence); ok {
		if state == routercontrol.ApplicationApplied {
			incarnation = evidence.MarkRouterVerified()
		} else if current, _ := evidence.CurrentRouterIncarnation(); current != "" {
			incarnation = current
		}
	}
	report.State = state
	report.RouterIncarnation = incarnation
	report.RealizationID = compiled.RealizationID
	credentialIDs := make([]string, 0, len(compiled.CredentialIDs))
	for id := range compiled.CredentialIDs {
		credentialIDs = append(credentialIDs, string(id))
	}
	sort.Strings(credentialIDs)
	for _, id := range credentialIDs {
		report.Credentials = append(report.Credentials, routercontrol.CredentialRevision{BindingID: routercontrol.ResourceID(id), Revision: compiled.CredentialIDs[routercontrol.ResourceID(id)]})
	}
	ids := make([]string, 0, len(compiled.ResourceNames))
	for id := range compiled.ResourceNames {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	for _, id := range ids {
		report.Resources = append(report.Resources, routercontrol.ResourceApplication{ResourceID: routercontrol.ResourceID(id), RealizationID: compiled.RealizationID, State: state, Reason: reason})
	}
	if len(report.Resources) == 0 && reason != "" {
		report.Resources = append(report.Resources, routercontrol.ResourceApplication{State: state, Reason: fmt.Sprintf("router realization: %s", reason)})
	}
	return report, compiled
}

func (e *Engine) restartRequired(desired qdr.RouterConfig) string {
	if e.StartupConfig == nil {
		return ""
	}
	startup := e.StartupConfig
	if startup.Metadata.Mode != desired.Metadata.Mode || startup.Metadata.DataConnectionCount != desired.Metadata.DataConnectionCount || !reflect.DeepEqual(startup.LogConfig, desired.LogConfig) {
		return "router startup settings changed; router restart required"
	}
	return ""
}
