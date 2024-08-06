package collector

import (
	"context"
	"log/slog"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/skupperproject/skupper/cmd/network-console-collector/internal/collector/graph"
	"github.com/skupperproject/skupper/cmd/network-console-collector/internal/collector/records"
	"github.com/skupperproject/skupper/pkg/vanflow"
	"github.com/skupperproject/skupper/pkg/vanflow/store"
)

type FlowState struct {
	ID     string
	Active bool

	Source    graph.Process
	Listener  graph.Listener
	Dest      graph.Process
	Connector graph.Connector
}

func (s FlowState) FullyQualified() bool {
	return s.Source.IsKnown() &&
		s.Listener.IsKnown() &&
		s.Listener.Parent().Parent().IsKnown() &&
		s.Dest.IsKnown() &&
		s.Connector.IsKnown() &&
		s.Connector.Parent().Parent().IsKnown()
}

func (s FlowState) LogFields() []any {
	return []any{
		slog.String("id", s.ID),
		slog.Bool("active", s.Active),
		slog.String("type", "transport"),
		slog.String("listener", s.Listener.ID()),
		slog.String("connector", s.Connector.ID()),
		slog.String("source_site", s.Listener.Parent().Parent().ID()),
		slog.String("source_proc", s.Source.ID()),
		slog.String("dest_site", s.Connector.Parent().Parent().ID()),
		slog.String("dest_proc", s.Dest.ID()),
	}
}
func (s FlowState) labels() prometheus.Labels {
	var (
		source     vanflow.ProcessRecord
		sourceSite vanflow.SiteRecord
		dest       vanflow.ProcessRecord
		cnctr      vanflow.ConnectorRecord
		destSite   vanflow.SiteRecord
	)

	source, _ = recordFromNode[vanflow.ProcessRecord](s.Source)
	sourceSite, _ = recordFromNode[vanflow.SiteRecord](s.Source.Parent())
	dest, _ = recordFromNode[vanflow.ProcessRecord](s.Dest)
	destSite, _ = recordFromNode[vanflow.SiteRecord](s.Dest.Parent())
	cnctr, _ = recordFromNode[vanflow.ConnectorRecord](s.Connector)
	return map[string]string{
		"source_site_id":   sourceSite.ID,
		"source_site_name": dref(sourceSite.Name),
		"dest_site_id":     destSite.ID,
		"dest_site_name":   dref(destSite.Name),
		"routing_key":      dref(cnctr.Address),
		"protocol":         dref(cnctr.Protocol),
		"source_process":   dref(source.Name),
		"dest_process":     dref(dest.Name),
	}
}

func recordFromNode[R vanflow.Record, T graph.Node](node T) (R, bool) {
	var r R
	entry, ok := node.Get()
	if !ok {
		return r, false
	}
	r, ok = entry.Record.(R)
	return r, ok
}

var flowMetricLables = []string{"source_site_id", "dest_site_id", "source_site_name", "dest_site_name", "routing_key", "protocol", "source_process", "dest_process"}

func newFlowManager(log *slog.Logger, graph *graph.Graph, reg *prometheus.Registry, flows, records store.Interface) *flowManager {
	manager := &flowManager{
		logger:          log,
		graph:           graph,
		flows:           flows,
		records:         records,
		flowStates:      make(map[string]*FlowState),
		idp:             newStableIdentityProvider(),
		sitePairs:       make(map[pair]pairState),
		procPairs:       make(map[pair]pairState),
		refreshSitePair: make(chan pair, 16),
		refreshProcPair: make(chan pair, 16),

		flowsCounter: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "skupper",
			Subsystem: "flow",
			Name:      "connections_total",
		}, flowMetricLables),
		flowBytesSentCounter: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "skupper",
			Subsystem: "flow",
			Name:      "sent_bytes_total",
		}, flowMetricLables),
		flowBytesReceivedCounter: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "skupper",
			Subsystem: "flow",
			Name:      "received_bytes_total",
		}, flowMetricLables),
	}

	reg.MustRegister(manager.flowsCounter, manager.flowBytesSentCounter, manager.flowBytesReceivedCounter)
	return manager
}

type flowManager struct {
	logger  *slog.Logger
	flows   store.Interface
	records store.Interface
	graph   *graph.Graph
	idp     idProvider

	mu         sync.RWMutex
	flowStates map[string]*FlowState

	pairMu          sync.Mutex
	sitePairs       map[pair]pairState
	refreshSitePair chan pair
	procPairs       map[pair]pairState
	refreshProcPair chan pair

	flowsCounter             *prometheus.CounterVec
	flowBytesSentCounter     *prometheus.CounterVec
	flowBytesReceivedCounter *prometheus.CounterVec
}

type pair struct {
	Source   string
	Dest     string
	Protocol string
}

type pairState struct {
	Count int
	Dirty bool
}

func (m *flowManager) runFlowPairManager(ctx context.Context) func() error {
	return func() error {
		defer func() {
			m.logger.Info("flow pair manager shutdown complete")
		}()
		for {
			select {
			case <-ctx.Done():
				return nil
			case p := <-m.refreshSitePair:
				func() {
					m.pairMu.Lock()
					defer m.pairMu.Unlock()
					if pairState, ok := m.sitePairs[p]; ok && pairState.Dirty {
						id := m.idp.ID("sitepair", p.Source, p.Dest, p.Protocol)
						entry, ok := m.records.Get(id)
						if !ok {
							record := records.SitePairRecord{
								ID:       id,
								Source:   p.Source,
								Dest:     p.Dest,
								Protocol: p.Protocol,
								Count:    uint64(pairState.Count),
							}
							m.records.Add(record, store.SourceRef{ID: "self"})
							return
						}
						if record, ok := entry.Record.(records.SitePairRecord); ok {
							record.Count++
							m.records.Update(record)
						}

					}
				}()
			case p := <-m.refreshProcPair:
				func() {
					m.pairMu.Lock()
					defer m.pairMu.Unlock()
					if pairState, ok := m.procPairs[p]; ok && pairState.Dirty {
						id := m.idp.ID("processpair", p.Source, p.Dest, p.Protocol)
						entry, ok := m.records.Get(id)
						if !ok {
							record := records.ProcPairRecord{
								ID:       id,
								Source:   p.Source,
								Dest:     p.Dest,
								Protocol: p.Protocol,
								Count:    uint64(pairState.Count),
							}
							m.records.Add(record, store.SourceRef{ID: "self"})
							return
						}

						if record, ok := entry.Record.(records.ProcPairRecord); ok {
							record.Count++
							m.records.Update(record)
						}

					}
				}()
			}
		}
	}

}
func (m *flowManager) incrementProcessPairs(state FlowState) {
	m.pairMu.Lock()
	defer m.pairMu.Unlock()
	var protocol string
	if c, ok := state.Connector.Get(); ok {
		if conn, ok := c.Record.(vanflow.ConnectorRecord); ok {
			protocol = dref(conn.Protocol)
		}
	}
	sitePair := pair{Source: state.Source.Parent().ID(), Dest: state.Dest.Parent().ID(), Protocol: protocol}
	procPair := pair{Source: state.Source.ID(), Dest: state.Dest.ID(), Protocol: protocol}
	ps := m.sitePairs[sitePair]
	ps.Dirty = true
	ps.Count++
	m.sitePairs[sitePair] = ps
	ps = m.procPairs[procPair]
	ps.Dirty = true
	ps.Count++
	m.procPairs[procPair] = ps
	select {
	case m.refreshSitePair <- sitePair:
	default:
	}
	select {
	case m.refreshProcPair <- procPair:
	default:
	}
}

func (m *flowManager) get(id string) FlowState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var state FlowState
	if flowstate, ok := m.flowStates[id]; ok {
		state = *flowstate
	}
	return state
}

func (m *flowManager) processEvent(event changeEvent) {
	if _, ok := event.(deleteEvent); ok {
		//todo(ck) decrement aggregate counts?
		return
	}

	m.mu.Lock()
	state, ok := m.flowStates[event.ID()]
	if !ok {
		state = &FlowState{ID: event.ID()}
		m.flowStates[event.ID()] = state
	}
	m.mu.Unlock()

	flowEntry, ok := m.flows.Get(event.ID())
	if !ok {
		return
	}
	switch record := flowEntry.Record.(type) {
	case vanflow.BIFlowTPRecord:
		alreadyQualified := state.FullyQualified()
		m.fillFlowState(record, state)
		log := m.logger.With(state.LogFields()...)
		if !state.FullyQualified() {
			log.Info("PENDING FLOW")
			return
		}

		var (
			prevOctets    uint64
			prevOctetsRev uint64
		)
		if update, ok := event.(updateEvent); ok {
			if prev, ok := update.Prev.(vanflow.BIFlowTPRecord); ok {
				prevOctets = dref(prev.Octets)
				prevOctetsRev = dref(prev.OctetsReverse)
			}

		}
		labels := state.labels()
		if !alreadyQualified {
			prevOctets, prevOctetsRev = 0, 0
			m.flowsCounter.With(labels).Inc()
			m.incrementProcessPairs(*state)
		}
		sentInc := float64(dref(record.Octets) - prevOctets)
		reveivedInc := float64(dref(record.OctetsReverse) - prevOctetsRev)
		m.flowBytesSentCounter.With(labels).Add(sentInc)
		m.flowBytesReceivedCounter.With(labels).Add(reveivedInc)
		log.Info("FLOW",
			slog.Float64("bytes_out", sentInc),
			slog.Float64("bytes_in", reveivedInc),
		)

	default:
	}
}

func (m *flowManager) pendingSourceProcess() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var pending []string
	for _, p := range m.flowStates {
		if !p.Source.IsKnown() && p.Listener.IsKnown() {
			pending = append(pending, p.ID)
		}
	}
	return pending
}

func (m *flowManager) fillFlowState(flow vanflow.BIFlowTPRecord, state *FlowState) {
	state.ID = flow.ID
	state.Active = dref(flow.EndTime).Compare(dref(flow.StartTime).Time) < 0
	listenerID := dref(flow.Parent)
	connectorID := dref(flow.ConnectorID)
	state.Listener = m.graph.Listener(listenerID)
	state.Connector = m.graph.Connector(connectorID)
	state.Dest = state.Connector.Target()
	if sourceSite, sourceHost := state.Listener.Parent().Parent().ID(), dref(flow.SourceHost); sourceSite != "" && sourceHost != "" {
		state.Source = m.graph.ConnectorTarget(graph.ConnectorTargetID(sourceSite, sourceHost)).Process()
	}
}
