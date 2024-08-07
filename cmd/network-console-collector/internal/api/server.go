package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/skupperproject/skupper/cmd/network-console-collector/internal/collector"
	"github.com/skupperproject/skupper/cmd/network-console-collector/internal/collector/records"
	"github.com/skupperproject/skupper/pkg/vanflow"
	"github.com/skupperproject/skupper/pkg/vanflow/store"
)

var emptyListResponse = SiteListResponse{
	BaseResponse: BaseResponse{
		Status: "unimplemented",
	},
	Results: []SiteRecord{},
}

var emptySingleResponse = BaseResponse{
	Status: "unimplemented",
}

var _ ServerInterface = (*server)(nil)

type server struct {
	logger  *slog.Logger
	coll    *collector.Collector
	records store.Interface
}

func buildServer(logger *slog.Logger, records store.Interface, c *collector.Collector) ServerInterface {
	return &server{
		logger:  logger,
		records: records,
		coll:    c,
	}
}

// (GET /api/v1alpha1/sites/)
func (c *server) Sites(w http.ResponseWriter, r *http.Request) {
	entries := listByType[vanflow.SiteRecord](c.records)
	results := make([]SiteRecord, len(entries))
	for i := range entries {
		results[i], _ = toSiteRecord(entries[i])
	}
	if err := handleResultSet(w, r, &SiteListResponse{}, results); err != nil {
		c.logWriteError(r, err)
	}
}

// (GET /api/v1alpha1/sites/{id}/)
func (c *server) SiteById(w http.ResponseWriter, r *http.Request, id string) {
	err := handleOptionalResult(w, &SiteResponse{}, withMapping(toSiteRecord).ByID(c.records, id))
	if err != nil {
		c.logWriteError(r, err)
	}
}

// (GET /api/v1alpha1/sites/{id}/routers/)
func (c *server) RoutersBySite(w http.ResponseWriter, r *http.Request, id string) {
	exemplar := store.Entry{Record: vanflow.RouterRecord{Parent: &id}}
	entries := index(c.records, collector.IndexByTypeParent, exemplar)
	results := make([]RouterRecord, len(entries))
	for i := range entries {
		results[i], _ = toRouterRecord(entries[i])
	}

	if err := handleResultSet(w, r, &RouterListResponse{}, results); err != nil {
		c.logWriteError(r, err)
	}
}

// (GET /api/v1alpha1/sites/{id}/processes/)
func (c *server) ProcessesBySite(w http.ResponseWriter, r *http.Request, id string) {
	exemplar := store.Entry{Record: vanflow.ProcessRecord{Parent: &id}}

	entries := index(c.records, collector.IndexByTypeParent, exemplar)
	results := make([]ProcessRecord, len(entries))
	for i := range entries {
		results[i], _ = c.asProcessRecord(entries[i])
	}

	if err := handleResultSet(w, r, &ProcessListResponse{}, results); err != nil {
		c.logWriteError(r, err)
	}
}

// (GET /api/v1alpha1/sites/{id}/flows/)
func (c *server) FlowsBySite(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/sites/{id}/links/)
func (c *server) LinksBySite(w http.ResponseWriter, r *http.Request, id string) {
	siteNode := c.coll.Graph().Site(id)
	linkNodes := siteNode.Links()
	linkEntries := make([]store.Entry, 0, len(linkNodes))
	for _, ln := range linkNodes {
		if le, ok := ln.Get(); ok {
			linkEntries = append(linkEntries, le)
		}
	}
	results := make([]LinkRecord, 0, len(linkEntries))

	for _, le := range linkEntries {
		if lr, ok := c.asLinkRecord(le); ok {
			results = append(results, lr)
		}
	}
	if err := handleResultSet(w, r, &LinkListResponse{}, results); err != nil {
		c.logWriteError(r, err)
	}
}

// (GET /api/v1alpha1/sitepairs/)
func (c *server) Sitepairs(w http.ResponseWriter, r *http.Request) {
	entries := listByType[records.SitePairRecord](c.records)
	results := make([]FlowAggregateRecord, len(entries))
	for i := range entries {
		results[i], _ = c.asSitePairs(entries[i])
	}
	if err := handleResultSet(w, r, &FlowAggregateListResponse{}, results); err != nil {
		c.logWriteError(r, err)
	}
}

func (c *server) asSitePairs(entry store.Entry) (FlowAggregateRecord, bool) {
	record, ok := entry.Record.(records.SitePairRecord)
	if !ok {
		return FlowAggregateRecord{}, false
	}
	var (
		sourceName string
		destName   string
	)
	if ss, ok := c.coll.Graph().Site(record.Source).Get(); ok {
		if sr, ok := ss.Record.(vanflow.SiteRecord); ok {
			sourceName = dref(sr.Name)
		}
	}
	if ds, ok := c.coll.Graph().Site(record.Dest).Get(); ok {
		if dr, ok := ds.Record.(vanflow.SiteRecord); ok {
			destName = dref(dr.Name)
		}
	}
	return FlowAggregateRecord{
		BaseRecord: BaseRecord{
			Identity:  record.ID,
			StartTime: uint64(record.Start.UnixMicro()),
		},
		PairType:        FlowAggregatePairTypeSITE,
		Protocol:        record.Protocol,
		SourceId:        record.Source,
		SourceName:      sourceName,
		DestinationId:   record.Dest,
		DestinationName: destName,
		RecordCount:     record.Count,
	}, true
}

// (GET /api/v1alpha1/sitepairs/{id}/)
func (c *server) SitepairByID(w http.ResponseWriter, r *http.Request, id string) {
	err := handleOptionalResult(w, &FlowAggregateResponse{}, withMapping(c.asSitePairs).ByID(c.records, id))
	if err != nil {
		c.logWriteError(r, err)
	}
}

// (GET /api/v1alpha1/routers/)
func (c *server) Routers(w http.ResponseWriter, r *http.Request) {
	entries := listByType[vanflow.RouterRecord](c.records)
	results := make([]RouterRecord, len(entries))
	for i := range entries {
		results[i], _ = toRouterRecord(entries[i])
	}
	if err := handleResultSet(w, r, &RouterListResponse{}, results); err != nil {
		c.logWriteError(r, err)
	}
}

// (GET /api/v1alpha1/routers/{id}/)
func (c *server) RouterByID(w http.ResponseWriter, r *http.Request, id string) {
	err := handleOptionalResult(w, &RouterResponse{}, withMapping(toRouterRecord).ByID(c.records, id))
	if err != nil {
		c.logWriteError(r, err)
	}
}

// (GET /api/v1alpha1/routers/{id}/connectors/)
func (c *server) ConnectorsByRouter(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/routers/{id}/flows/)
func (c *server) FlowsByRouter(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/routers/{id}/links/)
func (c *server) LinksByRouter(w http.ResponseWriter, r *http.Request, id string) {
	linkEntries := index(c.records, collector.IndexByTypeParent, store.Entry{Record: vanflow.LinkRecord{Parent: &id}})
	results := make([]LinkRecord, 0, len(linkEntries))

	for _, le := range linkEntries {
		if lr, ok := c.asLinkRecord(le); ok {
			results = append(results, lr)
		}
	}
	if err := handleResultSet(w, r, &LinkListResponse{}, results); err != nil {
		c.logWriteError(r, err)
	}
}

// (GET /api/v1alpha1/routers/{id}/listeners/)
func (c *server) ListenersByRouter(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/addresses/)
func (c *server) Addresses(w http.ResponseWriter, r *http.Request) {
	entries := listByType[records.AddressRecord](c.records)
	results := make([]AddressRecord, len(entries))
	for i := range entries {
		results[i], _ = c.asAddress(entries[i])
	}
	if err := handleResultSet(w, r, &AddressListResponse{}, results); err != nil {
		c.logWriteError(r, err)
	}
}

// (GET /api/v1alpha1/addresses/{id}/)
func (c *server) AddressByID(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusNotFound, emptySingleResponse)
}

// (GET /api/v1alpha1/addresses/{id}/connectors/)
func (c *server) ConnectorsByAddress(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/addresses/{id}/listeners/)
func (c *server) ListenersByAddress(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/addresses/{id}/processes/)
func (c *server) ProcessesByAddress(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/addresses/{id}/processpairs/)
func (c *server) ProcessPairsByAddress(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/connectors/)
func (c *server) Connectors(w http.ResponseWriter, r *http.Request) {
	entries := listByType[vanflow.ConnectorRecord](c.records)
	results := make([]ConnectorRecord, len(entries))
	for i := range entries {
		results[i], _ = c.asConnectorRecord(entries[i])
	}
	if err := handleResultSet(w, r, &ConnectorListResponse{}, results); err != nil {
		c.logWriteError(r, err)
	}
}

func (c *server) asConnectorRecord(in store.Entry) (ConnectorRecord, bool) {
	record, ok := in.Record.(vanflow.ConnectorRecord)
	if !ok {
		return ConnectorRecord{}, false
	}
	var (
		addressID  string
		targetName string
	)
	cn := c.coll.Graph().Connector(record.ID)
	addressID = cn.Address().ID()
	if record.ProcessID != nil {
		p, ok := c.records.Get(*record.ProcessID)
		if ok {
			if proc, ok := p.Record.(vanflow.ProcessRecord); ok {
				targetName = dref(proc.Name)
			}
		}
	}

	return ConnectorRecord{
		BaseRecord: toBase(record.BaseRecord, nil, in.Source.ID),
		Name:       dref(record.Name),
		Address:    dref(record.Address),
		AddressId:  &addressID,
		Target:     targetName,
		ProcessId:  dref(record.ProcessID),
		DestHost:   dref(record.DestHost),
		DestPort:   dref(record.DestPort),
		Protocol:   dref(record.Protocol),
	}, true
}

// (GET /api/v1alpha1/connectors/{id}/)
func (c *server) ConnectorByID(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusNotFound, emptySingleResponse)
}

// (GET /api/v1alpha1/links/)
func (c *server) Links(w http.ResponseWriter, r *http.Request) {
	linkEntries := listByType[vanflow.LinkRecord](c.records)
	results := make([]LinkRecord, 0, len(linkEntries))

	for _, le := range linkEntries {
		if lr, ok := c.asLinkRecord(le); ok {
			results = append(results, lr)
		}
	}
	if err := handleResultSet(w, r, &LinkListResponse{}, results); err != nil {
		c.logWriteError(r, err)
	}
}

func (c *server) asLinkRecord(in store.Entry) (r LinkRecord, ok bool) {
	link := in.Record.(vanflow.LinkRecord)
	if link.Status == nil || *link.Status != "up" || link.Peer == nil {
		return r, false
	}
	linkNode := c.coll.Graph().Link(link.ID)
	sourceSiteID := linkNode.Parent().Parent().ID()
	destSiteID := linkNode.Peer().Parent().Parent().ID()
	return LinkRecord{
		BaseRecord:        toBase(link.BaseRecord, link.Parent, in.Source.ID),
		Name:              dref(link.Name),
		Direction:         "outgoing",
		Mode:              dref(link.Role),
		LinkCost:          dref(link.LinkCost),
		SourceSiteId:      sourceSiteID,
		DestinationSiteId: destSiteID,
	}, true
}

// (GET /api/v1alpha1/links/{id}/)
func (c *server) LinkByID(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusNotFound, emptySingleResponse)
}

// (GET /api/v1alpha1/routeraccess/)
func (c *server) Routeraccess(w http.ResponseWriter, r *http.Request) {
	entries := listByType[vanflow.RouterAccessRecord](c.records)
	results := make([]RouterAccessRecord, len(entries))
	for i := range entries {
		results[i], _ = toRouterAccessRecord(entries[i])
	}
	if err := handleResultSet(w, r, &RouterAccessListResponse{}, results); err != nil {
		c.logWriteError(r, err)
	}
}

// (GET /api/v1alpha1/routeraccess/{id}/)
func (c *server) RouteraccessByID(w http.ResponseWriter, r *http.Request, id string) {
	err := handleOptionalResult(w, &RouterAccessResponse{}, withMapping(toRouterAccessRecord).ByID(c.records, id))
	if err != nil {
		c.logWriteError(r, err)
	}
}

func (c *server) asAddress(entry store.Entry) (AddressRecord, bool) {
	record, ok := entry.Record.(records.AddressRecord)
	if !ok {
		return AddressRecord{}, false
	}
	var (
		listenerCt  int
		connectorCt int
	)

	addressNode := c.coll.Graph().Address(record.ID)

	listenerCt = len(addressNode.Listeners())
	connectorCt = len(addressNode.Connectors())

	return AddressRecord{
		BaseRecord: BaseRecord{
			Identity:  record.ID,
			StartTime: uint64(record.Start.UnixMicro()),
		},
		Name:           record.Name,
		ListenerCount:  listenerCt,
		ConnectorCount: connectorCt,
		Protocol:       "tcp", // todo(ck)
	}, true
}

func (c *server) asRouterLink(entry store.Entry) (RouterLinkRecord, bool) {
	link, ok := entry.Record.(vanflow.LinkRecord)
	if !ok {
		return RouterLinkRecord{}, false
	}
	var (
		status       OperStatusType = OperStatusTypeDown
		sourceSiteID string
		destSiteID   string
	)
	if link.Status != nil && *link.Status == string(OperStatusTypeUp) {
		status = OperStatusTypeUp
	}
	linkNode := c.coll.Graph().Link(link.ID)
	sourceSiteID = linkNode.Parent().Parent().ID()
	destSiteID = linkNode.Peer().Parent().Parent().ID()
	return RouterLinkRecord{
		BaseRecord:        toBase(link.BaseRecord, nil, entry.Source.ID),
		LinkCost:          dref(link.LinkCost),
		Name:              dref(link.Name),
		Peer:              dref(link.Peer),
		Role:              dref(link.Role),
		Status:            status,
		SourceSiteId:      sourceSiteID,
		DestinationSiteId: destSiteID,
	}, true
}

// (GET /api/v1alpha1/routerlinks/)
func (c *server) Routerlinks(w http.ResponseWriter, r *http.Request) {
	links := listByType[vanflow.LinkRecord](c.records)
	results := make([]RouterLinkRecord, len(links))
	for i := range links {
		results[i], _ = c.asRouterLink(links[i])
	}
	if err := handleResultSet(w, r, &RouterLinkListResponse{}, results); err != nil {
		c.logWriteError(r, err)
	}
}

// (GET /api/v1alpha1/routerlinks/{id}/)
func (c *server) RouterlinkByID(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusNotFound, emptySingleResponse)
}

// (GET /api/v1alpha1/listeners/)
func (c *server) Listeners(w http.ResponseWriter, r *http.Request) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/listeners/{id}/)
func (c *server) ListenerByID(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusNotFound, emptySingleResponse)
}

// (GET /api/v1alpha1/listeners/{id}/flows)
func (c *server) FlowsByListener(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/processes/)
func (c *server) Processes(w http.ResponseWriter, r *http.Request) {
	entries := listByType[vanflow.ProcessRecord](c.records)
	results := make([]ProcessRecord, len(entries))
	for i := range entries {
		results[i], _ = c.asProcessRecord(entries[i])
	}
	if err := handleResultSet(w, r, &ProcessListResponse{}, results); err != nil {
		c.logWriteError(r, err)
	}
}
func (c *server) asProcessRecord(entry store.Entry) (ProcessRecord, bool) {
	process, ok := entry.Record.(vanflow.ProcessRecord)
	if !ok {
		return ProcessRecord{}, false
	}
	var pAddresses *[]string
	var addresses []string
	var processGroupID *string
	var siteName *string
	binding := "unbound"
	if process.Group != nil {
		groups := c.records.Index(collector.IndexByTypeName, store.Entry{Record: records.ProcessGroupRecord{Name: dref(process.Group)}})
		if len(groups) > 0 {
			gid := groups[0].Record.Identity()
			processGroupID = &gid
		}
	}
	node := c.coll.Graph().Process(process.ID)
	for _, cNode := range node.Connectors() {
		if addressEntry, ok := cNode.Address().Get(); ok {
			address, ok := addressEntry.Record.(records.AddressRecord)
			if !ok {
				continue
			}
			addresses = append(addresses, fmt.Sprintf("%s@%s@%s", address.Name, address.ID, "tcp"))
		}
	}
	if se, ok := node.Parent().Get(); ok {
		site, _ := se.Record.(vanflow.SiteRecord)
		siteName = site.Name

	}
	if len(addresses) > 0 {
		pAddresses = &addresses
		binding = "bound"
	}

	return ProcessRecord{
		BaseRecord:     toBase(process.BaseRecord, process.Parent, entry.Source.ID),
		Name:           process.Name,
		ParentName:     siteName,
		GroupName:      process.Group,
		GroupIdentity:  processGroupID,
		ProcessRole:    process.Mode,
		ImageName:      process.ImageName,
		Image:          process.ImageVersion,
		SourceHost:     process.SourceHost,
		Addresses:      pAddresses,
		ProcessBinding: &binding,
	}, true
}

// (GET /api/v1alpha1/processes/{id}/)
func (c *server) ProcessById(w http.ResponseWriter, r *http.Request, id string) {
	err := handleOptionalResult(w, &ProcessResponse{}, withMapping(c.asProcessRecord).ByID(c.records, id))
	if err != nil {
		c.logWriteError(r, err)
	}
}

// (GET /api/v1alpha1/processes/{id}/addresses/)
func (c *server) AddressesByProcess(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/processes/{id}/connector/)
func (c *server) ConnectorByProcess(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusNotFound, emptySingleResponse)
}

// (GET /api/v1alpha1/processpairs/)
func (c *server) Processpairs(w http.ResponseWriter, r *http.Request) {
	entries := listByType[records.ProcPairRecord](c.records)
	results := make([]FlowAggregateRecord, len(entries))
	for i := range entries {
		results[i], _ = c.asProcessPair(entries[i])
	}
	if err := handleResultSet(w, r, &FlowAggregateListResponse{}, results); err != nil {
		c.logWriteError(r, err)
	}
}
func (c *server) asProcessPair(entry store.Entry) (FlowAggregateRecord, bool) {
	record, ok := entry.Record.(records.ProcPairRecord)
	if !ok {
		return FlowAggregateRecord{}, false
	}
	var (
		sourceName string
		destName   string

		sourceSiteID   string
		sourceSiteName string
		destSiteID     string
		destSiteName   string
	)
	sourceNode := c.coll.Graph().Process(record.Source)
	if ss, ok := sourceNode.Get(); ok {
		if sr, ok := ss.Record.(vanflow.ProcessRecord); ok {
			sourceName = dref(sr.Name)
		}
	}
	destNode := c.coll.Graph().Process(record.Dest)
	if ds, ok := destNode.Get(); ok {
		if dr, ok := ds.Record.(vanflow.ProcessRecord); ok {
			destName = dref(dr.Name)
		}
	}
	if ssite, ok := sourceNode.Parent().Get(); ok {
		if ssr, ok := ssite.Record.(vanflow.SiteRecord); ok {
			sourceSiteName, sourceSiteID = dref(ssr.Name), ssr.ID
		}
	}
	if dsite, ok := destNode.Parent().Get(); ok {
		if dsr, ok := dsite.Record.(vanflow.SiteRecord); ok {
			destSiteName, destSiteID = dref(dsr.Name), dsr.ID
		}
	}

	return FlowAggregateRecord{
		BaseRecord: BaseRecord{
			Identity:  record.ID,
			StartTime: uint64(record.Start.UnixMicro()),
		},
		PairType:            FlowAggregatePairTypePROCESS,
		Protocol:            record.Protocol,
		SourceId:            record.Source,
		SourceSiteId:        &sourceSiteID,
		SourceSiteName:      &sourceSiteName,
		SourceName:          sourceName,
		DestinationId:       record.Dest,
		DestinationName:     destName,
		DestinationSiteId:   &destSiteID,
		DestinationSiteName: &destSiteName,
		RecordCount:         record.Count,
	}, true
}

// (GET /api/v1alpha1/processpairs/{id}/)
func (c *server) ProcesspairByID(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusNotFound, emptySingleResponse)
}

// (GET /api/v1alpha1/processgrouppairs/)
func (c *server) Processgrouppairs(w http.ResponseWriter, r *http.Request) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/processgrouppairs/{id}/)
func (c *server) ProcessgrouppairByID(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusNotFound, emptySingleResponse)
}

// (GET /api/v1alpha1/processgroups/)
func (c *server) Processgroups(w http.ResponseWriter, r *http.Request) {
	entries := listByType[records.ProcessGroupRecord](c.records)
	results := make([]ProcessGroupRecord, len(entries))
	for i := range entries {
		results[i], _ = c.asProcessGroupRecord(entries[i])
	}
	if err := handleResultSet(w, r, &ProcessGroupListResponse{}, results); err != nil {
		c.logWriteError(r, err)
	}
}
func (c *server) asProcessGroupRecord(entry store.Entry) (ProcessGroupRecord, bool) {
	process, ok := entry.Record.(records.ProcessGroupRecord)
	if !ok {
		return ProcessGroupRecord{}, false
	}
	allProcesses := c.records.Index(store.TypeIndex, store.Entry{Record: vanflow.ProcessRecord{}})
	var (
		pCount int
		role   string
	)
	for _, p := range allProcesses {
		if proc := p.Record.(vanflow.ProcessRecord); proc.Group != nil && *proc.Group == process.Name {
			pCount++
			if role == "" && proc.Mode != nil {
				role = *proc.Mode
			}
		}
	}
	// connectors referencing this process

	return ProcessGroupRecord{
		BaseRecord:       BaseRecord{Identity: process.ID, StartTime: uint64(process.Start.UnixMicro())},
		Name:             process.Name,
		ProcessCount:     pCount,
		ProcessGroupRole: role,
	}, true
}

// (GET /api/v1alpha1/processgroups/{id}/)
func (c *server) ProcessgroupByID(w http.ResponseWriter, r *http.Request, id string) {
	err := handleOptionalResult(w, &ProcessGroupResponse{}, withMapping(c.asProcessGroupRecord).ByID(c.records, id))
	if err != nil {
		c.logWriteError(r, err)
	}
}

// (GET /api/v1alpha1/processgroups/{id}/processes/)
func (c *server) ProcessesByProcessGroup(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/hosts/)
func (c *server) Hosts(w http.ResponseWriter, r *http.Request) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/hosts/{id}/)
func (c *server) HostsByID(w http.ResponseWriter, r *http.Request, id PathID) {
	encode(w, http.StatusNotFound, emptySingleResponse)
}
func (c *server) HostsBySite(w http.ResponseWriter, r *http.Request, id PathID) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/transportflows/)
func (c *server) Transportflows(w http.ResponseWriter, r *http.Request) {
	entries := listByType[vanflow.BIFlowTPRecord](c.coll.Flows)
	results := make([]TransportFlowRecord, len(entries))
	for i := range entries {
		results[i], _ = c.asTransportFlow(entries[i])
	}
	if err := handleResultSet(w, r, &TransportFlowListResponse{}, results); err != nil {
		c.logWriteError(r, err)
	}
}

func (c *server) asTransportFlow(entry store.Entry) (TransportFlowRecord, bool) {
	record, ok := entry.Record.(vanflow.BIFlowTPRecord)
	if !ok {
		return TransportFlowRecord{}, false
	}

	state := c.coll.FlowInfo(record.ID)
	var (
		dest  vanflow.ProcessRecord
		cnctr vanflow.ConnectorRecord
	)

	if de, ok := c.records.Get(state.DestProcID); ok {
		if r, ok := de.Record.(vanflow.ProcessRecord); ok {
			dest = r
		}
	}
	if ce, ok := c.records.Get(state.ConnectorID); ok {
		if r, ok := ce.Record.(vanflow.ConnectorRecord); ok {
			cnctr = r
		}
	}
	return TransportFlowRecord{
		BaseRecord:        toBase(record.BaseRecord, nil, entry.Source.ID),
		DestHost:          dref(dest.SourceHost),
		DestPort:          dref(cnctr.DestPort),
		DestProcessId:     state.DestProcID,
		DestProcessName:   state.DestProcName,
		DestSiteId:        state.DestSiteID,
		DestSiteName:      state.DestSiteName,
		Duration:          nil,
		FlowTrace:         dref(record.Trace),
		Latency:           dref(record.Latency),
		LatencyReverse:    dref(record.LatencyReverse),
		Octets:            dref(record.Octets),
		OctetsReverse:     dref(record.OctetsReverse),
		Protocol:          state.Protocol,
		SourceHost:        dref(record.SourceHost),
		SourcePort:        dref(record.SourcePort),
		SourceProcessId:   state.SourceProcID,
		SourceProcessName: state.SourceProcName,
		SourceSiteId:      state.SourceSiteID,
		SourceSiteName:    state.SourceSiteName,
	}, true
}

// (GET /api/v1alpha1/transportflows/{id}/)
func (c *server) TransportflowByID(w http.ResponseWriter, r *http.Request, id PathID) {
	err := handleOptionalResult(w, &TransportFlowResponse{}, withMapping(c.asTransportFlow).ByID(c.coll.Flows, id))
	if err != nil {
		c.logWriteError(r, err)
	}
}

func (c *server) logWriteError(r *http.Request, err error) {
	requestLogger(c.logger, r).Error("failed to write response", slog.Any("error", err))
}

func index(stor store.Interface, index string, exemplar store.Entry) []store.Entry {
	return ordered(stor.Index(index, exemplar))
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

func handleOptionalResult[R any, T SetResponseOptional[R]](w http.ResponseWriter, response T, getRecord func() (R, bool)) error {
	if record, ok := getRecord(); ok {
		response.Set(&record)
		response.SetCount(1)
	}
	if err := encode(w, http.StatusOK, response); err != nil {
		return fmt.Errorf("response write error: %s", err)
	}
	return nil
}

func handleResultSet[R any, T SetResponse[[]R]](w http.ResponseWriter, r *http.Request, response T, records []R) error {
	x, b := filterAndOrderResults(r, records)
	response.Set(x)
	response.SetCount(b.Count)
	response.SetStatus(b.Status)
	response.SetTotalCount(b.TotalCount)
	if err := encode(w, http.StatusOK, response); err != nil {
		return fmt.Errorf("response write error: %s", err)
	}
	return nil
}

func withMapping[R any](fn func(store.Entry) (R, bool)) oneToOne[R] {
	return oneToOne[R](fn)
}

type oneToOne[R any] func(store.Entry) (R, bool)

func (fn oneToOne[R]) ByID(stor store.Interface, id string) func() (R, bool) {
	return func() (r R, ok bool) {
		if e, ok := stor.Get(id); ok {
			return fn(e)
		}
		return r, false
	}
}
