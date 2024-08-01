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
	logger  *slog.Logger
	records store.Interface

	siteHandler    handleByID
	routerHandler  handleByID
	processHandler handleByID

	siteListHandler    handleStoreEntries
	routerListHandler  handleStoreEntries
	processListHandler handleStoreEntries
}

func buildServer(logger *slog.Logger, records store.Interface) *server {
	return &server{
		logger:  logger,
		records: records,

		siteHandler:    handlerForStore[SiteRecord, SiteResponse](logger, records, toSiteRecord),
		routerHandler:  handlerForStore[RouterRecord, RouterResponse](logger, records, toRouterRecord),
		processHandler: handlerForStore[ProcessRecord, ProcessResponse](logger, records, toProcessRecord),

		siteListHandler:    handlerForEntries[SiteRecord, SiteListResponse](logger, toSiteRecord),
		routerListHandler:  handlerForEntries[RouterRecord, RouterListResponse](logger, toRouterRecord),
		processListHandler: handlerForEntries[ProcessRecord, ProcessListResponse](logger, toProcessRecord),
	}
}

// (GET /api/v1alpha1/sites/)
func (c *server) Sites(w http.ResponseWriter, r *http.Request) {
	c.siteListHandler.Handle(w, r, listByType[vanflow.SiteRecord](c.records))
}

// (GET /api/v1alpha1/sites/{id}/)
func (c *server) SiteById(w http.ResponseWriter, r *http.Request, id string) {
	c.siteHandler.Handle(w, r, id)
}

// (GET /api/v1alpha1/sites/{id}/routers/)
func (c *server) RoutersBySite(w http.ResponseWriter, r *http.Request, id string) {
	exemplar := store.Entry{Record: vanflow.RouterRecord{Parent: &id}}
	c.routerListHandler.Handle(w, r, index(c.records, collector.IndexByTypeParent, exemplar))
}

// (GET /api/v1alpha1/sites/{id}/processes/)
func (c *server) ProcessesBySite(w http.ResponseWriter, r *http.Request, id string) {
	exemplar := store.Entry{Record: vanflow.ProcessRecord{Parent: &id}}
	c.routerListHandler.Handle(w, r, index(c.records, collector.IndexByTypeParent, exemplar))
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
	c.routerListHandler.Handle(w, r, listByType[vanflow.RouterRecord](c.records))
}

// (GET /api/v1alpha1/routers/{id}/)
func (c *server) RouterByID(w http.ResponseWriter, r *http.Request, id string) {
	c.routerHandler.Handle(w, r, id)
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

// (GET /api/v1alpha1/addresses/)
func (c *server) Addresses(w http.ResponseWriter, r *http.Request) {
	encode(w, http.StatusOK, emptyListResponse)
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
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/connectors/{id}/)
func (c *server) ConnectorByID(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusNotFound, emptySingleResponse)
}

// (GET /api/v1alpha1/links/)
func (c *server) Links(w http.ResponseWriter, r *http.Request) {
	encode(w, http.StatusOK, emptyListResponse)
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

// (GET /api/v1alpha1/routerlinks/)
func (c *server) Routerlinks(w http.ResponseWriter, r *http.Request) {
	encode(w, http.StatusOK, emptyListResponse)
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
	c.processListHandler.Handle(w, r, listByType[vanflow.ProcessRecord](c.records))
}

// (GET /api/v1alpha1/processes/{id}/)
func (c *server) ProcessById(w http.ResponseWriter, r *http.Request, id string) {
	c.processHandler.Handle(w, r, id)
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
	encode(w, http.StatusOK, emptyListResponse)
}

// (GET /api/v1alpha1/processgroups/{id}/)
func (c *server) ProcessgroupByID(w http.ResponseWriter, r *http.Request, id string) {
	encode(w, http.StatusNotFound, emptySingleResponse)
}

// (GET /api/v1alpha1/processgroups/{id}/processes/)
func (c *server) ProcessesByProcessGroup(w http.ResponseWriter, r *http.Request, id string) {
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

type wrapSetOptional[R any, T any] interface {
	SetResponseOptional[R]
	*T
}

type handleByID interface {
	Handle(w http.ResponseWriter, r *http.Request, id string)
}

func handlerForStore[R any, T any, TP wrapSetOptional[R, T]](log *slog.Logger, stor store.Interface, x func(store.Entry) (R, bool)) handleByID {
	return handler[R, T, TP]{
		X:      x,
		Logger: log,
		Stor:   stor,
	}
}

type handler[R any, T any, TP wrapSetOptional[R, T]] struct {
	X      func(store.Entry) (R, bool)
	Logger *slog.Logger
	Stor   store.Interface
}

func (h handler[R, T, TP]) Handle(w http.ResponseWriter, r *http.Request, id string) {
	var (
		response TP  = new(T)
		status   int = http.StatusNotFound
	)

	if entry, ok := h.Stor.Get(id); ok {

		if record, ok := h.X(entry); ok {
			status = http.StatusOK
			response.Set(&record)
			response.SetCount(1)
		}
	}
	if err := encode(w, status, response); err != nil {
		log := requestLogger(h.Logger, r)
		log.Error("response write error", slog.Any("error", err))
	}
}

type wrapSetRequired[R any, T any] interface {
	SetResponse[R]
	*T
}

type handleStoreEntries interface {
	Handle(w http.ResponseWriter, r *http.Request, entries []store.Entry)
}

func handlerForEntries[R any, T any, TP wrapSetRequired[[]R, T]](log *slog.Logger, x func(store.Entry) (R, bool)) handleStoreEntries {
	return listHandler[R, T, TP]{
		X:      x,
		Logger: log,
	}
}

type listHandler[R any, T any, TP wrapSetRequired[[]R, T]] struct {
	X      func(store.Entry) (R, bool)
	Logger *slog.Logger
}

func (h listHandler[R, T, TP]) Handle(w http.ResponseWriter, r *http.Request, entries []store.Entry) {
	var (
		response TP  = new(T)
		status   int = http.StatusNotFound
	)

	results := make([]R, 0, len(entries))
	for _, entry := range entries {
		if record, ok := h.X(entry); ok {
			results = append(results, record)
		}
	}
	results, base := filterAndOrderResults(r, results)
	response.Set(results)
	response.SetCount(base.Count)
	response.SetTotalCount(base.TotalCount)
	response.SetStatus(base.Status)
	response.SetTimeRangeCount(base.TimeRangeCount)
	if err := encode(w, status, response); err != nil {
		log := requestLogger(h.Logger, r)
		log.Error("response write error", slog.Any("error", err))
	}
}
