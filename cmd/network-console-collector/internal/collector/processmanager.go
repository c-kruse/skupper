package collector

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/skupperproject/skupper/cmd/network-console-collector/internal/collector/graph"
	"github.com/skupperproject/skupper/cmd/network-console-collector/internal/collector/records"
	"github.com/skupperproject/skupper/pkg/vanflow"
	"github.com/skupperproject/skupper/pkg/vanflow/store"
)

type processManager struct {
	logger *slog.Logger
	graph  *graph.Graph
	stor   store.Interface
	idp    idProvider

	mu     sync.Mutex
	groups map[string]int

	siteHosts map[string]map[string]bool

	checkGroup    chan string
	checkSiteHost chan tuple
}

func newProcessManager(logger *slog.Logger, stor store.Interface, graph *graph.Graph, idp idProvider) *processManager {
	return &processManager{
		logger:        logger,
		idp:           idp,
		graph:         graph,
		stor:          stor,
		groups:        make(map[string]int),
		siteHosts:     make(map[string]map[string]bool),
		checkGroup:    make(chan string, 32),
		checkSiteHost: make(chan tuple, 32),
	}
}

func (m *processManager) run(ctx context.Context) func() error {
	return func() error {
		defer m.logger.Info("process manager shutdown complete")
		for {
			select {
			case <-ctx.Done():
				return nil
			case siteHost := <-m.checkSiteHost:
				func() {
					m.mu.Lock()
					defer m.mu.Unlock()
					var connectors []vanflow.ConnectorRecord
					for _, c := range m.stor.Index(store.TypeIndex, store.Entry{Record: vanflow.ConnectorRecord{}}) {
						conn := c.Record.(vanflow.ConnectorRecord)
						if conn.DestHost == nil || siteHost.Host != *conn.DestHost {
							continue
						}
						if m.graph.Connector(conn.ID).Parent().Parent().ID() != siteHost.Site {
							continue
						}
						connectors = append(connectors, conn)
					}
					processes := m.stor.Index(IndexByParentHost, store.Entry{Record: vanflow.ProcessRecord{Parent: &siteHost.Site, SourceHost: &siteHost.Host}})
					procID := m.idp.ID("siteproc", siteHost.Host, siteHost.Site)
					var (
						needsSiteHost    bool
						hasTargetProcess bool
						hasSiteProcess   bool
						needsSiteProcess bool
					)
					needsSiteHost = len(connectors) > 0
					for _, p := range processes {
						if p.Source.ID != "self" {
							hasTargetProcess = true
						} else {
							hasSiteProcess = true
						}
					}

					needsSiteProcess = needsSiteHost && !hasTargetProcess
					hosts, ok := m.siteHosts[siteHost.Site]
					if !ok {
						hosts = make(map[string]bool)
						m.siteHosts[siteHost.Site] = hosts
					}
					switch {
					case needsSiteProcess:
						hosts[siteHost.Host] = false
					case !needsSiteHost:
						delete(hosts, siteHost.Host)
					default:
						hosts[siteHost.Host] = true
					}

					switch {
					case needsSiteProcess && !hasSiteProcess:
						name := fmt.Sprintf("site-server-%s-%s", siteHost.Host, shortSite(siteHost.Site))
						groupName := fmt.Sprintf("site-servers-%s", shortSite(siteHost.Site))
						role := "external"
						m.stor.Add(vanflow.ProcessRecord{
							BaseRecord: vanflow.NewBase(procID, time.Now()),
							Parent:     &siteHost.Site,
							Name:       &name,
							Group:      &groupName,
							SourceHost: &siteHost.Host,
							Mode:       &role,
						}, store.SourceRef{ID: "self"})
						m.logger.Info("Adding site server process for connector without suitable target",
							slog.String("id", procID),
							slog.String("name", name),
							slog.String("site_id", siteHost.Site),
							slog.String("host", siteHost.Host),
							slog.String("connector_id", connectors[0].ID),
						)
					case hasSiteProcess && !needsSiteProcess:
						for _, proc := range processes {
							if proc.Source.ID == "self" {
								m.logger.Info("Deleting site server process with no connectors",
									slog.String("id", procID),
									slog.String("site_id", siteHost.Site),
									slog.String("host", siteHost.Host),
								)
								m.stor.Delete(proc.Record.Identity())
							}
						}
					}

				}()

			case groupName := <-m.checkGroup:
				func() {
					m.mu.Lock()
					defer m.mu.Unlock()
					ct := m.groups[groupName]
					groups := m.stor.Index(IndexByTypeName, store.Entry{Record: records.ProcessGroupRecord{Name: groupName}})
					if ct <= 0 {
						delete(m.groups, groupName)
						for _, g := range groups {
							m.logger.Info("Deleting process group with no processes",
								slog.String("id", g.Record.Identity()),
								slog.String("name", groupName),
							)
							m.stor.Delete(g.Record.Identity())
						}
						return
					}
					if len(groups) > 0 {
						return
					}
					id := m.idp.ID("pg", groupName)
					m.logger.Info("Creating process group",
						slog.String("id", id),
						slog.String("name", groupName),
					)
					m.stor.Add(records.ProcessGroupRecord{ID: id, Name: groupName, Start: time.Now()}, store.SourceRef{ID: "self"})
				}()
			}
		}
	}
}

func (m *processManager) handleChangeEvent(event changeEvent, stor readonly) {
	var entry store.Entry
	var isDelete bool
	if d, ok := event.(deleteEvent); ok {
		entry = store.Entry{Record: d.Record}
		isDelete = true
	} else {
		entry, ok = stor.Get(event.ID())
		if !ok {
			return
		}
	}
	switch record := entry.Record.(type) {
	case vanflow.ConnectorRecord:
		if record.DestHost != nil {
			m.processPresent(m.graph.Connector(record.ID).Parent().Parent().ID(), *record.DestHost, !isDelete)
		}
	case vanflow.ProcessRecord:
		if record.Group != nil {
			m.ensureGroup(*record.Group, !isDelete)
		}
		if record.SourceHost != nil && record.Parent != nil {
			m.processPresent(*record.Parent, *record.SourceHost, !isDelete)
		}
	}
}

func (m *processManager) ensureGroup(name string, add bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var added bool
	if _, ok := m.groups[name]; !ok {
		m.groups[name] = 0
		added = true
	}
	if add {
		m.groups[name] = m.groups[name] + 1
	} else {
		m.groups[name] = m.groups[name] - 1
	}
	if m.groups[name] <= 0 || added {
		m.checkGroup <- name
	}
}

func (m *processManager) processPresent(site, host string, present bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !present {
		m.checkSiteHost <- tuple{Site: site, Host: host}
		return
	}
	if hosts, ok := m.siteHosts[site]; ok {
		if hasReal, ok := hosts[host]; ok && hasReal {
			return
		}
	}
	m.checkSiteHost <- tuple{Site: site, Host: host}
}

type tuple struct {
	Site string
	Host string
}
