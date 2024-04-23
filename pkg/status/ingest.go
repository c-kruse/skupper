package status

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/c-kruse/vanflow/eventsource"
	"github.com/c-kruse/vanflow/session"
	"github.com/c-kruse/vanflow/store"
	"github.com/skupperproject/skupper/pkg/status/internal/index"
)

func newFlowIngress(factory session.ContainerFactory, stores flowStores) *flowIngest {
	return &flowIngest{
		factory:          factory,
		stores:           stores,
		dispatchRegistry: stores.dispatchRegistry(),
		sources:          make(map[string]clientContext),
		errors:           make(chan error, 1),
		purgeSource:      make(chan store.SourceRef, 32),
		hints:            make(chan eventsource.Info),
	}
}

type clientContext struct {
	cancelCause context.CancelCauseFunc
	sourceRef   store.SourceRef
}

type flowIngest struct {
	factory          session.ContainerFactory
	discovery        *eventsource.Discovery
	stores           flowStores
	dispatchRegistry *store.DispatchRegistry
	ctx              context.Context

	mu      sync.Mutex
	sources map[string]clientContext

	errors      chan error
	purgeSource chan store.SourceRef
	hints       chan eventsource.Info
}

func (f *flowIngest) run(ctx context.Context) error {
	f.ctx = ctx
	discoveryContainer := f.factory.Create()
	discoveryContainer.OnSessionError(func(err error) {
		if _, ok := err.(session.RetryableError); !ok {
			f.errors <- err
			return
		}
		slog.Info("discovery session container error", slog.Any("error", err))
	})
	discoveryContainer.Start(ctx)
	f.discovery = eventsource.NewDiscovery(discoveryContainer, eventsource.DiscoveryOptions{})
	go f.discoverSources(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case purged := <-f.purgeSource:
			total, err := f.removeSourceRecords(purged)
			if err != nil {
				slog.Error("flow ingress error purging source records",
					slog.String("source", purged.String()),
					slog.Any("error", err))
			}
			slog.Info("flow ingress purged records from forgotten source",
				slog.String("source", purged.String()),
				slog.Int("count", total),
			)
		case source := <-f.hints:
			ok := f.discovery.Add(source)
			slog.Info("flow ingress received event source hint",
				slog.String("source", source.ID),
				slog.Bool("isNew", ok),
			)
		case err := <-f.errors:
			return fmt.Errorf("ingress error: %v", err)
		}
	}
}

func (f *flowIngest) removeSourceRecords(source store.SourceRef) (int, error) {
	var purged int
	purge := func(stor store.Interface, source store.SourceRef) error {
		records, err := stor.Index(f.ctx, index.BySource, store.Entry{Meta: store.Metadata{Source: source}}, nil)
		if err != nil {
			return err
		}
		for _, record := range records.Entries {
			if err := stor.Delete(f.ctx, record); err != nil {
				return err
			}
			purged++
		}
		return nil
	}
	if err := purge(f.stores.Sites, source); err != nil {
		return purged, fmt.Errorf("failed to purge source from sites store: %s", err)
	}
	if err := purge(f.stores.Routers, source); err != nil {
		return purged, fmt.Errorf("failed to purge source from routers store: %s", err)
	}
	if err := purge(f.stores.Links, source); err != nil {
		return purged, fmt.Errorf("failed to purge source from links store: %s", err)
	}
	if err := purge(f.stores.Connectors, source); err != nil {
		return purged, fmt.Errorf("failed to purge source from connectors store: %s", err)
	}
	if err := purge(f.stores.Listeners, source); err != nil {
		return purged, fmt.Errorf("failed to purge source from listeners store: %s", err)
	}
	return purged, nil
}

func (f *flowIngest) discoverSources(ctx context.Context) {
	err := f.discovery.Run(ctx, eventsource.DiscoveryHandlers{
		Discovered: f.onDiscoverSource,
		Forgotten:  f.onForgetSource,
	})
	if err != nil && ctx.Err() != nil {
		f.errors <- fmt.Errorf("flow ingress discovery error: %v", err)
	}
}

func (f *flowIngest) onDiscoverSource(source eventsource.Info) {
	clientCtx, cancel := context.WithCancelCause(f.ctx)
	// new container for client
	ctr := f.factory.Create()
	ctr.Start(clientCtx)
	client := eventsource.NewClient(ctr, eventsource.ClientOptions{Source: source})
	// register client with discovery to update lastseen, and monitor for staleness
	err := f.discovery.NewWatchClient(clientCtx, eventsource.WatchConfig{
		Client:      client,
		ID:          source.ID,
		Timeout:     time.Second * 30,
		GracePeriod: time.Second * 30,
	})
	if err != nil {
		slog.Error("status ingress error adding watch client for discovered source", slog.Any("error", err))
		f.discovery.Forget(source.ID)
		return
	}
	sourceRef := store.SourceRef{
		APIVersion: fmt.Sprint(source.Version),
		Type:       source.Type,
		Name:       source.ID,
	}
	// dispatch records to ingress store(s)
	dispatcher := f.dispatchRegistry.NewDispatcher(sourceRef)
	client.OnRecord(dispatcher.Dispatch)
	client.Listen(clientCtx, eventsource.FromSourceAddress())
	if source.Type == controllerType {
		client.Listen(f.ctx, eventsource.FromSourceAddressHeartbeats())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sources[source.ID] = clientContext{
		cancelCause: cancel,
		sourceRef:   sourceRef,
	}
	go func() {
		err := eventsource.FlushOnFirstMessage(clientCtx, client)
		if err != nil {
			slog.Error("error flushing on first message", slog.Any("error", err))
		}
		slog.Info("flush sent for new event source", slog.String("source", source.ID))
	}()
}

func (f *flowIngest) onForgetSource(source eventsource.Info) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cc, ok := f.sources[source.ID]
	if !ok {
		slog.Info("status ignoring discovery forget event for unknown client", slog.String("client", source.ID))
		return
	}
	cc.cancelCause(fmt.Errorf("event source inactive: %w", context.DeadlineExceeded))
	delete(f.sources, source.ID)
	f.purgeSource <- cc.sourceRef
}

type targetHosts struct {
	DestHost   string
	LookupHost string
}
