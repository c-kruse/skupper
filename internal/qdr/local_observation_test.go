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
