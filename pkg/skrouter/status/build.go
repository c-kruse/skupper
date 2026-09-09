package status

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/skupperproject/skupper/pkg/skrouter/config"
	"github.com/skupperproject/skupper/pkg/skrouter/mgmt"
	"github.com/skupperproject/skupper/pkg/skrouter/mgmt/entities"
)

type SiteIndex interface {
	Sites() []Site
	Routers() []RouterRef
	SiteForRouter(string) (Site, bool)
	RouterVersion(string) string
}

type Builder struct {
	Client          *mgmt.Client
	Group, Hostname string
}

func (b Builder) Build(ctx context.Context, target mgmt.Target, desired *config.Document, adaptor AdaptorConfig, applied Applied, index SiteIndex) (Document, error) {
	d := Document{Version: 1, Group: b.Group, Applied: applied, Network: Network{Sites: []Site{}}}
	if index != nil {
		d.Network.Sites = index.Sites()
		d.Network.Routers = index.Routers()
	}
	routers, err := mgmt.Query[entities.Router](ctx, target, mgmt.Fields(entities.RouterId, entities.RouterModeField, entities.RouterVersion, entities.RouterHostName))
	if err != nil {
		return d, err
	}
	if len(routers) > 0 {
		r := routers[0]
		d.Router = Router{ID: r.Id, Mode: string(r.Mode), Version: r.Version, Hostname: r.HostName}
	}
	if b.Hostname != "" {
		d.Router.Hostname = b.Hostname
	}
	// Current routers omit version from management's router entity. The
	// already-retained ROUTER record carries its build version.
	if d.Router.Version == "" && index != nil {
		d.Router.Version = index.RouterVersion(d.Router.ID)
	}
	connectors, err := mgmt.Query[entities.Connector](ctx, target, mgmt.Fields(entities.ConnectorName, entities.ConnectorRole, entities.ConnectorConnectionStatus, entities.ConnectorConnectionMsg))
	if err != nil {
		return d, err
	}
	connections, err := mgmt.Query[entities.Connection](ctx, target, mgmt.Fields(entities.ConnectionHost, entities.ConnectionLocalSocket, entities.ConnectionContainer, entities.ConnectionDir, entities.ConnectionRole, entities.ConnectionOpened, entities.ConnectionProperties))
	if err != nil {
		return d, err
	}
	listeners, err := mgmt.Query[entities.Listener](ctx, target, mgmt.Fields(entities.ListenerName, entities.ListenerRole))
	if err != nil {
		return d, err
	}
	tls, err := mgmt.Query[entities.TcpListener](ctx, target, mgmt.Fields(entities.TcpListenerName, entities.TcpListenerAddress, entities.TcpListenerOperStatus, entities.TcpListenerConnectionMsg))
	if err != nil {
		return d, err
	}
	tcs, err := mgmt.Query[entities.TcpConnector](ctx, target, mgmt.Fields(entities.TcpConnectorName, entities.TcpConnectorAddress, entities.TcpConnectorHost, entities.TcpConnectorPort))
	if err != nil {
		return d, err
	}
	las, err := mgmt.Query[entities.ListenerAddress](ctx, target, mgmt.Fields(entities.ListenerAddressAddress))
	if err != nil {
		return d, err
	}

	cm := map[string]entities.Connector{}
	for _, x := range connectors {
		cm[x.Name] = x
	}
	lm := map[string]entities.Listener{}
	for _, x := range listeners {
		lm[x.Name] = x
	}
	tlm := map[string]entities.TcpListener{}
	for _, x := range tls {
		tlm[x.Name] = x
	}
	tcm := map[string]entities.TcpConnector{}
	for _, x := range tcs {
		tcm[x.Name] = x
	}
	if desired == nil {
		desired = &config.Document{}
	}
	for _, want := range desired.Connectors {
		if !routerLinkRole(want.Value.Role) {
			continue
		}
		x, ok := cm[want.Value.Name]
		role := string(want.Value.Role)
		l := Link{Name: want.Value.Name, Role: role, Present: ok}
		if ok {
			l.ConnectionStatus = x.ConnectionStatus
			l.Message = x.ConnectionMsg
		}
		if c, ok := connectorConnection(connections, want.Value.Host, want.Value.Port, role); ok && l.ConnectionStatus == "SUCCESS" {
			l.RemoteRouterID = c.Container
			if v, ok := c.Properties["qd.access-id"]; ok {
				l.RemoteAccessID = fmt.Sprint(v)
			}
		}
		if index != nil {
			if s, ok := index.SiteForRouter(l.RemoteRouterID); ok {
				l.RemoteSiteID = s.ID
				l.RemoteSiteName = s.Name
			}
		}
		d.Links = append(d.Links, l)
	}
	for _, want := range desired.Listeners {
		if !routerLinkRole(want.Value.Role) {
			continue
		}
		x, ok := lm[want.Value.Name]
		a := RouterAccess{Name: want.Value.Name, Role: string(want.Value.Role), Present: ok}
		if ok {
			a.Peers = listenerPeers(connections, want.Value.Port, string(x.Role))
		}
		sort.Strings(a.Peers)
		d.RouterAccess = append(d.RouterAccess, a)
	}
	addresses := map[string]bool{}
	for _, want := range desired.TcpListeners {
		x, ok := tlm[want.Value.Name]
		d.TcpListeners = append(d.TcpListeners, TcpListener{Name: want.Value.Name, Address: want.Value.Address, Present: ok, OperStatus: string(x.OperStatus), Message: x.ConnectionMsg})
		addresses[want.Value.Address] = true
	}
	for _, want := range desired.ListenerAddresses {
		addresses[want.Value.Address] = true
	}
	for _, want := range desired.TcpConnectors {
		_, ok := tcm[want.Value.Name]
		d.TcpConnectors = append(d.TcpConnectors, TcpConnector{Name: want.Value.Name, Address: want.Value.Address, Host: want.Value.Host, Port: want.Value.Port, Present: ok})
	}
	for _, x := range las {
		addresses[x.Address] = true
	}

	prefixTarget := target
	if d.Router.Mode == "edge" && b.Client != nil {
		for _, c := range connections {
			if string(c.Dir) == "out" && c.Role == "edge" && c.Opened {
				prefixTarget = b.Client.Interior(c.Container)
				break
			}
		}
	}
	localAddresses, err := queryAddresses(ctx, target)
	if err != nil {
		return d, err
	}
	prefixAddresses := localAddresses
	if len(adaptor.AddressPrefixes) > 0 && prefixTarget.Address() != target.Address() {
		prefixAddresses, err = queryAddresses(ctx, prefixTarget)
		if err != nil {
			return d, err
		}
	}
	localByName := mobileAddresses(localAddresses)
	prefixByName := mobileAddresses(prefixAddresses)
	for name := range addresses {
		x := localByName[name]
		d.Addresses = append(d.Addresses, Address{Name: name, Reachable: reachable(x)})
	}
	for _, p := range adaptor.AddressPrefixes {
		d.Prefixes = append(d.Prefixes, prefixQuery(p.Prefix, prefixByName))
	}
	sortDocument(&d)
	return d, nil
}

func mobileAddresses(addresses []entities.RouterAddress) map[string]entities.RouterAddress {
	result := map[string]entities.RouterAddress{}
	for _, address := range addresses {
		if strings.HasPrefix(address.Name, "M") {
			result[strings.TrimPrefix(address.Name, "M")] = address
		}
	}
	return result
}

func reachable(address entities.RouterAddress) bool {
	return address.SubscriberCount+address.RemoteCount+address.InProcess > 0
}

func prefixQuery(prefix string, addresses map[string]entities.RouterAddress) PrefixQuery {
	query := PrefixQuery{Prefix: prefix, Matches: []string{}}
	for name, address := range addresses {
		if strings.HasPrefix(name, prefix) && reachable(address) {
			query.Matches = append(query.Matches, name)
		}
	}
	sort.Strings(query.Matches)
	if len(query.Matches) > MaxPrefixMatches {
		query.Matches = query.Matches[:MaxPrefixMatches]
		query.Truncated = true
	}
	return query
}

func routerLinkRole(role entities.Role) bool {
	return role == entities.Role("edge") || role == entities.Role("inter-router")
}

func connectorConnection(connections []entities.Connection, host, port, role string) (entities.Connection, bool) {
	// Management connection.name is connection/<host:port>, not the configured
	// connector name. Match the destination and role; never borrow another
	// connector's peer merely because it has the same role.
	endpoint := net.JoinHostPort(strings.Trim(host, "[]"), port)
	var result entities.Connection
	found := false
	for _, connection := range connections {
		if connection.Host == endpoint && string(connection.Dir) == "out" && connection.Role == role && connection.Opened {
			if found && result.Container != connection.Container {
				return entities.Connection{}, false
			}
			result, found = connection, true
		}
	}
	return result, found
}

func listenerPeers(connections []entities.Connection, port, role string) []string {
	var peers []string
	for _, connection := range connections {
		_, localPort, err := net.SplitHostPort(connection.LocalSocket)
		if err == nil && localPort == port && string(connection.Dir) == "in" && connection.Role == role && connection.Opened {
			peers = append(peers, connection.Container)
		}
	}
	return peers
}

func queryAddresses(ctx context.Context, target mgmt.Target) ([]entities.RouterAddress, error) {
	var all []entities.RouterAddress
	for offset := 0; ; offset += 500 {
		page, err := mgmt.Query[entities.RouterAddress](ctx, target, mgmt.Fields(entities.RouterAddressName, entities.RouterAddressSubscriberCount, entities.RouterAddressRemoteCount, entities.RouterAddressInProcess), mgmt.Page(offset, 500))
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < 500 {
			return all, nil
		}
	}
}
func sortDocument(d *Document) {
	sort.Slice(d.Links, func(i, j int) bool { return d.Links[i].Name < d.Links[j].Name })
	sort.Slice(d.RouterAccess, func(i, j int) bool { return d.RouterAccess[i].Name < d.RouterAccess[j].Name })
	sort.Slice(d.TcpListeners, func(i, j int) bool { return d.TcpListeners[i].Name < d.TcpListeners[j].Name })
	sort.Slice(d.TcpConnectors, func(i, j int) bool { return d.TcpConnectors[i].Name < d.TcpConnectors[j].Name })
	sort.Slice(d.Addresses, func(i, j int) bool { return d.Addresses[i].Name < d.Addresses[j].Name })
	sort.Slice(d.Prefixes, func(i, j int) bool { return d.Prefixes[i].Prefix < d.Prefixes[j].Prefix })
	sort.Slice(d.Network.Sites, func(i, j int) bool { return d.Network.Sites[i].ID < d.Network.Sites[j].ID })
	sort.Slice(d.Network.Routers, func(i, j int) bool { return d.Network.Routers[i].ID < d.Network.Routers[j].ID })
}
