package adaptor

import (
	"context"
	"net"
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
	return CredentialRealization{RealizationID: r.revision, Profile: qdr.SslProfile{CaCertFile: "/ca"}}, nil
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
			engine:    Engine{Router: &fakeLocalRouter{current: basicConfig()}, Credentials: &rotatingResolver{revision: "proxy-revision"}, RouterIncarnation: "router-1"},
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
				startup := basicConfig()
				startup.Metadata.DataConnectionCount = "3"
				return Engine{Router: &fakeLocalRouter{current: basicConfig()}, Credentials: noCredentials{}, RouterIncarnation: "router-1", StartupConfig: startup}
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
