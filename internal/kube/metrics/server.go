package metrics

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	iflag "github.com/skupperproject/skupper/internal/flag"
)

const metricsPath = "/metrics"

type Config struct {
	Address string
}

func BoundConfig(flags *flag.FlagSet) *Config {
	cfg := &Config{}
	iflag.StringVar(flags, &cfg.Address, "metrics-address", "SKUPPER_METRICS_ADDRESS", ":9000", "The address on which the metrics http server should listen.")
	return cfg
}

func NewServer(cfg *Config, registry *prometheus.Registry) *Server {
	mux := http.NewServeMux()
	mux.Handle(metricsPath, promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	srv := &http.Server{
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
		IdleTimeout:  120 * time.Second,
		Handler:      mux,
	}
	return &Server{
		config: *cfg,
		server: srv,
	}
}

type Server struct {
	config Config
	server *http.Server
}

func (s *Server) Start(stopCh <-chan struct{}) error {
	listenCtx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-stopCh:
			cancel()
		case <-listenCtx.Done():
		}
	}()
	defer cancel()

	var lc net.ListenConfig
	ln, err := lc.Listen(listenCtx, "tcp", s.config.Address)
	if err != nil {
		return fmt.Errorf("failed to start listener: %s", err)
	}
	go func() {
		if err := s.server.Serve(ln); err != nil {
			log.Printf("metrics server error: %s", err)
		}
	}()
	return nil
}
