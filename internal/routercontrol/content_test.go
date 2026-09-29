package routercontrol

import (
	"strings"
	"testing"
)

func testIntent() RouterIntent {
	target := TargetIdentity{NamespaceUID: "namespace-uid", SiteUID: "site-uid", RouterGroup: "group-a"}
	return RouterIntent{
		Target: target,
		Settings: RouterSettings{
			Mode:                RoutingModeInterior,
			DataConnectionCount: 2,
			Logging:             []RouterLogSetting{{Module: "ROUTER", Level: "debug+"}, {Level: "info"}},
			OwnedAddressKeys:    []string{"z", "a"},
		},
		RouterConnections: []RouterConnection{{ID: "connection", Host: "peer.example", Port: 55671, Role: "inter-router", TLS: TLSIntent{Mode: TLSModeDisabled}}},
		RouterListeners:   []RouterListener{},
		ServiceListeners: []ServiceListener{{
			ID: "listener", Host: "0.0.0.0", Port: 8080, Protocol: ProtocolTCP,
			RoutingKeys: []string{"priority-b", "priority-a"}, TLS: TLSIntent{Mode: TLSModeDisabled},
		}},
		ServiceConnectors: []ServiceConnector{{
			ID: "connector", RoutingKey: "priority-a", Protocol: ProtocolTCP, Target: target,
			Endpoints: []Endpoint{{ID: "endpoint-b", Host: "10.0.0.2", Port: 8080}, {ID: "endpoint-a", Host: "10.0.0.1", Port: 8080}},
			TLS:       TLSIntent{Mode: TLSModeDisabled},
		}},
		CredentialBindings: []CredentialBinding{},
	}
}

func TestCanonicalIntentDeterministicAndPreservesPriority(t *testing.T) {
	intent := testIntent()
	canonical, digestA, err := CanonicalIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonical), `"routingKeys":["priority-b","priority-a"]`) {
		t.Fatalf("routing key priority not preserved: %s", canonical)
	}
	if strings.Contains(string(canonical), `"routingStrategy"`) || strings.Contains(string(canonical), `"routingKeyWeights"`) {
		t.Fatalf("existing priority intent gained weighted optional fields: %s", canonical)
	}

	permuted := intent
	permuted.Settings.OwnedAddressKeys = []string{"a", "z"}
	permuted.ServiceConnectors[0].Endpoints = []Endpoint{intent.ServiceConnectors[0].Endpoints[1], intent.ServiceConnectors[0].Endpoints[0]}
	_, digestB, err := CanonicalIntent(permuted)
	if err != nil {
		t.Fatal(err)
	}
	if digestA != digestB {
		t.Fatalf("set permutations changed digest: %s != %s", digestA, digestB)
	}

	reordered := intent
	reordered.ServiceListeners = append([]ServiceListener(nil), intent.ServiceListeners...)
	reordered.ServiceListeners[0].RoutingKeys = []string{"priority-a", "priority-b"}
	_, digestC, err := CanonicalIntent(reordered)
	if err != nil {
		t.Fatal(err)
	}
	if digestA == digestC {
		t.Fatal("meaningful routing-key order did not change digest")
	}
}

func TestCanonicalWeightedListenerPreservesWeightsAndNormalizesKeyOrder(t *testing.T) {
	intent := testIntent()
	intent.ServiceListeners[0].RoutingStrategy = RoutingStrategyWeighted
	intent.ServiceListeners[0].RoutingKeys = []string{"xfoo", "foo"}
	intent.ServiceListeners[0].RoutingKeyWeights = map[string]uint{"foo": 1, "xfoo": 3}
	canonical, digestA, err := CanonicalIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonical), `"routingKeyWeights":{"foo":1,"xfoo":3}`) ||
		!strings.Contains(string(canonical), `"routingKeys":["foo","xfoo"]`) ||
		!strings.Contains(string(canonical), `"routingStrategy":"weighted"`) {
		t.Fatalf("weighted strategy was not serialized exactly: %s", canonical)
	}

	permuted := testIntent()
	permuted.ServiceListeners[0].RoutingStrategy = RoutingStrategyWeighted
	permuted.ServiceListeners[0].RoutingKeys = []string{"foo", "xfoo"}
	permuted.ServiceListeners[0].RoutingKeyWeights = map[string]uint{"xfoo": 3, "foo": 1}
	_, digestB, err := CanonicalIntent(permuted)
	if err != nil {
		t.Fatal(err)
	}
	if digestA != digestB {
		t.Fatalf("weighted map/key permutations changed digest: %s != %s", digestA, digestB)
	}

	changed := permuted
	changed.ServiceListeners = append([]ServiceListener(nil), permuted.ServiceListeners...)
	changed.ServiceListeners[0].RoutingKeyWeights = map[string]uint{"foo": 2, "xfoo": 3}
	_, digestC, err := CanonicalIntent(changed)
	if err != nil {
		t.Fatal(err)
	}
	if digestA == digestC {
		t.Fatal("weight-only change did not change intent digest")
	}
	delta, err := NewIntentDelta(permuted, changed)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta.ServiceListeners) != 1 || delta.ServiceListeners[0].RoutingKeyWeights["foo"] != 2 {
		t.Fatalf("weight-only change was not carried by delta: %#v", delta.ServiceListeners)
	}
	result, err := ApplyIntentDelta(permuted, delta)
	if err != nil {
		t.Fatal(err)
	}
	if result.ServiceListeners[0].RoutingKeyWeights["foo"] != 2 {
		t.Fatalf("weight-only delta was not applied: %#v", result.ServiceListeners[0])
	}
}

func TestWeightedListenerRequiresExactPositiveWeights(t *testing.T) {
	for _, weights := range []map[string]uint{{"priority-a": 1}, {"priority-a": 1, "priority-b": 0}, {"priority-a": 1, "priority-b": 2, "extra": 3}} {
		intent := testIntent()
		intent.ServiceListeners[0].RoutingStrategy = RoutingStrategyWeighted
		intent.ServiceListeners[0].RoutingKeyWeights = weights
		if _, _, err := CanonicalIntent(intent); err == nil {
			t.Fatalf("accepted invalid weights %#v", weights)
		}
	}
}

func TestOutboundClientTLSRequiresTrustOnly(t *testing.T) {
	intent := testIntent()
	intent.CredentialBindings = []CredentialBinding{{
		ID:        "ca-secret",
		Provider:  CredentialProviderKubernetesSecret,
		Reference: "peer-ca",
		Usages:    []string{CredentialUsageTrust},
	}}
	intent.ServiceConnectors[0].TLS = TLSIntent{Mode: TLSModeClient, CredentialBinding: "ca-secret"}
	if _, _, err := CanonicalIntent(intent); err != nil {
		t.Fatalf("trust-only outbound TLS rejected: %v", err)
	}

	intent.ServiceConnectors[0].TLS.Mode = TLSModeServer
	if _, _, err := CanonicalIntent(intent); err == nil {
		t.Fatal("accepted server TLS mode on connector")
	}
}

func TestOutboundMutualTLSRequiresTrustAndClientAuth(t *testing.T) {
	intent := testIntent()
	intent.CredentialBindings = []CredentialBinding{{
		ID:        "mutual-secret",
		Provider:  CredentialProviderKubernetesSecret,
		Reference: "mutual-tls",
		Usages:    []string{CredentialUsageTrust},
	}}
	intent.ServiceConnectors[0].TLS = TLSIntent{Mode: TLSModeMutual, CredentialBinding: "mutual-secret"}
	if _, _, err := CanonicalIntent(intent); err == nil {
		t.Fatal("accepted outbound mutual TLS without client-auth usage")
	}
	intent.CredentialBindings[0].Usages = append(intent.CredentialBindings[0].Usages, CredentialUsageClientAuth)
	if _, _, err := CanonicalIntent(intent); err != nil {
		t.Fatalf("outbound mutual TLS rejected with trust and client-auth: %v", err)
	}
}

func TestSharedListenerConnectorCredentialMergesUsages(t *testing.T) {
	intent := testIntent()
	intent.CredentialBindings = []CredentialBinding{{
		ID:        "shared-secret",
		Provider:  CredentialProviderKubernetesSecret,
		Reference: "traffic-credentials",
		Usages:    []string{CredentialUsageTrust, CredentialUsageServerAuth},
	}}
	intent.RouterListeners = []RouterListener{{
		ID: "router-listener", Host: "0.0.0.0", Port: 55671, Role: "inter-router",
		TLS: TLSIntent{Mode: TLSModeServer, CredentialBinding: "shared-secret"},
	}}
	intent.ServiceConnectors[0].TLS = TLSIntent{Mode: TLSModeClient, CredentialBinding: "shared-secret"}

	canonical, _, err := CanonicalIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonical), `"usages":["server-auth","trust"]`) {
		t.Fatalf("merged credential usages were not canonicalized: %s", canonical)
	}

	intent.RouterListeners[0].TLS.Mode = TLSModeMutual
	if _, _, err := CanonicalIntent(intent); err != nil {
		t.Fatalf("inbound mutual TLS rejected with server-auth and trust: %v", err)
	}

	intent.RouterListeners[0].TLS.Mode = TLSModeClient
	if _, _, err := CanonicalIntent(intent); err == nil {
		t.Fatal("accepted client TLS mode on listener")
	}

	wrongProvider := intent
	wrongProvider.RouterListeners = append([]RouterListener(nil), intent.RouterListeners...)
	wrongProvider.RouterListeners[0].TLS.Mode = TLSModeServer
	wrongProvider.CredentialBindings = append([]CredentialBinding(nil), intent.CredentialBindings...)
	wrongProvider.CredentialBindings[0].Provider = "kubernetes"
	if _, _, err := CanonicalIntent(wrongProvider); err == nil {
		t.Fatal("accepted obsolete kubernetes credential provider")
	}
}

func TestDecodeIntentRejectsAmbiguousAndCorruptContent(t *testing.T) {
	intent := testIntent()
	canonical, digest, err := CanonicalIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeIntent(canonical, Digest(strings.Repeat("0", 64))); err == nil {
		t.Fatal("accepted digest mismatch")
	}
	duplicate := []byte(`{"schemaVersion":"v1","schemaVersion":"v1"}`)
	if _, err := DecodeIntent(duplicate, ""); err == nil {
		t.Fatal("accepted duplicate JSON key")
	}
	corrupt := append([]byte(nil), canonical...)
	corrupt[len(corrupt)/2] ^= 1
	if _, err := DecodeIntent(corrupt, digest); err == nil {
		t.Fatal("accepted corrupt content")
	}
}

func TestDeltaRequiresAcceptedBaseAndVerifiedResult(t *testing.T) {
	base := testIntent()
	next := testIntent()
	next.ServiceListeners[0].Port = 9090
	delta, err := NewIntentDelta(base, next)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ApplyIntentDelta(base, delta)
	if err != nil {
		t.Fatal(err)
	}
	if result.ServiceListeners[0].Port != 9090 {
		t.Fatalf("delta result port = %d", result.ServiceListeners[0].Port)
	}

	wrongBase := testIntent()
	wrongBase.RouterConnections[0].Cost = 7
	if _, err := ApplyIntentDelta(wrongBase, delta); err == nil {
		t.Fatal("accepted mismatched delta base")
	}
	delta.ResultDigest = Digest(strings.Repeat("f", 64))
	if _, err := ApplyIntentDelta(base, delta); err == nil {
		t.Fatal("accepted corrupt result digest")
	}
}

func TestObservationUnknownIsNotKnownEmpty(t *testing.T) {
	empty := ObservationSnapshot{Scope: ObservationScopeAddresses, SampleSequence: 1, Knowledge: KnowledgeComplete, RouterIncarnation: "router-1"}
	if err := ValidateObservation(empty); err != nil {
		t.Fatalf("known empty rejected: %v", err)
	}
	unknown := ObservationSnapshot{Scope: ObservationScopeAddresses, SampleSequence: 2, Knowledge: KnowledgeUnknown, RouterIncarnation: "router-1", Reason: "query failed", RefreshRequestID: "refresh-1"}
	if err := ValidateObservation(unknown); err != nil {
		t.Fatalf("unknown rejected: %v", err)
	}
	unknown.Addresses = []LocalAddressObservation{{RoutingKey: "key"}}
	if err := ValidateObservation(unknown); err == nil {
		t.Fatal("unknown observation accepted current facts")
	}
}
