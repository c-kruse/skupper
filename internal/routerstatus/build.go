package status

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/skupperproject/skupper/internal/qdr"
)

type SiteIndex interface {
	Sites() []Site
	Routers() []RouterRef
	SiteForRouter(string) (Site, bool)
	RouterVersion(string) string
}

type QueryClient interface {
	QueryByAgentAddressContext(context.Context, string, []string, string, int, int) ([]qdr.Record, error)
}

type Builder struct {
	Client          QueryClient
	Group, Hostname string
}

func (b Builder) Build(ctx context.Context, desired *qdr.RouterConfig, adaptor qdr.AdaptorConfig, applied Applied, index SiteIndex) (Document, error) {
	d := Document{Version: 1, Group: b.Group, Applied: applied, Network: Network{Sites: []Site{}}}
	if index != nil {
		d.Network.Sites, d.Network.Routers = index.Sites(), index.Routers()
	}
	query := func(entity string, fields []string) ([]qdr.Record, error) {
		return b.Client.QueryByAgentAddressContext(ctx, entity, fields, "", 0, 0)
	}
	routers, err := query("io.skupper.router.router", []string{"id", "mode", "version", "hostName"})
	if err != nil {
		return d, err
	}
	if len(routers) != 0 {
		r := routers[0]
		d.Router = Router{ID: r.AsString("id"), Mode: r.AsString("mode"), Version: r.AsString("version"), Hostname: r.AsString("hostName")}
	}
	if b.Hostname != "" {
		d.Router.Hostname = b.Hostname
	}
	if d.Router.Version == "" && index != nil {
		d.Router.Version = index.RouterVersion(d.Router.ID)
	}
	connectors, err := query("io.skupper.router.connector", []string{"name", "role", "connectionStatus", "connectionMsg"})
	if err != nil {
		return d, err
	}
	connections, err := query("io.skupper.router.connection", []string{"host", "localSocket", "container", "dir", "role", "opened", "properties"})
	if err != nil {
		return d, err
	}
	listeners, err := query("io.skupper.router.listener", []string{"name", "role"})
	if err != nil {
		return d, err
	}
	tls, err := query("io.skupper.router.tcpListener", []string{"name", "address", "operStatus", "connectionMsg"})
	if err != nil {
		return d, err
	}
	tcs, err := query("io.skupper.router.tcpConnector", []string{"name", "address", "host", "port"})
	if err != nil {
		return d, err
	}
	las, err := query("io.skupper.router.listenerAddress", []string{"address"})
	if err != nil {
		return d, err
	}
	byName := func(records []qdr.Record) map[string]qdr.Record {
		m := map[string]qdr.Record{}
		for _, r := range records {
			m[r.AsString("name")] = r
		}
		return m
	}
	cm, lm, tlm, tcm := byName(connectors), byName(listeners), byName(tls), byName(tcs)
	if desired == nil {
		desired = &qdr.RouterConfig{}
	}
	for _, want := range desired.Connectors {
		if !routerLinkRole(want.Role) {
			continue
		}
		x, ok := cm[want.Name]
		role := string(want.Role)
		l := Link{Name: want.Name, Role: role, Present: ok}
		if ok {
			l.ConnectionStatus = x.AsString("connectionStatus")
			l.Message = x.AsString("connectionMsg")
		}
		if c, found := connectorConnection(connections, want.Host, want.Port, role); found && l.ConnectionStatus == "SUCCESS" {
			l.RemoteRouterID = c.AsString("container")
			if id, ok := c.AsRecord("properties")["qd.access-id"]; ok {
				l.RemoteAccessID = fmt.Sprint(id)
			}
		}
		if index != nil {
			if s, found := index.SiteForRouter(l.RemoteRouterID); found {
				l.RemoteSiteID, l.RemoteSiteName = s.ID, s.Name
			}
		}
		d.Links = append(d.Links, l)
	}
	for _, want := range desired.Listeners {
		if !routerLinkRole(want.Role) {
			continue
		}
		x, ok := lm[want.Name]
		a := RouterAccess{Name: want.Name, Role: string(want.Role), Present: ok}
		if ok {
			a.Peers = listenerPeers(connections, strconv.Itoa(int(want.Port)), x.AsString("role"))
			sort.Strings(a.Peers)
		}
		d.RouterAccess = append(d.RouterAccess, a)
	}
	addresses := map[string]bool{}
	for _, want := range desired.Bridges.TcpListeners {
		x, ok := tlm[want.Name]
		d.TcpListeners = append(d.TcpListeners, TcpListener{Name: want.Name, Address: want.Address, Present: ok, OperStatus: x.AsString("operStatus"), Message: x.AsString("connectionMsg")})
		addresses[want.Address] = true
	}
	for _, want := range desired.Bridges.ListenerAddresses {
		addresses[want.Address] = true
	}
	for _, want := range desired.Bridges.TcpConnectors {
		_, ok := tcm[want.Name]
		d.TcpConnectors = append(d.TcpConnectors, TcpConnector{Name: want.Name, Address: want.Address, Host: want.Host, Port: want.Port, Present: ok})
	}
	for _, x := range las {
		addresses[x.AsString("address")] = true
	}
	localAddresses, err := b.queryAddresses(ctx, "")
	if err != nil {
		return d, err
	}
	prefixAddresses := localAddresses
	if d.Router.Mode == qdr.ModeEdge && len(adaptor.AddressPrefixes) > 0 {
		for _, c := range connections {
			if c.AsString("dir") == "out" && c.AsString("role") == "edge" && c.AsBool("opened") {
				prefixAddresses, err = b.queryAddresses(ctx, qdr.GetRouterAgentAddress(c.AsString("container"), false))
				if err != nil {
					return d, err
				}
				break
			}
		}
	}
	localByName, prefixByName := mobileAddresses(localAddresses), mobileAddresses(prefixAddresses)
	for name := range addresses {
		d.Addresses = append(d.Addresses, Address{Name: name, Reachable: reachable(localByName[name])})
	}
	for _, p := range adaptor.AddressPrefixes {
		d.Prefixes = append(d.Prefixes, prefixQuery(p.Prefix, prefixByName))
	}
	sortDocument(&d)
	return d, nil
}

func (b Builder) queryAddresses(ctx context.Context, agent string) ([]qdr.Record, error) {
	var all []qdr.Record
	for offset := 0; ; offset += 500 {
		page, err := b.Client.QueryByAgentAddressContext(ctx, "io.skupper.router.router.address", []string{"name", "subscriberCount", "remoteCount", "inProcess"}, agent, offset, 500)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < 500 {
			return all, nil
		}
	}
}
func mobileAddresses(in []qdr.Record) map[string]qdr.Record {
	out := map[string]qdr.Record{}
	for _, r := range in {
		if n := r.AsString("name"); strings.HasPrefix(n, "M") {
			out[strings.TrimPrefix(n, "M")] = r
		}
	}
	return out
}
func reachable(r qdr.Record) bool {
	return r.AsInt("subscriberCount")+r.AsInt("remoteCount")+r.AsInt("inProcess") > 0
}
func prefixQuery(prefix string, addresses map[string]qdr.Record) PrefixQuery {
	q := PrefixQuery{Prefix: prefix, Matches: []string{}}
	for n, a := range addresses {
		if strings.HasPrefix(n, prefix) && reachable(a) {
			q.Matches = append(q.Matches, n)
		}
	}
	sort.Strings(q.Matches)
	if len(q.Matches) > MaxPrefixMatches {
		q.Matches = q.Matches[:MaxPrefixMatches]
		q.Truncated = true
	}
	return q
}
func routerLinkRole(role qdr.Role) bool {
	return role == qdr.Role("edge") || role == qdr.Role("inter-router")
}
func connectorConnection(cs []qdr.Record, host, port, role string) (qdr.Record, bool) {
	endpoint := net.JoinHostPort(strings.Trim(host, "[]"), port)
	var found qdr.Record
	for _, c := range cs {
		if c.AsString("host") == endpoint && c.AsString("dir") == "out" && c.AsString("role") == role && c.AsBool("opened") {
			if found != nil && found.AsString("container") != c.AsString("container") {
				return nil, false
			}
			found = c
		}
	}
	return found, found != nil
}
func listenerPeers(cs []qdr.Record, port, role string) []string {
	var out []string
	for _, c := range cs {
		_, p, e := net.SplitHostPort(c.AsString("localSocket"))
		if e == nil && p == port && c.AsString("dir") == "in" && c.AsString("role") == role && c.AsBool("opened") {
			out = append(out, c.AsString("container"))
		}
	}
	return out
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
