package adaptor

import (
	"context"
	"strings"
	"testing"

	"github.com/skupperproject/skupper/internal/qdr"
	"github.com/skupperproject/skupper/internal/routercontrol"
)

type rotatingResolver struct{ revision string }

func (r *rotatingResolver) Resolve(context.Context, routercontrol.CredentialBinding) (CredentialRealization, error) {
	return CredentialRealization{RealizationID: r.revision, Profile: qdr.SslProfile{CaCertFile: "/ca"}}, nil
}

func credentialIntent() routercontrol.RouterIntent {
	intent := testIntent()
	intent.CredentialBindings = []routercontrol.CredentialBinding{{ID: "traffic", Provider: routercontrol.CredentialProviderKubernetesSecret, Reference: "traffic", Usages: []string{routercontrol.CredentialUsageClientAuth}}}
	intent.ServiceConnectors[0].TLS = routercontrol.TLSIntent{Mode: routercontrol.TLSModeClient, CredentialBinding: "traffic", VerifyHostname: true}
	return intent
}

func TestEngineReportsCredentialRotationWithoutIntentChange(t *testing.T) {
	intent := credentialIntent()
	digest, err := DigestIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &rotatingResolver{revision: "revision-1"}
	router := &fakeLocalRouter{current: basicConfig()}
	engine := Engine{Router: router, Credentials: resolver, RouterIncarnation: "router-1"}
	first := engine.Realize(context.Background(), "session", 1, intent, digest)
	resolver.revision = "revision-2"
	second := engine.Realize(context.Background(), "session", 1, intent, digest)
	if first.State != routercontrol.ApplicationApplied || second.State != routercontrol.ApplicationApplied {
		t.Fatalf("rotation did not converge: first=%#v second=%#v", first, second)
	}
	if first.RealizationID == second.RealizationID {
		t.Fatal("credential rotation did not change realization identity")
	}
	if len(second.Credentials) != 1 || second.Credentials[0].BindingID != "traffic" || second.Credentials[0].Revision != "revision-2" {
		t.Fatalf("credential revision missing from report: %#v", second.Credentials)
	}
}

func TestEngineReportsRestartRequiredSettingsAsPending(t *testing.T) {
	intent := testIntent()
	intent.Settings.DataConnectionCount = 7
	digest, err := DigestIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	startup := basicConfig()
	startup.Metadata.DataConnectionCount = "3"
	engine := Engine{Router: &fakeLocalRouter{current: basicConfig()}, Credentials: noCredentials{}, RouterIncarnation: "router-1", StartupConfig: startup}
	report := engine.Realize(context.Background(), "session", 1, intent, digest)
	if report.State != routercontrol.ApplicationPending || len(report.Resources) == 0 || !strings.Contains(report.Resources[0].Reason, "restart required") {
		t.Fatalf("startup-only change was not explicit pending: %#v", report)
	}
}
