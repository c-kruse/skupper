package adaptor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/skupperproject/skupper/internal/routercontrol"
)

const intentDigestPrefix = "skupper-router-intent/v1\x00"

// DigestIntent returns the content identity of the normalized domain intent.
// Slices that are sets are sorted by stable ID; routing-key priority is kept.
func DigestIntent(intent routercontrol.RouterIntent) (routercontrol.Digest, error) {
	normalized := intent
	normalizeIntent(&normalized)
	if err := ValidateIntent(normalized); err != nil {
		return "", err
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(intentDigestPrefix), raw...))
	return routercontrol.Digest(hex.EncodeToString(sum[:])), nil
}

func normalizeIntent(intent *routercontrol.RouterIntent) {
	slices.SortFunc(intent.Settings.OwnedAddressKeys, func(a, b string) int { return cmpString(a, b) })
	slices.SortFunc(intent.RouterConnections, func(a, b routercontrol.RouterConnection) int { return cmpString(string(a.ID), string(b.ID)) })
	slices.SortFunc(intent.RouterListeners, func(a, b routercontrol.RouterListener) int { return cmpString(string(a.ID), string(b.ID)) })
	slices.SortFunc(intent.ServiceListeners, func(a, b routercontrol.ServiceListener) int { return cmpString(string(a.ID), string(b.ID)) })
	for i := range intent.ServiceConnectors {
		slices.SortFunc(intent.ServiceConnectors[i].Endpoints, func(a, b routercontrol.Endpoint) int { return cmpString(a.ID, b.ID) })
	}
	slices.SortFunc(intent.ServiceConnectors, func(a, b routercontrol.ServiceConnector) int { return cmpString(string(a.ID), string(b.ID)) })
	for i := range intent.CredentialBindings {
		slices.Sort(intent.CredentialBindings[i].Usages)
		slices.SortFunc(intent.CredentialBindings[i].Properties, func(a, b routercontrol.CredentialProperty) int {
			if c := cmpString(a.Name, b.Name); c != 0 {
				return c
			}
			return cmpString(a.Value, b.Value)
		})
	}
	slices.SortFunc(intent.CredentialBindings, func(a, b routercontrol.CredentialBinding) int { return cmpString(string(a.ID), string(b.ID)) })
}

func cmpString(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func ValidateIntent(intent routercontrol.RouterIntent) error {
	if intent.SchemaVersion != routercontrol.SchemaVersion {
		return fmt.Errorf("unsupported intent schema %q", intent.SchemaVersion)
	}
	if intent.Target.NamespaceUID == "" || intent.Target.SiteUID == "" || intent.Target.RouterGroup == "" {
		return errors.New("intent target is incomplete")
	}
	credentials := map[routercontrol.ResourceID]struct{}{}
	all := map[routercontrol.ResourceID]string{}
	add := func(id routercontrol.ResourceID, kind string) error {
		if id == "" {
			return fmt.Errorf("%s has empty id", kind)
		}
		if previous, found := all[id]; found {
			return fmt.Errorf("duplicate resource id %q (%s and %s)", id, previous, kind)
		}
		all[id] = kind
		return nil
	}
	for _, credential := range intent.CredentialBindings {
		if err := add(credential.ID, "credentialBinding"); err != nil {
			return err
		}
		if credential.Provider == "" || credential.Reference == "" {
			return fmt.Errorf("credential %q has no provider or reference", credential.ID)
		}
		credentials[credential.ID] = struct{}{}
	}
	validateTLS := func(owner routercontrol.ResourceID, tls routercontrol.TLSIntent) error {
		if tls.Mode == "" || tls.Mode == routercontrol.TLSModeDisabled {
			return nil
		}
		if _, found := credentials[tls.CredentialBinding]; !found {
			return fmt.Errorf("resource %q references unknown credential %q", owner, tls.CredentialBinding)
		}
		return nil
	}
	for _, resource := range intent.RouterConnections {
		if err := add(resource.ID, "routerConnection"); err != nil {
			return err
		}
		if resource.Host == "" || resource.Port == 0 {
			return fmt.Errorf("router connection %q has invalid endpoint", resource.ID)
		}
		if err := validateTLS(resource.ID, resource.TLS); err != nil {
			return err
		}
	}
	for _, resource := range intent.RouterListeners {
		if err := add(resource.ID, "routerListener"); err != nil {
			return err
		}
		if resource.Port == 0 {
			return fmt.Errorf("router listener %q has invalid port", resource.ID)
		}
		if err := validateTLS(resource.ID, resource.TLS); err != nil {
			return err
		}
	}
	for _, resource := range intent.ServiceListeners {
		if err := add(resource.ID, "serviceListener"); err != nil {
			return err
		}
		if resource.Port == 0 || len(resource.RoutingKeys) == 0 {
			return fmt.Errorf("service listener %q has no port or routing keys", resource.ID)
		}
		if err := validateTLS(resource.ID, resource.TLS); err != nil {
			return err
		}
	}
	for _, resource := range intent.ServiceConnectors {
		if err := add(resource.ID, "serviceConnector"); err != nil {
			return err
		}
		if resource.RoutingKey == "" || len(resource.Endpoints) == 0 {
			return fmt.Errorf("service connector %q has no routing key or endpoints", resource.ID)
		}
		for _, endpoint := range resource.Endpoints {
			if endpoint.ID == "" || endpoint.Host == "" || endpoint.Port == 0 {
				return fmt.Errorf("service connector %q has invalid endpoint", resource.ID)
			}
		}
		if err := validateTLS(resource.ID, resource.TLS); err != nil {
			return err
		}
	}
	return nil
}

func ApplyDelta(current routercontrol.RouterIntent, accepted routercontrol.Digest, delta routercontrol.IntentDelta) (routercontrol.RouterIntent, routercontrol.Digest, error) {
	if delta.BaseDigest != accepted {
		return current, accepted, fmt.Errorf("delta base %q does not match accepted digest %q", delta.BaseDigest, accepted)
	}
	if delta.Target != current.Target || delta.SchemaVersion != current.SchemaVersion {
		return current, accepted, errors.New("delta target or schema does not match accepted intent")
	}
	next := cloneIntent(current)
	if delta.Settings != nil {
		next.Settings = *delta.Settings
	}
	for _, deletion := range delta.Deletes {
		if err := deleteResource(&next, deletion); err != nil {
			return current, accepted, err
		}
	}
	upsertResources(&next, delta)
	digest, err := DigestIntent(next)
	if err != nil {
		return current, accepted, err
	}
	if digest != delta.ResultDigest {
		return current, accepted, fmt.Errorf("delta result digest %q, computed %q", delta.ResultDigest, digest)
	}
	return next, digest, nil
}

func cloneIntent(intent routercontrol.RouterIntent) routercontrol.RouterIntent {
	raw, _ := json.Marshal(intent)
	var result routercontrol.RouterIntent
	_ = json.Unmarshal(raw, &result)
	return result
}

func deleteResource(intent *routercontrol.RouterIntent, ref routercontrol.ResourceRef) error {
	switch ref.Kind {
	case "routerConnection":
		intent.RouterConnections = deleteByID(intent.RouterConnections, ref.ID, func(v routercontrol.RouterConnection) routercontrol.ResourceID { return v.ID })
	case "routerListener":
		intent.RouterListeners = deleteByID(intent.RouterListeners, ref.ID, func(v routercontrol.RouterListener) routercontrol.ResourceID { return v.ID })
	case "serviceListener":
		intent.ServiceListeners = deleteByID(intent.ServiceListeners, ref.ID, func(v routercontrol.ServiceListener) routercontrol.ResourceID { return v.ID })
	case "serviceConnector":
		intent.ServiceConnectors = deleteByID(intent.ServiceConnectors, ref.ID, func(v routercontrol.ServiceConnector) routercontrol.ResourceID { return v.ID })
	case "credentialBinding":
		intent.CredentialBindings = deleteByID(intent.CredentialBindings, ref.ID, func(v routercontrol.CredentialBinding) routercontrol.ResourceID { return v.ID })
	default:
		return fmt.Errorf("unknown deletion kind %q", ref.Kind)
	}
	return nil
}

func deleteByID[T any](values []T, id routercontrol.ResourceID, get func(T) routercontrol.ResourceID) []T {
	return slices.DeleteFunc(values, func(value T) bool { return get(value) == id })
}

func upsertResources(intent *routercontrol.RouterIntent, delta routercontrol.IntentDelta) {
	for _, value := range delta.RouterConnections {
		intent.RouterConnections = upsertByID(intent.RouterConnections, value, func(v routercontrol.RouterConnection) routercontrol.ResourceID { return v.ID })
	}
	for _, value := range delta.RouterListeners {
		intent.RouterListeners = upsertByID(intent.RouterListeners, value, func(v routercontrol.RouterListener) routercontrol.ResourceID { return v.ID })
	}
	for _, value := range delta.ServiceListeners {
		intent.ServiceListeners = upsertByID(intent.ServiceListeners, value, func(v routercontrol.ServiceListener) routercontrol.ResourceID { return v.ID })
	}
	for _, value := range delta.ServiceConnectors {
		intent.ServiceConnectors = upsertByID(intent.ServiceConnectors, value, func(v routercontrol.ServiceConnector) routercontrol.ResourceID { return v.ID })
	}
	for _, value := range delta.CredentialBindings {
		intent.CredentialBindings = upsertByID(intent.CredentialBindings, value, func(v routercontrol.CredentialBinding) routercontrol.ResourceID { return v.ID })
	}
}

func upsertByID[T any](values []T, value T, get func(T) routercontrol.ResourceID) []T {
	id := get(value)
	for i := range values {
		if get(values[i]) == id {
			values[i] = value
			return values
		}
	}
	return append(values, value)
}
