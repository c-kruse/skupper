// Package entities contains generated Go representations of Skupper Router
// management entities.
//
//go:generate go run ./internal/generate -schema skrouter.json -overrides overrides.yaml -output .
package entities

import "strconv"

// Port converts a numeric port to the string representation used by the router
// management schema.
func Port(port int) string { return strconv.Itoa(port) }
