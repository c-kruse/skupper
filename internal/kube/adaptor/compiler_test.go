package adaptor

import (
	"strings"
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
			{ID: "tls", Provider: routercontrol.CredentialProviderKubernetesSecret, Reference: "tls", Usages: []string{routercontrol.CredentialUsageTrust}},
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

func TestCompileIntentPreservesWeightedAndPriorityStrategies(t *testing.T) {
	intent := testIntent()
	intent.ServiceListeners = []routercontrol.ServiceListener{
		{ID: "weighted", Host: "0.0.0.0", Port: 8080, Protocol: routercontrol.ProtocolTCP, RoutingKeys: []string{"foo", "xfoo"}, RoutingStrategy: routercontrol.RoutingStrategyWeighted, RoutingKeyWeights: map[string]uint{"foo": 1, "xfoo": 3}, TLS: routercontrol.TLSIntent{Mode: routercontrol.TLSModeDisabled}},
		{ID: "priority", Host: "0.0.0.0", Port: 8081, Protocol: routercontrol.ProtocolTCP, RoutingKeys: []string{"xfoo", "foo"}, RoutingStrategy: routercontrol.RoutingStrategyPriority, TLS: routercontrol.TLSIntent{Mode: routercontrol.TLSModeDisabled}},
	}
	compiled, err := CompileIntent(intent, nil)
	if err != nil {
		t.Fatal(err)
	}
	weightedName := ownedName("tcp-listener", "weighted")
	priorityName := ownedName("tcp-listener", "priority")
	if compiled.Config.Bridges.TcpListeners[weightedName].MultiAddressStrategy != "weighted" || compiled.Config.Bridges.TcpListeners[priorityName].MultiAddressStrategy != "priority" {
		t.Fatalf("multi-address strategies changed: %#v", compiled.Config.Bridges.TcpListeners)
	}
	values := map[string]int{}
	for _, address := range compiled.Config.Bridges.ListenerAddresses {
		values[address.Listener+"/"+address.Address] = address.Value
	}
	if values[weightedName+"/foo"] != 1 || values[weightedName+"/xfoo"] != 3 {
		t.Fatalf("weighted values were not compiled exactly: %#v", values)
	}
	if values[priorityName+"/xfoo"] != 1 || values[priorityName+"/foo"] != 0 {
		t.Fatalf("priority order changed: %#v", values)
	}
}

func TestCompileIntentPreservesAMQPTLSModes(t *testing.T) {
	intent := routercontrol.RouterIntent{
		SchemaVersion: routercontrol.SchemaVersion,
		Target:        routercontrol.TargetIdentity{NamespaceUID: "namespace", SiteUID: "site", RouterGroup: "group"},
		Settings:      routercontrol.RouterSettings{Mode: routercontrol.RoutingModeInterior},
		CredentialBindings: []routercontrol.CredentialBinding{
			{ID: "trust", Provider: routercontrol.CredentialProviderKubernetesSecret, Reference: "trust", Usages: []string{routercontrol.CredentialUsageTrust}},
			{ID: "client", Provider: routercontrol.CredentialProviderKubernetesSecret, Reference: "client", Usages: []string{routercontrol.CredentialUsageTrust, routercontrol.CredentialUsageClientAuth}},
			{ID: "server-creds", Provider: routercontrol.CredentialProviderKubernetesSecret, Reference: "server", Usages: []string{routercontrol.CredentialUsageServerAuth}},
			{ID: "server-mutual-creds", Provider: routercontrol.CredentialProviderKubernetesSecret, Reference: "server-mutual", Usages: []string{routercontrol.CredentialUsageServerAuth, routercontrol.CredentialUsageTrust}},
		},
		RouterConnections: []routercontrol.RouterConnection{
			{ID: "trust-only", Host: "trust.example", Port: 55671, Role: "inter-router", TLS: routercontrol.TLSIntent{Mode: routercontrol.TLSModeClient, CredentialBinding: "trust"}},
			{ID: "mutual", Host: "mutual.example", Port: 55672, Role: "inter-router", TLS: routercontrol.TLSIntent{Mode: routercontrol.TLSModeMutual, CredentialBinding: "client"}},
			{ID: "plaintext", Host: "plain.example", Port: 55673, Role: "inter-router", TLS: routercontrol.TLSIntent{Mode: routercontrol.TLSModeDisabled}},
		},
		RouterListeners: []routercontrol.RouterListener{
			{ID: "server", Host: "0.0.0.0", Port: 55671, Role: "inter-router", TLS: routercontrol.TLSIntent{Mode: routercontrol.TLSModeServer, CredentialBinding: "server-creds"}},
			{ID: "mutual-listener", Host: "0.0.0.0", Port: 55672, Role: "inter-router", TLS: routercontrol.TLSIntent{Mode: routercontrol.TLSModeMutual, CredentialBinding: "server-mutual-creds"}},
			{ID: "plaintext-listener", Host: "0.0.0.0", Port: 55673, Role: "inter-router", TLS: routercontrol.TLSIntent{Mode: routercontrol.TLSModeDisabled}},
		},
	}
	credentials := map[routercontrol.ResourceID]CredentialRealization{}
	for _, binding := range intent.CredentialBindings {
		credentials[binding.ID] = CredentialRealization{Profile: qdr.SslProfile{CaCertFile: "/ca"}, RealizationID: string(binding.ID) + "-revision"}
	}
	compiled, err := CompileIntent(intent, credentials)
	if err != nil {
		t.Fatal(err)
	}
	trustOnly := compiled.Config.Connectors[ownedName("connection", "trust-only")]
	mutual := compiled.Config.Connectors[ownedName("connection", "mutual")]
	plaintext := compiled.Config.Connectors[ownedName("connection", "plaintext")]
	if trustOnly.VerifyHostname == nil || *trustOnly.VerifyHostname || trustOnly.SaslMechanisms != "" || trustOnly.SslProfile == "" {
		t.Fatalf("trust-only outbound changed authentication contract: %#v", trustOnly)
	}
	if mutual.VerifyHostname == nil || *mutual.VerifyHostname || mutual.SaslMechanisms != "EXTERNAL" || mutual.SslProfile == "" {
		t.Fatalf("mutual outbound lacks explicit false or SASL EXTERNAL: %#v", mutual)
	}
	if plaintext.SslProfile != "" || plaintext.SaslMechanisms != "" {
		t.Fatalf("plaintext outbound gained TLS/SASL: %#v", plaintext)
	}
	server := compiled.Config.Listeners[ownedName("router-listener", "server")]
	mutualListener := compiled.Config.Listeners[ownedName("router-listener", "mutual-listener")]
	plaintextListener := compiled.Config.Listeners[ownedName("router-listener", "plaintext-listener")]
	if !server.RequireSsl || server.AuthenticatePeer || server.SaslMechanisms != "" || server.SslProfile == "" {
		t.Fatalf("server-auth listener does not require TLS only: %#v", server)
	}
	if !mutualListener.RequireSsl || !mutualListener.AuthenticatePeer || mutualListener.SaslMechanisms != "EXTERNAL" || mutualListener.SslProfile == "" {
		t.Fatalf("mutual listener lacks required TLS/SASL: %#v", mutualListener)
	}
	if plaintextListener.RequireSsl || plaintextListener.AuthenticatePeer || plaintextListener.SaslMechanisms != "" || plaintextListener.SslProfile != "" {
		t.Fatalf("plaintext listener gained TLS/SASL: %#v", plaintextListener)
	}

	actual := cloneRouterConfig(&compiled.Config)
	if err := verifyOwned(&actual, &compiled.Config); err != nil {
		t.Fatalf("explicit false did not survive read-back: %v", err)
	}
	value := true
	changed := actual.Connectors[ownedName("connection", "mutual")]
	changed.VerifyHostname = &value
	actual.Connectors[changed.Name] = changed
	if err := verifyOwned(&actual, &compiled.Config); err == nil || !strings.Contains(err.Error(), `field "verifyHostname"`) {
		t.Fatalf("false-to-true security change was normalized away: %v", err)
	}
}
