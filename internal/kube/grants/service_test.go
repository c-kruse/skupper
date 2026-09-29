package grants

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gotest.tools/v3/assert"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	"github.com/skupperproject/skupper/internal/kube/client/fake"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

type testEffectGate struct {
	done chan struct{}
}

func (g *testEffectGate) Check() error {
	select {
	case <-g.done:
		return ErrNotLeader
	default:
		return nil
	}
}
func (g *testEffectGate) Done() <-chan struct{} { return g.done }

func activateService(service *Service, gate EffectGate) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.leader = true
	service.gate = gate
	service.leaderCtx = context.Background()
}

func TestServiceMarkerPreventsRedemptionReplay(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	token := tf.token("token", "test", server.URL, "code", "unused")
	token.UID = types.UID("token-uid")
	token.Annotations = map[string]string{redemptionAttemptAnnotation: string(token.UID)}
	site := tf.site("site", "test")
	site.UID = types.UID("site-uid")
	clients, err := fake.NewFakeClient("test", nil, []runtime.Object{token, site}, "")
	assert.NilError(t, err)
	service, err := NewService(ServiceOptions{
		Clients:          clients,
		CurrentNamespace: "controller",
		WatchNamespace:   "test",
		Config:           &GrantConfig{Enabled: true},
		LookupSite: func(ctx context.Context, namespace string) (*skupperv2alpha1.Site, error) {
			return clients.GetSkupperClient().SkupperV2alpha1().Sites(namespace).Get(ctx, site.Name, metav1.GetOptions{})
		},
	})
	assert.NilError(t, err)
	activateService(service, &testEffectGate{done: make(chan struct{})})

	assert.NilError(t, service.checkAccessToken("test/token", token))
	assert.Equal(t, requests.Load(), int32(0))
	latest, err := clients.GetSkupperClient().SkupperV2alpha1().AccessTokens("test").Get(context.Background(), token.Name, metav1.GetOptions{})
	assert.NilError(t, err)
	assert.Equal(t, latest.Status.Message, "Redemption outcome is unknown; replace the AccessToken to retry")
}

func TestServicePersistsMarkerBeforeRedemptionRequest(t *testing.T) {
	var clients internalclient.Clients
	var markerSeen atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		live, err := clients.GetSkupperClient().SkupperV2alpha1().AccessTokens("test").Get(context.Background(), "token", metav1.GetOptions{})
		if err == nil && live.Annotations[redemptionAttemptAnnotation] == string(live.UID) {
			markerSeen.Store(true)
		}
		_, _ = w.Write([]byte("invalid response"))
	}))
	defer server.Close()
	certificate, err := x509.ParseCertificate(server.Certificate().Raw)
	assert.NilError(t, err)
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}))

	token := tf.token("token", "test", server.URL, "code", ca)
	token.UID = types.UID("token-uid")
	site := tf.site("site", "test")
	site.UID = types.UID("site-uid")
	clients, err = fake.NewFakeClient("test", nil, []runtime.Object{token, site}, "")
	assert.NilError(t, err)
	service, err := NewService(ServiceOptions{
		Clients:          clients,
		CurrentNamespace: "controller",
		WatchNamespace:   "test",
		Config:           &GrantConfig{Enabled: true},
		LookupSite: func(ctx context.Context, namespace string) (*skupperv2alpha1.Site, error) {
			return clients.GetSkupperClient().SkupperV2alpha1().Sites(namespace).Get(ctx, site.Name, metav1.GetOptions{})
		},
	})
	assert.NilError(t, err)
	activateService(service, &testEffectGate{done: make(chan struct{})})

	assert.NilError(t, service.checkAccessToken("test/token", token))
	assert.Assert(t, markerSeen.Load())
	_, err = clients.GetKubeClient().CoreV1().Secrets("test").Get(context.Background(), redemptionResponseName(token), metav1.GetOptions{})
	assert.NilError(t, err)
}

func TestServiceRejectsEffectsAfterLeadershipLoss(t *testing.T) {
	token := tf.token("token", "test", "https://unused", "code", "ca")
	token.UID = types.UID("token-uid")
	site := tf.site("site", "test")
	site.UID = types.UID("site-uid")
	clients, err := fake.NewFakeClient("test", nil, []runtime.Object{token, site}, "")
	assert.NilError(t, err)
	service, err := NewService(ServiceOptions{Clients: clients, Config: &GrantConfig{Enabled: true}, LookupSite: func(context.Context, string) (*skupperv2alpha1.Site, error) { return site, nil }})
	assert.NilError(t, err)
	gate := &testEffectGate{done: make(chan struct{})}
	activateService(service, gate)
	authorize := service.redemptionAuthority(token, site.UID)
	assert.NilError(t, authorize(context.Background()))
	close(gate.done)
	assert.ErrorIs(t, authorize(context.Background()), ErrNotLeader)
}

func TestServiceStandbyCachesAndLeaderLifecycle(t *testing.T) {
	grant := tf.grant("grant", "test", "grant-uid")
	clients, err := fake.NewFakeClient("test", nil, []runtime.Object{grant}, "")
	assert.NilError(t, err)
	service, err := NewService(ServiceOptions{
		Clients:        clients,
		WatchNamespace: "test",
		Config:         &GrantConfig{Enabled: false},
	})
	assert.NilError(t, err)
	cacheCtx, stopCaches := context.WithCancel(context.Background())
	defer stopCaches()
	assert.NilError(t, service.StartCaches(cacheCtx))
	syncCtx, cancelSync := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelSync()
	assert.Assert(t, service.WaitForCacheSync(syncCtx))

	latest, err := clients.GetSkupperClient().SkupperV2alpha1().AccessGrants("test").Get(context.Background(), grant.Name, metav1.GetOptions{})
	assert.NilError(t, err)
	assert.Equal(t, latest.Status.Message, "", "warming standby caches must not update resources")

	gate := &testEffectGate{done: make(chan struct{})}
	result := make(chan error, 1)
	go func() { result <- service.RunLeader(context.Background(), gate) }()
	deadline := time.Now().Add(5 * time.Second)
	for latest.Status.Message != "AccessGrants are not enabled" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		latest, err = clients.GetSkupperClient().SkupperV2alpha1().AccessGrants("test").Get(context.Background(), grant.Name, metav1.GetOptions{})
		assert.NilError(t, err)
	}
	assert.Equal(t, latest.Status.Message, "AccessGrants are not enabled")
	close(gate.done)
	select {
	case err := <-result:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("leader service did not stop after gate loss")
	}
}

func TestServiceDefersAutoConfigurationUntilPrepare(t *testing.T) {
	pod := tf.pod("controller-0", "controller", map[string]string{
		"app":                                "controller",
		"apps.kubernetes.io/pod-index":       "0",
		"controller-revision-hash":           "revision",
		"statefulset.kubernetes.io/pod-name": "controller-0",
	}, ref1)
	clients, err := fake.NewFakeClient("controller", []runtime.Object{pod}, nil, "")
	assert.NilError(t, err)
	service, err := NewService(ServiceOptions{
		Clients:          clients,
		CurrentNamespace: "controller",
		WatchNamespace:   "controller",
		Config: &GrantConfig{
			Enabled:       true,
			AutoConfigure: true,
			Hostname:      pod.Name,
			Port:          9090,
		},
	})
	assert.NilError(t, err)
	_, err = clients.GetSkupperClient().SkupperV2alpha1().Certificates("controller").Get(context.Background(), "skupper-grant-server-ca", metav1.GetOptions{})
	assert.Assert(t, apierrors.IsNotFound(err), "standby construction must not create autoconfiguration resources")

	assert.NilError(t, service.Prepare(context.Background()))
	_, err = clients.GetSkupperClient().SkupperV2alpha1().Certificates("controller").Get(context.Background(), "skupper-grant-server-ca", metav1.GetOptions{})
	assert.NilError(t, err)
	access, err := clients.GetSkupperClient().SkupperV2alpha1().SecuredAccesses("controller").Get(context.Background(), "skupper-grant-server", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.DeepEqual(t, access.Spec.Selector, map[string]string{"app": "controller"})
}
