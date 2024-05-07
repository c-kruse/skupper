package main

import (
	"container/list"
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/c-kruse/vanflow"
	"github.com/c-kruse/vanflow/session"
	"github.com/c-kruse/vanflow/store"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type labelSet struct {
	SourceCluster   string
	SourceNamespace string
	SourceService   string
	SourceVersion   string
	DestCluster     string
	DestNamespace   string
	DestService     string
	DestVersion     string
}

func (l labelSet) ToProm() prometheus.Labels {
	return prometheus.Labels{
		"source_cluster":   l.SourceCluster,
		"source_namespace": l.SourceNamespace,
		"source_service":   l.SourceService,
		"source_version":   l.SourceVersion,
		"dest_cluster":     l.DestCluster,
		"dest_namespace":   l.DestNamespace,
		"dest_service":     l.DestService,
		"dest_version":     l.DestVersion,
	}
}

var (
	labels = []string{"extension", "reporter", "reporter_id", "security", "flags",
		"source_cluster", "source_namespace", "source_service", "source_version",
		"dest_cluster", "dest_namespace", "dest_service", "dest_version",
	}
	tcpSentTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kiali_ext_tcp_sent_total",
		Help: "total bytes sent in a TCP connection",
	}, labels)
	tcpReceivedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kiali_ext_tcp_received_total",
		Help: "total bytes received in a TCP connection",
	}, labels)
	tcpOpenedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kiali_ext_tcp_connections_opened_total",
		Help: "total tcp connections opened",
	}, labels)
	tcpClosedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kiali_ext_tcp_connections_closed_total",
		Help: "total tcp connections closed",
	}, labels)
)

func newFlowCollector(factory session.ContainerFactory) (*flowCollector, error) {

	reporterID, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	staticLabels := prometheus.Labels{"extension": "skupper", "reporter_id": reporterID, "reporter": "combined", "security": "plain", "flags": ""}

	collector := &flowCollector{
		vSent:     tcpSentTotal.MustCurryWith(staticLabels),
		vReceived: tcpReceivedTotal.MustCurryWith(staticLabels),
		vOpened:   tcpOpenedTotal.MustCurryWith(staticLabels),
		vClosed:   tcpClosedTotal.MustCurryWith(staticLabels),

		activeFlows:       newFlowsQueue(),
		flowsPendingMatch: newUnmatchedQueue(),
	}

	collector.flows = store.NewDefaultCachingStore(
		store.CacheConfig{
			Indexers: map[string]store.CacheIndexer{},
			EventHandlers: store.EventHandlerFuncs{
				OnAdd:    collector.addFlow,
				OnChange: collector.updateFlow,
			},
		},
	)

	collector.records = store.NewDefaultCachingStore(store.CacheConfig{
		Indexers: map[string]store.CacheIndexer{},
	})

	dispatch := &store.DispatchRegistry{}

	dispatch.RegisterStore(vanflow.FlowRecord{}, collector.flows)
	dispatch.RegisterStore(vanflow.SiteRecord{}, collector.records)
	dispatch.RegisterStore(vanflow.RouterRecord{}, collector.records)
	dispatch.RegisterStore(vanflow.ListenerRecord{}, collector.records)
	dispatch.RegisterStore(vanflow.ConnectorRecord{}, collector.records)
	dispatch.RegisterStore(vanflow.ProcessRecord{}, collector.records)

	collector.ingest = newFlowIngest(factory, dispatch)

	return collector, nil
}

type flowCollector struct {
	vSent     *prometheus.CounterVec
	vReceived *prometheus.CounterVec
	vOpened   *prometheus.CounterVec
	vClosed   *prometheus.CounterVec

	ingest *flowIngest

	records store.Interface // store topology

	flows             store.Interface   // store the actual flow records
	activeFlows       *flowsQueue       // index containing active flow pairs
	flowsPendingMatch *matchmakingQueue // index containing flows pending a matching pair

}

func (c *flowCollector) Run(ctx context.Context) error {
	go c.ingest.run(ctx)
	<-ctx.Done()
	return nil
}

func (c *flowCollector) addFlow(entry store.Entry) {
	c.updateFlow(store.Entry{}, entry)
}

func (c *flowCollector) updateFlow(prev, entry store.Entry) {
	flow := entry.Record.(*vanflow.FlowRecord)
	slog.Debug("flow update", slog.String("ID", flow.Identity()), slog.Any("meta", entry.Meta))

	pair, ok := c.activeFlows.Get(flow.ID)
	if !ok {
		pair, ok = c.flowsPendingMatch.MatchFlows(flow.ID, flow.Counterflow)
		if !ok {
			slog.Debug("adding pending flow pair match", slog.Any("counterflow", flow.Counterflow), slog.String("flow", flow.ID))
			c.flowsPendingMatch.Push(flow.ID, flow.Counterflow)
			return
		}
		labelset, source, dest, err := c.resolvePair(pair)
		if err != nil {
			slog.Debug("error resolving pair labels", slog.Any("counterflow", flow.Counterflow), slog.String("flow", flow.ID), slog.Any("error", err))
			return
		}
		pair.Labels = labelset
		c.activeFlows.Push(pair)
		if opened, err := c.vOpened.GetMetricWith(labelset.ToProm()); err == nil {
			opened.Inc()
		}
		if sent, err := c.vSent.GetMetricWith(labelset.ToProm()); err == nil {
			if source.Octets != nil {
				sent.Add(float64(*source.Octets))
			}
		}
		if received, err := c.vReceived.GetMetricWith(labelset.ToProm()); err == nil {
			if dest.Octets != nil {
				received.Add(float64(*dest.Octets))
			}
		}
	}

	if pf, ok := prev.Record.(*vanflow.FlowRecord); ok {
		var (
			octetsP uint64
			octetsC uint64
		)
		if pf.Octets != nil {
			octetsP = *pf.Octets
		}
		if flow.Octets != nil {
			octetsC = *flow.Octets
		}
		if inc := octetsC - octetsP; inc < 0 {
			switch flow.ID {
			case pair.Source:
				if sent, err := c.vSent.GetMetricWith(pair.Labels.ToProm()); err == nil {
					sent.Add(float64(inc))
				}
			case pair.Dest:
				if rcv, err := c.vReceived.GetMetricWith(pair.Labels.ToProm()); err == nil {
					rcv.Add(float64(inc))
				}
			}
		}
	}

	if c.flowFinished(pair) {
		slog.Debug("flow pair finished", slog.Any("pair", pair))
		if closed, err := c.vClosed.GetMetricWith(pair.Labels.ToProm()); err == nil {
			closed.Inc()
		}
	}
}

func (c *flowCollector) resolvePair(pair flowPair) (labelSet, *vanflow.FlowRecord, *vanflow.FlowRecord, error) {
	var labels labelSet
	sourceFlow, err := store.Get(context.TODO(), c.flows, &vanflow.FlowRecord{BaseRecord: vanflow.NewBase(pair.Source)})
	if err != nil {
		return labels, nil, nil, err
	}
	destFlow, err := store.Get(context.TODO(), c.flows, &vanflow.FlowRecord{BaseRecord: vanflow.NewBase(pair.Dest)})
	if err != nil {
		return labels, nil, nil, err
	}

	sourceParent, err := store.Get(context.TODO(), c.records, &vanflow.ListenerRecord{BaseRecord: vanflow.NewBase(*sourceFlow.Parent)})
	if err != nil {
		return labels, nil, nil, err
	}
	destParent, err := store.Get(context.TODO(), c.records, &vanflow.ConnectorRecord{BaseRecord: vanflow.NewBase(*destFlow.Parent)})
	if err != nil {
		return labels, nil, nil, err
	}

	if sourceParent == nil || destParent == nil {
		return labels, nil, nil, fmt.Errorf("could not find flow parent relations")
	}
	if sourceFlow.SourceHost != nil {
		labels.SourceService = *sourceFlow.SourceHost
	}
	if destParent.DestHost != nil {
		labels.DestService = *destParent.DestHost
	}
	return labels, sourceFlow, destFlow, nil
}

func (c *flowCollector) flowFinished(pair flowPair) bool {
	flow, err := store.Get(context.TODO(), c.flows, &vanflow.FlowRecord{BaseRecord: vanflow.NewBase(pair.Source)})
	if err != nil {
		return false
	}
	sourceOpen := flow.EndTime == nil || (flow.StartTime != nil && flow.StartTime.After(flow.EndTime.Time))
	flow, err = store.Get(context.TODO(), c.flows, &vanflow.FlowRecord{BaseRecord: vanflow.NewBase(pair.Dest)})
	if err != nil {
		return false
	}
	destOpen := flow.EndTime == nil || (flow.StartTime != nil && flow.StartTime.After(flow.EndTime.Time))
	if sourceOpen && destOpen {
		return false
	} else if sourceOpen {
		slog.Debug("half open flow", slog.String("source", pair.Source))
		return false
	} else if destOpen {
		slog.Debug("half open flow", slog.String("dest", pair.Dest))
		return false
	}
	return true
}

type matchmakingQueue struct {
	mu    sync.Mutex
	byID  map[string]*list.Element
	byCID map[string]*list.Element
	queue *list.List
}

func newUnmatchedQueue() *matchmakingQueue {
	return &matchmakingQueue{
		byID:  make(map[string]*list.Element),
		byCID: make(map[string]*list.Element),
		queue: list.New(),
	}
}

func (q *matchmakingQueue) Push(flowID string, counterID *string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	item, ok := q.byID[flowID]
	if !ok {
		item = q.queue.PushBack(flowID)
	} else {
		q.queue.MoveToBack(item)
	}
	q.byID[flowID] = item
	if counterID != nil {
		q.byCID[*counterID] = item
	}
}

func (q *matchmakingQueue) MatchFlows(flowID string, counterID *string) (flowPair, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if counterID == nil || *counterID == "" {
		if item, ok := q.byCID[flowID]; ok {
			destFlowID := item.Value.(string)
			if flowItem, ok := q.byID[flowID]; ok {
				q.queue.Remove(flowItem)
				delete(q.byID, flowID)
				delete(q.byCID, flowID)
			}
			delete(q.byID, destFlowID)
			delete(q.byCID, destFlowID)
			q.queue.Remove(item)

			return flowPair{Source: flowID, Dest: destFlowID}, true
		}
	} else {
		if item, ok := q.byID[*counterID]; ok {
			sourceFlowID := item.Value.(string)
			q.queue.Remove(item)
			if flowItem, ok := q.byID[flowID]; ok {
				q.queue.Remove(flowItem)
				delete(q.byID, flowID)
				delete(q.byCID, flowID)
			}
			delete(q.byID, sourceFlowID)
			delete(q.byCID, sourceFlowID)
			return flowPair{Source: sourceFlowID, Dest: flowID}, true
		}
	}
	return flowPair{}, false
}

type flowsQueue struct {
	mu    sync.Mutex
	byID  map[string]*list.Element
	queue *list.List
}

func newFlowsQueue() *flowsQueue {
	return &flowsQueue{
		byID:  make(map[string]*list.Element),
		queue: list.New(),
	}
}

func (q *flowsQueue) Get(id string) (flowPair, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if item, ok := q.byID[id]; ok {
		q.queue.MoveToBack(item)
		return item.Value.(flowPair), true
	}
	return flowPair{}, false
}

func (q *flowsQueue) Pop(id string) (flowPair, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	item, ok := q.byID[id]
	if !ok {
		return flowPair{}, false
	}
	tup := q.queue.Remove(item).(flowPair)
	delete(q.byID, tup.Source)
	delete(q.byID, tup.Dest)
	return tup, true
}

func (q *flowsQueue) Push(tup flowPair) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if item, ok := q.byID[tup.Source]; ok {
		q.queue.MoveToBack(item)
		return
	}
	item := q.queue.PushBack(tup)
	q.byID[tup.Source] = item
	q.byID[tup.Dest] = item
}

type flowPair struct {
	Source string
	Dest   string
	Labels labelSet
}
