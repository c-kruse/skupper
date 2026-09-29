package routercontrol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"

	"github.com/gowebpki/jcs"
)

const digestPrefix = "skupper-router-intent/v1\x00"

const (
	ResourceRouterConnection  = "routerConnection"
	ResourceRouterListener    = "routerListener"
	ResourceServiceListener   = "serviceListener"
	ResourceServiceConnector  = "serviceConnector"
	ResourceCredentialBinding = "credentialBinding"
)

// CanonicalIntent returns the normalized RFC 8785 encoding and its domain-
// separated SHA-256 digest. It does not mutate intent.
func CanonicalIntent(intent RouterIntent) ([]byte, Digest, error) {
	normalized, err := NormalizeIntent(intent)
	if err != nil {
		return nil, "", err
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return nil, "", err
	}
	canonical, err := jcs.Transform(raw)
	if err != nil {
		return nil, "", fmt.Errorf("canonicalize intent: %w", err)
	}
	return canonical, digest(canonical), nil
}

// DecodeIntent rejects duplicate keys, invalid Unicode, unknown fields,
// non-canonical input, invalid domain content, and a digest mismatch.
func DecodeIntent(content []byte, expected Digest) (RouterIntent, error) {
	canonical, err := jcs.Transform(content)
	if err != nil {
		return RouterIntent{}, fmt.Errorf("invalid RFC 8785 input: %w", err)
	}
	if !bytes.Equal(content, canonical) {
		return RouterIntent{}, errors.New("intent is not in canonical RFC 8785 form")
	}
	var intent RouterIntent
	if err := decodeStrict(content, &intent); err != nil {
		return RouterIntent{}, err
	}
	normalized, err := NormalizeIntent(intent)
	if err != nil {
		return RouterIntent{}, err
	}
	encoded, actual, err := CanonicalIntent(normalized)
	if err != nil {
		return RouterIntent{}, err
	}
	if !bytes.Equal(encoded, content) {
		return RouterIntent{}, errors.New("intent is not in normalized form")
	}
	if expected != "" && actual != expected {
		return RouterIntent{}, fmt.Errorf("intent digest mismatch: got %s, want %s", actual, expected)
	}
	return normalized, nil
}

func digest(canonical []byte) Digest {
	sum := sha256.Sum256(append([]byte(digestPrefix), canonical...))
	return Digest(hex.EncodeToString(sum[:]))
}

// NormalizeIntent fills schema defaults, converts nil complete collections to
// empty collections, and sorts sets by stable identity. RoutingKeys retains its
// caller-provided priority order.
func NormalizeIntent(intent RouterIntent) (RouterIntent, error) {
	copy := deepCopyIntent(intent)
	if copy.SchemaVersion == "" {
		copy.SchemaVersion = SchemaVersion
	}
	copy.RouterConnections = nonNil(copy.RouterConnections)
	copy.RouterListeners = nonNil(copy.RouterListeners)
	copy.ServiceListeners = nonNil(copy.ServiceListeners)
	copy.ServiceConnectors = nonNil(copy.ServiceConnectors)
	copy.CredentialBindings = nonNil(copy.CredentialBindings)
	copy.Settings.OwnedAddressKeys = nonNil(copy.Settings.OwnedAddressKeys)
	copy.Settings.Logging = nonNil(copy.Settings.Logging)

	sort.Strings(copy.Settings.OwnedAddressKeys)
	for i := range copy.Settings.Logging {
		if copy.Settings.Logging[i].Module == "DEFAULT" {
			copy.Settings.Logging[i].Module = ""
		}
	}
	sort.Slice(copy.Settings.Logging, func(i, j int) bool {
		return copy.Settings.Logging[i].Module < copy.Settings.Logging[j].Module
	})
	sort.Slice(copy.RouterConnections, func(i, j int) bool { return copy.RouterConnections[i].ID < copy.RouterConnections[j].ID })
	sort.Slice(copy.RouterListeners, func(i, j int) bool { return copy.RouterListeners[i].ID < copy.RouterListeners[j].ID })
	sort.Slice(copy.ServiceListeners, func(i, j int) bool { return copy.ServiceListeners[i].ID < copy.ServiceListeners[j].ID })
	sort.Slice(copy.ServiceConnectors, func(i, j int) bool { return copy.ServiceConnectors[i].ID < copy.ServiceConnectors[j].ID })
	sort.Slice(copy.CredentialBindings, func(i, j int) bool { return copy.CredentialBindings[i].ID < copy.CredentialBindings[j].ID })
	for i := range copy.ServiceListeners {
		copy.ServiceListeners[i].RoutingKeys = nonNil(copy.ServiceListeners[i].RoutingKeys)
	}
	for i := range copy.ServiceConnectors {
		copy.ServiceConnectors[i].Endpoints = nonNil(copy.ServiceConnectors[i].Endpoints)
		sort.Slice(copy.ServiceConnectors[i].Endpoints, func(a, b int) bool {
			return copy.ServiceConnectors[i].Endpoints[a].ID < copy.ServiceConnectors[i].Endpoints[b].ID
		})
	}
	for i := range copy.CredentialBindings {
		copy.CredentialBindings[i].Usages = nonNil(copy.CredentialBindings[i].Usages)
		copy.CredentialBindings[i].Properties = nonNil(copy.CredentialBindings[i].Properties)
		sort.Strings(copy.CredentialBindings[i].Usages)
		sort.Slice(copy.CredentialBindings[i].Properties, func(a, b int) bool {
			return copy.CredentialBindings[i].Properties[a].Name < copy.CredentialBindings[i].Properties[b].Name
		})
	}
	if err := ValidateIntent(copy); err != nil {
		return RouterIntent{}, err
	}
	return copy, nil
}

func deepCopyIntent(intent RouterIntent) RouterIntent {
	copy := intent
	copy.Settings.OwnedAddressKeys = append([]string(nil), intent.Settings.OwnedAddressKeys...)
	copy.Settings.Logging = append([]RouterLogSetting(nil), intent.Settings.Logging...)
	copy.RouterConnections = append([]RouterConnection(nil), intent.RouterConnections...)
	copy.RouterListeners = append([]RouterListener(nil), intent.RouterListeners...)
	copy.ServiceListeners = append([]ServiceListener(nil), intent.ServiceListeners...)
	for i := range copy.ServiceListeners {
		copy.ServiceListeners[i].RoutingKeys = append([]string(nil), intent.ServiceListeners[i].RoutingKeys...)
	}
	copy.ServiceConnectors = append([]ServiceConnector(nil), intent.ServiceConnectors...)
	for i := range copy.ServiceConnectors {
		copy.ServiceConnectors[i].Endpoints = append([]Endpoint(nil), intent.ServiceConnectors[i].Endpoints...)
	}
	copy.CredentialBindings = append([]CredentialBinding(nil), intent.CredentialBindings...)
	for i := range copy.CredentialBindings {
		copy.CredentialBindings[i].Usages = append([]string(nil), intent.CredentialBindings[i].Usages...)
		copy.CredentialBindings[i].Properties = append([]CredentialProperty(nil), intent.CredentialBindings[i].Properties...)
	}
	return copy
}

func nonNil[S ~[]E, E any](value S) S {
	if value == nil {
		return make(S, 0)
	}
	return value
}

// ValidateIntent validates the complete graph, including global resource-ID
// uniqueness and credential references.
func ValidateIntent(intent RouterIntent) error {
	if intent.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schema version %q", intent.SchemaVersion)
	}
	if err := validateTarget(intent.Target); err != nil {
		return err
	}
	if intent.Settings.Mode != RoutingModeInterior && intent.Settings.Mode != RoutingModeEdge {
		return fmt.Errorf("invalid routing mode %q", intent.Settings.Mode)
	}
	if err := uniqueStrings("owned address key", intent.Settings.OwnedAddressKeys); err != nil {
		return err
	}
	if err := validateLogging(intent.Settings.Logging); err != nil {
		return err
	}
	ids := map[ResourceID]string{}
	bindings := map[ResourceID]struct{}{}
	add := func(kind string, id ResourceID) error {
		if id == "" {
			return fmt.Errorf("%s has empty ID", kind)
		}
		if previous, found := ids[id]; found {
			return fmt.Errorf("duplicate resource ID %q in %s and %s", id, previous, kind)
		}
		ids[id] = kind
		return nil
	}
	for _, item := range intent.CredentialBindings {
		if err := add(ResourceCredentialBinding, item.ID); err != nil {
			return err
		}
		if item.Provider == "" || item.Reference == "" {
			return fmt.Errorf("credential binding %q requires provider and reference", item.ID)
		}
		if err := uniqueStrings("credential usage", item.Usages); err != nil {
			return fmt.Errorf("credential binding %q: %w", item.ID, err)
		}
		seen := map[string]struct{}{}
		for _, property := range item.Properties {
			if property.Name == "" {
				return fmt.Errorf("credential binding %q has empty property name", item.ID)
			}
			if _, exists := seen[property.Name]; exists {
				return fmt.Errorf("credential binding %q has duplicate property %q", item.ID, property.Name)
			}
			seen[property.Name] = struct{}{}
		}
		bindings[item.ID] = struct{}{}
	}
	checkTLS := func(owner ResourceID, value TLSIntent) error {
		switch value.Mode {
		case TLSModeDisabled:
			if value.CredentialBinding != "" {
				return fmt.Errorf("resource %q has credential with disabled TLS", owner)
			}
		case TLSModeServer, TLSModeClient, TLSModeMutual:
			if _, found := bindings[value.CredentialBinding]; !found {
				return fmt.Errorf("resource %q references unknown credential binding %q", owner, value.CredentialBinding)
			}
		default:
			return fmt.Errorf("resource %q has invalid TLS mode %q", owner, value.Mode)
		}
		return nil
	}
	for _, item := range intent.RouterConnections {
		if err := add(ResourceRouterConnection, item.ID); err != nil {
			return err
		}
		if item.Host == "" || item.Port == 0 || item.Role == "" {
			return fmt.Errorf("router connection %q requires host, port, and role", item.ID)
		}
		if err := checkTLS(item.ID, item.TLS); err != nil {
			return err
		}
		if item.ProxyCredentialBinding != "" {
			if _, found := bindings[item.ProxyCredentialBinding]; !found {
				return fmt.Errorf("router connection %q references unknown proxy credential binding %q", item.ID, item.ProxyCredentialBinding)
			}
		}
	}
	for _, item := range intent.RouterListeners {
		if err := add(ResourceRouterListener, item.ID); err != nil {
			return err
		}
		if item.Host == "" || item.Port == 0 || item.Role == "" {
			return fmt.Errorf("router listener %q requires host, port, and role", item.ID)
		}
		if err := checkTLS(item.ID, item.TLS); err != nil {
			return err
		}
	}
	for _, item := range intent.ServiceListeners {
		if err := add(ResourceServiceListener, item.ID); err != nil {
			return err
		}
		if item.Host == "" || item.Port == 0 || len(item.RoutingKeys) == 0 {
			return fmt.Errorf("service listener %q requires host, port, and routing keys", item.ID)
		}
		if err := validateProtocol(item.Protocol); err != nil {
			return fmt.Errorf("service listener %q: %w", item.ID, err)
		}
		if err := uniqueStrings("routing key", item.RoutingKeys); err != nil {
			return fmt.Errorf("service listener %q: %w", item.ID, err)
		}
		if err := checkTLS(item.ID, item.TLS); err != nil {
			return err
		}
	}
	for _, item := range intent.ServiceConnectors {
		if err := add(ResourceServiceConnector, item.ID); err != nil {
			return err
		}
		if item.RoutingKey == "" || len(item.Endpoints) == 0 {
			return fmt.Errorf("service connector %q requires routing key and endpoints", item.ID)
		}
		if err := validateProtocol(item.Protocol); err != nil {
			return fmt.Errorf("service connector %q: %w", item.ID, err)
		}
		if err := validateTarget(item.Target); err != nil {
			return fmt.Errorf("service connector %q target: %w", item.ID, err)
		}
		endpointIDs := map[string]struct{}{}
		for _, endpoint := range item.Endpoints {
			if endpoint.ID == "" || endpoint.Host == "" || endpoint.Port == 0 {
				return fmt.Errorf("service connector %q has invalid endpoint", item.ID)
			}
			if _, found := endpointIDs[endpoint.ID]; found {
				return fmt.Errorf("service connector %q has duplicate endpoint %q", item.ID, endpoint.ID)
			}
			endpointIDs[endpoint.ID] = struct{}{}
		}
		if err := checkTLS(item.ID, item.TLS); err != nil {
			return err
		}
	}
	return nil
}

func validateTarget(target TargetIdentity) error {
	if target.NamespaceUID == "" || target.SiteUID == "" || target.RouterGroup == "" {
		return errors.New("target requires namespace UID, site UID, and router group")
	}
	return nil
}

func validateProtocol(protocol Protocol) error {
	switch protocol {
	case ProtocolTCP, ProtocolUDP, ProtocolHTTP, ProtocolHTTP2:
		return nil
	default:
		return fmt.Errorf("invalid protocol %q", protocol)
	}
}

func uniqueStrings(name string, values []string) error {
	seen := map[string]struct{}{}
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("empty %s", name)
		}
		if _, found := seen[value]; found {
			return fmt.Errorf("duplicate %s %q", name, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateLogging(settings []RouterLogSetting) error {
	validModules := map[string]struct{}{
		"": {}, "DEFAULT": {}, "ROUTER": {}, "ROUTER_CORE": {},
		"ROUTER_HELLO": {}, "ROUTER_LS": {}, "ROUTER_MA": {}, "MESSAGE": {},
		"SERVER": {}, "AGENT": {}, "AUTHSERVICE": {}, "CONTAINER": {},
		"ERROR": {}, "POLICY": {}, "HTTP": {}, "CONN_MGR": {}, "PYTHON": {},
		"PROTOCOL": {}, "TCP_ADAPTOR": {}, "HTTP_ADAPTOR": {},
	}
	validLevels := map[string]struct{}{
		"trace": {}, "debug": {}, "info": {}, "notice": {}, "warning": {},
		"error": {}, "critical": {}, "trace+": {}, "debug+": {}, "info+": {},
		"notice+": {}, "warning+": {}, "error+": {}, "critical+": {},
	}
	seen := map[string]struct{}{}
	for _, setting := range settings {
		if _, found := validModules[setting.Module]; !found {
			return fmt.Errorf("invalid router logging module %q", setting.Module)
		}
		if _, found := validLevels[setting.Level]; !found {
			return fmt.Errorf("invalid router logging level %q", setting.Level)
		}
		module := setting.Module
		if module == "DEFAULT" {
			module = ""
		}
		if _, found := seen[module]; found {
			return fmt.Errorf("duplicate router logging module %q", setting.Module)
		}
		seen[module] = struct{}{}
	}
	return nil
}

func decodeStrict(content []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("decode content: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func equal[T any](a, b T) bool { return reflect.DeepEqual(a, b) }
