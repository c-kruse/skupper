package grants

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/skupperproject/skupper/internal/utils/tlscfg"
)

type Server struct {
	tlsEnabled bool
	lock       sync.RWMutex
	cert       *tls.Certificate
	server     *http.Server
	listener   net.Listener
	logger     *slog.Logger
}

func newServer(addr string, tlsEnabled bool, handler http.Handler) *Server {
	return &Server{
		server: &http.Server{
			Addr:         addr,
			Handler:      handler,
			ReadTimeout:  60 * time.Second,
			WriteTimeout: 60 * time.Second,
			TLSConfig:    tlscfg.Modern(),
		},
		tlsEnabled: tlsEnabled,
		logger:     slog.New(slog.Default().Handler()).With(slog.String("component", "kube.grants.server")),
	}
}

func (s *Server) start() {
	go s.listenAndServe()
}

func (s *Server) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.cert, nil
}

func (s *Server) setCertificate(cert *tls.Certificate) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.cert = cert
}

func (s *Server) setCertificateFromSecret(secret *corev1.Secret) error {
	cert, err := tls.X509KeyPair(secret.Data["tls.crt"], secret.Data["tls.key"])
	if err != nil {
		return err
	}
	s.setCertificate(&cert)
	return nil
}

func (s *Server) listen() error {
	listener, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		return err
	}
	s.logger.Info("Grant server listening", slog.Any("address", listener.Addr()))
	s.lock.Lock()
	s.listener = listener
	s.lock.Unlock()
	return nil
}

func (s *Server) serve() error {
	listener := s.currentListener()
	if listener == nil {
		return fmt.Errorf("Cannot serve before listen() is called")
	}
	return s.serveListener(listener)
}

func (s *Server) serveListener(listener net.Listener) error {
	if s.tlsEnabled {
		s.server.TLSConfig.GetCertificate = s.getCertificate
		return s.server.ServeTLS(listener, "", "")
	} else {
		return s.server.Serve(listener)
	}
}

func (s *Server) listenAndServe() error {
	if err := s.listen(); err != nil {
		s.logger.Error("Grant server failed to listen", slog.String("address", s.server.Addr), slog.Any("error", err))
		return err
	}
	listener := s.currentListener()
	defer func() {
		_ = listener.Close()
		s.clearListener(listener)
	}()
	return s.serveListener(listener)
}

func (s *Server) stop() error {
	err := s.server.Close()
	listener := s.takeListener()
	if listener != nil {
		_ = listener.Close()
	}
	return err
}

func (s *Server) run(ctx context.Context, gate EffectGate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := gate.Check(); err != nil {
		return err
	}
	if err := s.listen(); err != nil {
		return err
	}
	listener := s.currentListener()
	if err := ctx.Err(); err != nil {
		_ = listener.Close()
		s.clearListener(listener)
		return err
	}
	if err := gate.Check(); err != nil {
		_ = listener.Close()
		s.clearListener(listener)
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		select {
		case <-runCtx.Done():
		case <-gate.Done():
		}
		_ = s.server.Close()
		_ = listener.Close()
		s.clearListener(listener)
		close(done)
	}()
	err := s.serveListener(listener)
	cancel()
	<-done
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *Server) port() int {
	listener := s.currentListener()
	if listener == nil {
		return 0
	}
	return listener.Addr().(*net.TCPAddr).Port
}

func (s *Server) currentListener() net.Listener {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.listener
}

func (s *Server) takeListener() net.Listener {
	s.lock.Lock()
	defer s.lock.Unlock()
	listener := s.listener
	s.listener = nil
	return listener
}

func (s *Server) clearListener(listener net.Listener) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.listener == listener {
		s.listener = nil
	}
}
