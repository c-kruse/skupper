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
	GroupLabel       = "internal.skupper.io/router-group"
	DataKey          = "status.json.gz"
	MaxPrefixMatches = 128
)

type Document struct {
	Version       int            `json:"version"`
	Group         string         `json:"group"`
	Router        Router         `json:"router"`
	Links         []Link         `json:"links,omitempty"`
	RouterAccess  []RouterAccess `json:"routerAccess,omitempty"`
	TcpListeners  []TcpListener  `json:"tcpListeners,omitempty"`
	TcpConnectors []TcpConnector `json:"tcpConnectors,omitempty"`
	Addresses     []Address      `json:"addresses,omitempty"`
	Prefixes      []PrefixQuery  `json:"prefixes,omitempty"`
	Network       Network        `json:"network"`
}
type Router struct {
	ID       string `json:"id"`
	Mode     string `json:"mode"`
	Version  string `json:"version"`
	Hostname string `json:"hostname"`
	PodUID   string `json:"podUid,omitempty"`
}
type Link struct {
	Name             string `json:"name"`
	Role             string `json:"role"`
	Present          bool   `json:"present"`
	ConnectionStatus string `json:"connectionStatus"`
	Message          string `json:"message,omitempty"`
	RemoteRouterID   string `json:"remoteRouterId,omitempty"`
	RemoteAccessID   string `json:"remoteAccessId,omitempty"`
	RemoteSiteID     string `json:"remoteSiteId,omitempty"`
	RemoteSiteName   string `json:"remoteSiteName,omitempty"`
}
type RouterAccess struct {
	Name    string   `json:"name"`
	Role    string   `json:"role"`
	Present bool     `json:"present"`
	Peers   []string `json:"peers,omitempty"`
}
type TcpListener struct {
	Name       string `json:"name"`
	Address    string `json:"address"`
	Present    bool   `json:"present"`
	OperStatus string `json:"operStatus"`
	Message    string `json:"message,omitempty"`
}
type TcpConnector struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Host    string `json:"host"`
	Port    string `json:"port"`
	Present bool   `json:"present"`
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
	Sites   []Site      `json:"sites"`
	Routers []RouterRef `json:"routers,omitempty"`
}
type Site struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Platform  string `json:"platform"`
	Version   string `json:"version"`
}
type RouterRef struct {
	ID     string `json:"id"`
	SiteID string `json:"siteId"`
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
