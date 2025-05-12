package site

import (
	"strconv"

	"github.com/skupperproject/skupper/internal/qdr"
	"github.com/skupperproject/skupper/internal/sslprofile"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

type RouterAccessMap map[string]*skupperv2alpha1.RouterAccess

func (m RouterAccessMap) desiredListeners(profiles sslprofile.Provider) map[string]qdr.Listener {
	desired := map[string]qdr.Listener{}
	for _, ra := range m {
		profileName, err := sslprofile.RouterAccess(profiles, ra)
		if err != nil {
			continue
		}
		for _, role := range ra.Spec.Roles {
			name := ra.Name + "-" + role.Name
			desired[name] = qdr.Listener{
				Name:             name,
				Role:             qdr.GetRole(role.Name),
				Host:             ra.Spec.BindHost,
				Port:             role.GetPort(),
				SslProfile:       profileName,
				SaslMechanisms:   "EXTERNAL",
				AuthenticatePeer: true,
			}
		}
	}
	return desired
}

func (m RouterAccessMap) desiredConnectors(profiles sslprofile.Provider, targetGroups []string) []qdr.Connector {
	if len(targetGroups) == 0 {
		return nil
	}
	var connectors []qdr.Connector
	if role, ra := m.findInterRouterRole(); role != nil {
		profileName, err := sslprofile.RouterAccess(profiles, ra)
		if err != nil {
			return connectors
		}
		for _, group := range targetGroups {
			name := group
			connector := qdr.Connector{
				Name:       name,
				Host:       group,
				Role:       qdr.RoleInterRouter,
				Port:       strconv.Itoa(role.Port),
				SslProfile: profileName,
			}
			connectors = append(connectors, connector)
		}
	}
	return connectors
}

func (m RouterAccessMap) findInterRouterRole() (*skupperv2alpha1.RouterAccessRole, *skupperv2alpha1.RouterAccess) {
	for _, value := range m {
		if role := value.FindRole("inter-router"); role != nil {
			return role, value
		}
	}
	return nil, nil
}

func (m RouterAccessMap) DesiredConfig(targetGroups []string, profiles sslprofile.Provider) *RouterAccessConfig {
	return &RouterAccessConfig{
		listeners:  m.desiredListeners(profiles),
		connectors: m.desiredConnectors(profiles, targetGroups),
		profiles:   profiles,
	}
}

type RouterAccessConfig struct {
	listeners  map[string]qdr.Listener
	connectors []qdr.Connector
	profiles   sslprofile.Provider
}

func (g *RouterAccessConfig) Apply(config *qdr.RouterConfig) bool {
	changed := false
	lc := qdr.ListenersDifference(config.GetMatchingListeners(qdr.IsNotNormalListener), g.listeners)
	// delete before add with listeners, as changes are handled as delete and add
	for _, value := range lc.Deleted {
		if removed, _ := config.RemoveListener(value.Name); removed {
			delete(config.Listeners, value.Name)
			changed = true
		}
	}
	for _, value := range lc.Added {
		if config.AddListener(value) {
			changed = true
		}
	}
	for _, connector := range g.connectors {
		if config.AddConnector(connector) {
			changed = true
		}
	}
	if g.profiles.Apply(config) {
		changed = true
	}
	return changed
}
