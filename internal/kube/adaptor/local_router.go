package adaptor

import (
	"fmt"

	"github.com/skupperproject/skupper/internal/qdr"
)

type AMQPLocalRouter struct{ Pool *qdr.AgentPool }

func (r AMQPLocalRouter) Read() (*qdr.RouterConfig, error) {
	agent, err := r.Pool.Get()
	if err != nil {
		return nil, err
	}
	defer r.Pool.Put(agent)
	listeners, err := agent.GetLocalListeners()
	if err != nil {
		return nil, err
	}
	connectors, err := agent.GetLocalConnectors()
	if err != nil {
		return nil, err
	}
	bridges, err := agent.GetLocalBridgeConfig()
	if err != nil {
		return nil, err
	}
	profiles, err := agent.GetSslProfiles()
	if err != nil {
		return nil, err
	}
	proxyProfiles, err := agent.GetProxyProfiles()
	if err != nil {
		return nil, err
	}
	return &qdr.RouterConfig{Listeners: listeners, Connectors: connectors, Bridges: *bridges, SslProfiles: profiles, ProxyProfiles: proxyProfiles, Addresses: map[string]qdr.Address{}, LogConfig: map[string]qdr.LogConfig{}}, nil
}

func (r AMQPLocalRouter) Apply(desired *qdr.RouterConfig) error {
	if err := qdr.SyncSslProfilesToRouter(r.Pool, desired.SslProfiles); err != nil {
		return fmt.Errorf("ssl profiles: %w", err)
	}
	if err := qdr.SyncProxyProfilesToRouter(r.Pool, desired.ProxyProfiles); err != nil {
		return fmt.Errorf("proxy profiles: %w", err)
	}
	if err := qdr.SyncBridgeConfig(r.Pool, &desired.Bridges); err != nil {
		return fmt.Errorf("bridge config: %w", err)
	}
	if err := qdr.SyncRouterConfig(r.Pool, desired, true); err != nil {
		return fmt.Errorf("router config: %w", err)
	}
	return nil
}
