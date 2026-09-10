// Package status describes and builds the stable, local view of a Skupper router.
package status

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
)

const (
	ConfigMapLabel   = "internal.skupper.io/router-status"
	DataKey          = "status.json.gz"
	MaxPrefixMatches = 128
)

// Document is what a router pod publishes about itself. It carries only what
// the controller consumes; add fields when a consumer appears.
type Document struct {
	Version      int           `json:"version"`
	Router       Router        `json:"router"`
	Links        []Link        `json:"links,omitempty"`
	TcpListeners []TcpListener `json:"tcpListeners,omitempty"`
	Addresses    []Address     `json:"addresses,omitempty"`
	Prefixes     []PrefixQuery `json:"prefixes,omitempty"`
	Network      Network       `json:"network"`
}
type Router struct {
	ID       string `json:"id"`
	Mode     string `json:"mode"`
	Hostname string `json:"hostname"`
	PodUID   string `json:"podUid,omitempty"`
}
type Link struct {
	Name             string `json:"name"`
	Present          bool   `json:"present"`
	ConnectionStatus string `json:"connectionStatus"`
	Message          string `json:"message,omitempty"`
	RemoteSiteID     string `json:"remoteSiteId,omitempty"`
	RemoteSiteName   string `json:"remoteSiteName,omitempty"`
}
type TcpListener struct {
	Name       string `json:"name"`
	Present    bool   `json:"present"`
	OperStatus string `json:"operStatus"`
	Message    string `json:"message,omitempty"`
}
type Address struct {
	Name      string `json:"name"`
	Reachable bool   `json:"reachable"`
}
type PrefixQuery struct {
	Prefix    string   `json:"prefix"`
	Matches   []string `json:"matches"`
	Truncated bool     `json:"truncated"`
}
type Network struct {
	Sites []Site `json:"sites"`
}
type Site struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func Encode(doc *Document) ([]byte, error) {
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	w, err := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	// Fixed metadata makes identical documents byte-for-byte identical.
	w.Header.ModTime = (w.Header.ModTime).UTC()
	if _, err = w.Write(data); err == nil {
		err = w.Close()
	}
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
func Decode(data []byte) (*Document, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("router status gzip: %w", err)
	}
	defer r.Close()
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("router status gzip: %w", err)
	}
	var doc Document
	if err = json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("router status json: %w", err)
	}
	if doc.Version != 1 {
		return nil, fmt.Errorf("router status version: unsupported version %d", doc.Version)
	}
	return &doc, nil
}
