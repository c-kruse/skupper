package routercontrol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/gowebpki/jcs"
)

// NewIntentDelta computes a complete, base-checked change. The result digest is
// the digest of next, not of the delta envelope.
func NewIntentDelta(base, next RouterIntent) (IntentDelta, error) {
	base, err := NormalizeIntent(base)
	if err != nil {
		return IntentDelta{}, fmt.Errorf("invalid base: %w", err)
	}
	next, err = NormalizeIntent(next)
	if err != nil {
		return IntentDelta{}, fmt.Errorf("invalid result: %w", err)
	}
	if base.Target != next.Target {
		return IntentDelta{}, errors.New("delta target cannot change")
	}
	_, baseDigest, _ := CanonicalIntent(base)
	_, resultDigest, _ := CanonicalIntent(next)
	delta := IntentDelta{
		SchemaVersion: next.SchemaVersion,
		Target:        next.Target,
		BaseDigest:    baseDigest,
		ResultDigest:  resultDigest,
	}
	if !equal(base.Settings, next.Settings) {
		settings := next.Settings
		delta.Settings = &settings
	}
	delta.RouterConnections, delta.Deletes = diffResources(
		base.RouterConnections, next.RouterConnections, ResourceRouterConnection, delta.Deletes,
		func(item RouterConnection) ResourceID { return item.ID },
	)
	delta.RouterListeners, delta.Deletes = diffResources(
		base.RouterListeners, next.RouterListeners, ResourceRouterListener, delta.Deletes,
		func(item RouterListener) ResourceID { return item.ID },
	)
	delta.ServiceListeners, delta.Deletes = diffResources(
		base.ServiceListeners, next.ServiceListeners, ResourceServiceListener, delta.Deletes,
		func(item ServiceListener) ResourceID { return item.ID },
	)
	delta.ServiceConnectors, delta.Deletes = diffResources(
		base.ServiceConnectors, next.ServiceConnectors, ResourceServiceConnector, delta.Deletes,
		func(item ServiceConnector) ResourceID { return item.ID },
	)
	delta.CredentialBindings, delta.Deletes = diffResources(
		base.CredentialBindings, next.CredentialBindings, ResourceCredentialBinding, delta.Deletes,
		func(item CredentialBinding) ResourceID { return item.ID },
	)
	sort.Slice(delta.Deletes, func(i, j int) bool {
		if delta.Deletes[i].Kind == delta.Deletes[j].Kind {
			return delta.Deletes[i].ID < delta.Deletes[j].ID
		}
		return delta.Deletes[i].Kind < delta.Deletes[j].Kind
	})
	return delta, nil
}

func diffResources[T any](base, next []T, kind string, deletes []ResourceRef, id func(T) ResourceID) ([]T, []ResourceRef) {
	baseByID := make(map[ResourceID]T, len(base))
	nextByID := make(map[ResourceID]T, len(next))
	for _, item := range base {
		baseByID[id(item)] = item
	}
	for _, item := range next {
		nextByID[id(item)] = item
	}
	upserts := make([]T, 0, len(next))
	for _, item := range next {
		previous, found := baseByID[id(item)]
		if !found || !equal(previous, item) {
			upserts = append(upserts, item)
		}
	}
	for _, item := range baseByID {
		if _, found := nextByID[id(item)]; !found {
			deletes = append(deletes, ResourceRef{Kind: kind, ID: id(item)})
		}
	}
	return upserts, deletes
}

// CanonicalDelta returns an RFC 8785 encoding after validating its shape.
func CanonicalDelta(delta IntentDelta) ([]byte, error) {
	if err := validateDelta(delta); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(delta)
	if err != nil {
		return nil, err
	}
	canonical, err := jcs.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize delta: %w", err)
	}
	return canonical, nil
}

func DecodeDelta(content []byte) (IntentDelta, error) {
	canonical, err := jcs.Transform(content)
	if err != nil {
		return IntentDelta{}, fmt.Errorf("invalid RFC 8785 input: %w", err)
	}
	if !bytes.Equal(content, canonical) {
		return IntentDelta{}, errors.New("delta is not in canonical RFC 8785 form")
	}
	var delta IntentDelta
	if err := decodeStrict(content, &delta); err != nil {
		return IntentDelta{}, err
	}
	if err := validateDelta(delta); err != nil {
		return IntentDelta{}, err
	}
	return delta, nil
}

func validateDelta(delta IntentDelta) error {
	if delta.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported delta schema version %q", delta.SchemaVersion)
	}
	if err := validateTarget(delta.Target); err != nil {
		return err
	}
	if !validDigest(delta.BaseDigest) || !validDigest(delta.ResultDigest) {
		return errors.New("delta requires valid base and result digests")
	}
	seen := map[ResourceID]string{}
	add := func(kind string, id ResourceID) error {
		if id == "" {
			return fmt.Errorf("%s has empty ID", kind)
		}
		if previous, found := seen[id]; found {
			return fmt.Errorf("delta repeats resource ID %q in %s and %s", id, previous, kind)
		}
		seen[id] = kind
		return nil
	}
	for _, item := range delta.RouterConnections {
		if err := add(ResourceRouterConnection, item.ID); err != nil {
			return err
		}
	}
	for _, item := range delta.RouterListeners {
		if err := add(ResourceRouterListener, item.ID); err != nil {
			return err
		}
	}
	for _, item := range delta.ServiceListeners {
		if err := add(ResourceServiceListener, item.ID); err != nil {
			return err
		}
	}
	for _, item := range delta.ServiceConnectors {
		if err := add(ResourceServiceConnector, item.ID); err != nil {
			return err
		}
	}
	for _, item := range delta.CredentialBindings {
		if err := add(ResourceCredentialBinding, item.ID); err != nil {
			return err
		}
	}
	for _, item := range delta.Deletes {
		switch item.Kind {
		case ResourceRouterConnection, ResourceRouterListener, ResourceServiceListener, ResourceServiceConnector, ResourceCredentialBinding:
		default:
			return fmt.Errorf("unknown deletion kind %q", item.Kind)
		}
		if err := add("deletion", item.ID); err != nil {
			return err
		}
	}
	return nil
}

// ApplyIntentDelta applies a delta to a copy only when its base matches, then
// validates and hashes the full result before returning it.
func ApplyIntentDelta(base RouterIntent, delta IntentDelta) (RouterIntent, error) {
	base, err := NormalizeIntent(base)
	if err != nil {
		return RouterIntent{}, fmt.Errorf("invalid accepted base: %w", err)
	}
	_, baseDigest, _ := CanonicalIntent(base)
	if delta.BaseDigest != baseDigest {
		return RouterIntent{}, fmt.Errorf("delta base mismatch: got %s, accepted %s", delta.BaseDigest, baseDigest)
	}
	if delta.Target != base.Target {
		return RouterIntent{}, errors.New("delta target does not match accepted base")
	}
	if err := validateDelta(delta); err != nil {
		return RouterIntent{}, err
	}
	result := deepCopyIntent(base)
	if delta.Settings != nil {
		result.Settings = *delta.Settings
	}
	for _, deletion := range delta.Deletes {
		found := false
		switch deletion.Kind {
		case ResourceRouterConnection:
			result.RouterConnections, found = deleteResource(result.RouterConnections, deletion.ID, func(item RouterConnection) ResourceID { return item.ID })
		case ResourceRouterListener:
			result.RouterListeners, found = deleteResource(result.RouterListeners, deletion.ID, func(item RouterListener) ResourceID { return item.ID })
		case ResourceServiceListener:
			result.ServiceListeners, found = deleteResource(result.ServiceListeners, deletion.ID, func(item ServiceListener) ResourceID { return item.ID })
		case ResourceServiceConnector:
			result.ServiceConnectors, found = deleteResource(result.ServiceConnectors, deletion.ID, func(item ServiceConnector) ResourceID { return item.ID })
		case ResourceCredentialBinding:
			result.CredentialBindings, found = deleteResource(result.CredentialBindings, deletion.ID, func(item CredentialBinding) ResourceID { return item.ID })
		}
		if !found {
			return RouterIntent{}, fmt.Errorf("delta deletes absent %s %q", deletion.Kind, deletion.ID)
		}
	}
	result.RouterConnections = upsertResources(result.RouterConnections, delta.RouterConnections, func(item RouterConnection) ResourceID { return item.ID })
	result.RouterListeners = upsertResources(result.RouterListeners, delta.RouterListeners, func(item RouterListener) ResourceID { return item.ID })
	result.ServiceListeners = upsertResources(result.ServiceListeners, delta.ServiceListeners, func(item ServiceListener) ResourceID { return item.ID })
	result.ServiceConnectors = upsertResources(result.ServiceConnectors, delta.ServiceConnectors, func(item ServiceConnector) ResourceID { return item.ID })
	result.CredentialBindings = upsertResources(result.CredentialBindings, delta.CredentialBindings, func(item CredentialBinding) ResourceID { return item.ID })
	result, err = NormalizeIntent(result)
	if err != nil {
		return RouterIntent{}, fmt.Errorf("invalid delta result: %w", err)
	}
	_, resultDigest, _ := CanonicalIntent(result)
	if resultDigest != delta.ResultDigest {
		return RouterIntent{}, fmt.Errorf("delta result digest mismatch: got %s, want %s", resultDigest, delta.ResultDigest)
	}
	return result, nil
}

func deleteResource[T any](items []T, id ResourceID, getID func(T) ResourceID) ([]T, bool) {
	result := make([]T, 0, len(items))
	found := false
	for _, item := range items {
		if getID(item) == id {
			found = true
			continue
		}
		result = append(result, item)
	}
	return result, found
}

func upsertResources[T any](items, upserts []T, getID func(T) ResourceID) []T {
	result := append([]T(nil), items...)
	index := make(map[ResourceID]int, len(result))
	for i, item := range result {
		index[getID(item)] = i
	}
	for _, item := range upserts {
		if i, found := index[getID(item)]; found {
			result[i] = item
		} else {
			index[getID(item)] = len(result)
			result = append(result, item)
		}
	}
	return result
}

func validDigest(value Digest) bool {
	if len(value) != sha256HexLength {
		return false
	}
	for _, c := range value {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

const sha256HexLength = 64
