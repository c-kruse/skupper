package collector

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"hash/fnv"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/skupperproject/skupper/cmd/network-console-collector/internal/collector/graph"
	"github.com/skupperproject/skupper/cmd/network-console-collector/internal/collector/records"
	"github.com/skupperproject/skupper/pkg/vanflow"
	"github.com/skupperproject/skupper/pkg/vanflow/eventsource"
	"github.com/skupperproject/skupper/pkg/vanflow/session"
	"github.com/skupperproject/skupper/pkg/vanflow/store"
	"golang.org/x/sync/errgroup"
)

func New(logger *slog.Logger, factory session.ContainerFactory, reg *prometheus.Registry) *Collector {
	sessionCtr := factory.Create()

	discovery := eventsource.NewDiscovery(sessionCtr, eventsource.DiscoveryOptions{})

	c := &Collector{
		logger:        logger,
		session:       sessionCtr,
		registry:      reg,
		discovery:     discovery,
		sourceRef:     store.SourceRef{Version: "v1alpha1", ID: "collector"},
		recordMapping: make(eventsource.RecordStoreMap),
		purgeQueue:    make(chan store.SourceRef, 8),
		events:        make(chan changeEvent, 32),
		flowEvents:    make(chan changeEvent, 1024),
		clients:       make(map[string]*eventsource.Client),
	}

	c.Records = store.NewSyncMapStore(store.SyncMapStoreConfig{
		Handlers: store.EventHandlerFuncs{
			OnAdd:    c.handleStoreAdd,
			OnChange: c.handleStoreChange,
			OnDelete: c.handleStoreDelete,
		},
		Indexers: map[string]store.Indexer{
			store.SourceIndex:      store.SourceIndexer,
			store.TypeIndex:        store.TypeIndexer,
			IndexByTypeParent:      indexByTypeParent,
			IndexByAddress:         indexByAddress,
			IndexByParentHost:      indexByParentHost,
			IndexByLifecycleStatus: indexByLifecycleStatus,
			IndexByTypeName:        indexByTypeName,
		},
	})

	c.Flows = store.NewSyncMapStore(store.SyncMapStoreConfig{
		Handlers: store.EventHandlerFuncs{
			OnAdd:    c.handleFlowAdd,
			OnChange: c.handleFlowChange,
			OnDelete: c.handleFlowDelete,
		},
		Indexers: map[string]store.Indexer{
			store.SourceIndex: store.SourceIndexer,
			store.TypeIndex:   store.TypeIndexer,
			IndexByTypeParent: indexByTypeParent,
		},
	})

	for _, record := range recordTypes {
		c.recordMapping[record.GetTypeMeta().String()] = c.Records
	}
	c.recordMapping[vanflow.BIFlowTPRecord{}.GetTypeMeta().String()] = c.Flows
	// c.recordMapping[vanflow.BIFlowA{}.GetTypeMeta().String()] = c.flows

	c.g = graph.NewGraph(c.Records)
	c.flowManager = newFlowManager(logger, c.g, c.registry, c.Flows, c.Records)
	return c
}

type Collector struct {
	Records       store.Interface
	Flows         store.Interface
	g             *graph.Graph
	registry      *prometheus.Registry
	recordMapping eventsource.RecordStoreMap

	session   session.Container
	discovery *eventsource.Discovery
	sourceRef store.SourceRef

	logger *slog.Logger

	mu      sync.Mutex
	clients map[string]*eventsource.Client

	flowManager *flowManager

	purgeQueue chan store.SourceRef
	events     chan changeEvent
	flowEvents chan changeEvent
}

type changeEvent interface {
	ID() string
	GetTypeMeta() vanflow.TypeMeta
}

type addEvent struct {
	Record vanflow.Record
}

func (i addEvent) ID() string                    { return i.Record.Identity() }
func (i addEvent) GetTypeMeta() vanflow.TypeMeta { return i.Record.GetTypeMeta() }

type deleteEvent struct {
	Record vanflow.Record
}

func (i deleteEvent) ID() string                    { return i.Record.Identity() }
func (i deleteEvent) GetTypeMeta() vanflow.TypeMeta { return i.Record.GetTypeMeta() }

type updateEvent struct {
	Prev vanflow.Record
	Curr vanflow.Record
}

func (i updateEvent) ID() string                    { return i.Curr.Identity() }
func (i updateEvent) GetTypeMeta() vanflow.TypeMeta { return i.Curr.GetTypeMeta() }

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
		if source.Type == "ROUTER" {
			client.Listen(ctx, eventsource.FromSourceAddressFlows())
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
	g.Go(c.runSession(ctx))
	g.Go(c.runWorkQueue(ctx))
	g.Go(c.runDiscovery(ctx))
	g.Go(c.runRecordCleanup(ctx))
	g.Go(c.flowManager.runFlowPairManager(ctx))
	return g.Wait()
}

func (c *Collector) handleStoreAdd(e store.Entry) {
	select {
	case c.events <- addEvent{Record: e.Record}:
	default:
		c.logger.Error("Store event queue full")
	}
}

func (c *Collector) handleStoreChange(p, e store.Entry) {

	select {
	case c.events <- updateEvent{Prev: p.Record, Curr: e.Record}:
	default:
		c.logger.Error("Store event queue full")
	}
}
func (c *Collector) handleStoreDelete(e store.Entry) {

	select {
	case c.events <- deleteEvent{Record: e.Record}:
	default:
		c.logger.Error("Store event queue full")
	}
}

func (c *Collector) handleFlowAdd(e store.Entry) {

	select {
	case c.flowEvents <- addEvent{Record: e.Record}:
	default:
		c.logger.Error("Flow event queue full")
	}
}

func (c *Collector) handleFlowChange(p, e store.Entry) {
	select {
	case c.flowEvents <- updateEvent{Prev: p.Record, Curr: e.Record}:
	default:
		c.logger.Error("Flow event queue full")
	}
}

func (c *Collector) handleFlowDelete(e store.Entry) {
	c.flowEvents <- deleteEvent{Record: e.Record}
}

func (c *Collector) updateGraph(event changeEvent, stor store.Interface) {
	if dEvent, ok := event.(deleteEvent); ok {
		c.g.Unindex(dEvent.Record)
		return
	}
	entry, ok := stor.Get(event.ID())
	if !ok {
		return
	}
	c.g.Reindex(entry.Record)
}

func (c *Collector) Graph() *graph.Graph {
	return c.g
}

func (c *Collector) FlowInfo(id string) FlowState {
	return c.flowManager.get(id)
}

func (c *Collector) runWorkQueue(ctx context.Context) func() error {
	reactors := map[vanflow.TypeMeta][]func(event changeEvent, stor store.Interface){}
	for _, r := range recordTypes {
		reactors[r.GetTypeMeta()] = append(reactors[r.GetTypeMeta()], c.updateGraph)
	}

	idp := newStableIdentityProvider()
	reactors[records.AddressRecord{}.GetTypeMeta()] = append(reactors[records.AddressRecord{}.GetTypeMeta()], c.updateGraph)
	reactors[vanflow.ConnectorRecord{}.GetTypeMeta()] = append(reactors[vanflow.ConnectorRecord{}.GetTypeMeta()], ensureAddressHandler(idp), ensureSiteServerProcessHandler(idp, c.g))
	reactors[vanflow.ListenerRecord{}.GetTypeMeta()] = append(reactors[vanflow.ListenerRecord{}.GetTypeMeta()], ensureAddressHandler(idp))
	reactors[vanflow.ProcessRecord{}.GetTypeMeta()] = append(reactors[vanflow.ProcessRecord{}.GetTypeMeta()], ensureProcessGroupHandler(idp), ensureSiteServerProcessHandler(idp, c.g))

	reactors[vanflow.BIFlowTPRecord{}.GetTypeMeta()] = append(reactors[vanflow.BIFlowTPRecord{}.GetTypeMeta()], logFlow)

	return func() error {
		defer func() {
			c.logger.Info("queue worker shutdown complete")
		}()
		for {
			select {
			case <-ctx.Done():
				return nil
			case event := <-c.flowEvents:
				c.flowManager.processEvent(event)
			case event := <-c.events:
				for _, reactor := range reactors[event.GetTypeMeta()] {
					reactor(event, c.Records)
				}
			}
		}
	}
}

func dref[T any](p *T) T {
	var t T
	if p != nil {
		return *p
	}
	return t
}

func logFlow(e changeEvent, stor store.Interface) {
}

type idProvider interface {
	ID(prefix string, part string, parts ...string) string
}

func ensureProcessGroupHandler(idp idProvider) func(event changeEvent, stor store.Interface) {
	return func(event changeEvent, stor store.Interface) {
		if _, ok := event.(deleteEvent); ok {
			return
		}
		entry, ok := stor.Get(event.ID())
		if !ok {
			return
		}
		proccess := entry.Record.(vanflow.ProcessRecord)
		if proccess.Group == nil {
			return
		}
		groupName := *proccess.Group
		startTime := time.Now()
		if proccess.StartTime != nil {
			startTime = proccess.StartTime.Time
		}

		groups := stor.Index(IndexByTypeName, store.Entry{Record: records.ProcessGroupRecord{Name: groupName}})
		if len(groups) > 0 {
			return
		}
		id := idp.ID("pg", groupName)
		stor.Add(records.ProcessGroupRecord{ID: id, Name: groupName, Start: startTime}, store.SourceRef{})
	}
}

func ensureSiteServerProcessHandler(idp idProvider, graph *graph.Graph) func(event changeEvent, stor store.Interface) {
	return func(event changeEvent, stor store.Interface) {
		if _, ok := event.(deleteEvent); ok {
			return
		}
		entry, ok := stor.Get(event.ID())
		if !ok {
			return
		}
		switch record := entry.Record.(type) {
		case vanflow.ProcessRecord:
			sourceHost := record.SourceHost
			siteID := record.Parent
			if sourceHost == nil || siteID == nil || entry.Source.ID == "self" {
				slog.Info("skip checking Site proc", slog.Any("id", record.ID))
				return
			}
			stor.Delete(idp.ID("siteproc", *sourceHost, *siteID))
		case vanflow.ConnectorRecord:
			processID := record.ProcessID
			destHost := record.DestHost
			siteID := graph.Connector(record.ID).Parent().Parent().ID()
			if destHost == nil || siteID == "" || (processID != nil && *processID != "") {
				return
			}

			procID := idp.ID("siteproc", *destHost, siteID)
			name := fmt.Sprintf("site-server-%s-%s", *destHost, shortSite(siteID))
			groupName := fmt.Sprintf("site-servers-%s", shortSite(siteID))
			role := "external"
			stor.Add(vanflow.ProcessRecord{
				BaseRecord: vanflow.NewBase(procID, time.Now()),
				Parent:     &siteID,
				Name:       &name,
				Group:      &groupName,
				SourceHost: destHost,
				Mode:       &role,
			}, store.SourceRef{ID: "self"})
		}
	}
}

func shortSite(s string) string {
	return strings.Split(s, "-")[0]
}
func ensureAddressHandler(idp idProvider) func(event changeEvent, stor store.Interface) {
	return func(event changeEvent, stor store.Interface) {
		if _, ok := event.(deleteEvent); ok {
			return
		}
		entry, ok := stor.Get(event.ID())
		if !ok {
			return
		}
		var (
			address   string
			startTime time.Time
			protocol  string = "tcp"
		)
		switch r := entry.Record.(type) {
		case vanflow.ListenerRecord:
			if r.Address != nil {
				address = *r.Address
			}
			if r.StartTime != nil {
				startTime = r.StartTime.Time
			}
			if r.Protocol != nil {
				protocol = *r.Protocol
			}
		case vanflow.ConnectorRecord:
			if r.Address != nil {
				address = *r.Address
			}
			if r.StartTime != nil {
				startTime = r.StartTime.Time
			}
			if r.Protocol != nil {
				protocol = *r.Protocol
			}
		default:
		}
		if address == "" {
			return
		}
		addresses := stor.Index(IndexByTypeName, store.Entry{Record: records.AddressRecord{Name: address}})
		if len(addresses) > 0 {
			return
		}
		addressID := idp.ID("adr", address, protocol)
		stor.Add(records.AddressRecord{ID: addressID, Name: address, Protocol: protocol, Start: startTime}, store.SourceRef{})
	}
}

func (c *Collector) runSession(ctx context.Context) func() error {
	return func() error {
		defer func() {
			c.logger.Info("session shutdown complete")
		}()
		sessionErrors := make(chan error, 1)
		c.session.OnSessionError(func(err error) {
			sessionErrors <- err
		})
		c.session.Start(ctx)
		for {
			select {
			case <-ctx.Done():
				return nil
			case err := <-sessionErrors:
				retryable, ok := err.(session.RetryableError)
				if !ok {
					return fmt.Errorf("unrecoverable session error: %w", err)
				}
				c.logger.Error("session error on collector container",
					slog.Any("error", retryable),
					slog.Duration("delay", retryable.Retry()),
				)

			}
		}
	}
}

func (c *Collector) runDiscovery(ctx context.Context) func() error {
	return func() error {
		defer func() {
			c.logger.Info("discovery shutdown complete")
		}()
		return c.discovery.Run(ctx, eventsource.DiscoveryHandlers{
			Discovered: c.discoveryHandler(ctx),
			Forgotten:  c.handleForgotten,
		})
	}
}

func (c *Collector) runRecordCleanup(ctx context.Context) func() error {
	return func() error {
		defer func() {
			c.logger.Info("record cleanup worker shutdown complete")
		}()
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
	IndexByTypeName        = "ByTypeAndName"
)

func indexByTypeName(e store.Entry) []string {
	optionalSingle := func(prefix string, s *string) []string {
		if s != nil {
			return []string{fmt.Sprintf("%s/%s", prefix, *s)}
		}
		return nil
	}
	switch record := e.Record.(type) {
	case records.AddressRecord:
		return optionalSingle(record.GetTypeMeta().String(), &record.Name)
	case records.ProcessGroupRecord:
		return optionalSingle(record.GetTypeMeta().String(), &record.Name)
	case vanflow.SiteRecord:
		return optionalSingle(record.GetTypeMeta().String(), record.Name)
	case vanflow.RouterRecord:
		return optionalSingle(record.GetTypeMeta().String(), record.Name)
	case vanflow.LinkRecord:
		return optionalSingle(record.GetTypeMeta().String(), record.Name)
	case vanflow.ListenerRecord:
		return optionalSingle(record.GetTypeMeta().String(), record.Name)
	case vanflow.ConnectorRecord:
		return optionalSingle(record.GetTypeMeta().String(), record.Name)
	case vanflow.ProcessRecord:
		return optionalSingle(record.GetTypeMeta().String(), record.Name)
	default:
		return nil
	}
}

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

func sourceRef(source eventsource.Info) store.SourceRef {
	return store.SourceRef{
		Version: fmt.Sprint(source.Version),
		ID:      source.ID,
	}
}

type hashIDer struct {
	mu   sync.Mutex
	hash hash.Hash
	buff []byte
}

func newStableIdentityProvider() idProvider {
	h := fnv.New64()
	return &hashIDer{
		hash: h,
		buff: make([]byte, 0, h.Size()),
	}
}

func (c *hashIDer) ID(prefix string, part string, parts ...string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hash.Reset()
	c.hash.Write([]byte(prefix))
	c.hash.Write([]byte(part))
	for _, p := range parts {
		c.hash.Write([]byte(p))
	}
	sum := c.hash.Sum(c.buff)
	out := bytes.NewBuffer([]byte(prefix + "-"))
	hex.NewEncoder(out).Write(sum)
	return out.String()
}
