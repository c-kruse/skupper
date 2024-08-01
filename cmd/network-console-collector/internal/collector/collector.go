package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/skupperproject/skupper/pkg/vanflow"
	"github.com/skupperproject/skupper/pkg/vanflow/eventsource"
	"github.com/skupperproject/skupper/pkg/vanflow/session"
	"github.com/skupperproject/skupper/pkg/vanflow/store"
	"golang.org/x/sync/errgroup"
)

func New(logger *slog.Logger, factory session.ContainerFactory) *Collector {
	sessionCtr := factory.Create()
	sessionCtr.OnSessionError(func(err error) {
		logger.Error("session error on collector container", slog.Any("error", err))
	})

	discovery := eventsource.NewDiscovery(sessionCtr, eventsource.DiscoveryOptions{})

	c := &Collector{
		logger:        logger,
		session:       sessionCtr,
		discovery:     discovery,
		recordMapping: make(eventsource.RecordStoreMap),
		purgeQueue:    make(chan store.SourceRef, 8),
		clients:       make(map[string]*eventsource.Client),
		Records: store.NewSyncMapStore(store.SyncMapStoreConfig{
			Indexers: map[string]store.Indexer{
				store.SourceIndex:      store.SourceIndexer,
				store.TypeIndex:        store.TypeIndexer,
				IndexByTypeParent:      indexByTypeParent,
				IndexByAddress:         indexByAddress,
				IndexByParentHost:      indexByParentHost,
				IndexByLifecycleStatus: indexByLifecycleStatus,
			},
		}),
	}
	for _, record := range recordTypes {
		c.recordMapping[record.GetTypeMeta().String()] = c.Records
	}
	// c.recordMapping[vanflow.BIFlowT{}.GetTypeMeta().String()] = c.flows
	// c.recordMapping[vanflow.BIFlowA{}.GetTypeMeta().String()] = c.flows

	return c
}

type Collector struct {
	Records       store.Interface
	recordMapping eventsource.RecordStoreMap

	session   session.Container
	discovery *eventsource.Discovery

	logger *slog.Logger

	mu      sync.Mutex
	clients map[string]*eventsource.Client

	purgeQueue chan store.SourceRef
}

func (c *Collector) discoveryHandler(ctx context.Context) func(eventsource.Info) {
	return func(source eventsource.Info) {
		c.logger.Info("starting client for new source", slog.String("id", source.ID), slog.String("type", source.Type))
		client := eventsource.NewClient(c.session, eventsource.ClientOptions{
			Source: source,
		})

		// register client with discovery to update lastseen, and monitor for staleness
		err := c.discovery.NewWatchClient(ctx, eventsource.WatchConfig{
			Client:      client,
			ID:          source.ID,
			Timeout:     time.Second * 30,
			GracePeriod: time.Second * 30,
		})

		if err != nil {
			c.logger.Error("error creating watcher for discoverd source", slog.Any("error", err))
			c.discovery.Forget(source.ID)
			return
		}

		router := eventsource.RecordStoreRouter{
			Stores: c.recordMapping,
			Source: sourceRef(source),
		}
		client.OnRecord(router.Route)
		client.Listen(ctx, eventsource.FromSourceAddress())
		if source.Type == "CONTROLLER" {
			client.Listen(ctx, eventsource.FromSourceAddressHeartbeats())
		}

		c.mu.Lock()
		defer c.mu.Unlock()
		c.clients[source.ID] = client

		go func() {
			ctx, cancel := context.WithTimeout(ctx, time.Second*5)
			defer cancel()
			if err := eventsource.FlushOnFirstMessage(ctx, client); err != nil {
				if errors.Is(err, ctx.Err()) {
					c.logger.Info("timed out waiting for first message. sending flush anyways")
					err = client.SendFlush(ctx)
				}
				if err != nil {
					c.logger.Error("error sending flush", slog.Any("error", err))
				}
			}
		}()
	}
}

func (c *Collector) handleForgotten(source eventsource.Info) {
	c.logger.Info("handling forgotten source", slog.String("id", source.ID))
	c.mu.Lock()
	defer c.mu.Unlock()
	client, ok := c.clients[source.ID]
	if ok {
		client.Close()
		delete(c.clients, source.ID)
	}
	c.purgeQueue <- sourceRef(source)
}

func (c *Collector) Run(ctx context.Context) error {
	c.session.Start(ctx)
	g, ctx := errgroup.WithContext(ctx)
	g.Go(c.runDiscovery(ctx))
	g.Go(c.runRecordCleanup(ctx))
	return g.Wait()
}

func (c *Collector) runDiscovery(ctx context.Context) func() error {
	return func() error {
		return c.discovery.Run(ctx, eventsource.DiscoveryHandlers{
			Discovered: c.discoveryHandler(ctx),
			Forgotten:  c.handleForgotten,
		})
	}
}

func (c *Collector) runRecordCleanup(ctx context.Context) func() error {
	return func() error {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		terminatedExemplar := store.Entry{
			Record: vanflow.SiteRecord{BaseRecord: vanflow.NewBase("", time.Unix(1, 0), time.Unix(2, 0))},
		}
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				terminated := c.Records.Index(IndexByLifecycleStatus, terminatedExemplar)
				for _, e := range terminated {
					c.Records.Delete(e.Record.Identity())
				}
				if ct := len(terminated); ct > 0 {
					c.logger.Info("purged terminated records",
						slog.Int("count", ct),
					)
				}
			case source := <-c.purgeQueue:
				ct := c.purge(source)
				c.logger.Info("purged records from forgotten source",
					slog.String("source", source.ID),
					slog.Int("count", ct),
				)
			}
		}
	}
}

func (c *Collector) purge(source store.SourceRef) int {
	matching := c.Records.Index(store.SourceIndex, store.Entry{Metadata: store.Metadata{Source: source}})
	for _, record := range matching {
		c.Records.Delete(record.Record.Identity())
	}
	return len(matching)
}

var recordTypes []vanflow.Record = []vanflow.Record{
	vanflow.SiteRecord{},
	vanflow.RouterRecord{},
	vanflow.LinkRecord{},
	vanflow.RouterAccessRecord{},
	vanflow.ConnectorRecord{},
	vanflow.ListenerRecord{},
	vanflow.ProcessRecord{},
}

const (
	IndexByTypeParent      = "ByTypeAndParent"
	IndexByAddress         = "ByAddress"
	IndexByParentHost      = "ByParentHost"
	IndexByLifecycleStatus = "ByLifecycleStatus"
)

func indexByParentHost(e store.Entry) []string {
	if proc, ok := e.Record.(vanflow.ProcessRecord); ok {
		if proc.Parent != nil && proc.SourceHost != nil {
			return []string{fmt.Sprintf("%s/%s", *proc.Parent, *proc.SourceHost)}
		}
	}
	return nil
}
func indexByTypeParent(e store.Entry) []string {
	optionalSingle := func(prefix string, s *string) []string {
		if s != nil {
			return []string{fmt.Sprintf("%s/%s", prefix, *s)}
		}
		return nil
	}
	switch record := e.Record.(type) {
	case vanflow.RouterRecord:
		return optionalSingle(record.GetTypeMeta().String(), record.Parent)
	case vanflow.LinkRecord:
		return optionalSingle(record.GetTypeMeta().String(), record.Parent)
	case vanflow.RouterAccessRecord:
		return optionalSingle(record.GetTypeMeta().String(), record.Parent)
	case vanflow.ConnectorRecord:
		return optionalSingle(record.GetTypeMeta().String(), record.Parent)
	case vanflow.ListenerRecord:
		return optionalSingle(record.GetTypeMeta().String(), record.Parent)
	case vanflow.ProcessRecord:
		return optionalSingle(record.GetTypeMeta().String(), record.Parent)
	default:
		return nil
	}
}
func indexByAddress(e store.Entry) []string {
	optionalSingle := func(s *string) []string {
		if s != nil {
			return []string{*s}
		}
		return nil
	}
	switch record := e.Record.(type) {
	case vanflow.ConnectorRecord:
		return optionalSingle(record.Address)
	case vanflow.ListenerRecord:
		return optionalSingle(record.Address)
	default:
		return nil
	}
}
func indexByLifecycleStatus(e store.Entry) []string {
	lifecycleState := func(b vanflow.BaseRecord) []string {
		var (
			started bool
			ended   bool
		)
		if b.StartTime != nil && b.StartTime.After(time.Unix(0, 0)) {
			started = true
		}
		if b.EndTime != nil && b.EndTime.After(time.Unix(0, 0)) {
			ended = true
		}
		switch {
		case !started && !ended:
			return []string{"INACTIVE"}
		case started && !ended:
			return []string{"ACTIVE"}
		default:
			return []string{"TERMINATED"}
		}
	}
	switch record := e.Record.(type) {
	case vanflow.SiteRecord:
		return lifecycleState(record.BaseRecord)
	case vanflow.RouterRecord:
		return lifecycleState(record.BaseRecord)
	case vanflow.LinkRecord:
		return lifecycleState(record.BaseRecord)
	case vanflow.RouterAccessRecord:
		return lifecycleState(record.BaseRecord)
	case vanflow.ConnectorRecord:
		return lifecycleState(record.BaseRecord)
	case vanflow.ListenerRecord:
		return lifecycleState(record.BaseRecord)
	case vanflow.ProcessRecord:
		return lifecycleState(record.BaseRecord)
	default:
		return nil
	}
}

func listByType[T vanflow.Record](stor store.Interface) []store.Entry {
	var r T
	return ordered(stor.Index(store.TypeIndex, store.Entry{Record: r}))
}

func ordered(entries []store.Entry) []store.Entry {
	sort.Slice(entries, func(i, j int) bool {
		return strings.Compare(entries[i].Record.Identity(), entries[j].Record.Identity()) < 0
	})
	return entries
}

func sourceRef(source eventsource.Info) store.SourceRef {
	return store.SourceRef{
		Version: fmt.Sprint(source.Version),
		ID:      source.ID,
	}
}
