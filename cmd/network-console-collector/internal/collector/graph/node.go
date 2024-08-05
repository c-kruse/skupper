package graph

import (
	"fmt"

	"github.com/heimdalr/dag"
	"github.com/skupperproject/skupper/cmd/network-console-collector/internal/collector/records"
	"github.com/skupperproject/skupper/pkg/vanflow"
	"github.com/skupperproject/skupper/pkg/vanflow/store"
)

type Graph struct {
	dag  *dag.DAG
	stor store.Interface

	verticies map[string]Node
}

func NewGraph(stor store.Interface) *Graph {
	return &Graph{
		dag:  dag.NewDAG(),
		stor: stor,
	}
}

func (g *Graph) Get(id string) Node {
	v, err := g.dag.GetVertex(id)
	if err != nil {
		return Unknown{}
	}
	return v.(Node)
}

func (g *Graph) Unindex(in vanflow.Record) {
	g.dag.DeleteVertex(in.Identity())
}

func (g *Graph) Reindex(in vanflow.Record) {
	id := in.Identity()
	vertex, _ := g.dag.GetVertex(id)
	switch record := in.(type) {
	case vanflow.SiteRecord:
		if vertex != nil {
			return
		}
		g.dag.AddVertex(Site{g.newBase(id)})
	case vanflow.RouterRecord:
		g.dag.AddVertex(Router{g.newBase(id)})
		var edges []Node
		if record.Parent != nil {
			edges = []Node{Site{g.newBase(*record.Parent)}}
		}
		g.ensureParents(id, edges)
	case vanflow.LinkRecord:
		g.dag.AddVertex(Link{g.newBase(id)})
		var edges []Node
		if record.Parent != nil {
			edges = append(edges, Router{g.newBase(*record.Parent)})
		}
		if record.Peer != nil {
			edges = append(edges, RouterAccess{g.newBase(*record.Peer)})
		}
		g.ensureParents(id, edges)
	case vanflow.RouterAccessRecord:
		g.dag.AddVertex(RouterAccess{g.newBase(id)})
		var edges []Node
		if record.Parent != nil {
			edges = []Node{Router{g.newBase(*record.Parent)}}
		}
		g.ensureParents(id, edges)
	case vanflow.ListenerRecord:
		g.dag.AddVertex(Listener{g.newBase(id)})
		var edges []Node
		if record.Parent != nil {
			edges = append(edges, Router{g.newBase(*record.Parent)})
		}
		if record.Address != nil && record.Protocol != nil {
			edges = append(edges, RoutingKey{g.newBase(RoutingKeyID(*record.Address, *record.Protocol))})
		}
		g.ensureParents(id, edges)
	case vanflow.ProcessRecord:
		g.dag.AddVertex(Process{g.newBase(id)})
		var edges []Node
		if record.Parent != nil {
			edges = append(edges, Site{g.newBase(*record.Parent)})
		}
		g.ensureParents(id, edges)
	case vanflow.ConnectorRecord:
		g.dag.AddVertex(Connector{g.newBase(id)})
		var edges []Node
		if record.Parent != nil {
			edges = append(edges, Router{g.newBase(*record.Parent)})
		}
		if record.Address != nil && record.Protocol != nil {
			edges = append(edges, RoutingKey{g.newBase(RoutingKeyID(*record.Address, *record.Protocol))})
		}
		if record.ProcessID != nil {
			edges = append(edges, Process{g.newBase(*record.ProcessID)})
		}
		g.ensureParents(id, edges)
	case records.AddressRecord:
		addressVtx := Address{g.newBase(id)}
		g.dag.AddVertex(addressVtx)
		routingKeyID := RoutingKeyID(record.Name, record.Protocol)
		g.dag.AddVertex(RoutingKey{g.newBase(routingKeyID)})
		g.ensureParents(routingKeyID, []Node{addressVtx})
	}
}

func (g *Graph) ensureParents(id string, nodes []Node) {
	nm := make(map[string]Node, len(nodes))
	for _, n := range nodes {
		nm[n.ID()] = n
	}
	parents, _ := g.dag.GetParents(id)
	for pID := range parents {
		if _, ok := nm[pID]; ok {
			delete(nm, pID)
			continue
		}
		g.dag.DeleteEdge(pID, id)
	}

	for nID, node := range nm {
		g.dag.AddVertex(node)
		g.dag.AddEdge(nID, id)
	}
}

func (g *Graph) newBase(id string) baseNode {
	return baseNode{
		dag:      g.dag,
		identity: id,
		stor:     g.stor,
	}
}

type Node interface {
	ID() string
	Get() (store.Entry, bool)

	Parent() Node
}

type baseNode struct {
	dag      *dag.DAG
	stor     store.Interface
	identity string
}

func (n baseNode) ID() string {
	return n.identity
}

func (b baseNode) Get() (entry store.Entry, found bool) {
	if b.identity == "" {
		return entry, false
	}
	return b.stor.Get(b.identity)
}

type Unknown struct{ baseNode }

func (u Unknown) Parent() Node {
	return u
}

type Site struct {
	baseNode
}

func (n Site) Parent() Node {
	return Unknown{}
}

func (n Site) Routers() []Router {
	return childrenByType[Router](n.dag, n.identity)
}

func (n Site) Links() []Link {
	var out []Link
	for _, router := range n.Routers() {
		out = append(out, childrenByType[Link](n.dag, router.ID())...)
	}
	return out
}

func typeIs[T Node](n Node) bool {
	_, ok := n.(T)
	return ok
}

func (n Site) RouterAccess() []RouterAccess {
	var out []RouterAccess
	for _, router := range n.Routers() {
		out = append(out, childrenByType[RouterAccess](n.dag, router.ID())...)
	}
	return out
}

func parentOfType[T Node](g *dag.DAG, id string) T {
	var t T
	if g == nil {
		return t
	}
	parents, _ := g.GetParents(id)
	for _, parent := range parents {
		if pt, ok := parent.(T); ok {
			return pt
		}
	}
	return t
}

func childrenByType[T Node](g *dag.DAG, id string) []T {
	var results []T
	if g == nil {
		return results
	}
	children, _ := g.GetChildren(id)
	for _, child := range children {
		if childNode, ok := child.(T); ok {
			results = append(results, childNode)
		}
	}
	return results
}

type Router struct {
	baseNode
}

func (n Router) Parent() Node            { return parentOfType[Site](n.dag, n.identity) }
func (n Router) Listeners() []Listener   { return childrenByType[Listener](n.dag, n.identity) }
func (n Router) Connectors() []Connector { return childrenByType[Connector](n.dag, n.identity) }

type Link struct {
	baseNode
}

func (n Link) Parent() Node { return parentOfType[Router](n.dag, n.identity) }

func (n Link) Peer() Node {
	return parentOfType[RouterAccess](n.dag, n.identity)
}

type RouterAccess struct {
	baseNode
}

func (n RouterAccess) Parent() Node { return parentOfType[Router](n.dag, n.identity) }

func (n RouterAccess) Peers() []Link { return childrenByType[Link](n.dag, n.identity) }

type Listener struct {
	baseNode
}

func (n Listener) Parent() Node  { return parentOfType[Router](n.dag, n.identity) }
func (n Listener) Address() Node { return nil }

type Connector struct {
	baseNode
}

func (n Connector) Parent() Node { return parentOfType[Router](n.dag, n.identity) }

func (n Connector) Address() Address {
	if addr, ok := parentOfType[RoutingKey](n.dag, n.identity).Parent().(Address); ok {
		return addr
	}
	return Address{}
}
func (n Connector) Target() Node { return nil }

type Process struct {
	baseNode
}

func (n Process) Parent() Node            { return parentOfType[Site](n.dag, n.identity) }
func (n Process) Addresses() []Address    { return nil }
func (n Process) Connectors() []Connector { return childrenByType[Connector](n.dag, n.identity) }

func RoutingKeyID(address, protocol string) string {
	return fmt.Sprintf("%s:%s", protocol, address)
}

type RoutingKey struct {
	baseNode
}

func (n RoutingKey) Parent() Node { return parentOfType[Address](n.dag, n.identity) }

type Address struct {
	baseNode
}

func (n Address) Parent() Node            { return Unknown{} }
func (n Address) Connectors() []Connector { return childrenByType[Connector](n.dag, n.identity) }
func (n Address) Listeners() []Listener   { return childrenByType[Listener](n.dag, n.identity) }
