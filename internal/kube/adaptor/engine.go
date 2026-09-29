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
	RestartConfig     RestartConfigStore
	RouterIncarnation string // fallback used only before local management is available
	Metrics           RuntimeMetrics
}

type routerRealizationEvidence interface {
	CurrentRouterIncarnation() (string, bool)
	MarkRouterVerified() string
}

type runningSettingsReader interface {
	ReadRunningSettings() (*qdr.RouterConfig, error)
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
			for _, id := range resourcesUsingCredential(intent, binding.ID) {
				report.Resources = append(report.Resources, routercontrol.ResourceApplication{ResourceID: id, State: routercontrol.ApplicationFailed, Reason: "required traffic credential is unavailable"})
			}
			if len(report.Resources) == 0 {
				report.Resources = []routercontrol.ResourceApplication{{ResourceID: binding.ID, State: routercontrol.ApplicationFailed, Reason: "required traffic credential is unavailable"}}
			}
			return report, CompiledIntent{}
		}
		credentials[binding.ID] = realization
	}
	compiled, err := CompileIntent(intent, credentials)
	if err != nil {
		report.State = routercontrol.ApplicationFailed
		return report, CompiledIntent{}
	}
	persisted := true
	result := ApplyResult{}
	if e.RestartConfig == nil {
		persisted = false
		result.Err = fmt.Errorf("persist router restart configuration: store unavailable")
	} else {
		if err := e.RestartConfig.Persist(compiled.Config); err != nil {
			persisted = false
			result.Err = fmt.Errorf("persist router restart configuration: %w", err)
		}
	}
	if persisted {
		result = ReconcileWithMetrics(e.Router, compiled, e.Metrics)
	}
	restartReason := ""
	if result.Applied {
		restartReason = e.restartRequired(compiled.Config)
	}
	if result.Applied && restartReason == "" {
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
	return report, compiled
}

func resourcesUsingCredential(intent routercontrol.RouterIntent, binding routercontrol.ResourceID) []routercontrol.ResourceID {
	ids := map[routercontrol.ResourceID]struct{}{}
	for _, resource := range intent.RouterConnections {
		if resource.TLS.CredentialBinding == binding || resource.ProxyCredentialBinding == binding {
			ids[resource.ID] = struct{}{}
		}
	}
	for _, resource := range intent.RouterListeners {
		if resource.TLS.CredentialBinding == binding {
			ids[resource.ID] = struct{}{}
		}
	}
	for _, resource := range intent.ServiceListeners {
		if resource.TLS.CredentialBinding == binding {
			ids[resource.ID] = struct{}{}
		}
	}
	for _, resource := range intent.ServiceConnectors {
		if resource.TLS.CredentialBinding == binding {
			ids[resource.ID] = struct{}{}
		}
	}
	result := make([]routercontrol.ResourceID, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func (e *Engine) restartRequired(desired qdr.RouterConfig) string {
	reader, ok := e.Router.(runningSettingsReader)
	if !ok {
		return "router startup settings are unverified; router restart required"
	}
	running, err := reader.ReadRunningSettings()
	if err != nil || running == nil {
		return "router startup settings are unverified; router restart required"
	}
	if normalizeDataConnectionCount(running.Metadata.DataConnectionCount) != normalizeDataConnectionCount(desired.Metadata.DataConnectionCount) ||
		running.Metadata.Mode != desired.Metadata.Mode ||
		!reflect.DeepEqual(normalizeLogConfig(running.LogConfig), normalizeLogConfig(desired.LogConfig)) {
		return "router startup settings changed; router restart required"
	}
	return ""
}

func normalizeDataConnectionCount(value string) string {
	if value == "" {
		return "auto"
	}
	return value
}

func normalizeLogConfig(config map[string]qdr.LogConfig) map[string]qdr.LogConfig {
	result := cloneMap(config)
	if _, found := result["DEFAULT"]; !found {
		result["DEFAULT"] = qdr.LogConfig{Module: "DEFAULT", Enable: "info+"}
	}
	return result
}
