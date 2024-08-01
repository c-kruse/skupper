package api

import (
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"

	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/skupperproject/skupper/pkg/vanflow/store"
)

type Config struct {
	EnableConsole     bool
	ConsolePath       string
	PrometheusAPIBase *url.URL
	CORSAllowAll      bool
	UseAccessLogging  bool
}

func NewServer(cfg Config,
	logger *slog.Logger,
	recordStore store.Interface,
	metricsRegistry *prometheus.Registry,
	specFS fs.FS) http.Handler {
	router := mux.NewRouter()

	server := buildServer(logger, recordStore)

	var middlewares []mux.MiddlewareFunc = []mux.MiddlewareFunc{
		handlers.CompressHandler,
	}
	var proxyMiddlewares []mux.MiddlewareFunc

	if cfg.CORSAllowAll { // do not use CORS handler with proxy handlers: the upstream service should handle this
		middlewares = append(middlewares, handlers.CORS())
	}

	if cfg.UseAccessLogging {
		m := func(next http.Handler) http.Handler {
			return handlers.LoggingHandler(os.Stdout, next)
		}
		middlewares = append(middlewares, m)
		proxyMiddlewares = append(proxyMiddlewares, m)
	}
	addRoutes(router, server, metricsRegistry, specFS, cfg.EnableConsole, cfg.ConsolePath, cfg.PrometheusAPIBase, middlewares, proxyMiddlewares)

	return router
}

const proxyPath = "/api/v1alpha1/internal/prom"

func addRoutes(router *mux.Router,
	server ServerInterface,
	metricsRegistry *prometheus.Registry,
	specFS fs.FS,
	enableConsole bool,
	consoleDist string,
	targetPromAPI *url.URL,
	middlewares []mux.MiddlewareFunc,
	proxyMiddlewares []mux.MiddlewareFunc,
) {
	proxyRouter := router.PathPrefix(proxyPath).Subrouter()
	stdRouter := router.NewRoute().Subrouter()
	stdRouter.StrictSlash(true)
	stdRouter.Handle("/metrics", handleMetrics(metricsRegistry))
	stdRouter.PathPrefix("/swagger").Handler(http.StripPrefix("/swagger/", handleSwagger(specFS)))
	HandlerWithOptions(server, GorillaServerOptions{BaseRouter: stdRouter})

	if !enableConsole {
		return
	}
	stdRouter.Path("/api/v1alpha1/user").Handler(handleNoContent())
	stdRouter.Path("/api/v1alpha1/logout").Handler(handleNoContent())
	proxyRouter.PathPrefix("/").Handler(http.StripPrefix(proxyPath, handleProxyPrometheusAPI(targetPromAPI)))

	stdRouter.PathPrefix("/").Handler(handleConsoleAssets(consoleDist))
	stdRouter.Use(middlewares...)
	proxyRouter.Use(proxyMiddlewares...)
}

func handleMetrics(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg})
}

func handleSwagger(content fs.FS) http.Handler {
	return http.FileServer(http.FS(content))
}
func handleConsoleAssets(consoleDir string) http.Handler {
	return http.FileServer(http.Dir(consoleDir))
}

func handleProxyPrometheusAPI(target *url.URL) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(target)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/query/":
			r.URL.Path = "/query"
			fallthrough
		case "/query":
			proxy.ServeHTTP(w, r)
		case "/rangequery":
			fallthrough
		case "/rangequery/":
			fallthrough
		case "/query_range/":
			r.URL.Path = "/query_range"
			fallthrough
		case "/query_range":
			proxy.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func handleNoContent() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
}
