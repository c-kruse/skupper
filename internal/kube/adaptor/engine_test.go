package adaptor

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skupperproject/skupper/internal/qdr"
	"github.com/skupperproject/skupper/internal/routercontrol"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type rotatingResolver struct{ revision string }

func (r *rotatingResolver) Resolve(context.Context, routercontrol.CredentialBinding) (CredentialRealization, error) {
	return CredentialRealization{RealizationID: r.revision, Profile: qdr.SslProfile{CaCertFile: "/" + r.revision + "/ca"}}, nil
}

type memoryRestartStore struct{}

func (memoryRestartStore) Persist(qdr.RouterConfig) error { return nil }

type failingResolver struct{ err error }

func (r failingResolver) Resolve(context.Context, routercontrol.CredentialBinding) (CredentialRealization, error) {
	return CredentialRealization{}, r.err
}

func credentialIntent() routercontrol.RouterIntent {
	intent := testIntent()
	intent.CredentialBindings = []routercontrol.CredentialBinding{{ID: "traffic", Provider: routercontrol.CredentialProviderKubernetesSecret, Reference: "traffic", Usages: []string{routercontrol.CredentialUsageTrust}}}
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
	engine := Engine{Router: router, Credentials: resolver, RestartConfig: memoryRestartStore{}, RouterIncarnation: "router-1"}
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

func TestEngineReportsMissingCredentialOnAffectedResourceWithoutSecretDetails(t *testing.T) {
	intent := credentialIntent()
	digest, err := DigestIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	engine := Engine{Router: &fakeLocalRouter{current: basicConfig()}, Credentials: failingResolver{err: errors.New(`secret "sensitive-name" not found`)}, RestartConfig: memoryRestartStore{}, RouterIncarnation: "router-1"}
	report := engine.Realize(context.Background(), "session", 1, intent, digest)
	if report.State != routercontrol.ApplicationFailed || len(report.Resources) != 1 || report.Resources[0].ResourceID != "connector" || report.Resources[0].Reason != "required traffic credential is unavailable" {
		t.Fatalf("missing credential was not reported on the affected resource: %#v", report)
	}
	if strings.Contains(report.Resources[0].Reason, "sensitive-name") {
		t.Fatalf("credential diagnostic disclosed the Secret reference: %q", report.Resources[0].Reason)
	}
}

func TestEngineReportsRestartRequiredSettingsAsPending(t *testing.T) {
	intent := testIntent()
	intent.Settings.DataConnectionCount = 7
	digest, err := DigestIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	running := basicConfig()
	running.Metadata.DataConnectionCount = "3"
	engine := Engine{Router: &fakeLocalRouter{current: basicConfig(), runningSettings: running}, Credentials: noCredentials{}, RestartConfig: memoryRestartStore{}, RouterIncarnation: "router-1"}
	report := engine.Realize(context.Background(), "session", 1, intent, digest)
	if report.State != routercontrol.ApplicationPending || len(report.Resources) == 0 || !strings.Contains(report.Resources[0].Reason, "restart required") {
		t.Fatalf("startup-only change was not explicit pending: %#v", report)
	}
}

type applicationTransportSink struct {
	reports chan routercontrol.ApplicationReport
}

func (s *applicationTransportSink) Connected(routercontrol.SessionKey, string, routercontrol.Hello) {}
func (s *applicationTransportSink) Accepted(routercontrol.SessionKey, routercontrol.Accepted)       {}
func (s *applicationTransportSink) Application(_ context.Context, _ routercontrol.SessionKey, report routercontrol.ApplicationReport) error {
	s.reports <- report
	return nil
}
func (s *applicationTransportSink) Observation(context.Context, routercontrol.SessionKey, routercontrol.ObservationSnapshot) error {
	return nil
}
func (s *applicationTransportSink) Disconnected(routercontrol.SessionKey, string) {}

func TestEngineDocumentOnlyFailuresPassApplicationTransport(t *testing.T) {
	tests := []struct {
		name      string
		intent    routercontrol.RouterIntent
		engine    Engine
		wantState routercontrol.ApplicationState
	}{
		{
			name: "compile failure",
			intent: routercontrol.RouterIntent{
				SchemaVersion: routercontrol.SchemaVersion,
				Target:        routercontrol.TargetIdentity{NamespaceUID: "n", SiteUID: "s", RouterGroup: "g"},
				Settings:      routercontrol.RouterSettings{Mode: routercontrol.RoutingModeInterior},
				CredentialBindings: []routercontrol.CredentialBinding{{
					ID: "proxy", Provider: routercontrol.CredentialProviderKubernetesSecret, Reference: "proxy", Usages: []string{routercontrol.CredentialUsageProxy},
				}},
				RouterConnections: []routercontrol.RouterConnection{{
					ID: "link", Host: "peer", Port: 55671, Role: "inter-router", TLS: routercontrol.TLSIntent{Mode: routercontrol.TLSModeDisabled}, ProxyCredentialBinding: "proxy",
				}},
			},
			engine:    Engine{Router: &fakeLocalRouter{current: basicConfig()}, Credentials: &rotatingResolver{revision: "proxy-revision"}, RestartConfig: memoryRestartStore{}, RouterIncarnation: "router-1"},
			wantState: routercontrol.ApplicationFailed,
		},
		{
			name: "empty document restart required",
			intent: routercontrol.RouterIntent{
				SchemaVersion: routercontrol.SchemaVersion,
				Target:        routercontrol.TargetIdentity{NamespaceUID: "n", SiteUID: "s", RouterGroup: "g"},
				Settings:      routercontrol.RouterSettings{Mode: routercontrol.RoutingModeInterior, DataConnectionCount: 7},
			},
			engine: func() Engine {
				running := basicConfig()
				running.Metadata.DataConnectionCount = "3"
				return Engine{Router: &fakeLocalRouter{current: basicConfig(), runningSettings: running}, Credentials: noCredentials{}, RestartConfig: memoryRestartStore{}, RouterIncarnation: "router-1"}
			}(),
			wantState: routercontrol.ApplicationPending,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			publisher := routercontrol.NewPublisher()
			if _, err := publisher.Publish(test.intent); err != nil {
				t.Fatal(err)
			}
			sink := &applicationTransportSink{reports: make(chan routercontrol.ApplicationReport, 1)}
			server, err := routercontrol.NewServer(publisher, func(context.Context, routercontrol.TargetIdentity) (routercontrol.SessionIdentity, error) {
				return routercontrol.SessionIdentity{PodUID: "pod", ServiceAccountUID: "sa"}, nil
			}, sink, routercontrol.DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			options, err := routercontrol.GRPCServerOptions(routercontrol.DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			listener := bufconn.Listen(1024 * 1024)
			grpcServer := grpc.NewServer(options...)
			routercontrol.RegisterRouterControlServer(grpcServer, server)
			go func() { _ = grpcServer.Serve(listener) }()
			defer grpcServer.Stop()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			connection, err := grpc.DialContext(ctx, "bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
				return listener.Dial()
			}), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			client, err := routercontrol.OpenClientSession(ctx, connection, routercontrol.Hello{Target: test.intent.Target, AdaptorInstanceID: "adaptor", RouterIncarnation: "router-1"}, routercontrol.DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			update, err := client.NextIntent()
			if err != nil {
				t.Fatal(err)
			}
			if err := client.Accept(update); err != nil {
				t.Fatal(err)
			}
			report := test.engine.Realize(ctx, client.SessionID(), update.Sequence, update.Intent, update.Digest)
			if report.State != test.wantState || len(report.Resources) != 0 {
				t.Fatalf("invalid document-level report: %#v", report)
			}
			if err := client.SendApplication(report); err != nil {
				t.Fatalf("application transport rejected report: %v", err)
			}
			select {
			case received := <-sink.reports:
				if received.State != test.wantState || len(received.Resources) != 0 {
					t.Fatalf("transport changed report: %#v", received)
				}
			case <-ctx.Done():
				t.Fatal("server did not receive application report")
			}
		})
	}
}

type failingRestartStore struct{ err error }

func (s failingRestartStore) Persist(qdr.RouterConfig) error { return s.err }

type retiringResolver struct {
	revision string
	retired  []map[string]struct{}
}

func (r *retiringResolver) Resolve(context.Context, routercontrol.CredentialBinding) (CredentialRealization, error) {
	return CredentialRealization{RealizationID: r.revision, Profile: qdr.SslProfile{CaCertFile: "/" + r.revision + "/ca"}}, nil
}

func (r *retiringResolver) Retire(live map[string]struct{}) error {
	copy := make(map[string]struct{}, len(live))
	for id := range live {
		copy[id] = struct{}{}
	}
	r.retired = append(r.retired, copy)
	return nil
}

func TestEnginePersistsSameDigestCredentialRotationBeforeApplying(t *testing.T) {
	intent := credentialIntent()
	digest, err := DigestIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	resolver := &retiringResolver{revision: "revision-1"}
	router := &fakeLocalRouter{current: basicConfig()}
	engine := Engine{Router: router, Credentials: resolver, RestartConfig: FileRestartConfigStore{Directory: directory}, RouterIncarnation: "router-1"}
	if report := engine.Realize(context.Background(), "session", 1, intent, digest); report.State != routercontrol.ApplicationApplied {
		t.Fatalf("initial realization failed: %#v", report)
	}
	resolver.revision = "revision-2"
	if report := engine.Realize(context.Background(), "session", 1, intent, digest); report.State != routercontrol.ApplicationApplied {
		t.Fatalf("credential rotation failed: %#v", report)
	}
	data, err := os.ReadFile(filepath.Join(directory, "skrouterd.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "/revision-2/ca") || strings.Contains(string(data), "/revision-1/ca") {
		t.Fatalf("restart config does not contain only the current credential revision: %s", data)
	}
	if len(resolver.retired) != 2 {
		t.Fatalf("credential retirement calls = %d, want 2 successful complete realizations", len(resolver.retired))
	}
}

func TestEnginePersistenceFailurePreservesLiveRouterAndCredentials(t *testing.T) {
	intent := credentialIntent()
	digest, _ := DigestIntent(intent)
	resolver := &retiringResolver{revision: "new"}
	router := &fakeLocalRouter{current: basicConfig()}
	engine := Engine{Router: router, Credentials: resolver, RestartConfig: failingRestartStore{err: errors.New("disk full")}, RouterIncarnation: "router-1"}
	report := engine.Realize(context.Background(), "session", 1, intent, digest)
	if report.State != routercontrol.ApplicationPending || !strings.Contains(report.Resources[0].Reason, "persist router restart configuration") {
		t.Fatalf("persistence failure was not reported pending: %#v", report)
	}
	if router.applies != 0 {
		t.Fatalf("router was mutated %d times after persistence failure", router.applies)
	}
	if len(resolver.retired) != 0 {
		t.Fatal("credential material was retired after persistence failure")
	}
}

func TestEnginePartialRealizationDoesNotRetireCredentials(t *testing.T) {
	intent := credentialIntent()
	digest, _ := DigestIntent(intent)
	resolver := &retiringResolver{revision: "new"}
	router := &fakeLocalRouter{current: basicConfig(), apply: func(*qdr.RouterConfig) {}}
	engine := Engine{Router: router, Credentials: resolver, RestartConfig: FileRestartConfigStore{Directory: t.TempDir()}, RouterIncarnation: "router-1"}
	if report := engine.Realize(context.Background(), "session", 1, intent, digest); report.State != routercontrol.ApplicationPending {
		t.Fatalf("partial realization was not pending: %#v", report)
	}
	if len(resolver.retired) != 0 {
		t.Fatal("credential material was retired after incomplete live realization")
	}
}

func TestEngineAdaptorRestartDoesNotUseUpdatedFileAsRunningEvidence(t *testing.T) {
	intent := testIntent()
	intent.Settings.DataConnectionCount = 7
	digest, _ := DigestIntent(intent)
	running := basicConfig()
	running.Metadata.DataConnectionCount = "3"
	router := &fakeLocalRouter{current: basicConfig(), runningSettings: running}
	directory := t.TempDir()
	engine := Engine{Router: router, Credentials: noCredentials{}, RestartConfig: FileRestartConfigStore{Directory: directory}, RouterIncarnation: "router-1"}
	first := engine.Realize(context.Background(), "session", 1, intent, digest)
	if first.State != routercontrol.ApplicationPending || !strings.Contains(first.Resources[0].Reason, "restart required") {
		t.Fatalf("running old settings were not pending: %#v", first)
	}
	// A new adaptor process sees the newly persisted file, but the same router
	// process still reports its old settings through management.
	restartedAdaptor := Engine{Router: router, Credentials: noCredentials{}, RestartConfig: FileRestartConfigStore{Directory: directory}, RouterIncarnation: "router-1"}
	second := restartedAdaptor.Realize(context.Background(), "session-2", 1, intent, digest)
	if second.State != routercontrol.ApplicationPending || !strings.Contains(second.Resources[0].Reason, "restart required") {
		t.Fatalf("disk state was mistaken for running settings: %#v", second)
	}
}
