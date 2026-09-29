package qdr

import "testing"

func TestDecodeLocalAddressPreservesClassAndPhase(t *testing.T) {
	record := Record{"key": "M1orders", "subscriberCount": int32(2), "inProcess": uint32(3), "remoteCount": int64(4)}
	got, err := DecodeLocalAddress(record)
	if err != nil {
		t.Fatal(err)
	}
	if got.Class != 'M' || got.Phase != 1 || got.RoutingKey != "orders" || got.SubscriberCount != 2 || got.InProcess != 3 || got.RemoteCount != 4 {
		t.Fatalf("incorrect address decode: %#v", got)
	}
}

func TestDecodeLocalAddressRejectsPartialRecord(t *testing.T) {
	_, err := DecodeLocalAddress(Record{"key": "M0orders", "subscriberCount": 1, "inProcess": 2})
	if err == nil {
		t.Fatal("missing remoteCount was converted to zero")
	}
}

func TestDecodeLocalAddressRejectsNegativeCountsAndEdgeSummary(t *testing.T) {
	if _, err := DecodeLocalAddress(Record{"key": "M0orders", "subscriberCount": -1, "inProcess": 0, "remoteCount": 0}); err == nil {
		t.Fatal("negative count was accepted")
	}
	if _, err := DecodeLocalAddress(Record{"key": "Hedge-router", "subscriberCount": 1, "inProcess": 0, "remoteCount": 0}); err == nil {
		t.Fatal("edge-summary address was decoded as phased mobile traffic")
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
