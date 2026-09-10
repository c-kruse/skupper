package status

import (
	"testing"
	"time"

	"github.com/skupperproject/skupper/pkg/vanflow"
	"github.com/skupperproject/skupper/pkg/vanflow/eventsource"
	"github.com/skupperproject/skupper/pkg/vanflow/session"
)

func TestNetworkIndexBuildsStableSiteRouterMapping(t *testing.T) {
	notifications := 0
	index := NewNetworkIndex(session.NewMockContainerFactory(), func() { notifications++ })
	source := eventsource.Info{ID: "source", Version: 1}
	route := eventsource.RecordStoreRouter{Stores: index.mapping, Source: sourceRef(source)}
	end := vanflow.Time{Time: time.Now()}

	route.Route(vanflow.RecordMessage{Records: []vanflow.Record{
		vanflow.SiteRecord{BaseRecord: vanflow.NewBase("site-b"), Name: ptr("Beta")},
		vanflow.SiteRecord{BaseRecord: vanflow.NewBase("site-a"), Name: ptr("Alpha")},
		vanflow.RouterRecord{BaseRecord: vanflow.NewBase("source:0"), Parent: ptr("site-a"), Name: ptr("0/router-a")},
		vanflow.RouterRecord{BaseRecord: vanflow.BaseRecord{ID: "ended", EndTime: &end}, Parent: ptr("site-b"), Name: ptr("0/router-ended")},
		vanflow.LinkRecord{BaseRecord: vanflow.NewBase("ignored")},
	}})

	if notifications != 4 { // SITE and ROUTER records only; LINK is not retained.
		t.Fatalf("got %d notifications, want 4", notifications)
	}
	if got := index.Sites(); len(got) != 2 || got[0] != (Site{ID: "site-a", Name: "Alpha"}) {
		t.Fatalf("unexpected sites: %#v", got)
	}
	if got, ok := index.SiteForRouter("router-a"); !ok || got.ID != "site-a" || got.Name != "Alpha" {
		t.Fatalf("unexpected router site: %#v, %v", got, ok)
	}
	if _, ok := index.SiteForRouter("router-ended"); ok {
		t.Fatal("ended router must not resolve to a site")
	}
	if _, ok := index.SiteForRouter("source:0"); ok {
		t.Fatal("record identity must not be exposed as router ID")
	}
}

func TestNetworkIndexPurgesForgottenSource(t *testing.T) {
	index := NewNetworkIndex(session.NewMockContainerFactory(), nil)
	source := eventsource.Info{ID: "source", Version: 3}
	route := eventsource.RecordStoreRouter{Stores: index.mapping, Source: sourceRef(source)}
	route.Route(vanflow.RecordMessage{Records: []vanflow.Record{
		vanflow.SiteRecord{BaseRecord: vanflow.NewBase("site")},
		vanflow.RouterRecord{BaseRecord: vanflow.NewBase("r:0"), Parent: ptr("site"), Name: ptr("0/router")},
	}})

	index.forgotten(source)
	if got := index.Sites(); len(got) != 0 {
		t.Fatalf("forgotten sites remain: %#v", got)
	}
	if _, ok := index.SiteForRouter("router"); ok {
		t.Fatal("forgotten routers remain")
	}
}

func ptr(value string) *string { return &value }
