package adaptor

import (
	"testing"

	"github.com/skupperproject/skupper/internal/routercontrol"
)

func testIntent() routercontrol.RouterIntent {
	return routercontrol.RouterIntent{
		SchemaVersion:     routercontrol.SchemaVersion,
		Target:            routercontrol.TargetIdentity{NamespaceUID: "n", SiteUID: "s", RouterGroup: "g"},
		Settings:          routercontrol.RouterSettings{Mode: routercontrol.RoutingModeInterior},
		ServiceConnectors: []routercontrol.ServiceConnector{{ID: "connector", RoutingKey: "orders", Protocol: routercontrol.ProtocolTCP, Endpoints: []routercontrol.Endpoint{{ID: "pod", Host: "10.0.0.2", Port: 8080}}}},
	}
}

func TestApplyDeltaChecksAcceptedBaseAndResult(t *testing.T) {
	current := testIntent()
	base, err := DigestIntent(current)
	if err != nil {
		t.Fatal(err)
	}
	next := cloneIntent(current)
	next.ServiceConnectors[0].Endpoints[0].Host = "10.0.0.9"
	result, err := DigestIntent(next)
	if err != nil {
		t.Fatal(err)
	}
	delta := routercontrol.IntentDelta{SchemaVersion: routercontrol.SchemaVersion, Target: current.Target, BaseDigest: base, ResultDigest: result, ServiceConnectors: next.ServiceConnectors}
	got, gotDigest, err := ApplyDelta(current, base, delta)
	if err != nil {
		t.Fatal(err)
	}
	if gotDigest != result || got.ServiceConnectors[0].Endpoints[0].Host != "10.0.0.9" {
		t.Fatalf("wrong delta result: %#v %s", got, gotDigest)
	}
	delta.BaseDigest = "stale"
	unchanged, unchangedDigest, err := ApplyDelta(got, gotDigest, delta)
	if err == nil {
		t.Fatal("wrong-base delta was accepted")
	}
	if unchangedDigest != gotDigest || unchanged.ServiceConnectors[0].Endpoints[0].Host != "10.0.0.9" {
		t.Fatal("wrong-base delta mutated accepted intent")
	}
}

func TestDigestNormalizesSetsButPreservesRoutingPriority(t *testing.T) {
	a := testIntent()
	a.ServiceListeners = []routercontrol.ServiceListener{{ID: "listener", Port: 9090, Protocol: routercontrol.ProtocolTCP, RoutingKeys: []string{"gold", "silver"}}}
	b := cloneIntent(a)
	b.ServiceConnectors = append([]routercontrol.ServiceConnector{{ID: "another", RoutingKey: "x", Protocol: routercontrol.ProtocolTCP, Endpoints: []routercontrol.Endpoint{{ID: "x", Host: "host", Port: 1}}}}, b.ServiceConnectors...)
	a.ServiceConnectors = append(a.ServiceConnectors, b.ServiceConnectors[0])
	da, _ := DigestIntent(a)
	db, _ := DigestIntent(b)
	if da != db {
		t.Fatalf("set order changed digest: %s != %s", da, db)
	}
	b.ServiceListeners[0].RoutingKeys = []string{"silver", "gold"}
	db, _ = DigestIntent(b)
	if da == db {
		t.Fatal("meaningful routing priority did not change digest")
	}
}
