package status

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/c-kruse/vanflow"
	"github.com/c-kruse/vanflow/eventsource"
	"github.com/c-kruse/vanflow/session"
	"github.com/c-kruse/vanflow/store"
	"github.com/google/go-cmp/cmp"
	"github.com/skupperproject/skupper/pkg/network"
	"github.com/skupperproject/skupper/pkg/status/internal/index"
	"github.com/skupperproject/skupper/pkg/status/internal/queue"
)

const (
	controllerType = "CONTROLLER"
)

func NewFlowStatusCollector(factory session.ContainerFactory) StatusCollector {
	c := &flowCollector{
		factory:                factory,
		targetHostsByConnector: make(map[string]targetHosts),
		hasNext:                make(chan struct{}, 1),
	}

	c.SiteQ = queue.New[*vanflow.SiteRecord]()
	c.RouterQ = queue.New[*vanflow.RouterRecord]()
	c.LinkQ = queue.New[*vanflow.LinkRecord]()
	c.ConnectorQ = queue.New[*vanflow.ConnectorRecord]()
	c.ListenerQ = queue.New[*vanflow.ListenerRecord]()
	c.ProcessQ = queue.New[*vanflow.ProcessRecord]()

	c.Sites = store.NewDefaultCachingStore(store.CacheConfig{
		Indexers: map[string]store.CacheIndexer{
			index.BySource: index.SourceIndex,
		},
		EventHandlers: c.SiteQ.Handler(),
	})
	c.Routers = store.NewDefaultCachingStore(store.CacheConfig{
		Indexers: map[string]store.CacheIndexer{
			index.BySource: index.SourceIndex,
			index.ByParent: index.ByRouterParentIndexer,
		},
		EventHandlers: c.RouterQ.Handler(),
	})
	c.Links = store.NewDefaultCachingStore(store.CacheConfig{
		Indexers: map[string]store.CacheIndexer{
			index.BySource: index.SourceIndex,
			index.ByParent: index.ByLinkParentIndexer,
		},
		EventHandlers: c.LinkQ.Handler(),
	})
	c.Connectors = store.NewDefaultCachingStore(store.CacheConfig{
		Indexers: map[string]store.CacheIndexer{
			index.BySource:  index.SourceIndex,
			index.ByParent:  index.ByConnectorParentIndexer,
			index.ByAddress: index.ByConnectorAddressIndexer,
		},
		EventHandlers: c.ConnectorQ.Handler(),
	})
	c.Listeners = store.NewDefaultCachingStore(store.CacheConfig{
		Indexers: map[string]store.CacheIndexer{
			index.BySource:  index.SourceIndex,
			index.ByParent:  index.ByListenerParentIndexer,
			index.ByAddress: index.ByListenerAddressIndexer,
		},
		EventHandlers: c.ListenerQ.Handler(),
	})
	c.Processes = store.NewDefaultCachingStore(store.CacheConfig{
		Indexers: map[string]store.CacheIndexer{
			index.BySource: index.SourceIndex,
			index.ByParent: index.ByProcessParentIndexer,
			index.ByHost:   index.ByProcessHostIndexer,
		},
		EventHandlers: c.ProcessQ.Handler(),
	})
	c.ingest = newFlowIngress(c.factory, c.flowStores)
	return c
}

type flowCollector struct {
	factory session.ContainerFactory
	ingest  *flowIngest

	flowStores

	// flowcollector keeps a tiny bit of state about connectors
	// outside of the flow records
	mu                     sync.Mutex
	targetHostsByConnector map[string]targetHosts

	hasNext chan struct{}
}

func (c *flowCollector) HintEventSource(source eventsource.Info) {
	c.ingest.hints <- source
}

func (c *flowCollector) Run(ctx context.Context, changeHandler func(status network.NetworkStatusInfo)) {
	done := make(chan error, 1)
	go func() {
		done <- c.ingest.run(ctx)
	}()
	go c.handleStoreEvents(ctx)
	var prev network.NetworkStatusInfo
	for {
		select {
		case ingressErr := <-done:
			slog.Error("StatusCollector shutdown due to unexpected error: %v", ingressErr)
			return
		case <-ctx.Done():
			return
		case <-c.hasNext:
			next := c.build(ctx)
			if cmp.Equal(prev, next) {
				continue
			}
			prev = next
			changeHandler(next)
		}
	}
}

func (c *flowCollector) build(ctx context.Context) network.NetworkStatusInfo {
	var info network.NetworkStatusInfo
	siteEntries, err := c.Sites.List(ctx, nil)
	if err != nil {
		slog.Error("could not list sites", slog.Any("error", err))
		return info
	}
	sites := make([]vanflow.SiteRecord, 0, len(siteEntries.Entries))
	for _, entry := range siteEntries.Entries {
		sr, ok := entry.Record.(*vanflow.SiteRecord)
		if !ok || sr == nil {
			continue
		}
		sites = append(sites, *sr)
	}
	for _, siteRecord := range sites {
		var siteInfo network.SiteStatusInfo
		siteInfo.Site = asSiteInfo(siteRecord)
		siteID := siteRecord.ID

		routers, err := index.Fetch(ctx, c.Routers, index.ByParent, &vanflow.RouterRecord{Parent: &siteID})
		if err != nil {
			slog.Error("could not list routers", slog.Any("error", err))
			return info
		}
		for _, routerRecord := range routers {
			if routerRecord == nil {
				continue
			}
			var routerInfo network.RouterStatusInfo
			routerInfo.Router = asRouterInfo(*routerRecord)
			routerID := routerRecord.ID

			links, err := index.Fetch(ctx, c.Links, index.ByParent, &vanflow.LinkRecord{Parent: &routerID})
			if err != nil {
				slog.Error("could not list links", slog.Any("error", err))
				return info
			}
			routerInfo.Links = make([]network.LinkInfo, len(links))
			for i, linkRecord := range links {
				if linkRecord == nil {
					continue
				}
				routerInfo.Links[i] = asLinkInfo(*linkRecord)
			}

			connectors, err := index.Fetch(ctx, c.Connectors, index.ByParent, &vanflow.ConnectorRecord{Parent: &routerID})
			if err != nil {
				slog.Error("could not list connectors", slog.Any("error", err))
				return info
			}
			routerInfo.Connectors = make([]network.ConnectorInfo, 0, len(connectors))
			for _, connectorRecord := range connectors {
				if connectorRecord == nil {
					continue
				}
				connector := asConnectorInfo(*connectorRecord)
				c.mu.Lock()
				targets := c.targetHostsByConnector[connectorRecord.ID]
				c.mu.Unlock()

				targetProcesses, err := index.Fetch(ctx, c.Processes, index.ByHost, &vanflow.ProcessRecord{SourceHost: &targets.DestHost, Hostname: &targets.LookupHost})
				if err != nil {
					slog.Error("could not list processes by host", slog.Any("error", err))
					return info
				}
				for _, process := range targetProcesses {
					if process.Name == nil {
						continue
					}
					connector.Target = *process.Name
					break
				}
				routerInfo.Connectors = append(routerInfo.Connectors, connector)
			}
			listeners, err := index.Fetch(ctx, c.Listeners, index.ByParent, &vanflow.ListenerRecord{Parent: &routerID})
			if err != nil {
				slog.Error("could not list listeners", slog.Any("error", err))
				return info
			}
			routerInfo.Listeners = make([]network.ListenerInfo, 0, len(listeners))
			for _, listenerRecord := range listeners {
				if listenerRecord == nil {
					continue
				}
				routerInfo.Listeners = append(routerInfo.Listeners, asListenerInfo(*listenerRecord))
			}
			siteInfo.RouterStatus = append(siteInfo.RouterStatus, routerInfo)
		}
		info.SiteStatus = append(info.SiteStatus, siteInfo)
	}
	addresses, err := c.Connectors.IndexValues(ctx, index.ByAddress)
	if err != nil {
		slog.Error("could not get connector addresses", slog.Any("error", err))
		return info
	}
	for _, address := range addresses {
		connectors, err := index.Fetch(ctx, c.Connectors, index.ByAddress, &vanflow.ConnectorRecord{Address: &address})
		if err != nil {
			slog.Error("could not get connectors for address", slog.Any("error", err), slog.String("address", address))
		}
		listeners, err := index.Fetch(ctx, c.Listeners, index.ByAddress, &vanflow.ListenerRecord{Address: &address})
		if err != nil {
			slog.Error("could not get listeners for address", slog.Any("error", err), slog.String("address", address))
		}
		info.Addresses = append(info.Addresses, asAddressInfo(address, connectors, listeners))
	}
	return info
}

func (a *flowCollector) notify() {
	select {
	case a.hasNext <- struct{}{}:
	default:
	}
}

func isEndTimeSet(endtime *vanflow.Time) bool {
	return endtime != nil && endtime.After(time.Unix(1, 0))
}

func (c *flowCollector) handleStoreEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-c.SiteQ.Events:
			if event.Action != queue.Deleted {
				site := *event.Record
				if isEndTimeSet(site.EndTime) {
					c.Sites.Delete(ctx, store.Entry{Record: site})
				}
			}
			c.notify()
		case event := <-c.RouterQ.Events:
			if event.Action == queue.Changed {
				c.handleRouterChange(ctx, *event.Record)
			}
			c.notify()
		case event := <-c.LinkQ.Events:
			if event.Action == queue.Changed {
				c.handleLinkChange(ctx, *event.Record)
			}
			c.notify()
		case event := <-c.ConnectorQ.Events:
			connector := *event.Record
			if event.Action == queue.Deleted {
				c.handleConnectorDelete(ctx, connector)
			} else {
				c.handleConnectorChange(ctx, connector)
			}
			c.notify()
		case event := <-c.ListenerQ.Events:
			if event.Action == queue.Changed {
				c.handleListenerChange(ctx, *event.Record)
			}
			c.notify()
		case event := <-c.ProcessQ.Events:
			if event.Action == queue.Changed {
				c.handleProcessChange(ctx, *event.Record)
			}
			c.notify()
		}
	}
}

func (c *flowCollector) handleRouterChange(ctx context.Context, router vanflow.RouterRecord) {
	if isEndTimeSet(router.EndTime) {
		c.Routers.Delete(ctx, store.Entry{Record: router})
	}
}

func (c *flowCollector) handleLinkChange(ctx context.Context, link vanflow.LinkRecord) {
	if isEndTimeSet(link.EndTime) {
		c.Links.Delete(ctx, store.Entry{Record: link})
		return
	}
}

func (c *flowCollector) handleConnectorDelete(_ context.Context, connector vanflow.ConnectorRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.targetHostsByConnector, connector.ID)
}

func (c *flowCollector) handleConnectorChange(ctx context.Context, connector vanflow.ConnectorRecord) {
	if isEndTimeSet(connector.EndTime) {
		c.Connectors.Delete(ctx, store.Entry{Record: connector})
		return
	}
	if connector.Parent == nil || connector.DestHost == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	targets := c.targetHostsByConnector[connector.ID]
	if targets.DestHost != *connector.DestHost {
		targets.DestHost = *connector.DestHost
		targets.LookupHost = lookupHost(*connector.DestHost)
	}
	c.targetHostsByConnector[connector.ID] = targets
}

func (c *flowCollector) handleListenerChange(ctx context.Context, listener vanflow.ListenerRecord) {
	if isEndTimeSet(listener.EndTime) {
		c.Listeners.Delete(ctx, store.Entry{Record: listener})
	}
}

func (c *flowCollector) handleProcessChange(ctx context.Context, process vanflow.ProcessRecord) {
	if isEndTimeSet(process.EndTime) {
		c.Processes.Delete(ctx, store.Entry{Record: process})
		return
	}
}

func lookupHost(host string) string {
	ip := net.ParseIP(host)
	if ip != nil {
		return ""
	}
	addrs, err := net.LookupHost(host)
	if err != nil || len(addrs) == 0 {
		return ""
	}
	return addrs[0]
}

type flowStores struct {
	Sites      store.Interface
	SiteQ      queue.Queue[*vanflow.SiteRecord]
	Routers    store.Interface
	RouterQ    queue.Queue[*vanflow.RouterRecord]
	Links      store.Interface
	LinkQ      queue.Queue[*vanflow.LinkRecord]
	Connectors store.Interface
	ConnectorQ queue.Queue[*vanflow.ConnectorRecord]
	Listeners  store.Interface
	ListenerQ  queue.Queue[*vanflow.ListenerRecord]
	Processes  store.Interface
	ProcessQ   queue.Queue[*vanflow.ProcessRecord]
}

func (s flowStores) dispatchRegistry() *store.DispatchRegistry {
	reg := &store.DispatchRegistry{}
	reg.RegisterStore(vanflow.SiteRecord{}, s.Sites)
	reg.RegisterStore(vanflow.RouterRecord{}, s.Routers)
	reg.RegisterStore(vanflow.LinkRecord{}, s.Links)
	reg.RegisterStore(vanflow.ConnectorRecord{}, s.Connectors)
	reg.RegisterStore(vanflow.ListenerRecord{}, s.Listeners)
	reg.RegisterStore(vanflow.ProcessRecord{}, s.Processes)
	return reg
}
