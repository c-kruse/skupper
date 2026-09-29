package routercontrol

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type testGate struct {
	done chan struct{}
	err  error
}

func (g *testGate) Check() error          { return g.err }
func (g *testGate) Done() <-chan struct{} { return g.done }

func TestSessionRevokesDeletedPodAndLostAssignment(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(t *testing.T, fixture *sessionFixture)
	}{
		{name: "deleted Pod", change: func(t *testing.T, fixture *sessionFixture) {
			if err := fixture.client.CoreV1().Pods("site-ns").Delete(context.Background(), "router-pod", metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "lost assignment", change: func(_ *testing.T, fixture *sessionFixture) { fixture.authorized.Store(false) }},
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
	fixture.gate.err = ErrNotLeader
	close(fixture.gate.done)
	select {
	case <-session.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("session remained active after leadership loss")
	}
}

type sessionFixture struct {
	client     *fake.Clientset
	install    *Installation
	identity   Identity
	leaf       *x509.Certificate
	gate       *testGate
	authorized atomic.Bool
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
	fixture := &sessionFixture{client: client, install: installation, identity: enrollment.Identity, leaf: leaf, gate: &testGate{done: make(chan struct{})}}
	fixture.authorized.Store(true)
	return fixture
}

func (f *sessionFixture) session(t *testing.T, lifetime time.Duration) *Session {
	t.Helper()
	leaf := *f.leaf
	leaf.NotAfter = time.Now().Add(lifetime)
	authenticator := &Authenticator{Kube: f.client, Installation: f.install, Gate: f.gate, RecheckEvery: 5 * time.Millisecond, Authorize: func(context.Context, Identity) error {
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
