package status

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/skupperproject/skupper/internal/qdr"
)

type queryCall struct {
	entity, agent string
	offset, count int
}
type fakeQuery struct {
	records    map[string][]qdr.Record
	calls      []queryCall
	failOffset int
}

func (f *fakeQuery) QueryByAgentAddressContext(ctx context.Context, entity string, _ []string, agent string, offset, count int) ([]qdr.Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.calls = append(f.calls, queryCall{entity, agent, offset, count})
	if offset == f.failOffset && f.failOffset > 0 {
		return nil, errors.New("page failed")
	}
	rows := f.records[agent+"|"+entity]
	if count == 0 {
		return rows, nil
	}
	if offset >= len(rows) {
		return nil, nil
	}
	end := offset + count
	if end > len(rows) {
		end = len(rows)
	}
	return rows[offset:end], nil
}

type fakeIndex map[string]Site // router ID -> site

func (f fakeIndex) Sites() []Site {
	var sites []Site
	for _, s := range f {
		sites = append(sites, s)
	}
	return sites
}
func (f fakeIndex) SiteForRouter(routerID string) (Site, bool) {
	s, ok := f[routerID]
	return s, ok
}

func TestBuildDesiredAndInteriorPrefixQuery(t *testing.T) {
	interior := qdr.GetRouterAgentAddress("interior-a", false)
	f := &fakeQuery{records: map[string][]qdr.Record{
		"|io.skupper.router.router":                    {{"id": "edge-a", "mode": "edge"}},
		"|io.skupper.router.connector":                 {{"name": "link", "role": "edge", "connectionStatus": "SUCCESS"}},
		"|io.skupper.router.connection":                {{"host": "west:4567", "dir": "out", "role": "edge", "opened": true, "container": "interior-a"}},
		"|io.skupper.router.tcpListener":               {{"name": "tl", "operStatus": "up"}},
		"|io.skupper.router.listenerAddress":           {},
		"|io.skupper.router.router.address":            {{"name": "Msvc-local", "subscriberCount": 1}},
		interior + "|io.skupper.router.router.address": {{"name": "Msvc-remote", "remoteCount": 1}},
	}}
	desired := &qdr.RouterConfig{Connectors: map[string]qdr.Connector{"link": {Name: "link", Role: "edge", Host: "west", Port: "4567"}, "missing": {Name: "missing", Role: "edge", Host: "none", Port: "1"}}, Bridges: qdr.BridgeConfig{TcpListeners: qdr.TcpEndpointMap{"tl": {Name: "tl", Address: "svc-local"}}, ListenerAddresses: qdr.ListenerAddressMap{}}}
	index := fakeIndex{"interior-a": {ID: "site-west", Name: "west"}}
	doc, err := (Builder{Client: f}).Build(context.Background(), desired, qdr.AdaptorConfig{AddressPrefixes: []qdr.AddressPrefix{{Prefix: "svc-"}}}, index)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Links) != 2 || !doc.Links[0].Present || doc.Links[0].RemoteSiteID != "site-west" || doc.Links[0].RemoteSiteName != "west" || doc.Links[1].Present || doc.Links[1].RemoteSiteID != "" {
		t.Fatalf("links: %#v", doc.Links)
	}
	if len(doc.TcpListeners) != 1 || !doc.TcpListeners[0].Present || doc.TcpListeners[0].OperStatus != "up" || !doc.Addresses[0].Reachable {
		t.Fatalf("listeners/address: %#v %#v", doc.TcpListeners, doc.Addresses)
	}
	if len(doc.Network.Sites) != 1 {
		t.Fatalf("network: %#v", doc.Network)
	}
	if !reflect.DeepEqual(doc.Prefixes[0].Matches, []string{"svc-remote"}) {
		t.Fatalf("prefix: %#v", doc.Prefixes)
	}
	for _, c := range f.calls {
		if c.agent != "" && c.agent != interior {
			t.Fatalf("non-local query routed unexpectedly: %#v", c)
		}
		if c.agent == interior && c.entity != "io.skupper.router.router.address" {
			t.Fatalf("entity routed to interior: %#v", c)
		}
	}
}

func TestAddressPaginationBoundaryAndError(t *testing.T) {
	rows := make([]qdr.Record, 500)
	for i := range rows {
		rows[i] = qdr.Record{"name": fmt.Sprintf("M%03d", i)}
	}
	f := &fakeQuery{records: map[string][]qdr.Record{"|io.skupper.router.router.address": rows}, failOffset: 500}
	_, err := (Builder{Client: f}).queryAddresses(context.Background(), "")
	if err == nil {
		t.Fatal("expected second-page error")
	}
	if got := f.calls[len(f.calls)-1]; got.offset != 500 || got.count != 500 {
		t.Fatalf("last call: %#v", got)
	}
	f.failOffset = 0
	f.records["|io.skupper.router.router.address"] = append(rows, qdr.Record{"name": "Mlast", "inProcess": uint32(1)})
	all, err := (Builder{Client: f}).queryAddresses(context.Background(), "")
	if err != nil || len(all) != 501 || all[500].AsString("name") != "Mlast" {
		t.Fatalf("paginated rows = %d, error = %v", len(all), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = (Builder{Client: f}).queryAddresses(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error: %v", err)
	}
}

func TestConnectionHelpers(t *testing.T) {
	cs := []qdr.Record{
		{"host": "west:4567", "dir": "out", "role": "edge", "opened": true, "container": "router-west"},
		{"host": "east:5678", "dir": "out", "role": "edge", "opened": true, "container": "router-east"},
		{"host": "east:5678", "dir": "out", "role": "edge", "opened": false, "container": "stale"},
	}
	c, ok := connectorConnection(cs, "east", "5678", "edge")
	if !ok || c.AsString("container") != "router-east" {
		t.Fatal(c, ok)
	}
	if _, ok := connectorConnection(cs, "missing", "5678", "edge"); ok {
		t.Fatal("missing connector acquired a peer")
	}
}
func TestPrefixQuerySortsAndTruncates129(t *testing.T) {
	a := map[string]qdr.Record{}
	for i := 128; i >= 0; i-- {
		n := fmt.Sprintf("svc-%03d", i)
		a[n] = qdr.Record{"name": "M" + n, "remoteCount": 1}
	}
	a["svc-unreachable"] = qdr.Record{"name": "Msvc-unreachable"}
	q := prefixQuery("svc-", a)
	if len(q.Matches) != MaxPrefixMatches || !q.Truncated || q.Matches[0] != "svc-000" || q.Matches[127] != "svc-127" {
		t.Fatalf("query: %#v", q)
	}
}

func TestMobileAddressesAndReachability(t *testing.T) {
	addresses := mobileAddresses([]qdr.Record{
		{"name": "Mremote", "remoteCount": uint32(1)},
		{"name": "Llocal", "subscriberCount": uint32(1)},
		{"name": "Min-process", "inProcess": uint64(1)},
		{"name": "Munreachable"},
	})
	if len(addresses) != 3 || !reachable(addresses["remote"]) || !reachable(addresses["in-process"]) || reachable(addresses["unreachable"]) {
		t.Fatalf("mobile reachability: %#v", addresses)
	}
	if routerLinkRole("normal") || routerLinkRole("route-container") || !routerLinkRole("edge") || !routerLinkRole("inter-router") {
		t.Fatal("incorrect router link roles")
	}
}
