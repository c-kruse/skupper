package entities

import (
	"reflect"
	"testing"

	"github.com/skupperproject/skupper/pkg/skrouter/mgmt"
)

func requireEntity[E any, PE mgmt.Entity[E]]() {}

func TestGeneratedTypesImplementEntity(t *testing.T) {
	requireEntity[Listener]()
	requireEntity[Connector]()
	requireEntity[SslProfile]()
	requireEntity[RouterNode]()
}

func TestListenerMetadataAndDefaults(t *testing.T) {
	if got, want := mgmt.TypeOf[Listener](), ListenerType; got != want {
		t.Fatalf("TypeOf[Listener]() = %p, want %p", got, want)
	}
	if ListenerType.Name != "io.skupper.router.listener" || ListenerType.Short != "listener" || ListenerType.Core {
		t.Fatalf("unexpected Listener metadata: %#v", ListenerType)
	}
	if ListenerType.Supports("UPDATE") {
		t.Fatal("listener unexpectedly supports UPDATE")
	}
	if !ListenerType.Supports("CREATE") || !ListenerType.Supports("DELETE") {
		t.Fatalf("listener operations = %v", ListenerType.Operations)
	}
	if got, want := mgmt.Names[Listener](mgmt.Fields(ListenerName, ListenerPort, ListenerHealthz)), []string{"name", "port", "healthz"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("field names = %v, want %v", got, want)
	}
	if got, found := mgmt.TypeByName(ListenerType.Name); !found || got != ListenerType {
		t.Fatalf("TypeByName(%q) = %p, %t", ListenerType.Name, got, found)
	}

	listener := Listener{Port: "custom"}
	listener.ApplyDefaults(mgmt.Fields(ListenerPort))
	if listener.Port != "custom" || listener.Cost != 1 || listener.Role != RoleNormal {
		t.Fatalf("unexpected scalar defaults: %#v", listener)
	}
	if !listener.Healthz || !listener.Metrics || !listener.Websockets {
		t.Fatalf("expected HTTP defaults to be enabled: %#v", listener)
	}
	if listener.Type != ListenerType.Name {
		t.Fatalf("type default = %q, want %q", listener.Type, ListenerType.Name)
	}
}

func TestExplicitPresenceAndAttributeOrder(t *testing.T) {
	partial := mgmt.Partial[Listener]{
		Value: Listener{
			Cost:             0,
			AuthenticatePeer: false,
			OpenProperties:   map[string]any{},
		},
		Fields: mgmt.Fields(ListenerOpenProperties, ListenerAuthenticatePeer, ListenerCost),
	}
	var names []string
	var values []any
	for name, value := range mgmt.Attributes[Listener](partial) {
		names = append(names, name)
		values = append(values, value)
	}
	if want := []string{"cost", "authenticatePeer", "openProperties"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("attribute names = %v, want %v", names, want)
	}
	if values[0] != int64(0) || values[1] != false {
		t.Fatalf("zero values were not preserved: %#v", values)
	}
	if properties, ok := values[2].(map[string]any); !ok || properties == nil || len(properties) != 0 {
		t.Fatalf("empty map was not preserved: %#v", values[2])
	}
}

func TestGeneratedDecodersAcceptWireVariants(t *testing.T) {
	var listener Listener
	if err := listener.SetManagementField(ListenerCost, uint32(12)); err != nil {
		t.Fatal(err)
	}
	if err := listener.SetManagementField(ListenerRole, int32(1)); err != nil {
		t.Fatal(err)
	}
	if listener.Cost != 12 || listener.Role != RoleInterRouter {
		t.Fatalf("decoded listener = %#v", listener)
	}

	var node RouterNode
	if err := node.SetManagementField(RouterNodeLinkState, []any{int32(2), uint64(3)}); err != nil {
		t.Fatal(err)
	}
	if err := node.SetManagementField(RouterNodeValidOrigins, []any{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(node.LinkState, []int64{2, 3}) || !reflect.DeepEqual(node.ValidOrigins, []string{"a", "b"}) {
		t.Fatalf("decoded node = %#v", node)
	}

	var profile SslProfile
	if err := profile.SetManagementField(SslProfileOrdinal, int32(9)); err != nil {
		t.Fatal(err)
	}
	if profile.Ordinal != 9 {
		t.Fatalf("ordinal = %d", profile.Ordinal)
	}
}

func TestGeneratedComparisonAndNonZeroFields(t *testing.T) {
	left := Listener{Name: "listener", Cost: 1, OpenProperties: map[string]any{"product": "a"}}
	right := Listener{Name: "listener", Cost: 2, OpenProperties: map[string]any{"product": "b"}}
	selected := mgmt.Fields(ListenerName, ListenerCost, ListenerOpenProperties)
	if left.Equal(&right, selected) {
		t.Fatal("Equal returned true for selected differences")
	}
	want := mgmt.Fields(ListenerCost, ListenerOpenProperties)
	if got := left.Diff(&right, selected); got != want {
		t.Fatalf("Diff = %v, want %v", mgmt.Names[Listener](got), mgmt.Names[Listener](want))
	}
	if got := left.NonZeroFields(); !got.Has(ListenerName) || !got.Has(ListenerCost) || !got.Has(ListenerOpenProperties) {
		t.Fatalf("NonZeroFields = %v", mgmt.Names[Listener](got))
	}
}

func TestConnectionUpdateMetadata(t *testing.T) {
	if !ConnectionType.Supports("UPDATE") {
		t.Fatalf("connection operations = %v", ConnectionType.Operations)
	}
	if ConnectionCreateFields != 0 {
		t.Fatalf("connection create fields = %v", mgmt.Names[Connection](ConnectionCreateFields))
	}
	want := mgmt.Fields(ConnectionAdminStatus, ConnectionEnableProtocolTrace)
	if ConnectionUpdateFields != want {
		t.Fatalf("connection update fields = %v, want %v", mgmt.Names[Connection](ConnectionUpdateFields), mgmt.Names[Connection](want))
	}
}

func TestCoreAgentClassificationAndPort(t *testing.T) {
	for _, typ := range []*mgmt.Type{ConnectionType, RouterLinkType, RouterAddressType, ConfigAddressType, AutoLinkType, RouterMetricsType} {
		if !typ.Core {
			t.Errorf("%s is not classified as core", typ.Name)
		}
	}
	for _, typ := range []*mgmt.Type{ListenerType, ConnectorType, RouterNodeType, TcpListenerType} {
		if typ.Core {
			t.Errorf("%s is unexpectedly classified as core", typ.Name)
		}
	}
	if got := Port(5672); got != "5672" {
		t.Fatalf("Port(5672) = %q", got)
	}
}
