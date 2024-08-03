package collector

import (
	"github.com/heimdalr/dag"
	"github.com/skupperproject/skupper/pkg/vanflow"
)

const maxDepth = 12

// ParentNode does a BFS for the first parent node matching the type parameter
func ParentNode[N any](g *Graph, rootID string) (string, bool) {
	var vID string
	vIDs := []string{rootID}
	for depth := 0; len(vIDs) > 0; depth++ {
		if depth > maxDepth {
			return "", false
		}
		vID, vIDs = vIDs[0], vIDs[1:]

		rootV, err := g.dag.GetVertex(vID)
		if err != nil {
			return "", false
		}
		var isPeer func(e any) bool
		if peerFunc, ok := rootV.(Peer); ok {
			isPeer = peerFunc.Peer
		}
		pMap, err := g.dag.GetParents(vID)
		if err != nil {
			return "", false
		}
		for vID, v := range pMap {
			if _, ok := v.(N); ok {
				return vID, true
			}
			if isPeer != nil && isPeer(v) {
				continue
			}
			vIDs = append(vIDs, vID)
		}
	}
	return "", false
}

// ChildNodes does a BFS for the first parent node matching the type parameter
func ChildNodes[N any](g *Graph, rootID string) []string {
	var nodes []string
	var vID string
	vIDs := []string{rootID}
	for depth := 0; len(vIDs) > 0; depth++ {
		if depth > maxDepth {
			return nodes
		}
		vID, vIDs = vIDs[0], vIDs[1:]

		cMap, err := g.dag.GetChildren(vID)
		if err != nil {
			return nodes
		}
		for vID, v := range cMap {
			if _, ok := v.(N); ok {
				nodes = append(nodes, vID)
				continue
			}
			vIDs = append(vIDs, vID)
		}
	}
	return nodes
}

func NodesByType[N any](g *Graph) []string {
	var nodes []string
	for vID, v := range g.dag.GetVertices() {
		if _, ok := v.(N); ok {
			nodes = append(nodes, vID)
		}
	}
	return nodes
}

type Graph struct {
	dag *dag.DAG
}

func (g *Graph) init() {
	if g.dag == nil {
		g.dag = dag.NewDAG()
	}
}

func (g *Graph) Record(r vanflow.Record) error {
	g.init()
	switch record := r.(type) {
	case vanflow.SiteRecord:
		g.dag.AddVertex(SiteNode(record.ID))
	case vanflow.RouterRecord:
		v, _ := g.dag.AddVertex(RouterNode(record.ID))
		if parent := record.Parent; parent != nil {
			pv, _ := g.dag.AddVertex(SiteNode(*parent))
			g.dag.AddEdge(pv, v)
		}
	case vanflow.LinkRecord:
		v, _ := g.dag.AddVertex(RouterLinkNode(record.ID))
		if parent := record.Parent; parent != nil {
			pv, _ := g.dag.AddVertex(RouterNode(*parent))
			g.dag.AddEdge(pv, v)
		}
		if peer := record.Peer; peer != nil {
			pv, _ := g.dag.AddVertex(RouterAccessNode(*peer))
			if err := g.dag.AddEdge(pv, v); err != nil {
				return err
			}
		}
	case vanflow.RouterAccessRecord:
		v, _ := g.dag.AddVertex(RouterAccessNode(record.ID))
		if parent := record.Parent; parent != nil {
			pv, _ := g.dag.AddVertex(RouterNode(*parent))
			g.dag.AddEdge(pv, v)
		}
	case vanflow.ListenerRecord:
		v, _ := g.dag.AddVertex(ListenerNode(record.ID))
		if parent := record.Parent; parent != nil {
			pv, _ := g.dag.AddVertex(RouterNode(*parent))
			g.dag.AddEdge(pv, v)
		}
		if address := record.Address; address != nil {
			av, _ := g.dag.AddVertex(RoutingKeyNode(*address))
			g.dag.AddEdge(av, v)
		}
	case vanflow.ConnectorRecord:
		v, _ := g.dag.AddVertex(ConnectorNode(record.ID))
		if parent := record.Parent; parent != nil {
			pv, _ := g.dag.AddVertex(RouterNode(*parent))
			g.dag.AddEdge(pv, v)
		}
		if address := record.Address; address != nil {
			av, _ := g.dag.AddVertex(RoutingKeyNode(*address))
			g.dag.AddEdge(av, v)
		}
	case AddressRecord:
		v, _ := g.dag.AddVertex(AddressNode(record.ID))
		rkv, _ := g.dag.AddVertex(RoutingKeyNode(record.Name))
		g.dag.AddEdge(v, rkv)
	}
	return nil
}

func (g *Graph) Remove(r vanflow.Record) error {
	g.init()
	return g.dag.DeleteVertex(r.Identity())
}

type Peer interface {
	Peer(any) bool
}

type SiteNode string

func (n SiteNode) ID() string { return string(n) }

type RouterNode string

func (n RouterNode) ID() string { return string(n) }

type RouterLinkNode string

func (n RouterLinkNode) ID() string { return string(n) }

func (n RouterLinkNode) Peer(p any) bool {
	switch p.(type) {
	case RouterAccessNode:
		return true
	default:
		return false
	}
}

type RouterAccessNode string

func (n RouterAccessNode) ID() string { return string(n) }

type ListenerNode string

func (n ListenerNode) ID() string { return string(n) }

type ConnectorNode string

func (n ConnectorNode) ID() string { return string(n) }

type RoutingKeyNode string

func (n RoutingKeyNode) ID() string { return string(n) }

type AddressNode string

func (n AddressNode) ID() string { return string(n) }
