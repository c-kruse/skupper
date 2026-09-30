package routercontrol

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type testGate struct {
	done chan struct{}
	mu   sync.RWMutex
	err  error
}

func (g *testGate) Check() error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.err
}
func (g *testGate) Done() <-chan struct{} { return g.done }
func (g *testGate) SetError(err error) {
	g.mu.Lock()
	g.err = err
	g.mu.Unlock()
}

func TestSessionRevokesDeletedPodAndLostAssignment(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(t *testing.T, fixture *sessionFixture)
	}{
		{name: "deleted Pod", change: func(t *testing.T, fixture *sessionFixture) {
			if err := fixture.client.CoreV1().Pods("site-ns").Delete(context.Background(), "router-pod", metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
			fixture.revocations.InvalidateAuthorization(AuthorizationPod, "site-ns", "router-pod")
		}},
		{name: "lost assignment", change: func(_ *testing.T, fixture *sessionFixture) {
			fixture.revocations.InvalidateAuthorization(AuthorizationAssignment, "site-ns", "skupper")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSessionFixture(t)
			session := fixture.session(t, time.Hour)
			test.change(t, fixture)
			select {
			case <-session.Context().Done():
			case <-time.After(time.Second):
				t.Fatal("session was not revoked")
			}
			if err := session.Check(); err == nil {
				t.Fatal("revoked session passed its gate")
			}
		})
	}
}

func TestSessionClosesAtCertificateExpiryAndLeadershipLoss(t *testing.T) {
	fixture := newSessionFixture(t)
	session := fixture.session(t, 20*time.Millisecond)
	select {
	case <-session.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("session remained active after certificate expiry")
	}
	if err := session.Check(); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expiry error = %v", err)
	}

	fixture = newSessionFixture(t)
	session = fixture.session(t, time.Hour)
	fixture.gate.SetError(ErrNotLeader)
	close(fixture.gate.done)
	select {
	case <-session.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("session remained active after leadership loss")
	}
}

func TestSessionRejectsExpiredCertificateBeforeAuthorization(t *testing.T) {
	fixture := newSessionFixture(t)
	leaf := *fixture.leaf
	leaf.NotAfter = testNow
	called := false
	authenticator := &Authenticator{Kube: fixture.client, Installation: fixture.install, Gate: fixture.gate, Revocations: fixture.revocations, Now: func() time.Time { return testNow }, Authorize: func(context.Context, Identity) error {
		called = true
		return nil
	}}
	if _, err := authenticator.Session(context.Background(), tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{&leaf, fixture.install.ClientCA}}}); err == nil || !strings.Contains(err.Error(), "not currently valid") {
		t.Fatalf("expired certificate error = %v", err)
	}
	if called {
		t.Fatal("authorization ran for an already-expired certificate")
	}
}

func TestInitialAuthorizationHasHardDeadline(t *testing.T) {
	fixture := newSessionFixture(t)
	release := make(chan struct{})
	authenticator := &Authenticator{Kube: fixture.client, Installation: fixture.install, Gate: fixture.gate, Revocations: fixture.revocations, Now: func() time.Time { return testNow }, AuthorizationTimeout: 20 * time.Millisecond, Authorize: func(context.Context, Identity) error {
		<-release // Deliberately ignore context to model a stuck dependency.
		return nil
	}}
	started := time.Now()
	_, err := authenticator.Session(context.Background(), tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{fixture.leaf, fixture.install.ClientCA}}})
	close(release)
	if err == nil || !errors.Is(err, ErrAuthorizationUnavailable) {
		t.Fatalf("hung initial authorization error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("hung initial authorization blocked for %v", elapsed)
	}
}

func TestBlockedAdmissionCannotSuppressLeadershipLoss(t *testing.T) {
	fixture := newSessionFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	authenticator := &Authenticator{Kube: fixture.client, Installation: fixture.install, Gate: fixture.gate, Revocations: fixture.revocations, Now: func() time.Time { return testNow }, AuthorizationTimeout: time.Hour, Authorize: func(context.Context, Identity) error {
		close(started)
		<-release // Deliberately ignore context.
		return nil
	}}
	result := make(chan error, 1)
	go func() {
		_, err := authenticator.Session(context.Background(), tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{fixture.leaf, fixture.install.ClientCA}}})
		result <- err
	}()
	<-started
	fixture.gate.SetError(ErrNotLeader)
	close(fixture.gate.done)
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("blocked admission succeeded after leadership loss")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked authorization suppressed leadership revocation")
	}
	close(release)
}

func TestAdmissionRaceWithInvalidationFailsClosed(t *testing.T) {
	fixture := newSessionFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	authenticator := &Authenticator{Kube: fixture.client, Installation: fixture.install, Gate: fixture.gate, Revocations: fixture.revocations, Now: func() time.Time { return testNow }, Authorize: func(context.Context, Identity) error {
		close(started)
		<-release
		return nil
	}}
	result := make(chan error, 1)
	go func() {
		_, err := authenticator.Session(context.Background(), tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{fixture.leaf, fixture.install.ClientCA}}})
		result <- err
	}()
	<-started
	fixture.revocations.InvalidateAuthorization(AuthorizationSite, "site-ns", "west")
	close(release)
	if err := <-result; err == nil || !strings.Contains(err.Error(), "changed during admission") {
		t.Fatalf("admission race error = %v", err)
	}
}

func TestSharedRevocationsCoverCompleteIdentityAndAssignmentChain(t *testing.T) {
	for _, change := range []struct {
		kind AuthorizationKind
		name string
	}{
		{AuthorizationNamespace, "site-ns"}, {AuthorizationAssignment, "skupper"},
		{AuthorizationAllocation, "skupper-controller-allocations"}, {AuthorizationPod, "router-pod"},
		{AuthorizationServiceAccount, "router-sa"}, {AuthorizationRouterGroup, "skupper-router"},
		{AuthorizationSite, "west"},
	} {
		t.Run(string(change.kind), func(t *testing.T) {
			fixture := newSessionFixture(t)
			session := fixture.session(t, time.Hour)
			fixture.revocations.InvalidateAuthorization(change.kind, "site-ns", change.name)
			select {
			case <-session.Context().Done():
			case <-time.After(time.Second):
				t.Fatal("affected session was not promptly revoked")
			}
		})
	}
}

func TestWatchFailureRequestsAuditButFreshnessStillBoundsSession(t *testing.T) {
	fixture := newSessionFixture(t)
	fixture.revocations.freshness = 30 * time.Millisecond
	session := fixture.session(t, time.Hour)
	fixture.revocations.AuthorizationWatchFailed(fmt.Errorf("watch closed"))
	if batches := fixture.revocations.due(fixture.revocations.now()); len(batches) != 1 {
		t.Fatalf("watch failure scheduled %d audit batches, want 1", len(batches))
	}
	select {
	case <-session.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("session exceeded its authorization freshness deadline")
	}
}

func TestAdmissionDuringBrokenWatchStillRequiresLiveReadsAndExpires(t *testing.T) {
	fixture := newSessionFixture(t)
	fixture.revocations.freshness = 30 * time.Millisecond
	fixture.revocations.AuthorizationWatchFailed(fmt.Errorf("watch unavailable"))
	fixture.client.ClearActions()
	session := fixture.session(t, time.Hour)
	if got := len(fixture.client.Actions()); got != 5 {
		t.Fatalf("admission during watch failure made %d workload reads, want 5", got)
	}
	select {
	case <-session.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("post-failure admission exceeded its fresh authorization bound")
	}
}

func TestScopedInvalidationLeavesUnaffectedSessionOpen(t *testing.T) {
	fixture := newSessionFixture(t)
	session := fixture.session(t, time.Hour)
	fixture.revocations.InvalidateAuthorization(AuthorizationPod, "site-ns", "another-pod")
	fixture.revocations.InvalidateAuthorization(AuthorizationSite, "another-namespace", "west")
	select {
	case <-session.Context().Done():
		t.Fatalf("unaffected session was revoked: %v", session.Check())
	case <-time.After(20 * time.Millisecond):
	}
	session.Close()
}

type sessionFixture struct {
	client      *fake.Clientset
	install     *Installation
	identity    Identity
	leaf        *x509.Certificate
	gate        *testGate
	revocations *SessionRevocations
	authorized  atomic.Bool
}

func newSessionFixture(t *testing.T) *sessionFixture {
	t.Helper()
	client, installation, enroller := enrollmentFixture(t)
	enrollment, err := enroller.Enroll(context.Background(), testToken(testNow.Add(time.Hour)), testCSR(t, pkix.Name{}, nil))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(enrollment.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	fixture := &sessionFixture{client: client, install: installation, identity: enrollment.Identity, leaf: leaf, gate: &testGate{done: make(chan struct{})}, revocations: NewSessionRevocations()}
	fixture.authorized.Store(true)
	return fixture
}

func (f *sessionFixture) session(t *testing.T, lifetime time.Duration) *Session {
	t.Helper()
	leaf := *f.leaf
	leaf.NotAfter = testNow.Add(lifetime)
	authenticator := &Authenticator{Kube: f.client, Installation: f.install, Gate: f.gate, Revocations: f.revocations, Now: func() time.Time { return testNow }, Authorize: func(context.Context, Identity) error {
		if !f.authorized.Load() {
			return ErrUnauthorized
		}
		return nil
	}}
	session, err := authenticator.Session(context.Background(), tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{&leaf, f.install.ClientCA}}})
	if err != nil {
		t.Fatal(err)
	}
	return session
}
