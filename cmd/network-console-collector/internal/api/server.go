package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/skupperproject/skupper/cmd/network-console-collector/internal/collector"
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
	records store.Interface
	graph   *collector.Graph
}

func buildServer(logger *slog.Logger, records store.Interface, g *collector.Graph) *server {
	return &server{
		logger:  logger,
		records: records,
		graph:   g,
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
		results[i], _ = toProcessRecord(entries[i])
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
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/sitepairs/)
func (c *server) Sitepairs(w http.ResponseWriter, r *http.Request) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/sitepairs/{id}/)
func (c *server) SitepairByID(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusNotFound, emptySingleResponse)
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
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/routers/{id}/listeners/)
func (c *server) ListenersByRouter(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
}

func (c *server) addresses() map[string]AddressRecord {
	routingKeys := collector.NodesByType[collector.RoutingKeyNode](c.graph)
	records := make(map[string]AddressRecord, len(routingKeys))
	for _, address := range routingKeys {
		listenrIDs := collector.ChildNodes[collector.ListenerNode](c.graph, address)
		connectorIDs := collector.ChildNodes[collector.ListenerNode](c.graph, address)
		if len(listenrIDs)+len(connectorIDs) == 0 {
			continue
		}
		records[address] = AddressRecord{
			BaseRecord:     BaseRecord{Identity: address},
			Name:           address,
			ConnectorCount: len(connectorIDs),
			ListenerCount:  len(listenrIDs),
		}
	}

	return nil
}

// (GET /api/v1alpha1/addresses/)
func (c *server) Addresses(w http.ResponseWriter, r *http.Request) {
	entries := listByType[collector.AddressRecord](c.records)
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
	if record.Address != nil {
		if id, ok := collector.ParentNode[collector.AddressNode](c.graph, *record.Address); ok {
			addressID = id
		}
	}
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
	accessEntry, ok := c.records.Get(*link.Peer)
	if !ok {
		return r, false
	}
	routerAccess, ok := accessEntry.Record.(vanflow.RouterAccessRecord)
	if !ok {
		return r, false
	}

	var (
		sourceSiteID string
		destSiteID   string
	)
	if peerSite, ok := collector.ParentNode[collector.SiteNode](c.graph, routerAccess.ID); ok {
		destSiteID = peerSite
	}
	if linkSite, ok := collector.ParentNode[collector.SiteNode](c.graph, link.ID); ok {
		sourceSiteID = linkSite
	}
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
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/routeraccess/{id}/)
func (c *server) RouteraccessByID(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusNotFound, emptySingleResponse)
}

func (c *server) asAddress(entry store.Entry) (AddressRecord, bool) {
	record, ok := entry.Record.(collector.AddressRecord)
	if !ok {
		return AddressRecord{}, false
	}
	var (
		listenerCt  int
		connectorCt int
	)
	listenerIDs := collector.ChildNodes[collector.ListenerNode](c.graph, record.Name)
	listenerCt = len(listenerIDs)

	connectorIDs := collector.ChildNodes[collector.ConnectorNode](c.graph, record.Name)
	connectorCt = len(connectorIDs)

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
	sourceSiteID, _ = collector.ParentNode[collector.SiteNode](c.graph, link.ID)
	if link.Peer != nil {
		destSiteID, _ = collector.ParentNode[collector.SiteNode](c.graph, *link.Peer)
	}
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
	var addresses *[]string
	var processGroupID *string
	if process.Group != nil {
		groups := c.records.Index(collector.IndexByTypeName, store.Entry{Record: collector.ProcessGroupRecord{Name: dref(process.Group)}})
		if len(groups) > 0 {
			gid := groups[0].Record.Identity()
			processGroupID = &gid
		}
	}
	// connectors referencing this process

	return ProcessRecord{
		BaseRecord:    toBase(process.BaseRecord, process.Parent, entry.Source.ID),
		Name:          process.Name,
		GroupName:     process.Group,
		GroupIdentity: processGroupID,
		ProcessRole:   process.Mode,
		ImageName:     process.ImageName,
		Image:         process.ImageVersion,
		SourceHost:    process.SourceHost,
		Addresses:     addresses,
	}, true
}

// (GET /api/v1alpha1/processes/{id}/)
func (c *server) ProcessById(w http.ResponseWriter, r *http.Request, id string) {
	err := handleOptionalResult(w, &ProcessResponse{}, withMapping(toProcessRecord).ByID(c.records, id))
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
	encode(w, http.StatusOK, emptyListResponse)
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
}

// (GET /api/v1alpha1/processgroups/{id}/)
func (c *server) ProcessgroupByID(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusNotFound, emptySingleResponse)
}

// (GET /api/v1alpha1/processgroups/{id}/processes/)
func (c *server) ProcessesByProcessGroup(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
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
