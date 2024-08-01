package api

import (
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
	addressServer
	connectorServer
	processServer
	processgroupServer
	linkServer
	siteServer
	routerServer
	listenerServer
}

func buildServer(logger *slog.Logger, records store.Interface) *server {
	return &server{
		siteServer: siteServer{
			logger:  logger,
			records: records,
		},
		routerServer: routerServer{
			logger:  logger,
			records: records,
		},
		processServer: processServer{
			logger:  logger,
			records: records,
		},
	}
}

type siteServer struct {
	logger  *slog.Logger
	records store.Interface
}

// (GET /api/v1alpha1/sites/)
func (c *siteServer) Sites(w http.ResponseWriter, r *http.Request) {
	log := requestLogger(c.logger, r)
	var response SiteListResponse

	sites := listByType[vanflow.SiteRecord](c.records)
	response.Results = make([]SiteRecord, len(sites))
	for i := range sites {
		response.Results[i] = toSiteRecord(sites[i])
	}
	response.Results, response.BaseResponse = filterAndOrderResults(r, response.Results)
	if err := encode(w, http.StatusOK, response); err != nil {
		log.Error("response write error", slog.Any("error", err))
	}
}

// (GET /api/v1alpha1/sites/{id}/)
func (c *siteServer) SiteById(w http.ResponseWriter, r *http.Request, id string) {
	log := requestLogger(c.logger, r)

	var (
		response SiteResponse
		status   int = http.StatusNotFound
	)

	if site, ok := c.records.Get(id); ok {
		status = http.StatusOK
		record := toSiteRecord(site)
		response.Results = &record
		response.Count = 1
	}
	if err := encode(w, status, response); err != nil {
		log.Error("response write error", slog.Any("error", err))
	}
}

// (GET /api/v1alpha1/sites/{id}/routers/)
func (c *siteServer) RoutersBySite(w http.ResponseWriter, r *http.Request, id string) {
	log := requestLogger(c.logger, r)
	var response RouterListResponse

	exemplar := store.Entry{Record: vanflow.RouterRecord{Parent: &id}}
	entries := index(c.records, collector.IndexByTypeParent, exemplar)
	response.Results = make([]RouterRecord, len(entries))
	for i := range entries {
		response.Results[i] = toRouterRecord(entries[i])
	}
	response.Results, response.BaseResponse = filterAndOrderResults(r, response.Results)
	if err := encode(w, http.StatusOK, response); err != nil {
		log.Error("response write error", slog.Any("error", err))
	}
}

// (GET /api/v1alpha1/sites/{id}/processes/)
func (c *siteServer) ProcessesBySite(w http.ResponseWriter, r *http.Request, id string) {
	log := requestLogger(c.logger, r)
	var response ProcessListResponse

	exemplar := store.Entry{Record: vanflow.ProcessRecord{Parent: &id}}
	entries := index(c.records, collector.IndexByTypeParent, exemplar)
	response.Results = make([]ProcessRecord, len(entries))
	for i := range entries {
		response.Results[i] = toProcessRecord(entries[i])
	}
	response.Results, response.BaseResponse = filterAndOrderResults(r, response.Results)
	if err := encode(w, http.StatusOK, response); err != nil {
		log.Error("response write error", slog.Any("error", err))
	}
}

// (GET /api/v1alpha1/sites/{id}/flows/)
func (c *siteServer) FlowsBySite(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/sites/{id}/links/)
func (c *siteServer) LinksBySite(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/sitepairs/)
func (c *siteServer) Sitepairs(w http.ResponseWriter, r *http.Request) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/sitepairs/{id}/)
func (c *siteServer) SitepairByID(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusNotFound, emptySingleResponse)
}

type routerServer struct {
	logger  *slog.Logger
	records store.Interface
}

// (GET /api/v1alpha1/routers/)
func (c *routerServer) Routers(w http.ResponseWriter, r *http.Request) {
	log := requestLogger(c.logger, r)
	var response RouterListResponse

	entries := listByType[vanflow.RouterRecord](c.records)
	response.Results = make([]RouterRecord, len(entries))
	for i := range entries {
		response.Results[i] = toRouterRecord(entries[i])
	}
	response.Results, response.BaseResponse = filterAndOrderResults(r, response.Results)
	if err := encode(w, http.StatusOK, response); err != nil {
		log.Error("response write error", slog.Any("error", err))
	}
}

// (GET /api/v1alpha1/routers/{id}/)
func (c *routerServer) RouterByID(w http.ResponseWriter, r *http.Request, id string) {
	var (
		response RouterResponse
		status   int = http.StatusNotFound
		log          = requestLogger(c.logger, r)
	)

	if entry, ok := c.records.Get(id); ok {
		status = http.StatusOK
		record := toRouterRecord(entry)
		response.Results = &record
		response.Count = 1
	}
	if err := encode(w, status, response); err != nil {
		log.Error("response write error", slog.Any("error", err))
	}
}

// (GET /api/v1alpha1/routers/{id}/connectors/)
func (c *routerServer) ConnectorsByRouter(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/routers/{id}/flows/)
func (c *routerServer) FlowsByRouter(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/routers/{id}/links/)
func (c *routerServer) LinksByRouter(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/routers/{id}/listeners/)
func (c *routerServer) ListenersByRouter(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
}

type addressServer struct {
}

// (GET /api/v1alpha1/addresses/)
func (c *addressServer) Addresses(w http.ResponseWriter, r *http.Request) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/addresses/{id}/)
func (c *addressServer) AddressByID(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusNotFound, emptySingleResponse)
}

// (GET /api/v1alpha1/addresses/{id}/connectors/)
func (c *addressServer) ConnectorsByAddress(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/addresses/{id}/listeners/)
func (c *addressServer) ListenersByAddress(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/addresses/{id}/processes/)
func (c *addressServer) ProcessesByAddress(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/addresses/{id}/processpairs/)
func (c *addressServer) ProcessPairsByAddress(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
}

type connectorServer struct{}

// (GET /api/v1alpha1/connectors/)
func (c *connectorServer) Connectors(w http.ResponseWriter, r *http.Request) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/connectors/{id}/)
func (c *connectorServer) ConnectorByID(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusNotFound, emptySingleResponse)
}

type linkServer struct{}

// (GET /api/v1alpha1/links/)
func (c *linkServer) Links(w http.ResponseWriter, r *http.Request) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/links/{id}/)
func (c *linkServer) LinkByID(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusNotFound, emptySingleResponse)
}

// (GET /api/v1alpha1/routeraccess/)
func (c *linkServer) Routeraccess(w http.ResponseWriter, r *http.Request) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/routeraccess/{id}/)
func (c *linkServer) RouteraccessByID(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusNotFound, emptySingleResponse)
}

// (GET /api/v1alpha1/routerlinks/)
func (c *linkServer) Routerlinks(w http.ResponseWriter, r *http.Request) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/routerlinks/{id}/)
func (c *linkServer) RouterlinkByID(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusNotFound, emptySingleResponse)
}

type listenerServer struct{}

// (GET /api/v1alpha1/listeners/)
func (c *listenerServer) Listeners(w http.ResponseWriter, r *http.Request) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/listeners/{id}/)
func (c *listenerServer) ListenerByID(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusNotFound, emptySingleResponse)
}

// (GET /api/v1alpha1/listeners/{id}/flows)
func (c *listenerServer) FlowsByListener(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
}

type processServer struct {
	logger  *slog.Logger
	records store.Interface
}

// (GET /api/v1alpha1/processes/)
func (c *processServer) Processes(w http.ResponseWriter, r *http.Request) {
	log := requestLogger(c.logger, r)
	var response ProcessListResponse

	entries := listByType[vanflow.ProcessRecord](c.records)
	response.Results = make([]ProcessRecord, len(entries))
	for i := range entries {
		response.Results[i] = toProcessRecord(entries[i])
	}
	response.Results, response.BaseResponse = filterAndOrderResults(r, response.Results)
	if err := encode(w, http.StatusOK, response); err != nil {
		log.Error("response write error", slog.Any("error", err))
	}
}

// (GET /api/v1alpha1/processes/{id}/)
func (c *processServer) ProcessById(w http.ResponseWriter, r *http.Request, id string) {
	var (
		response ProcessResponse
		status   int = http.StatusNotFound
		log          = requestLogger(c.logger, r)
	)

	if entry, ok := c.records.Get(id); ok {
		status = http.StatusOK
		record := toProcessRecord(entry)
		response.Results = &record
		response.Count = 1
	}
	if err := encode(w, status, response); err != nil {
		log.Error("response write error", slog.Any("error", err))
	}
}

// (GET /api/v1alpha1/processes/{id}/addresses/)
func (c *processServer) AddressesByProcess(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/processes/{id}/connector/)
func (c *processServer) ConnectorByProcess(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusNotFound, emptySingleResponse)
}

// (GET /api/v1alpha1/processpairs/)
func (c *processServer) Processpairs(w http.ResponseWriter, r *http.Request) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/processpairs/{id}/)
func (c *processServer) ProcesspairByID(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusNotFound, emptySingleResponse)
}

type processgroupServer struct{}

// (GET /api/v1alpha1/processgrouppairs/)
func (c *processgroupServer) Processgrouppairs(w http.ResponseWriter, r *http.Request) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/processgrouppairs/{id}/)
func (c *processgroupServer) ProcessgrouppairByID(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusNotFound, emptySingleResponse)
}

// (GET /api/v1alpha1/processgroups/)
func (c *processgroupServer) Processgroups(w http.ResponseWriter, r *http.Request) {
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/processgroups/{id}/)
func (c *processgroupServer) ProcessgroupByID(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusNotFound, emptySingleResponse)
}

// (GET /api/v1alpha1/processgroups/{id}/processes/)
func (c *processgroupServer) ProcessesByProcessGroup(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusOK, emptyListResponse)
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
