package main

import (
	"encoding/json"

	"github.com/skupperproject/skupper/pkg/flow/store"
	v1 "github.com/skupperproject/skupper/pkg/flow/v1"
)

type RecordsResponse struct {
	Data  []store.Entry `json:"data"`
	Error string        `json:"error,omitempty"`
	Refs  []string      `json:"refs,omitempty"`
}

type ReplaceResponse struct {
	Error  string `json:"error,omitempty"`
	Status string `json:"status,omitempty"`
}

type ReplaceRequest struct {
	Data []PartialEntry `json:"data"`
}

type PartialEntry struct {
	Meta     store.Metadata
	TypeMeta v1.TypeMeta
	// don't unmarshal until we have the type info from Metadata
	Record json.RawMessage
}
