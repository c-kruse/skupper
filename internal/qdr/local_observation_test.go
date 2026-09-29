package qdr

import (
	"strings"
	"testing"

	amqp "github.com/interconnectedcloud/go-amqp"
)

func TestDecodeLocalAddressPreservesClassAndExactRoutingKey(t *testing.T) {
	record := Record{"key": "Morders", "subscriberCount": int32(2), "inProcess": uint32(3), "remoteCount": int64(4)}
	got, err := DecodeLocalAddress(record)
	if err != nil {
		t.Fatal(err)
	}
	if got.Class != 'M' || got.RoutingKey != "orders" || got.SubscriberCount != 2 || got.InProcess != 3 || got.RemoteCount != 4 {
		t.Fatalf("incorrect address decode: %#v", got)
	}
	digit, err := DecodeLocalAddress(Record{"key": "M0orders", "subscriberCount": 0, "inProcess": 0, "remoteCount": 0})
	if err != nil || digit.RoutingKey != "0orders" {
		t.Fatalf("leading routing-key digit was treated as a phase: address=%#v err=%v", digit, err)
	}
}

func TestDecodeLocalAddressRejectsPartialRecord(t *testing.T) {
	_, err := DecodeLocalAddress(Record{"key": "Morders", "subscriberCount": 1, "inProcess": 2})
	if err == nil {
		t.Fatal("missing remoteCount was converted to zero")
	}
}

func TestDecodeLocalAddressRejectsNegativeCountsAndEdgeSummary(t *testing.T) {
	if _, err := DecodeLocalAddress(Record{"key": "Morders", "subscriberCount": -1, "inProcess": 0, "remoteCount": 0}); err == nil {
		t.Fatal("negative count was accepted")
	}
	if _, err := DecodeLocalAddress(Record{"key": "Hedge-router", "subscriberCount": 1, "inProcess": 0, "remoteCount": 0}); err == nil {
		t.Fatal("edge-summary address was decoded as mobile traffic")
	}
}

func TestManagementLogAttributesRedactsProxyPasswordWithoutChangingRequest(t *testing.T) {
	attributes := Record{"host": "proxy", "password": "secret"}
	logged := managementLogAttributes("io.skupper.router.proxyProfile", attributes)
	if logged["password"] != "[redacted]" {
		t.Fatalf("proxy password not redacted: %#v", logged)
	}
	if attributes["password"] != "secret" {
		t.Fatal("redaction changed management request")
	}
}

func TestProxyManagementResponseErrorOmitsCredentialBearingDescription(t *testing.T) {
	const placeholderPassword = "placeholder-proxy-password"
	response := &amqp.Message{ApplicationProperties: map[string]any{
		"statusCode":        int32(400),
		"statusDescription": "duplicate entity attributes include password=" + placeholderPassword,
	}}

	for _, operation := range []string{"CREATE", "UPDATE"} {
		err := managementResponseError(operation, "io.skupper.router.proxyProfile", "proxy", response)
		if err == nil {
			t.Fatal("mock management failure was accepted")
		}
		if strings.Contains(err.Error(), placeholderPassword) || strings.Contains(err.Error(), "statusDescription") {
			t.Fatal("credential-bearing management description escaped sanitization")
		}
		for _, expected := range []string{operation, "io.skupper.router.proxyProfile", "proxy", "400"} {
			if !strings.Contains(err.Error(), expected) {
				t.Fatalf("safe management diagnostic omitted %q", expected)
			}
		}
	}
}

func TestDecodeConnectorIncludesLocalOperationalState(t *testing.T) {
	connector := asConnector(Record{
		"name":             "site-link",
		"host":             "peer.example",
		"port":             "55671",
		"connectionStatus": "SUCCESS",
		"connectionMsg":    "Connection Opened: dir=out",
	})
	if connector.ConnectionStatus != "SUCCESS" || connector.ConnectionMsg != "Connection Opened: dir=out" {
		t.Fatalf("connector operational fields not decoded: %#v", connector)
	}
}

func TestConnectorSecurityFieldsEncodeForStartupAndDynamicManagement(t *testing.T) {
	verifyHostname := false
	connector := Connector{Name: "peer", Host: "peer.example", Port: "55671", VerifyHostname: &verifyHostname, SaslMechanisms: "EXTERNAL"}
	record := connector.toRecord()
	if verify, found := record["verifyHostname"]; !found || verify != false {
		t.Fatalf("dynamic connector did not encode explicit false: %#v", record)
	}
	if record["saslMechanisms"] != "EXTERNAL" {
		t.Fatalf("dynamic connector omitted SASL EXTERNAL: %#v", record)
	}
	decoded := asConnector(record)
	if decoded.VerifyHostname == nil || *decoded.VerifyHostname || decoded.SaslMechanisms != "EXTERNAL" {
		t.Fatalf("connector management read-back lost security settings: %#v", decoded)
	}

	config := InitialConfig("router", "site", "version", false, 3)
	config.AddConnector(connector)
	startup, err := MarshalRouterConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(startup, `"verifyHostname": false`) || !strings.Contains(startup, `"saslMechanisms": "EXTERNAL"`) {
		t.Fatalf("startup connector omitted explicit security settings: %s", startup)
	}
	roundTrip, err := UnmarshalRouterConfig(startup)
	if err != nil {
		t.Fatal(err)
	}
	got := roundTrip.Connectors[connector.Name]
	if got.VerifyHostname == nil || *got.VerifyHostname || got.SaslMechanisms != "EXTERNAL" {
		t.Fatalf("startup round trip lost connector security settings: %#v", got)
	}
}

func TestListenerSecurityFieldsEncodeForStartupAndDynamicManagement(t *testing.T) {
	listener := Listener{Name: "peer", Port: 55671, SslProfile: "tls", RequireSsl: true, AuthenticatePeer: true, SaslMechanisms: "EXTERNAL"}
	record := listener.toRecord()
	if record["requireSsl"] != true || record["authenticatePeer"] != true || record["saslMechanisms"] != "EXTERNAL" {
		t.Fatalf("dynamic listener omitted security settings: %#v", record)
	}
	decoded := asListener(record)
	if !decoded.RequireSsl || !decoded.AuthenticatePeer || decoded.SaslMechanisms != "EXTERNAL" {
		t.Fatalf("listener management read-back lost security settings: %#v", decoded)
	}
	config := InitialConfig("router", "site", "version", false, 3)
	config.AddListener(listener)
	startup, err := MarshalRouterConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(startup, `"requireSsl": true`) || !strings.Contains(startup, `"saslMechanisms": "EXTERNAL"`) {
		t.Fatalf("startup listener omitted security settings: %s", startup)
	}
	roundTrip, err := UnmarshalRouterConfig(startup)
	if err != nil {
		t.Fatal(err)
	}
	got := roundTrip.Listeners[listener.Name]
	if !got.RequireSsl || !got.AuthenticatePeer || got.SaslMechanisms != "EXTERNAL" {
		t.Fatalf("startup round trip lost listener security settings: %#v", got)
	}
}
