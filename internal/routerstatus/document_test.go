package status

import (
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"
)

func TestEncodeStableAndDecodeVersion(t *testing.T) {
	doc := &Document{Version: 1, Network: Network{Sites: []Site{}}}
	a, err := Encode(doc)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Encode(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("gzip encoding is not stable")
	}
	decoded, err := Decode(a)
	if err != nil || decoded.Version != 1 {
		t.Fatalf("Decode() = %#v, %v", decoded, err)
	}

	var raw bytes.Buffer
	w := gzip.NewWriter(&raw)
	_, _ = io.WriteString(w, `{"version":2,"group":"group","applied":{},"router":{},"network":{"sites":[]}}`)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(raw.Bytes()); err == nil || !strings.Contains(err.Error(), "unsupported version 2") {
		t.Fatalf("Decode(version 2) error = %v", err)
	}
}

func TestSortDocument(t *testing.T) {
	doc := Document{Links: []Link{{Name: "z"}, {Name: "a"}}, Addresses: []Address{{Name: "z"}, {Name: "a"}}}
	sortDocument(&doc)
	if doc.Links[0].Name != "a" || doc.Addresses[0].Name != "a" {
		t.Fatalf("document not sorted: %#v", doc)
	}
}
