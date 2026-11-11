package controller

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skupperproject/skupper/internal/certs"
	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	fakeclient "github.com/skupperproject/skupper/internal/kube/client/fake"
	"github.com/skupperproject/skupper/internal/kube/grants"
	"github.com/skupperproject/skupper/internal/kube/securedaccess"
	"github.com/skupperproject/skupper/internal/kube/watchers"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	skupperfake "github.com/skupperproject/skupper/pkg/generated/client/clientset/versioned/fake"
	"gotest.tools/v3/assert"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	discoveryfake "k8s.io/client-go/discovery/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

type routingMetric struct{ count *atomic.Int64 }

func (m routingMetric) Inc() {
	if m.count != nil {
		m.count.Add(1)
	}
}
func (m routingMetric) Dec()            {}
func (m routingMetric) Observe(float64) { m.Inc() }

type routingMetrics struct{ access, gateway, certificate, accessRetries atomic.Int64 }

func (*routingMetrics) NewAddedMetric(string) watchers.CounterMetric      { return routingMetric{} }
func (*routingMetrics) NewDepthMetric(string) watchers.GaugeMetric        { return routingMetric{} }
func (*routingMetrics) NewDelayedMetric(string) watchers.ObservableMetric { return routingMetric{} }
func (m *routingMetrics) NewRetriesMetric(kind string) watchers.CounterMetric {
	if kind == skupperv2alpha1.SchemeGroupVersion.String()+" SecuredAccess" {
		return routingMetric{&m.accessRetries}
	}
	return routingMetric{}
}
func (m *routingMetrics) NewWorkDurationMetric(kind string) watchers.ObservableMetric {
	switch kind {
	case "AccessScope":
		return routingMetric{&m.access}
	case "GatewayScope":
		return routingMetric{&m.gateway}
	case "CertificateScope":
		return routingMetric{&m.certificate}
	default:
		return routingMetric{}
	}
}

// Use production registration and real informers, with serial queue processing
// on the test goroutine so assertions never race domain state. No manual cache
// population, domain Handle calls or resource callback invocation.
func routingController(t *testing.T, clients internalclient.Clients, access *securedaccess.Config, options ...watchers.EventProcessorCustomizer) (*Controller, func()) {
	t.Helper()
	c, err := NewController(clients, &Config{Namespace: "controller", Name: "routing-test", GrantConfig: &grants.GrantConfig{}, SecuredAccessConfig: access}, options...)
	assert.NilError(t, err)
	stop := make(chan struct{})
	c.eventProcessor.StartWatchers(stop)
	syncContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	assert.Assert(t, c.eventProcessor.WaitForCacheSync(syncContext.Done()), "all registered inventories must sync before scope processing")
	c.namespaces.recover()
	var stopped atomic.Bool
	shutdown := func() {
		if stopped.CompareAndSwap(false, true) {
			// Legacy Site creates private Secret subscriptions even without a
			// Site definition. Its lifetime is intentionally not migrated here;
			// explicitly retire test-owned Sites before stopping the test API.
			for _, site := range c.sites {
				site.Deleted()
			}
			close(stop)
			c.eventProcessor.Stop()
		}
	}
	t.Cleanup(shutdown)
	return c, shutdown
}

func routingUntil(t *testing.T, c *Controller, description string, check func() bool) {
	t.Helper()
	err := wait.PollUntilContextTimeout(context.Background(), 10*time.Millisecond, 10*time.Second, true, func(context.Context) (bool, error) {
		c.eventProcessor.TestProcessAll()
		return check(), nil
	})
	assert.NilError(t, err, description)
}

func TestRoutingCertificateWatchDelivery(t *testing.T) {
	ctx := context.Background()
	leaf := &skupperv2alpha1.Certificate{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "leaf", UID: "leaf"}, Spec: skupperv2alpha1.CertificateSpec{Ca: "ca", Subject: "server", Hosts: []string{"example.test", "10.1.2.3"}, Server: true}}
	second := leaf.DeepCopy()
	second.Name, second.UID = "second", "second"
	clients, err := fakeclient.NewFakeClient("test", nil, []runtime.Object{leaf, second}, "")
	assert.NilError(t, err)
	c, _ := routingController(t, clients, &securedaccess.Config{})
	definitions := clients.Skupper.SkupperV2alpha1().Certificates("test")
	secrets := clients.Kube.CoreV1().Secrets("test")
	routingUntil(t, c, "initial Certificate list reports missing CA", func() bool {
		current, _ := definitions.Get(ctx, "leaf", metav1.GetOptions{})
		return current != nil && meta.IsStatusConditionFalse(current.Status.Conditions, skupperv2alpha1.CONDITION_TYPE_READY)
	})
	ca, err := certs.GenerateSecret("ca", "test CA", nil, 8*time.Hour, nil)
	assert.NilError(t, err)
	ca.Namespace = "test"
	_, err = secrets.Create(ctx, ca, metav1.CreateOptions{})
	assert.NilError(t, err)
	// Only the Secret arrives now: both differently named CA dependents wake.
	routingUntil(t, c, "CA arrival wakes both consumers", func() bool {
		for _, name := range []string{"leaf", "second"} {
			current, _ := definitions.Get(ctx, name, metav1.GetOptions{})
			if current == nil || !meta.IsStatusConditionTrue(current.Status.Conditions, skupperv2alpha1.CONDITION_TYPE_READY) {
				return false
			}
		}
		return true
	})
	before, err := secrets.Get(ctx, "leaf", metav1.GetOptions{})
	assert.NilError(t, err)
	decoded, err := certs.DecodeCertificate(before.Data["tls.crt"])
	assert.NilError(t, err)
	assert.Equal(t, decoded.Subject.CommonName, "server")
	assert.NilError(t, decoded.VerifyHostname("example.test"))
	assert.NilError(t, decoded.VerifyHostname("10.1.2.3"))
	assert.NilError(t, secrets.Delete(ctx, "leaf", metav1.DeleteOptions{}))
	routingUntil(t, c, "same-name Secret deletion recreates output", func() bool {
		current, err := secrets.Get(ctx, "leaf", metav1.GetOptions{})
		return err == nil && string(current.Data["tls.crt"]) != string(before.Data["tls.crt"])
	})
	// Synchronous authors share one write-through view before informer delivery.
	alice := []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "alice", UID: "alice"}}
	bob := []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "bob", UID: "bob"}}
	assert.NilError(t, c.certMgr.Ensure("test", "shared", "ca", "first", []string{"alice.example"}, false, true, alice))
	assert.NilError(t, c.certMgr.Ensure("test", "shared", "ca", "second", []string{"bob.example"}, false, true, bob))
	routingUntil(t, c, "queued acknowledgment preserves shared host/subject authoring", func() bool {
		current, _ := definitions.Get(ctx, "shared", metav1.GetOptions{})
		return current != nil && meta.IsStatusConditionTrue(current.Status.Conditions, skupperv2alpha1.CONDITION_TYPE_READY)
	})
	shared, err := definitions.Get(ctx, "shared", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.Equal(t, shared.Spec.Subject, "first")
	assert.DeepEqual(t, shared.Spec.Hosts, []string{"alice.example", "bob.example"})
	routingUntil(t, c, "shared status acknowledgment has reached the cache", func() bool {
		observed, _ := c.certificateWatcher.Get("test/shared")
		return observed != nil && meta.IsStatusConditionTrue(observed.Status.Conditions, skupperv2alpha1.CONDITION_TYPE_READY)
	})
	shared, err = definitions.Get(ctx, "shared", metav1.GetOptions{})
	assert.NilError(t, err)
	shared.Spec.Subject = "external"
	_, err = definitions.Update(ctx, shared, metav1.UpdateOptions{})
	assert.NilError(t, err)
	routingUntil(t, c, "external Certificate observation invalidates authored view", func() bool {
		observed, _ := c.certificateWatcher.Get("test/shared")
		return observed != nil && observed.Spec.Subject == "external"
	})
	assert.NilError(t, c.certMgr.Ensure("test", "shared", "ca", "ignored-shared-subject", []string{"bob.example"}, false, true, bob))
	shared, err = definitions.Get(ctx, "shared", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.Equal(t, shared.Spec.Subject, "external", "authoring must not overwrite from stale write-through state")
}

func TestRoutingAccessDependenciesAndRestart(t *testing.T) {
	ctx := context.Background()
	sa := &skupperv2alpha1.SecuredAccess{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "edge", UID: "access"}, Spec: skupperv2alpha1.SecuredAccessSpec{AccessType: "local", Issuer: "issuer", Certificate: "shared", Ports: []skupperv2alpha1.SecuredAccessPort{{Name: "alpha", Port: 8080, TargetPort: 8181}}}}
	blocker := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "edge", UID: "foreign"}}
	clients, err := fakeclient.NewFakeClient("test", []runtime.Object{blocker}, []runtime.Object{sa}, "")
	assert.NilError(t, err)
	clients.Discovery.(*discoveryfake.FakeDiscovery).Resources = nil
	var deny atomic.Bool
	var failed atomic.Int64
	clients.Kube.(*k8sfake.Clientset).PrependReactor("delete", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		if deny.Load() {
			failed.Add(1)
			return true, nil, errors.New("injected cleanup failure")
		}
		return false, nil, nil
	})
	config := &securedaccess.Config{EnabledAccessTypes: []string{"local"}}
	noResync := func(p *watchers.EventProcessor) { p.SetResync(0); p.SetResyncShort(0) }
	c, stop := routingController(t, clients, config, noResync)
	assert.Assert(t, c.accessCaches.Gateways == nil && c.accessCaches.TLSRoutes == nil && c.accessCaches.HTTPProxies == nil, "absent providers must stay unavailable, not synced-empty")
	accesses := clients.Skupper.SkupperV2alpha1().SecuredAccesses("test")
	certificates := clients.Skupper.SkupperV2alpha1().Certificates("test")
	services := clients.Kube.CoreV1().Services("test")
	routingUntil(t, c, "foreign same-name blocker is refused", func() bool {
		current, _ := accesses.Get(ctx, "edge", metav1.GetOptions{})
		return current != nil && len(current.Status.Conditions) > 0 && !current.IsReady()
	})
	foreign, err := services.Get(ctx, "edge", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.Equal(t, foreign.UID, blocker.UID)
	assert.NilError(t, services.Delete(ctx, "edge", metav1.DeleteOptions{}))
	routingUntil(t, c, "unfiltered blocker deletion wakes Access", func() bool {
		current, _ := accesses.Get(ctx, "edge", metav1.GetOptions{})
		return current != nil && current.IsReady()
	})
	current, err := accesses.Get(ctx, "edge", metav1.GetOptions{})
	assert.NilError(t, err)
	current.Spec.Certificate = "replacement"
	_, err = accesses.Update(ctx, current, metav1.UpdateOptions{})
	assert.NilError(t, err)
	routingUntil(t, c, "retarget discovers old aggregate from surviving owners", func() bool {
		_, oldErr := certificates.Get(ctx, "shared", metav1.GetOptions{})
		next, newErr := certificates.Get(ctx, "replacement", metav1.GetOptions{})
		return apierrors.IsNotFound(oldErr) && newErr == nil && next.Spec.Ca == "issuer"
	})
	current, err = accesses.Get(ctx, "edge", metav1.GetOptions{})
	assert.NilError(t, err)
	current.Spec.Issuer = ""
	_, err = accesses.Update(ctx, current, metav1.UpdateOptions{})
	assert.NilError(t, err)
	routingUntil(t, c, "issuer removal cleans last aggregate", func() bool {
		_, err := certificates.Get(ctx, "replacement", metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	})
	deny.Store(true)
	assert.NilError(t, accesses.Delete(ctx, "edge", metav1.DeleteOptions{}))
	routingUntil(t, c, "deleted own key retries cleanup through exhaustion", func() bool { return failed.Load() >= 6 })
	// Let delayed retries finish without introducing another resource event.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		c.eventProcessor.TestProcessAll()
		time.Sleep(10 * time.Millisecond)
	}
	_, err = services.Get(ctx, "edge", metav1.GetOptions{})
	assert.NilError(t, err, "failed cleanup must leave a durable reconstruction source")
	stop()
	deny.Store(false)
	c, _ = routingController(t, clients, config, noResync)
	routingUntil(t, c, "restart inventory reconstructs cleanup with definition absent", func() bool {
		_, err := services.Get(ctx, "edge", metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	})
}

func TestRoutingDeletedAccessWithoutOwnedOutputs(t *testing.T) {
	ctx := context.Background()
	sa := &skupperv2alpha1.SecuredAccess{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "empty", UID: "empty"}, Spec: skupperv2alpha1.SecuredAccessSpec{AccessType: "disabled"}}
	foreign := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "empty", UID: "foreign"}}
	clients, err := fakeclient.NewFakeClient("test", []runtime.Object{foreign}, []runtime.Object{sa}, "")
	assert.NilError(t, err)
	metrics := &routingMetrics{}
	c, _ := routingController(t, clients, &securedaccess.Config{}, watchers.WithMetricsProvider(metrics), func(p *watchers.EventProcessor) { p.SetResync(0); p.SetResyncShort(0) })
	api := clients.Skupper.SkupperV2alpha1().SecuredAccesses("test")
	routingUntil(t, c, "initial invalid Access is observed", func() bool {
		current, _ := api.Get(ctx, sa.Name, metav1.GetOptions{})
		return current != nil && len(current.Status.Conditions) != 0
	})
	// Drain its final status observation before measuring the absent own key.
	routingUntil(t, c, "status reached cache", func() bool {
		current, _ := c.accessCaches.Accesses.Get(sa.Key())
		return current != nil && len(current.Status.Conditions) != 0
	})
	c.eventProcessor.TestProcessAll()
	before := metrics.access.Load()
	assert.NilError(t, api.Delete(ctx, sa.Name, metav1.DeleteOptions{}))
	routingUntil(t, c, "deleted definition still enqueues own scope without inventory", func() bool {
		current, _ := c.accessCaches.Accesses.Get(sa.Key())
		return current == nil && metrics.access.Load() > before
	})
	services, err := clients.Kube.CoreV1().Services("test").List(ctx, metav1.ListOptions{})
	assert.NilError(t, err)
	assert.Equal(t, len(services.Items), 1)
	assert.Equal(t, services.Items[0].UID, foreign.UID)
	assert.Equal(t, len(services.Items[0].OwnerReferences), 0)
}

func TestRoutingLegacyRouterAccess(t *testing.T) {
	for _, failStatus := range []bool{false, true} {
		t.Run(map[bool]string{false: "deletion", true: "callback error"}[failStatus], func(t *testing.T) {
			ctx := context.Background()
			site := f.site("mysite", "test", "none", false, false)
			ra := f.routerAccess("edge", "test", "local", "", false, "", f.role("inter-router", 55671))
			ra.UID = "router-access"
			clients, err := fakeclient.NewFakeClient("test", nil, []runtime.Object{site, ra}, "")
			assert.NilError(t, err)
			// Fake clients lack API-server UID allocation. Provider write authorization
			// requires real Access identity even for resources authored during recovery.
			clients.Skupper.(*skupperfake.Clientset).PrependReactor("create", "securedaccesses", func(action k8stesting.Action) (bool, runtime.Object, error) {
				object := action.(k8stesting.CreateAction).GetObject().(*skupperv2alpha1.SecuredAccess)
				object.UID = types.UID(object.Name)
				return false, nil, nil
			})
			var failNext atomic.Bool
			clients.Skupper.(*skupperfake.Clientset).PrependReactor("update", "routeraccesses", func(action k8stesting.Action) (bool, runtime.Object, error) {
				if action.GetSubresource() == "status" && failNext.Swap(false) {
					return true, nil, errors.New("injected RouterAccess status failure")
				}
				return false, nil, nil
			})
			metrics := &routingMetrics{}
			c, err := NewController(clients, &Config{Namespace: "controller", Name: "routing-test", GrantConfig: &grants.GrantConfig{}, SecuredAccessConfig: &securedaccess.Config{EnabledAccessTypes: []string{"local"}}}, watchers.WithMetricsProvider(metrics))
			assert.NilError(t, err)
			stop := make(chan struct{})
			t.Cleanup(func() {
				for _, site := range c.sites {
					site.Deleted()
				}
				close(stop)
				c.eventProcessor.Stop()
			})
			assert.NilError(t, c.init(stop)) // Includes legacy recovery and second Pod sync.
			api := clients.Skupper.SkupperV2alpha1().RouterAccesses("test")
			routingUntil(t, c, "Access endpoints propagate through legacy Site callback", func() bool {
				current, _ := api.Get(ctx, ra.Name, metav1.GetOptions{})
				return current != nil && len(current.Status.Endpoints) > 0
			})
			accesses := clients.Skupper.SkupperV2alpha1().SecuredAccesses("test")
			current, err := accesses.Get(ctx, "edge", metav1.GetOptions{})
			assert.NilError(t, err)
			if failStatus {
				current.Status.Endpoints = []skupperv2alpha1.Endpoint{{Name: "inter-router", Host: "external.example", Port: "1234"}}
				failNext.Store(true)
				_, err = accesses.UpdateStatus(ctx, current, metav1.UpdateOptions{})
				assert.NilError(t, err)
				routingUntil(t, c, "legacy callback errors reach serial queue retry", func() bool { return metrics.accessRetries.Load() > 0 })
				return
			}
			// A surviving RouterAccess still demands this Access: the legacy
			// RouterAccess callback can recreate it after withdrawal. Observe
			// the real status transition, not a permanently empty endpoint set.
			changes, err := api.Watch(ctx, metav1.ListOptions{})
			assert.NilError(t, err)
			defer changes.Stop()
			assert.NilError(t, accesses.Delete(ctx, "edge", metav1.DeleteOptions{}))
			routingUntil(t, c, "nil Access observation withdraws legacy RouterAccess endpoints", func() bool {
				for {
					select {
					case event := <-changes.ResultChan():
						current := event.Object.(*skupperv2alpha1.RouterAccess)
						if current.Name == ra.Name && len(current.Status.Endpoints) == 0 {
							return true
						}
					default:
						return false
					}
				}
			})
		})
	}
}

func TestRoutingOptionalProviderRegistration(t *testing.T) {
	clients, err := fakeclient.NewFakeClient("test", nil, nil, "")
	assert.NilError(t, err)
	discovery := clients.Discovery.(*discoveryfake.FakeDiscovery)
	installed := discovery.Resources
	discovery.Resources = nil
	config := &Config{Namespace: "controller", Name: "routing-test", GrantConfig: &grants.GrantConfig{}, SecuredAccessConfig: &securedaccess.Config{}}
	old, err := NewController(clients, config)
	assert.NilError(t, err)
	defer old.eventProcessor.Stop()
	assert.Assert(t, old.accessCaches.Gateways == nil && old.accessCaches.TLSRoutes == nil && old.accessCaches.HTTPProxies == nil)
	// CRD appearance alone cannot create a subscription in the old controller.
	discovery.Resources = installed
	assert.Assert(t, old.accessCaches.Gateways == nil)
	fresh, err := NewController(clients, config)
	assert.NilError(t, err)
	defer fresh.eventProcessor.Stop()
	assert.Assert(t, fresh.accessCaches.Gateways != nil && fresh.accessCaches.TLSRoutes != nil && fresh.accessCaches.HTTPProxies != nil)
	assert.Assert(t, !fresh.accessCaches.Gateways.HasSynced()(), "registered does not mean synced-empty")
	stop := make(chan struct{})
	defer close(stop)
	fresh.eventProcessor.StartWatchers(stop)
	syncContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	assert.Assert(t, fresh.eventProcessor.WaitForCacheSync(syncContext.Done()))
	assert.Equal(t, len(fresh.accessCaches.Gateways.List()), 0)
	assert.Assert(t, fresh.accessCaches.Gateways.HasSynced()(), "only a completed list is authoritative empty inventory")
}

func TestRoutingSecretDependencyFilter(t *testing.T) {
	ctx := context.Background()
	signing := &skupperv2alpha1.Certificate{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "signing", UID: "signing"}, Spec: skupperv2alpha1.CertificateSpec{Signing: true, Ca: "ca", Subject: "self"}}
	unrelated := &skupperv2alpha1.Certificate{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "unrelated", UID: "unrelated"}, Spec: skupperv2alpha1.CertificateSpec{Ca: "another-ca", Subject: "other"}}
	otherNamespace := unrelated.DeepCopy()
	otherNamespace.Namespace, otherNamespace.Spec.Ca = "other", "ca"
	clients, err := fakeclient.NewFakeClient("test", nil, []runtime.Object{signing, unrelated, otherNamespace}, "")
	assert.NilError(t, err)
	metrics := &routingMetrics{}
	c, _ := routingController(t, clients, &securedaccess.Config{}, watchers.WithMetricsProvider(metrics), func(p *watchers.EventProcessor) { p.SetResync(0); p.SetResyncShort(0) })
	routingUntil(t, c, "initial definitions have been evaluated", func() bool {
		for _, cert := range c.certificateWatcher.List() {
			if len(cert.Status.Conditions) == 0 {
				return false
			}
		}
		return true
	})
	// Wait for startup's output and status notifications to finish. Counting
	// scope executions catches false dependencies even when their plans no-op.
	quiet := func() {
		last, changed := metrics.certificate.Load(), time.Now()
		routingUntil(t, c, "Certificate queue becomes quiet", func() bool {
			now := metrics.certificate.Load()
			if now != last {
				last, changed = now, time.Now()
			}
			return time.Since(changed) >= 100*time.Millisecond
		})
	}
	quiet()
	before := metrics.certificate.Load()
	_, err = clients.Kube.CoreV1().Secrets("test").Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "ca"}}, metav1.CreateOptions{})
	assert.NilError(t, err)
	routingUntil(t, c, "Secret notification wakes same-name scope even without definition", func() bool { return metrics.certificate.Load() > before })
	quiet()
	assert.Equal(t, metrics.certificate.Load()-before, int64(1), "signing Certificates, different CAs and other namespaces are not dependents")
}
