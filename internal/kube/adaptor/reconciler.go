package adaptor

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/skupperproject/skupper/internal/qdr"
)

// LocalRouter is deliberately local-only. Implementations must not topology-walk.
type LocalRouter interface {
	Read() (*qdr.RouterConfig, error)
	Apply(*qdr.RouterConfig) error
}

type dependencySafeLocalRouter interface {
	ApplyDependents(*qdr.RouterConfig) error
	PruneDependencies(*qdr.RouterConfig) error
}

type ApplyResult struct {
	Applied bool
	Actual  *qdr.RouterConfig
	Err     error
}

// Reconcile preserves unrelated and protected bootstrap entities, applies only
// the adaptor-owned projection, then verifies the router by a fresh read.
func Reconcile(router LocalRouter, compiled CompiledIntent) ApplyResult {
	actual, err := router.Read()
	if err != nil {
		return ApplyResult{Err: fmt.Errorf("read local router: %w", err)}
	}
	desired := mergeOwned(actual, &compiled.Config)
	if staged, ok := router.(dependencySafeLocalRouter); ok {
		if err := staged.ApplyDependents(desired); err != nil {
			return ApplyResult{Actual: actual, Err: fmt.Errorf("apply local router dependents: %w", err)}
		}
		dependents, err := router.Read()
		if err != nil {
			return ApplyResult{Err: fmt.Errorf("verify local router dependents: %w", err)}
		}
		if err := verifyOwnedDependents(dependents, desired); err != nil {
			return ApplyResult{Actual: dependents, Err: err}
		}
		if err := staged.PruneDependencies(desired); err != nil {
			return ApplyResult{Actual: dependents, Err: fmt.Errorf("prune local router dependencies: %w", err)}
		}
	} else if err := router.Apply(desired); err != nil {
		return ApplyResult{Actual: actual, Err: fmt.Errorf("apply local router: %w", err)}
	}
	verified, err := router.Read()
	if err != nil {
		return ApplyResult{Err: fmt.Errorf("verify local router: %w", err)}
	}
	if err := verifyOwned(verified, desired); err != nil {
		return ApplyResult{Actual: verified, Err: err}
	}
	return ApplyResult{Applied: true, Actual: verified}
}

func mergeOwned(actual, compiled *qdr.RouterConfig) *qdr.RouterConfig {
	desired := cloneRouterConfig(actual)
	removeOwned(&desired)
	for name, value := range compiled.SslProfiles {
		desired.SslProfiles[name] = value
	}
	for name, value := range compiled.ProxyProfiles {
		desired.ProxyProfiles[name] = value
	}
	for name, value := range compiled.Listeners {
		if isOwned(name) {
			desired.Listeners[name] = value
		}
	}
	for name, value := range compiled.Connectors {
		desired.Connectors[name] = value
	}
	for name, value := range compiled.Addresses {
		desired.Addresses[name] = value
	}
	for name, value := range compiled.Bridges.TcpListeners {
		desired.Bridges.TcpListeners[name] = value
	}
	for name, value := range compiled.Bridges.TcpConnectors {
		desired.Bridges.TcpConnectors[name] = value
	}
	for name, value := range compiled.Bridges.ListenerAddresses {
		desired.Bridges.ListenerAddresses[name] = value
	}
	return &desired
}

func removeOwned(config *qdr.RouterConfig) {
	for name := range config.SslProfiles {
		if isOwned(name) {
			delete(config.SslProfiles, name)
		}
	}
	for name := range config.ProxyProfiles {
		if isOwned(name) {
			delete(config.ProxyProfiles, name)
		}
	}
	for name := range config.Listeners {
		if isOwned(name) {
			delete(config.Listeners, name)
		}
	}
	for name := range config.Connectors {
		if isOwned(name) {
			delete(config.Connectors, name)
		}
	}
	for name := range config.Addresses {
		if isOwned(name) {
			delete(config.Addresses, name)
		}
	}
	for name := range config.Bridges.TcpListeners {
		if isOwned(name) {
			delete(config.Bridges.TcpListeners, name)
		}
	}
	for name := range config.Bridges.TcpConnectors {
		if isOwned(name) {
			delete(config.Bridges.TcpConnectors, name)
		}
	}
	for name := range config.Bridges.ListenerAddresses {
		if isOwned(name) {
			delete(config.Bridges.ListenerAddresses, name)
		}
	}
}

func isOwned(name string) bool { return strings.HasPrefix(name, ownedNamePrefix) }

func verifyOwned(actual, desired *qdr.RouterConfig) error {
	return verifyOwnedConfig(actual, desired, true)
}

func verifyOwnedDependents(actual, desired *qdr.RouterConfig) error {
	return verifyOwnedConfig(actual, desired, false)
}

func verifyOwnedConfig(actual, desired *qdr.RouterConfig, profiles bool) error {
	a := cloneRouterConfig(actual)
	d := cloneRouterConfig(desired)
	retainOwned(&a)
	retainOwned(&d)
	normalizeManagedReadback(&a, &d)
	normalizeManagedReadback(&d, &d)
	if !profiles {
		a.SslProfiles = nil
		d.SslProfiles = nil
		a.ProxyProfiles = nil
		d.ProxyProfiles = nil
	}
	if !reflect.DeepEqual(a, d) {
		return fmt.Errorf("local router read-back does not match desired owned configuration")
	}
	return nil
}

func normalizeManagedReadback(config, desired *qdr.RouterConfig) {
	for name, profile := range config.SslProfiles {
		profile.Ordinal = 0
		profile.OldestValidOrdinal = 0
		config.SslProfiles[name] = profile
	}
	for name, profile := range config.ProxyProfiles {
		// QDR marks proxy passwords hidden and does not return the submitted
		// value. The credential revision is reported separately.
		profile.Password = ""
		config.ProxyProfiles[name] = profile
	}
	for name, endpoint := range config.Bridges.TcpListeners {
		endpoint.OperStatus = ""
		endpoint.ConnectionMsg = ""
		if endpoint.Observer == "auto" {
			endpoint.Observer = ""
		}
		if endpoint.Host == "0.0.0.0" && desired.Bridges.TcpListeners[name].Host == "" {
			endpoint.Host = ""
		}
		config.Bridges.TcpListeners[name] = endpoint
	}
	for name, endpoint := range config.Bridges.TcpConnectors {
		endpoint.OperStatus = ""
		endpoint.ConnectionMsg = ""
		if endpoint.Observer == "auto" {
			endpoint.Observer = ""
		}
		if endpoint.VerifyHostname != nil && *endpoint.VerifyHostname && desired.Bridges.TcpConnectors[name].VerifyHostname == nil {
			endpoint.VerifyHostname = nil
		}
		config.Bridges.TcpConnectors[name] = endpoint
	}
	for name, connector := range config.Connectors {
		wanted := desired.Connectors[name]
		connector.ConnectionStatus = ""
		connector.ConnectionMsg = ""
		if wanted.Cost == 0 && connector.Cost == 1 {
			connector.Cost = 0
		}
		if wanted.LinkCapacity == 0 {
			connector.LinkCapacity = 0
		}
		if wanted.MaxFrameSize == 0 {
			connector.MaxFrameSize = 0
		}
		if wanted.MaxSessionFrames == 0 {
			connector.MaxSessionFrames = 0
		}
		config.Connectors[name] = connector
	}
	for name, listener := range config.Listeners {
		wanted := desired.Listeners[name]
		if wanted.Cost == 0 {
			listener.Cost = 0
		}
		if wanted.LinkCapacity == 0 {
			listener.LinkCapacity = 0
		}
		if wanted.MaxFrameSize == 0 {
			listener.MaxFrameSize = 0
		}
		if wanted.MaxSessionFrames == 0 {
			listener.MaxSessionFrames = 0
		}
		listener.Websockets = wanted.Websockets
		listener.Healthz = wanted.Healthz
		listener.Metrics = wanted.Metrics
		config.Listeners[name] = listener
	}
}

func retainOwned(config *qdr.RouterConfig) {
	for name := range config.SslProfiles {
		if !isOwned(name) {
			delete(config.SslProfiles, name)
		}
	}
	for name := range config.ProxyProfiles {
		if !isOwned(name) {
			delete(config.ProxyProfiles, name)
		}
	}
	for name := range config.Listeners {
		if !isOwned(name) {
			delete(config.Listeners, name)
		}
	}
	for name := range config.Connectors {
		if !isOwned(name) {
			delete(config.Connectors, name)
		}
	}
	for name := range config.Addresses {
		if !isOwned(name) {
			delete(config.Addresses, name)
		}
	}
	for name := range config.Bridges.TcpListeners {
		if !isOwned(name) {
			delete(config.Bridges.TcpListeners, name)
		}
	}
	for name := range config.Bridges.TcpConnectors {
		if !isOwned(name) {
			delete(config.Bridges.TcpConnectors, name)
		}
	}
	for name := range config.Bridges.ListenerAddresses {
		if !isOwned(name) {
			delete(config.Bridges.ListenerAddresses, name)
		}
	}
	config.Metadata = qdr.RouterMetadata{}
	config.SiteConfig = nil
	config.LogConfig = nil
}

func cloneRouterConfig(config *qdr.RouterConfig) qdr.RouterConfig {
	copy := *config
	copy.SslProfiles = cloneMap(config.SslProfiles)
	copy.ProxyProfiles = cloneMap(config.ProxyProfiles)
	copy.Listeners = cloneMap(config.Listeners)
	copy.Connectors = cloneMap(config.Connectors)
	copy.Addresses = cloneMap(config.Addresses)
	copy.LogConfig = cloneMap(config.LogConfig)
	copy.Bridges = qdr.NewBridgeConfigCopy(config.Bridges)
	return copy
}

func cloneMap[K comparable, V any](source map[K]V) map[K]V {
	result := make(map[K]V, len(source))
	for k, v := range source {
		result[k] = v
	}
	return result
}
