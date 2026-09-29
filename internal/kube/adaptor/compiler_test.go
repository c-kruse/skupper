package adaptor

import (
	"testing"

	"github.com/skupperproject/skupper/internal/qdr"
	"github.com/skupperproject/skupper/internal/routercontrol"
)

func TestCompileIntentIncludesTypedSettingsProxyAndStableCredentialIdentity(t *testing.T) {
	intent := routercontrol.RouterIntent{
		SchemaVersion: routercontrol.SchemaVersion,
		Target:        routercontrol.TargetIdentity{NamespaceUID: "namespace", SiteUID: "site", RouterGroup: "group"},
		Settings: routercontrol.RouterSettings{
			Mode:                routercontrol.RoutingModeInterior,
			DataConnectionCount: 7,
			Logging:             []routercontrol.RouterLogSetting{{Module: "TCP_ADAPTOR", Level: "debug"}},
		},
		CredentialBindings: []routercontrol.CredentialBinding{
			{ID: "proxy", Provider: routercontrol.CredentialProviderKubernetesSecret, Reference: "proxy", Usages: []string{routercontrol.CredentialUsageProxy}},
			{ID: "tls", Provider: routercontrol.CredentialProviderKubernetesSecret, Reference: "tls", Usages: []string{routercontrol.CredentialUsageClientAuth}},
		},
		RouterConnections: []routercontrol.RouterConnection{{
			ID: "link", Host: "peer", Port: 55671, Role: "inter-router",
			TLS:                    routercontrol.TLSIntent{Mode: routercontrol.TLSModeClient, CredentialBinding: "tls", VerifyHostname: true},
			ProxyCredentialBinding: "proxy",
		}},
	}
	proxy := qdr.ProxyProfile{Host: "proxy", Port: "3128", Username: "user", Password: "secret"}
	tls := qdr.SslProfile{CaCertFile: "/ca"}
	first, err := CompileIntent(intent, map[routercontrol.ResourceID]CredentialRealization{
		"proxy": {ProxyProfile: &proxy, RealizationID: "proxy-revision"},
		"tls":   {Profile: tls, RealizationID: "tls-revision"},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := CompileIntent(intent, map[routercontrol.ResourceID]CredentialRealization{
		"tls":   {Profile: tls, RealizationID: "tls-revision"},
		"proxy": {ProxyProfile: &proxy, RealizationID: "proxy-revision"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.RealizationID != second.RealizationID {
		t.Fatalf("realization depends on map iteration: %q != %q", first.RealizationID, second.RealizationID)
	}
	if first.Config.Metadata.DataConnectionCount != "7" {
		t.Fatalf("data connection count not compiled: %#v", first.Config.Metadata)
	}
	if got := first.Config.LogConfig["TCP_ADAPTOR"].Enable; got != "debug+" {
		t.Fatalf("logging setting not compiled: %q", got)
	}
	connector := first.Config.Connectors[ownedName("connection", "link")]
	if connector.ProxyProfile == "" || first.Config.ProxyProfiles[connector.ProxyProfile].Host != "proxy" {
		t.Fatalf("proxy binding not compiled: %#v", connector)
	}
}

func TestCompileIntentRejectsUnsupportedApplicationProtocols(t *testing.T) {
	intent := testIntent()
	intent.ServiceConnectors[0].Protocol = routercontrol.ProtocolUDP
	if _, err := CompileIntent(intent, nil); err == nil {
		t.Fatal("UDP silently fell back to TCP")
	}
	intent.ServiceConnectors[0].Protocol = routercontrol.ProtocolHTTP
	if _, err := CompileIntent(intent, nil); err == nil {
		t.Fatal("HTTP silently fell back to TCP")
	}
}
