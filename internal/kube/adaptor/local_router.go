package adaptor

import (
	"errors"
	"fmt"
	"sync"

	"github.com/skupperproject/skupper/internal/qdr"
	"k8s.io/apimachinery/pkg/util/uuid"
)

var ErrRouterRealizationUnverified = errors.New("router management connection has not been verified for the current realization")

// AMQPLocalRouter treats each recovery from a management failure as a new
// router incarnation. QDR does not expose a reliable process-start identity,
// so no Applied or operational evidence survives a management reconnection.
type AMQPLocalRouter struct {
	Pool *qdr.AgentPool

	mu          sync.Mutex
	incarnation string
	verified    string
	connected   bool
}

func (r *AMQPLocalRouter) managementSucceeded() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.connected {
		r.incarnation = string(uuid.NewUUID())
		r.connected = true
		r.verified = ""
	}
	return r.incarnation
}

func (r *AMQPLocalRouter) managementFailed() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connected = false
	r.verified = ""
}

func (r *AMQPLocalRouter) CurrentRouterIncarnation() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.incarnation, r.connected && r.verified == r.incarnation
}

func (r *AMQPLocalRouter) MarkRouterVerified() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.connected {
		r.verified = r.incarnation
	}
	return r.incarnation
}

func (r *AMQPLocalRouter) Read() (*qdr.RouterConfig, error) {
	agent, err := r.Pool.Get()
	if err != nil {
		r.managementFailed()
		return nil, err
	}
	defer r.Pool.Put(agent)
	listeners, err := agent.GetLocalListeners()
	if err != nil {
		r.managementFailed()
		return nil, err
	}
	connectors, err := agent.GetLocalConnectors()
	if err != nil {
		r.managementFailed()
		return nil, err
	}
	bridges, err := agent.GetLocalBridgeConfig()
	if err != nil {
		r.managementFailed()
		return nil, err
	}
	profiles, err := agent.GetSslProfiles()
	if err != nil {
		r.managementFailed()
		return nil, err
	}
	proxyProfiles, err := agent.GetProxyProfiles()
	if err != nil {
		r.managementFailed()
		return nil, err
	}
	r.managementSucceeded()
	return &qdr.RouterConfig{Listeners: listeners, Connectors: connectors, Bridges: *bridges, SslProfiles: profiles, ProxyProfiles: proxyProfiles, Addresses: map[string]qdr.Address{}, LogConfig: map[string]qdr.LogConfig{}}, nil
}

func (r *AMQPLocalRouter) Apply(desired *qdr.RouterConfig) error {
	if err := r.ApplyDependents(desired); err != nil {
		return err
	}
	return r.PruneDependencies(desired)
}

func (r *AMQPLocalRouter) ApplyDependents(desired *qdr.RouterConfig) error {
	if err := qdr.UpsertSslProfilesToRouter(r.Pool, desired.SslProfiles); err != nil {
		r.managementFailed()
		return fmt.Errorf("ssl profiles: %w", err)
	}
	if err := qdr.UpsertProxyProfilesToRouter(r.Pool, desired.ProxyProfiles); err != nil {
		r.managementFailed()
		return fmt.Errorf("proxy profiles: %w", err)
	}
	if err := qdr.SyncBridgeConfig(r.Pool, &desired.Bridges); err != nil {
		if !errors.Is(err, qdr.ErrNotConverged) {
			r.managementFailed()
		}
		return fmt.Errorf("bridge config: %w", err)
	}
	if err := qdr.SyncRouterConfig(r.Pool, desired, true); err != nil {
		r.managementFailed()
		return fmt.Errorf("router config: %w", err)
	}
	return nil
}

func (r *AMQPLocalRouter) PruneDependencies(desired *qdr.RouterConfig) error {
	if err := qdr.PruneSslProfilesFromRouter(r.Pool, desired.SslProfiles); err != nil {
		r.managementFailed()
		return fmt.Errorf("prune ssl profiles: %w", err)
	}
	if err := qdr.PruneProxyProfilesFromRouter(r.Pool, desired.ProxyProfiles); err != nil {
		r.managementFailed()
		return fmt.Errorf("prune proxy profiles: %w", err)
	}
	return nil
}

func (r *AMQPLocalRouter) GetLocalAddresses(wanted map[string]struct{}) ([]qdr.LocalAddress, error) {
	agent, err := r.Pool.Get()
	if err != nil {
		r.managementFailed()
		return nil, err
	}
	addresses, err := agent.GetLocalAddresses(wanted)
	r.Pool.Put(agent)
	if err != nil {
		r.managementFailed()
		return nil, err
	}
	r.managementSucceeded()
	if _, verified := r.CurrentRouterIncarnation(); !verified {
		return nil, ErrRouterRealizationUnverified
	}
	return addresses, nil
}
