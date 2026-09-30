package routercontrol

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"reflect"
	"sync"
	"time"

	protocol "github.com/skupperproject/skupper/internal/routercontrol"
	"k8s.io/client-go/kubernetes"
)

type Authenticator struct {
	Kube                 kubernetes.Interface
	Installation         *Installation
	Authorize            AssignmentAuthorizer
	Gate                 LeaderGate
	Revocations          *SessionRevocations
	Now                  func() time.Time
	AuthorizationTimeout time.Duration
}

// ServerTLSConfig rejects bearer-only clients and validates client-auth chains.
// Per-connection live authorization must additionally be established with Session.
func (a *Authenticator) ServerTLSConfig() *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(a.Installation.ClientCA)
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{{Certificate: [][]byte{a.Installation.ServerCert.Raw, a.Installation.ServerCA.Raw}, PrivateKey: a.Installation.ServerKey, Leaf: a.Installation.ServerCert}},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}
}

type Session struct {
	Identity    Identity
	ctx         context.Context
	cancel      context.CancelFunc
	gate        LeaderGate
	revocations *SessionRevocations
	mu          sync.RWMutex
	err         error
}

func (a *Authenticator) Session(parent context.Context, state tls.ConnectionState) (*Session, error) {
	if a.Gate != nil {
		if err := a.Gate.Check(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNotLeader, err)
		}
	}
	if len(state.VerifiedChains) != 1 || len(state.VerifiedChains[0]) == 0 {
		return nil, ErrUnauthenticated
	}
	if a.Revocations == nil {
		return nil, fmt.Errorf("router-control authorization revocations are not configured")
	}
	leaf := state.VerifiedChains[0][0]
	identity, err := identityFromCertificate(leaf)
	if err != nil {
		return nil, err
	}
	if identity.Installation != a.Installation.Name {
		return nil, fmt.Errorf("%w: certificate belongs to another installation", ErrUnauthenticated)
	}
	now := a.now()
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return nil, fmt.Errorf("%w: client certificate is not currently valid", ErrUnauthenticated)
	}
	revision := a.Revocations.begin(identity.Namespace)
	if err := a.authorizeWithin(parent, identity); err != nil {
		return nil, err
	}
	if a.Gate != nil {
		if err := a.Gate.Check(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNotLeader, err)
		}
	}
	now = a.now()
	if !now.Before(leaf.NotAfter) {
		return nil, fmt.Errorf("%w: client certificate expired during authorization", ErrUnauthenticated)
	}
	ctx, cancel := context.WithCancel(parent)
	session := &Session{Identity: identity, ctx: ctx, cancel: cancel, gate: a.Gate, revocations: a.Revocations}
	if err := a.Revocations.register(session, revision); err != nil {
		cancel()
		return nil, err
	}
	if a.Gate != nil {
		if err := a.Gate.Check(); err != nil {
			session.Close()
			return nil, fmt.Errorf("%w: %v", ErrNotLeader, err)
		}
	}
	now = a.now()
	if !now.Before(leaf.NotAfter) {
		session.Close()
		return nil, fmt.Errorf("%w: client certificate expired during authorization registration", ErrUnauthenticated)
	}
	if err := session.Check(); err != nil {
		session.Close()
		return nil, err
	}
	go session.monitor(leaf.NotAfter.Sub(now))
	return session, nil
}

func (a *Authenticator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Authenticator) authorizeWithin(ctx context.Context, identity Identity) error {
	timeout := a.AuthorizationTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	authCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- a.authorize(authCtx, identity) }()
	select {
	case err := <-result:
		return err
	case <-authCtx.Done():
		return fmt.Errorf("%w: live authorization was not confirmed: %v", ErrUnauthorized, authCtx.Err())
	case <-gateDone(a.Gate):
		return ErrNotLeader
	}
}

func (a *Authenticator) authorize(ctx context.Context, expected Identity) error {
	enroller := Enroller{Kube: a.Kube, Installation: a.Installation}
	actual, err := enroller.liveIdentity(ctx, expected.Namespace, expected.ServiceAccount, expected.ServiceAccountUID, expected.PodName, expected.PodUID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf("%w: current workload identity changed", ErrUnauthorized)
	}
	if a.Authorize == nil {
		return fmt.Errorf("%w: no assignment authorizer", ErrUnauthorized)
	}
	if err := a.Authorize(ctx, actual); err != nil {
		return fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	return nil
}

func (s *Session) monitor(untilExpiry time.Duration) {
	if untilExpiry < 0 {
		untilExpiry = 0
	}
	timer := time.NewTimer(untilExpiry)
	defer timer.Stop()
	defer s.revocations.unregister(s)
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-timer.C:
			s.fail(fmt.Errorf("client certificate expired"))
			return
		case <-gateDone(s.gate):
			s.fail(ErrNotLeader)
			return
		}
	}
}

func (s *Session) Context() context.Context { return s.ctx }

func (s *Session) Close() {
	s.cancel()
	if s.revocations != nil {
		s.revocations.unregister(s)
	}
}

func (s *Session) Check() error {
	if s.gate != nil {
		if err := s.gate.Check(); err != nil {
			return fmt.Errorf("%w: %v", ErrNotLeader, err)
		}
	}
	select {
	case <-s.ctx.Done():
		s.mu.RLock()
		defer s.mu.RUnlock()
		if s.err != nil {
			return s.err
		}
		return s.ctx.Err()
	default:
		return nil
	}
}

// CheckTarget prevents a message from substituting another authenticated target.
func (s *Session) CheckTarget(target protocol.TargetIdentity) error {
	if err := s.Check(); err != nil {
		return err
	}
	if string(target.NamespaceUID) != string(s.Identity.NamespaceUID) || string(target.SiteUID) != string(s.Identity.SiteUID) || target.RouterGroup != s.Identity.Group {
		return fmt.Errorf("%w: message target differs from authenticated target", ErrUnauthorized)
	}
	return nil
}

func (s *Session) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
	s.cancel()
}

func gateDone(gate LeaderGate) <-chan struct{} {
	if gate == nil {
		return nil
	}
	return gate.Done()
}
