package config

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/skupperproject/skupper/pkg/skrouter/mgmt"
	"github.com/skupperproject/skupper/pkg/skrouter/mgmt/entities"
)

func TestRepresentativeConfigRoundTripAndOrder(t *testing.T) {
	fixture := `[
 ["listener",{"port":5672,"name":"z","authenticatePeer":"no","cost":"0"}],
 ["router",{"id":"router-1","mode":"interior","helloMaxAgeSeconds":"3","timestampsInUTC":"yes"}],
 ["site",{"name":"site-1","namespace":"test"}],
 ["sslProfile",{"name":"server","caCertFile":"/etc/ca.crt"}],
 ["proxyProfile",{"name":"proxy","host":"proxy.example","port":3128}],
 ["connector",{"name":"uplink","host":"other","port":55671,"verifyHostname":"false"}],
 ["address",{"prefix":"mc/","distribution":"multicast","priority":"0"}],
 ["tcpListener",{"name":"in","address":"svc","port":8080,"authenticatePeer":"on"}],
 ["tcpConnector",{"name":"out","address":"svc","port":"8080","verifyHostname":0}],
 ["listenerAddress",{"name":"la","address":"svc","value":"0","listener":"in"}],
 ["log",{"module":"ROUTER","enable":"debug","includeTimestamp":"true"}],
 ["listener",{"name":"a","port":"5671"}]
]`
	doc, err := Parse([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Listeners[0].Value.Port != "5672" || doc.Listeners[0].Value.Cost != 0 || doc.Listeners[0].Value.AuthenticatePeer {
		t.Fatalf("scalar coercion failed: %#v", doc.Listeners[0].Value)
	}
	if !doc.Listeners[0].Fields.Has(entities.ListenerCost) || !doc.TcpConnectors[0].Fields.Has(entities.TcpConnectorVerifyHostname) {
		t.Fatal("explicit zero/false field presence was lost")
	}
	one, err := doc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	two, err := doc.Marshal()
	if err != nil || string(one) != string(two) {
		t.Fatalf("marshal is not deterministic: %v", err)
	}
	if strings.Index(string(one), `"name":"a"`) > strings.Index(string(one), `"name":"z"`) {
		t.Fatalf("listeners were not sorted: %s", one)
	}
	again, err := Parse(one)
	if err != nil {
		t.Fatal(err)
	}
	if !doc.Equal(again) {
		t.Fatalf("round trip differs:\n%s", one)
	}
}

func TestEmptyRouterOmitted(t *testing.T) {
	doc, err := Parse([]byte(`[["tcpConnector",{"name":"bridge","port":8080}]]`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := doc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"router"`) {
		t.Fatalf("empty router emitted: %s", b)
	}
}

func TestParseRejectsInvalidData(t *testing.T) {
	tests := []string{
		`{}`,
		`[["unknown",{}]]`,
		`[["router",{"bogus":1}]]`,
		`[["router",[]]]`,
		`[["router",{}],["router",{}]]`,
		`[["listener",{"cost":"not-a-number"}]]`,
		`[["listener",{"authenticatePeer":"perhaps"}]]`,
		`[["listener",{"role":"invalid"}]]`,
		`[["router",{}]] trailing`,
	}
	for _, input := range tests {
		if _, err := Parse([]byte(input)); err == nil {
			t.Errorf("Parse(%s) unexpectedly succeeded", input)
		}
	}
}

func TestMarshalRejectsUnknownMaskBits(t *testing.T) {
	doc, err := Parse([]byte(`[["router",{"id":"x"}]]`))
	if err != nil {
		t.Fatal(err)
	}
	doc.Router.Fields |= 1 << 63
	if _, err := doc.Marshal(); err == nil {
		t.Fatal("unknown field mask bit accepted")
	}
}

func TestGoldenPresenceAndEscaping(t *testing.T) {
	doc, err := Parse([]byte(`[["listener",{"cost":0,"healthz":false,"host":"","name":"a\"b","openProperties":{"z":2,"a":1}}]]`))
	if err != nil {
		t.Fatal(err)
	}
	data, err := doc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	want := `[["listener",{"name":"a\"b","host":"","cost":0,"healthz":false,"openProperties":{"a":1,"z":2}}]]`
	if string(data) != want {
		t.Fatalf("got %s\nwant %s", data, want)
	}
	withoutFalse := doc
	withoutFalse.Listeners = append([]mgmt.Partial[entities.Listener](nil), doc.Listeners...)
	withoutFalse.Listeners[0].Fields = withoutFalse.Listeners[0].Fields.Without(mgmt.Fields(entities.ListenerHealthz))
	if doc.Equal(withoutFalse) {
		t.Fatal("omitted and explicit false compare equal")
	}
}

func TestEqualIgnoresRepeatedSectionOrder(t *testing.T) {
	a, err := Parse([]byte(`[["listener",{"port":"2"}],["listener",{"port":"1"}],["log",{"module":"ROUTER"}],["log",{"module":"DEFAULT"}]]`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse([]byte(`[["log",{"module":"DEFAULT"}],["listener",{"port":"1"}],["log",{"module":"ROUTER"}],["listener",{"port":"2"}]]`))
	if err != nil {
		t.Fatal(err)
	}
	if !a.Equal(b) {
		t.Fatal("order affected equality")
	}
}

func TestMarshalPropagatesMapEncodingError(t *testing.T) {
	doc := Document{Listeners: []mgmt.Partial[entities.Listener]{{Value: entities.Listener{OpenProperties: map[string]any{"bad": math.NaN()}}, Fields: mgmt.Fields(entities.ListenerOpenProperties)}}}
	if _, err := doc.Marshal(); err == nil {
		t.Fatal("invalid JSON value silently emitted")
	}
}

func TestInvalidScalarAndTupleShapes(t *testing.T) {
	for _, input := range []string{
		`null`, `[] []`, `[null]`, `[[1,{}]]`, `[["listener"]]`,
		`[["listener",null]]`, `[["listener",{"cost":null}]]`,
		`[["listener",{"cost":1.5}]]`, `[["listener",{"cost":"9223372036854775808"}]]`,
		`[["listener",{"healthz":2}]]`, `[["listener",{"host":true}]]`,
	} {
		if _, err := Parse([]byte(input)); err == nil {
			t.Errorf("accepted %s", input)
		}
	}
}

func TestOpenPropertiesUseWireNumbers(t *testing.T) {
	doc, err := Parse([]byte(`[["listener",{"openProperties":{"nested":[-1,18446744073709551615,1.5,{"zero":0}]}}]]`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"nested": []any{int64(-1), uint64(math.MaxUint64), float64(1.5), map[string]any{"zero": int64(0)}}}
	if !reflect.DeepEqual(doc.Listeners[0].Value.OpenProperties, want) {
		t.Fatalf("non-wire values: %#v", doc.Listeners[0].Value.OpenProperties)
	}
	for _, number := range []string{"18446744073709551616", "1e999"} {
		if _, err := Parse([]byte(`[["listener",{"openProperties":{"bad":` + number + `}}]]`)); err == nil {
			t.Fatalf("accepted overflowing number %s", number)
		}
	}
}
