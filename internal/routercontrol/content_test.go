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
