package graph

import (
	"testing"

	"github.com/skupperproject/skupper/pkg/vanflow"
)

func TestGraph(t *testing.T) {
	g := NewGraph(nil)
	g.Reindex(vanflow.RouterRecord{BaseRecord: vanflow.NewBase("router2"), Parent: ptrTo("site2")})
	g.Reindex(vanflow.RouterAccessRecord{BaseRecord: vanflow.NewBase("access2"), Parent: ptrTo("router2")})

	g.Reindex(vanflow.RouterRecord{BaseRecord: vanflow.NewBase("router1"), Parent: ptrTo("site1")})
	g.Reindex(vanflow.LinkRecord{BaseRecord: vanflow.NewBase("link1"), Parent: ptrTo("router1")})
	g.Reindex(vanflow.LinkRecord{BaseRecord: vanflow.NewBase("link2"), Parent: ptrTo("router1"), Peer: ptrTo("access2")})
	g.Reindex(vanflow.RouterAccessRecord{BaseRecord: vanflow.NewBase("access1"), Parent: ptrTo("router1")})
	g.Reindex(vanflow.RouterRecord{BaseRecord: vanflow.NewBase("site1")})

}

func ptrTo[T any](c T) *T {
	return &c
}
