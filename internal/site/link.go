package site

import (
	"reflect"
	"strings"

	"github.com/skupperproject/skupper/internal/qdr"
	"github.com/skupperproject/skupper/internal/sslprofile"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

type Link struct {
	name       string
	profiles   sslprofile.Provider
	definition *skupperv2alpha1.Link
}

func NewLink(name string, profiles sslprofile.Provider) *Link {
	return &Link{
		name:     name,
		profiles: profiles,
	}
}

func (l *Link) Apply(current *qdr.RouterConfig) bool {
	if l.definition == nil {
		return false
	}
	profileName, err := sslprofile.Link(l.profiles, l.definition)
	if err != nil {
		changed, _ := current.RemoveConnector(l.name)
		if l.profiles.Apply(current) {
			changed = true
		}
		return changed
	}
	role := qdr.RoleInterRouter
	if current.IsEdge() {
		role = qdr.RoleEdge
	}
	endpoint, ok := l.definition.Spec.GetEndpointForRole(string(role))
	if !ok {
		return false
	}
	connector := qdr.Connector{
		Name:       l.name,
		Cost:       int32(l.definition.Spec.Cost),
		SslProfile: profileName,
		Role:       role,
		Host:       endpoint.Host,
		Port:       endpoint.Port,
	}
	changed := current.AddConnector(connector)
	if l.profiles.Apply(current) {
		changed = true
	}
	return changed //TODO: optimise by indicating if no change was actually needed
}

type LinkMap map[string]*Link

func (m LinkMap) Apply(current *qdr.RouterConfig) bool {
	for _, config := range m {
		config.Apply(current)
	}
	for _, connector := range current.Connectors {
		if !strings.HasPrefix(connector.Name, "auto-mesh") {
			if _, ok := m[connector.Name]; !ok {
				current.RemoveConnector(connector.Name)
			}
		}
	}
	return true //TODO: can optimise by indicating if no change was required
}

func (link *Link) Update(definition *skupperv2alpha1.Link) bool {
	changed := !reflect.DeepEqual(link.definition, definition)
	link.definition = definition
	return changed
}

func (link *Link) Definition() *skupperv2alpha1.Link {
	return link.definition
}

type RemoveConnector struct {
	name string
}

func (o *RemoveConnector) Apply(current *qdr.RouterConfig) bool {
	if changed, connector := current.RemoveConnector(o.name); changed {
		unreferenced := current.UnreferencedSslProfiles()
		if _, ok := unreferenced[connector.SslProfile]; ok {
			current.RemoveSslProfile(connector.SslProfile)
		}
		return true
	}
	return false
}

func NewRemoveConnector(name string) qdr.ConfigUpdate {
	return &RemoveConnector{
		name: name,
	}
}
