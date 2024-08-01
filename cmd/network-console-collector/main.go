package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/skupperproject/skupper/cmd/network-console-collector/internal/api"
	"github.com/skupperproject/skupper/cmd/network-console-collector/internal/collector"
	"github.com/skupperproject/skupper/pkg/vanflow/session"

	"github.com/skupperproject/skupper/pkg/version"
)

func run(cfg Config) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	logger := slog.New(slog.Default().Handler())

	// Startup message
	logger.Info("Network Console Collector starting", slog.String("skupper_version", version.Version))

	var (
		sessionConfig session.ContainerConfig
		err           error
	)
	sessionConfig.TLSConfig, err = cfg.RouterTLS.config()
	if err != nil {
		return fmt.Errorf("failed to load router tls configuration: %s", err)
	}

	if cfg.RouterTLS.hasCert() {
		sessionConfig.SASLType = session.SASLTypeExternal
	}

	reg := prometheus.NewRegistry()
	collector := collector.New(logger.With(slog.String("component", "collector")), session.NewContainerFactory(cfg.RouterURL, sessionConfig))

	specContent, err := getSpecContent()
	if err != nil {
		return fmt.Errorf("failed to crate static filesystem for openapi spec: %s", err)
	}

	targetPromAPI, err := defaultPrometheusAPI(cfg.PrometheusAPI)
	if err != nil {
		return fmt.Errorf("error parsing prometheus-api as URL: %s", err)
	}

	handler := api.NewServer(
		api.Config{
			EnableConsole:     cfg.EnableConsole,
			ConsolePath:       cfg.ConsoleLocation,
			PrometheusAPIBase: targetPromAPI,
			CORSAllowAll:      cfg.CORSAllowAll,
			UseAccessLogging:  !cfg.APIDisableAccessLogs,
		}, logger.With(slog.String("component", "api")),
		collector.Records,
		reg,
		specContent,
	)

	s := &http.Server{
		Addr:         cfg.APIListenAddress,
		Handler:      handler,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	tlsEnabled := cfg.APITLS.hasCert()
	if tlsEnabled {
		s.TLSConfig, err = cfg.APITLS.config()
		if err != nil {
			return fmt.Errorf("could not set up certs for api server: %s", err)
		}
	}

	runErrors := make(chan error, 1)
	go func() {
		logger.Info("Starting Network Console API Server",
			slog.String("address", cfg.APIListenAddress),
			slog.Bool("tls", tlsEnabled),
			slog.Bool("console", cfg.EnableConsole))
		var err error
		if tlsEnabled {
			err = s.ListenAndServeTLS("", "")
		} else {
			err = s.ListenAndServe()
		}
		if err != nil {
			runErrors <- fmt.Errorf("server error running api server: %s", err)
		}
	}()

	if cfg.EnableProfile {
		// serve only over localhost loopback
		const pprofAddr = "localhost:9970"
		go func() {
			logger.Info("Starting Network Console Profiling Server",
				slog.String("address", pprofAddr))
			if err := http.ListenAndServe(pprofAddr, nil); err != nil {
				runErrors <- fmt.Errorf("server error running profiler server: %s", err)
			}
		}()
	}

	go func() {
		if err = collector.Run(ctx); err != nil {
			runErrors <- fmt.Errorf("collector error: %s", err)
		}
	}()

	select {
	case err := <-runErrors:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, sCancel := context.WithTimeout(context.Background(), time.Second)
	defer sCancel()
	if err := s.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown did not complete gracefully: %s", err)
	}

	return nil
}

func main() {
	var cfg Config
	flags := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	// if -version used, report and exit
	isVersion := flags.Bool("version", false, "Report the version of skupper the Network Console Collector was built against")

	flags.StringVar(&cfg.RouterURL, "router-endpoint", "amqps://skupper-router-local", "URL to the skupper router amqp(s) endpoint")
	flags.StringVar(&cfg.RouterTLS.Cert, "router-tls-cert", "", "Path to the client certificate for the router endpoint")
	flags.StringVar(&cfg.RouterTLS.Key, "router-tls-key", "", "Path to the client key for the router endpoint")
	flags.StringVar(&cfg.RouterTLS.CA, "router-tls-ca", "", "Path to the CA certificate file for the router endpoint")
	flags.BoolVar(&cfg.RouterTLS.SkipVerify, "router-tls-insecure", false, "Set to skip verification of the router certificate and host name")

	flags.StringVar(&cfg.APIListenAddress, "listen", ":8080", "The address that the API Server will listen on")
	flags.BoolVar(&cfg.APIDisableAccessLogs, "disable-access-logs", false, "Disables access logging for the API Server")
	flags.StringVar(&cfg.APITLS.Cert, "tls-cert", "", "Path to the API Server certificate file")
	flags.StringVar(&cfg.APITLS.Key, "tls-key", "", "Path to the API Server certificate key file matching tls-cert")

	flags.BoolVar(&cfg.EnableConsole, "enable-console", true, "Enables the web console")
	flags.StringVar(&cfg.ConsoleLocation, "console-location", "/app/console", "Location where the console assets are installed")
	flags.StringVar(&cfg.PrometheusAPI, "prometheus-api", "http://network-console-prometheus:9090", "Base Prometheus API HTTP endpoint for console")

	flags.DurationVar(&cfg.FlowRecordTTL, "flow-record-ttl", 15*time.Minute, "How long to retain flow records in memory")
	flags.BoolVar(&cfg.CORSAllowAll, "cors-allow-all", false, "Development option to allow all origins")
	flags.BoolVar(&cfg.EnableProfile, "profile", false, "Exposes the runtime profiling facilities from net/http/pprof on http://localhost:9970")

	flags.Parse(os.Args[1:])
	if *isVersion {
		fmt.Println(version.Version)
		os.Exit(0)
	}

	if err := run(cfg); err != nil {
		slog.Error("network console collector run error", slog.Any("error", err))
		os.Exit(1)
	}
}

func defaultPrometheusAPI(base string) (*url.URL, error) {
	targetPromAPI, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	if targetPromAPI.Path == "" {
		targetPromAPI.Path = "/"
	}
	targetPromAPI = targetPromAPI.JoinPath("/api/v1/")
	return targetPromAPI, nil
}
