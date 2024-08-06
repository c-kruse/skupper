package collector

import (
	"log/slog"
	"sync"
	"time"

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
		logger:     log,
		graph:      graph,
		flows:      flows,
		records:    records,
		flowStates: make(map[string]*FlowState),

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

	mu         sync.RWMutex
	flowStates map[string]*FlowState

	flowsCounter             *prometheus.CounterVec
	flowBytesSentCounter     *prometheus.CounterVec
	flowBytesReceivedCounter *prometheus.CounterVec
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
		idp := newStableIdentityProvider()
		var protocol string
		if c, ok := state.Connector.Get(); ok {
			if conn, ok := c.Record.(vanflow.ConnectorRecord); ok {
				protocol = dref(conn.Protocol)
			}
		}
		sitePairID := idp.ID("sitepair", state.Source.Parent().ID(), state.Dest.Parent().ID(), protocol)
		sitePair, ok := m.records.Get(sitePairID)
		if !ok {
			sitePairRecord := records.SitePairRecord{
				ID:       sitePairID,
				Source:   state.Source.Parent().ID(),
				Dest:     state.Dest.Parent().ID(),
				Protocol: protocol,
				Start:    time.Now(),
			}
			m.records.Add(sitePairRecord, store.SourceRef{ID: "self"})
			sitePair, _ = m.records.Get(sitePairID)
		}
		procPairID := idp.ID("processpair", state.Source.ID(), state.Dest.ID(), protocol)
		procPair, ok := m.records.Get(procPairID)
		if !ok {
			procPairRecord := records.ProcPairRecord{
				ID:       procPairID,
				Source:   state.Source.ID(),
				Dest:     state.Dest.ID(),
				Protocol: protocol,
				Start:    time.Now(),
			}
			m.records.Add(procPairRecord, store.SourceRef{ID: "self"})
			procPair, _ = m.records.Get(procPairID)
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
			spr := sitePair.Record.(records.SitePairRecord)
			spr.Count++
			ppr := procPair.Record.(records.ProcPairRecord)
			ppr.Count++
			m.records.Update(spr)
			m.records.Update(ppr)
		}
		sentInc := float64(dref(record.Octets) - prevOctets)
		reveivedInc := float64(dref(record.OctetsReverse) - prevOctetsRev)
		m.flowBytesSentCounter.With(labels).Add(sentInc)
		m.flowBytesReceivedCounter.With(labels).Add(reveivedInc)
		log.Info("FLOW",
			slog.Uint64("bytes_out", dref(record.Octets)),
			slog.Uint64("bytes_in", dref(record.OctetsReverse)),
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
