package collector

import (
	"context"
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

	Protocol string
	Address  string

	ListenerID     string
	SourceProcID   string
	SourceProcName string
	SourceSiteID   string
	SourceSiteName string

	ConnectorID  string
	DestProcID   string
	DestProcName string
	DestSiteID   string
	DestSiteName string
}

func (s FlowState) FullyQualified() bool {
	for _, val := range s.labels() {
		if val == "" {
			return false
		}
	}
	return true
}

func (s FlowState) LogFields() []any {
	return []any{
		slog.String("id", s.ID),
		slog.Bool("active", s.Active),
		slog.String("type", "transport"),
		slog.String("listener", s.ListenerID),
		slog.String("connector", s.ConnectorID),
		slog.String("source_site", s.SourceSiteID),
		slog.String("source_proc", s.SourceProcID),
		slog.String("dest_site", s.DestSiteID),
		slog.String("dest_proc", s.DestProcID),
	}
}
func (s FlowState) labels() prometheus.Labels {
	return map[string]string{
		"source_site_id":   s.SourceSiteID,
		"source_site_name": s.SourceSiteName,
		"dest_site_id":     s.DestSiteID,
		"dest_site_name":   s.DestSiteName,
		"routing_key":      s.Address,
		"protocol":         s.Protocol,
		"source_process":   s.SourceProcName,
		"dest_process":     s.DestProcName,
	}
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
		connectorsCache: make(map[string]connectorAttrs),
		processesCache:  make(map[string]processAttrs),

		flowOpenedCounter: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "skupper",
			Subsystem: "flow",
			Name:      "connections_opened_total",
		}, flowMetricLables),
		flowClosedCounter: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "skupper",
			Subsystem: "flow",
			Name:      "connections_closed_total",
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

	reg.MustRegister(
		manager.flowOpenedCounter,
		manager.flowClosedCounter,
		manager.flowBytesSentCounter,
		manager.flowBytesReceivedCounter,
	)
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

	attrMu          sync.Mutex
	connectorsCache map[string]connectorAttrs
	processesCache  map[string]processAttrs

	pairMu    sync.Mutex
	sitePairs map[pair]pairState
	procPairs map[pair]pairState

	flowOpenedCounter        *prometheus.CounterVec
	flowClosedCounter        *prometheus.CounterVec
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
		ticker := time.NewTicker(time.Second * 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				func() {
					m.pairMu.Lock()
					defer m.pairMu.Unlock()
					for p, pairState := range m.sitePairs {
						if !pairState.Dirty {
							continue
						}
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
							record.Count = uint64(pairState.Count)
							m.records.Update(record)
						}
					}
					for p, pairState := range m.procPairs {
						if !pairState.Dirty {
							continue
						}
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
							record.Count = uint64(pairState.Count)
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
	if c, ok := m.records.Get(state.ConnectorID); ok {
		if conn, ok := c.Record.(vanflow.ConnectorRecord); ok {
			protocol = dref(conn.Protocol)
		}
	}
	sitePair := pair{Source: state.SourceSiteID, Dest: state.DestSiteID, Protocol: protocol}
	procPair := pair{Source: state.SourceProcID, Dest: state.DestProcID, Protocol: protocol}
	ps := m.sitePairs[sitePair]
	ps.Dirty = true
	ps.Count = ps.Count + 1
	m.sitePairs[sitePair] = ps
	ps = m.procPairs[procPair]
	ps.Dirty = true
	ps.Count = ps.Count + 1
	m.procPairs[procPair] = ps
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

func (m *flowManager) handleCacheInvalidatingEvent(event changeEvent, stor store.Interface) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.connectorsCache, event.ID())
	delete(m.processesCache, event.ID())
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
		previouslyActive := state.Active
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
			previouslyActive = true
			m.flowOpenedCounter.With(labels).Inc()
			m.incrementProcessPairs(*state)
		}
		if !state.Active && previouslyActive {
			m.flowClosedCounter.With(labels).Inc()
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

func (m *flowManager) fillFlowState(flow vanflow.BIFlowTPRecord, state *FlowState) {
	state.ID = flow.ID
	state.Active = dref(flow.EndTime).Compare(dref(flow.StartTime).Time) < 0
	state.ListenerID = dref(flow.Parent)
	state.ConnectorID = dref(flow.ConnectorID)
	listener := m.graph.Listener(state.ListenerID)
	connector := m.graph.Connector(state.ConnectorID)
	dest := connector.Target()
	var source graph.Process
	if sourceSite, sourceHost := listener.Parent().Parent().ID(), dref(flow.SourceHost); sourceSite != "" && sourceHost != "" {
		source = m.graph.ConnectorTarget(graph.ConnectorTargetID(sourceSite, sourceHost)).Process()
	}

	if cattrs, ok := m.connectorAttrs(state.ConnectorID); ok {
		state.Protocol = cattrs.Protocol
		state.Address = cattrs.Address
	}
	if sourceattrs, ok := m.processAttrs(source.ID()); ok {
		state.SourceProcID = source.ID()
		state.SourceProcName = sourceattrs.Name
		state.SourceSiteID = sourceattrs.SiteID
		state.SourceSiteName = sourceattrs.SiteName
	}
	if destattrs, ok := m.processAttrs(dest.ID()); ok {
		state.DestProcID = dest.ID()
		state.DestProcName = destattrs.Name
		state.DestSiteID = destattrs.SiteID
		state.DestSiteName = destattrs.SiteName
	}
}

type connectorAttrs struct {
	Protocol string
	Address  string
}

type processAttrs struct {
	Name     string
	SiteID   string
	SiteName string
}

func (m *flowManager) connectorAttrs(id string) (connectorAttrs, bool) {
	var attrs connectorAttrs
	m.attrMu.Lock()
	defer m.attrMu.Unlock()
	if cattr, ok := m.connectorsCache[id]; ok {
		return cattr, true
	}

	entry, ok := m.records.Get(id)
	if !ok {
		return attrs, false
	}
	cnctr, ok := entry.Record.(vanflow.ConnectorRecord)
	if !ok {
		return attrs, false
	}

	var complete bool
	if cnctr.Address != nil && cnctr.Protocol != nil {
		complete = true
		attrs.Address = *cnctr.Address
		attrs.Protocol = *cnctr.Protocol
		m.connectorsCache[id] = attrs
	}

	return attrs, complete
}
func (m *flowManager) processAttrs(id string) (processAttrs, bool) {
	var attrs processAttrs
	m.attrMu.Lock()
	defer m.attrMu.Unlock()
	if pattr, ok := m.processesCache[id]; ok {
		return pattr, true
	}

	entry, ok := m.records.Get(id)
	if !ok {
		return attrs, false
	}
	proc, ok := entry.Record.(vanflow.ProcessRecord)
	if !ok || proc.Parent == nil {
		return attrs, false
	}

	entry, ok = m.records.Get(*proc.Parent)
	if !ok {
		return attrs, false
	}
	site, ok := entry.Record.(vanflow.SiteRecord)
	if !ok {
		return attrs, false
	}

	var complete bool
	if proc.Name != nil && site.Name != nil {
		complete = true
		attrs.Name = *proc.Name
		attrs.SiteID = site.ID
		attrs.SiteName = *site.Name
		m.processesCache[id] = attrs
	}

	return attrs, complete
}
