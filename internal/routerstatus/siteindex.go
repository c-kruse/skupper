package status

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/skupperproject/skupper/pkg/vanflow"
	"github.com/skupperproject/skupper/pkg/vanflow/eventsource"
	"github.com/skupperproject/skupper/pkg/vanflow/session"
	"github.com/skupperproject/skupper/pkg/vanflow/store"
)

// NetworkIndex maintains the part of the VanFlow network model needed by the
// router status publisher. Its read methods are safe to call while Run is
// receiving updates.
type NetworkIndex struct {
	session   session.Container
	discovery *eventsource.Discovery
	records   store.Interface
	mapping   eventsource.RecordStoreMap
	notify    func()

	mu      sync.Mutex
	ctx     context.Context
	clients map[string]*eventsource.Client
}

func NewNetworkIndex(factory session.ContainerFactory, notify func()) *NetworkIndex {
	container := factory.Create()
	n := &NetworkIndex{
		session:   container,
		discovery: eventsource.NewDiscovery(container, eventsource.DiscoveryOptions{}),
		notify:    notify,
		clients:   map[string]*eventsource.Client{},
	}
	n.records = store.NewSyncMapStore(store.SyncMapStoreConfig{
		Handlers: store.EventHandlerFuncs{
			OnAdd:    func(store.Entry) { n.changed() },
			OnChange: func(store.Entry, store.Entry) { n.changed() },
			OnDelete: func(store.Entry) { n.changed() },
		},
	})
	n.mapping = eventsource.RecordStoreMap{
		(vanflow.SiteRecord{}).GetTypeMeta().String():   n.records,
		(vanflow.RouterRecord{}).GetTypeMeta().String(): n.records,
	}
	return n
}

// Run discovers VanFlow sources and consumes their records until ctx ends.
func (n *NetworkIndex) Run(ctx context.Context) {
	n.mu.Lock()
	n.ctx = ctx
	n.mu.Unlock()
	n.session.Start(ctx)
	_ = n.discovery.Run(ctx, eventsource.DiscoveryHandlers{
		Discovered: n.discovered,
		Forgotten:  n.forgotten,
	})
}

func (n *NetworkIndex) changed() {
	if n.notify != nil {
		n.notify()
	}
}

func (n *NetworkIndex) discovered(source eventsource.Info) {
	n.mu.Lock()
	ctx := n.ctx
	n.mu.Unlock()
	if ctx == nil {
		return
	}

	client := eventsource.NewClient(n.session, eventsource.ClientOptions{Source: source})
	if err := n.discovery.NewWatchClient(ctx, eventsource.WatchConfig{
		Client: client, ID: source.ID,
		Timeout: 30 * time.Second, GracePeriod: 30 * time.Second,
	}); err != nil {
		n.discovery.Forget(source.ID)
		return
	}

	// The eventsource/session receiver API has no AMQP record-type filter. The
	// two-entry router below is therefore the narrowest available subscription:
	// other record types may arrive on the source address but are discarded.
	router := eventsource.RecordStoreRouter{
		Stores: n.mapping,
		Source: sourceRef(source),
	}
	client.OnRecord(router.Route)
	_ = client.Listen(ctx, eventsource.FromSourceAddress())
	if source.Type == "CONTROLLER" {
		_ = client.Listen(ctx, eventsource.FromSourceAddressHeartbeats())
	}

	n.mu.Lock()
	n.clients[source.ID] = client
	n.mu.Unlock()

	go func() {
		flushCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := eventsource.FlushOnFirstMessage(flushCtx, client); err != nil && flushCtx.Err() != nil && ctx.Err() == nil {
			_ = client.SendFlush(ctx)
		}
	}()
}

func (n *NetworkIndex) forgotten(source eventsource.Info) {
	n.mu.Lock()
	client := n.clients[source.ID]
	delete(n.clients, source.ID)
	n.mu.Unlock()
	if client != nil {
		client.Close()
	}

	ref := sourceRef(source)
	for _, entry := range n.records.Index(store.SourceIndex, store.Entry{Metadata: store.Metadata{Source: ref}}) {
		n.records.Delete(entry.Record.Identity())
	}
}

func sourceRef(source eventsource.Info) store.SourceRef {
	return store.SourceRef{ID: source.ID, Version: fmt.Sprint(source.Version)}
}

func (n *NetworkIndex) Sites() []Site {
	var sites []Site
	for _, entry := range n.records.List() {
		record, ok := entry.Record.(vanflow.SiteRecord)
		if !ok || ended(record.EndTime) {
			continue
		}
		sites = append(sites, Site{ID: record.Identity(), Name: stringValue(record.Name)})
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].ID < sites[j].ID })
	return sites
}

// SiteForRouter resolves a router's VanFlow name ("0/<routerID>") to its site.
func (n *NetworkIndex) SiteForRouter(routerID string) (Site, bool) {
	for _, entry := range n.records.List() {
		record, ok := entry.Record.(vanflow.RouterRecord)
		if !ok || ended(record.EndTime) || record.Parent == nil || stringValue(record.Name) != "0/"+routerID {
			continue
		}
		for _, site := range n.Sites() {
			if site.ID == *record.Parent {
				return site, true
			}
		}
		return Site{}, false
	}
	return Site{}, false
}

func ended(end *vanflow.Time) bool {
	return end != nil && !end.Time.IsZero()
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
