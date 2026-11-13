package securedaccess

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	routev1 "github.com/openshift/api/route/v1"
	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	fakeclient "github.com/skupperproject/skupper/internal/kube/client/fake"
	"github.com/skupperproject/skupper/internal/kube/resource"
	"github.com/skupperproject/skupper/internal/kube/site/labels"
	"github.com/skupperproject/skupper/internal/kube/watchers"
	saapi "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	skupperfake "github.com/skupperproject/skupper/pkg/generated/client/clientset/versioned/fake"
	"gotest.tools/v3/assert"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

type testContext struct {
	policy   *labels.LabelsAndAnnotations
	excluded string
}

func (c *testContext) IsControlled(ns string) bool { return ns != c.excluded }
func (c *testContext) Namespace() string           { return "controller" }
func (c *testContext) Name() string                { return "skupper-controller" }
func (c *testContext) SnapshotMetadata(ns string) *labels.LabelsAndAnnotations {
	return c.policy.Snapshot(ns)
}
func (c *testContext) SetLabels(ns, name, kind string, ls map[string]string) bool {
	return c.policy.SetLabels(ns, name, kind, ls)
}
func (c *testContext) SetAnnotations(ns, name, kind string, as map[string]string) bool {
	return c.policy.SetAnnotations(ns, name, kind, as)
}

func testAccess(kind string) *saapi.SecuredAccess {
	return &saapi.SecuredAccess{
		ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "edge", UID: "access-uid", Generation: 1},
		Spec: saapi.SecuredAccessSpec{AccessType: kind, Selector: map[string]string{"app": "backend"},
			Ports: []saapi.SecuredAccessPort{{Name: "alpha", Port: 8080, TargetPort: 8181}, {Name: "beta", Port: 9090, TargetPort: 9191}}},
	}
}
func testConfig() Config {
	return Config{
		EnabledAccessTypes: []string{"local", "nodeport", "loadbalancer", "route", "ingress", "ingress-nginx", "contour-http-proxy", "gateway"},
		ClusterHost:        "cluster.example", IngressDomain: "ingress.example", HttpProxyDomain: "proxy.example",
		GatewayClass: "test-gateway", GatewayDomain: "gateway.example", GatewayPort: 7443,
	}
}

type scopeHarness struct {
	t       *testing.T
	m       *SecuredAccessManager
	clients *internalclient.KubeClient
	stores  map[string]cache.Store
}

func testWatcher[T runtime.Object](kind string, stores map[string]cache.Store) *watchers.ResourceWatcher[T] {
	informer := cache.NewSharedIndexInformer(&cache.ListWatch{}, nil, 0, cache.Indexers{})
	stores[kind] = informer.GetStore()
	return watchers.NewResourceWatcher[T](nil, schema.GroupVersion{}, informer)
}
func newHarness(t *testing.T, clients *internalclient.KubeClient) *scopeHarness {
	t.Helper()
	if clients == nil {
		var err error
		clients, err = fakeclient.NewFakeClient("test", nil, nil, "")
		assert.NilError(t, err)
	}
	m := NewSecuredAccessManager(clients, ptrConfig(testConfig()), &testContext{policy: labels.NewLabelsAndAnnotations("controller")})
	w := &ResourceCaches{}
	m.BindCaches(w)
	h := &scopeHarness{t: t, m: m, clients: clients, stores: map[string]cache.Store{}}
	w.Services = testWatcher[*corev1.Service]("Service", h.stores)
	w.Ingresses = testWatcher[*networkingv1.Ingress]("Ingress", h.stores)
	w.Routes = testWatcher[*routev1.Route]("Route", h.stores)
	w.Accesses = testWatcher[*saapi.SecuredAccess]("SecuredAccess", h.stores)
	w.Certificates = testWatcher[*saapi.Certificate]("Certificate", h.stores)
	w.HTTPProxies = testWatcher[*unstructured.Unstructured]("HTTPProxy", h.stores)
	w.TLSRoutes = testWatcher[*unstructured.Unstructured]("TLSRoute", h.stores)
	w.Gateways = testWatcher[*unstructured.Unstructured]("Gateway", h.stores)
	return h
}
func ptrConfig(c Config) *Config { return &c }
func (h *scopeHarness) observe() {
	t := h.t
	t.Helper()
	ctx := context.Background()
	for _, s := range h.stores {
		for _, o := range s.List() {
			assert.NilError(t, s.Delete(o))
		}
	}
	sas, e := h.clients.Skupper.SkupperV2alpha1().SecuredAccesses("").List(ctx, metav1.ListOptions{})
	assert.NilError(t, e)
	for i := range sas.Items {
		assert.NilError(t, h.stores["SecuredAccess"].Add(sas.Items[i].DeepCopy()))
	}
	cs, e := h.clients.Skupper.SkupperV2alpha1().Certificates("").List(ctx, metav1.ListOptions{})
	assert.NilError(t, e)
	for i := range cs.Items {
		assert.NilError(t, h.stores["Certificate"].Add(cs.Items[i].DeepCopy()))
	}
	svcs, e := h.clients.Kube.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	assert.NilError(t, e)
	for i := range svcs.Items {
		assert.NilError(t, h.stores["Service"].Add(svcs.Items[i].DeepCopy()))
	}
	is, e := h.clients.Kube.NetworkingV1().Ingresses("").List(ctx, metav1.ListOptions{})
	assert.NilError(t, e)
	for i := range is.Items {
		assert.NilError(t, h.stores["Ingress"].Add(is.Items[i].DeepCopy()))
	}
	if h.clients.Route != nil {
		rs, e := h.clients.Route.RouteV1().Routes("").List(ctx, metav1.ListOptions{})
		assert.NilError(t, e)
		for i := range rs.Items {
			assert.NilError(t, h.stores["Route"].Add(rs.Items[i].DeepCopy()))
		}
	}
	for kind, gvr := range map[string]schema.GroupVersionResource{"HTTPProxy": httpProxyResource, "TLSRoute": resource.TlsRouteResource(), "Gateway": resource.GatewayResource()} {
		os, e := h.clients.Dynamic.Resource(gvr).Namespace("").List(ctx, metav1.ListOptions{})
		if kind == "HTTPProxy" && apierrors.IsNotFound(e) {
			continue
		}
		assert.NilError(t, e)
		for i := range os.Items {
			assert.NilError(t, h.stores[kind].Add(os.Items[i].DeepCopy()))
		}
	}
}
func (h *scopeHarness) put(sa *saapi.SecuredAccess) {
	h.t.Helper()
	_, err := h.clients.Skupper.SkupperV2alpha1().SecuredAccesses(sa.Namespace).Create(context.Background(), sa, metav1.CreateOptions{})
	assert.NilError(h.t, err)
	h.observe()
}
func (h *scopeHarness) run(key string) error { return h.m.Handle(watchers.ResourceChange{Key: key}) }
func (h *scopeHarness) runCertificates() error {
	keys := map[string]bool{}
	for _, sa := range h.m.watcher.Accesses.List() {
		if target := CertificateTarget(sa); target != "" {
			keys[sa.Namespace+"/"+target] = true
		}
	}
	for _, c := range h.m.watcher.Certificates.List() {
		keys[c.Namespace+"/"+c.Name] = true
	}
	var ordered []string
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	for _, key := range ordered {
		if err := h.m.certificates.Handle(watchers.ResourceChange{Key: key}); err != nil {
			return err
		}
	}
	return nil
}
func (h *scopeHarness) status(name string) *saapi.SecuredAccess {
	h.t.Helper()
	sa, e := h.clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Get(context.Background(), name, metav1.GetOptions{})
	assert.NilError(h.t, e)
	return sa
}

// apiRecorder is implemented by every fake client in the harness.
type apiRecorder interface {
	Actions() []k8stesting.Action
	ClearActions()
}

// recorders returns the fake clients; the integration suite's real clients
// record nothing.
func (h *scopeHarness) recorders() []apiRecorder {
	var recorders []apiRecorder
	for _, c := range []any{h.clients.Kube, h.clients.Skupper, h.clients.Dynamic, h.clients.Route} {
		if r, ok := c.(apiRecorder); ok {
			recorders = append(recorders, r)
		}
	}
	return recorders
}
func (h *scopeHarness) clearActions() {
	for _, r := range h.recorders() {
		r.ClearActions()
	}
}

// mutations returns the API writes recorded since the last clearActions.
func (h *scopeHarness) mutations() []k8stesting.Action {
	var writes []k8stesting.Action
	for _, r := range h.recorders() {
		for _, a := range r.Actions() {
			if a.GetVerb() != "get" && a.GetVerb() != "list" && a.GetVerb() != "watch" {
				writes = append(writes, a)
			}
		}
	}
	return writes
}

// settle runs the Access and Certificate scopes, observing their writes, until
// a pass writes nothing. Without fake clients it runs a fixed three passes.
func (h *scopeHarness) settle(name string) {
	h.t.Helper()
	fake := len(h.recorders()) != 0
	for i := 0; i < 5; i++ {
		h.clearActions()
		h.observe()
		assert.NilError(h.t, h.run("test/"+name))
		h.observe()
		assert.NilError(h.t, h.runCertificates())
		if (fake && len(h.mutations()) == 0) || (!fake && i == 2) {
			h.observe()
			return
		}
	}
	h.t.Fatalf("scopes for %s did not converge: %v", name, h.mutations())
}

// noWrites asserts that settled Access and Certificate scopes issue no writes.
func (h *scopeHarness) noWrites(name string) {
	h.t.Helper()
	h.clearActions()
	assert.NilError(h.t, h.run("test/"+name))
	assert.NilError(h.t, h.runCertificates())
	assert.Equal(h.t, len(h.mutations()), 0, "unexpected mutations: %v", h.mutations())
}

func TestScopeProviders(t *testing.T) {
	for _, kind := range []string{"local", "nodeport", "loadbalancer", "route", "ingress", "ingress-nginx", "contour-http-proxy"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, nil)
			sa := testAccess(kind)
			sa.Spec.Issuer = "issuer"
			h.put(sa)
			h.settle(sa.Name)
			ctx := context.Background()
			svc, e := h.clients.Kube.CoreV1().Services("test").Get(ctx, "edge", metav1.GetOptions{})
			assert.NilError(t, e)
			assert.DeepEqual(t, svc.Spec.Selector, sa.Spec.Selector)
			assert.Equal(t, len(svc.Spec.Ports), 2)
			assert.Equal(t, svc.Spec.Ports[0].TargetPort.IntVal, int32(8181))
			var expected []saapi.Endpoint
			switch kind {
			case "nodeport":
				svc.Spec.Ports[0].NodePort = 30123
				svc.Spec.Ports[1].NodePort = 30456
				_, e = h.clients.Kube.CoreV1().Services("test").Update(ctx, svc, metav1.UpdateOptions{})
				assert.NilError(t, e)
				expected = []saapi.Endpoint{{Name: "alpha", Host: "cluster.example", Port: "30123"}, {Name: "beta", Host: "cluster.example", Port: "30456"}}
			case "loadbalancer":
				svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "10.20.30.40", Hostname: "ignored.example"}, {Hostname: "lb.example"}}
				_, e = h.clients.Kube.CoreV1().Services("test").UpdateStatus(ctx, svc, metav1.UpdateOptions{})
				assert.NilError(t, e)
				expected = []saapi.Endpoint{
					{Name: "alpha", Host: "10.20.30.40", Port: "8080"}, {Name: "beta", Host: "10.20.30.40", Port: "9090"},
					{Name: "alpha", Host: "lb.example", Port: "8080"}, {Name: "beta", Host: "lb.example", Port: "9090"},
				}
			case "route":
				for _, port := range sa.Spec.Ports {
					r, e := h.clients.Route.RouteV1().Routes("test").Get(ctx, "edge-"+port.Name, metav1.GetOptions{})
					assert.NilError(t, e)
					r.Status.Ingress = []routev1.RouteIngress{{Host: port.Name + ".route.example"}}
					_, e = h.clients.Route.RouteV1().Routes("test").UpdateStatus(ctx, r, metav1.UpdateOptions{})
					assert.NilError(t, e)
					expected = append(expected, saapi.Endpoint{Name: port.Name, Host: port.Name + ".route.example", Port: "443"})
				}
			case "ingress", "ingress-nginx":
				expected = []saapi.Endpoint{{Name: "alpha", Host: "alpha.test.ingress.example", Port: "443"}, {Name: "beta", Host: "beta.test.ingress.example", Port: "443"}}
			case "contour-http-proxy":
				expected = []saapi.Endpoint{{Name: "alpha", Host: "edge-alpha.test.proxy.example", Port: "443"}, {Name: "beta", Host: "edge-beta.test.proxy.example", Port: "443"}}
			case "local":
				expected = []saapi.Endpoint{{Name: "alpha", Host: "edge.test", Port: "8080"}, {Name: "beta", Host: "edge.test", Port: "9090"}}
			}
			h.settle(sa.Name)
			assert.DeepEqual(t, h.status(sa.Name).Status.Endpoints, expected)
			assert.Assert(t, h.status(sa.Name).IsReady())
			h.noWrites(sa.Name)
			// Restart uses only API observations, including allocated ports/status.
			h = newHarness(t, h.clients)
			h.observe()
			h.noWrites(sa.Name)
		})
	}
}

func TestScopeRouteDomain(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)
	sa := testAccess("route")
	sa.Spec.Settings = map[string]string{"domain": "users.example"}
	h.put(sa)
	h.settle(sa.Name)
	routes := h.clients.Route.RouteV1().Routes("test")
	created, err := routes.Get(ctx, "edge-alpha", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.Equal(t, created.Spec.Host, "edge-alpha.test.users.example")
	// The fake API does not allocate UIDs; seed one to detect replacement.
	created.UID = "route-uid"
	created, err = routes.Update(ctx, created, metav1.UpdateOptions{})
	assert.NilError(t, err)
	// A changed domain is repaired in place.
	current := h.status(sa.Name)
	current.Spec.Settings["domain"] = "other.example"
	_, err = h.clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Update(ctx, current, metav1.UpdateOptions{})
	assert.NilError(t, err)
	h.settle(sa.Name)
	updated, err := routes.Get(ctx, "edge-alpha", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.Equal(t, updated.Spec.Host, "edge-alpha.test.other.example")
	assert.Equal(t, updated.UID, created.UID)
	// Without a domain, repairing drift keeps the existing host.
	current = h.status(sa.Name)
	current.Spec.Settings = nil
	_, err = h.clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Update(ctx, current, metav1.UpdateOptions{})
	assert.NilError(t, err)
	h.settle(sa.Name)
	drifted, err := routes.Get(ctx, "edge-alpha", metav1.GetOptions{})
	assert.NilError(t, err)
	drifted.Spec.TLS.Termination = routev1.TLSTerminationEdge
	_, err = routes.Update(ctx, drifted, metav1.UpdateOptions{})
	assert.NilError(t, err)
	h.settle(sa.Name)
	retained, err := routes.Get(ctx, "edge-alpha", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.Equal(t, retained.Spec.TLS.Termination, routev1.TLSTerminationPassthrough)
	assert.Equal(t, retained.Spec.Host, "edge-alpha.test.other.example")
	h.noWrites(sa.Name)
}

func TestScopeEmptyAccessType(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hasRoute bool
	}{
		{name: "route API available defaults to route", hasRoute: true},
		{name: "route API absent defaults to loadbalancer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			clients, err := fakeclient.NewFakeClient("test", nil, nil, "")
			assert.NilError(t, err)
			if !tc.hasRoute {
				clients.Route = nil
			}
			h := newHarness(t, clients)
			if !tc.hasRoute {
				h.m.watcher.Routes = nil
			}
			sa := testAccess("")
			h.put(sa)
			h.settle(sa.Name)
			svc, err := h.clients.Kube.CoreV1().Services("test").Get(ctx, "edge", metav1.GetOptions{})
			assert.NilError(t, err)
			if tc.hasRoute {
				_, err := h.clients.Route.RouteV1().Routes("test").Get(ctx, "edge-alpha", metav1.GetOptions{})
				assert.NilError(t, err)
				assert.Equal(t, svc.Spec.Type, corev1.ServiceTypeClusterIP)
			} else {
				assert.Equal(t, svc.Spec.Type, corev1.ServiceTypeLoadBalancer)
			}
			assert.Equal(t, h.status(sa.Name).Spec.AccessType, "", "defaulting must not rewrite the user's spec")
		})
	}
}

func TestUnsupportedAccessTypePreservesOutputs(t *testing.T) {
	for _, provider := range []string{"loadbalancer", "ingress", "route", "contour-http-proxy"} {
		for _, invalid := range []string{"unknown", "disabled", "Route API absent", "unknown enabled"} {
			t.Run(provider+"/"+invalid, func(t *testing.T) {
				ctx := context.Background()
				h := newHarness(t, nil)
				sa := testAccess(provider)
				h.put(sa)
				h.settle(sa.Name)
				// Retain complete outputs, including allocations, status, external
				// metadata and provider objects, not just the Service type.
				outputs := func() map[string]runtime.Object {
					result := map[string]runtime.Object{}
					for kind, store := range h.stores {
						if kind == "SecuredAccess" {
							continue
						}
						for _, object := range store.List() {
							result[kind+"/"+object.(metav1.Object).GetName()] = object.(runtime.Object).DeepCopyObject()
						}
					}
					return result
				}
				before := outputs()
				sa = h.status(sa.Name)
				sa.Spec.AccessType = "not-an-access-type"
				switch invalid {
				case "disabled":
					sa.Spec.AccessType = "local"
					h.m.config.EnabledAccessTypes = []string{provider}
				case "Route API absent":
					sa.Spec.AccessType = "route"
					h.m.watcher.Routes = nil
				case "unknown enabled":
					h.m.config.EnabledAccessTypes = append(h.m.config.EnabledAccessTypes, sa.Spec.AccessType)
				}
				// Other edited fields must not leak through the invalid type.
				sa.Spec.Ports[0].Port = 12345
				sa.Spec.Selector = map[string]string{"app": "changed"}
				_, err := h.clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Update(ctx, sa, metav1.UpdateOptions{})
				assert.NilError(t, err)
				h.observe()
				h.clearActions()
				assert.NilError(t, h.run(sa.Key()))
				for _, action := range h.mutations() {
					assert.Equal(t, action.GetResource().Resource, "securedaccesses")
					assert.Equal(t, action.GetSubresource(), "status")
				}
				h.observe()
				assert.DeepEqual(t, outputs(), before)
				assert.Equal(t, h.status(sa.Name).Status.Message, "unsupported access type")
				assert.Assert(t, !h.status(sa.Name).IsReady())
				// Restart reconstructs the same refusal, not cleanup/adoption.
				config := h.m.config
				h = newHarness(t, h.clients)
				h.m.config = config
				if invalid == "Route API absent" {
					h.m.watcher.Routes = nil
				}
				h.observe()
				h.noWrites(sa.Name)
				assert.DeepEqual(t, outputs(), before)
				// Correcting the type resumes normal desired-state reconciliation.
				h = newHarness(t, h.clients)
				sa = h.status(sa.Name)
				sa.Spec.AccessType = provider
				_, err = h.clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Update(ctx, sa, metav1.UpdateOptions{})
				assert.NilError(t, err)
				h.settle(sa.Name)
				service, err := h.clients.Kube.CoreV1().Services("test").Get(ctx, sa.Name, metav1.GetOptions{})
				assert.NilError(t, err)
				assert.Equal(t, service.Spec.Ports[0].Port, int32(12345))
				assert.DeepEqual(t, service.Spec.Selector, sa.Spec.Selector)
				// Deleting an invalid definition must still clean its owned outputs.
				sa = h.status(sa.Name)
				sa.Spec.AccessType = "not-an-access-type"
				_, err = h.clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Update(ctx, sa, metav1.UpdateOptions{})
				assert.NilError(t, err)
				h.settle(sa.Name)
				assert.NilError(t, h.clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Delete(ctx, sa.Name, metav1.DeleteOptions{}))
				h = newHarness(t, h.clients)
				h.settle(sa.Name)
				assert.Equal(t, len(outputs()), 0)
			})
		}
	}
}

func TestScopeFailuresRetryAndOwnership(t *testing.T) {
	ctx := context.Background()
	t.Run("Route API unavailable", func(t *testing.T) {
		h := newHarness(t, nil)
		h.m.watcher.Routes = nil
		h.put(testAccess("route"))
		h.settle("edge")
		assert.Assert(t, !h.status("edge").IsReady())
		assert.Equal(t, h.status("edge").Status.Message, "unsupported access type")
		_, err := h.clients.Kube.CoreV1().Services("test").Get(ctx, "edge", metav1.GetOptions{})
		assert.Assert(t, apierrors.IsNotFound(err), "an initially unsupported Access must not create a Service")
		h.noWrites("edge")
	})
	t.Run("execution failure survives successful status", func(t *testing.T) {
		h := newHarness(t, nil)
		h.put(testAccess("local"))
		boom := errors.New("service write denied")
		k := h.clients.Kube.(*k8sfake.Clientset)
		k.PrependReactor("create", "services", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, boom })
		err := h.run("test/edge")
		assert.Assert(t, errors.Is(err, boom))
		assert.Assert(t, !h.status("edge").IsReady())
		assert.Equal(t, h.status("edge").Status.Message, "service write denied")
		k.ReactionChain = k.ReactionChain[1:]
		h.settle("edge")
		h.noWrites("edge")
	})
	for _, owner := range []string{"Site", "unowned", "old-access", "mixed"} {
		t.Run(owner, func(t *testing.T) {
			h := newHarness(t, nil)
			sa := testAccess("local")
			h.put(sa)
			svc := &corev1.Service{ObjectMeta: exposureMetadata(sa, sa.Name), Spec: corev1.ServiceSpec{Selector: map[string]string{"do": "not-touch"}}}
			switch owner {
			case "Site":
				svc.OwnerReferences[0].Kind = "Site"
			case "unowned":
				svc.OwnerReferences = nil
			case "old-access":
				svc.OwnerReferences[0].UID = "old-uid"
			case "mixed":
				svc.OwnerReferences = append(svc.OwnerReferences, metav1.OwnerReference{Kind: "Site", UID: "site-uid"})
			}
			_, e := h.clients.Kube.CoreV1().Services("test").Create(ctx, svc, metav1.CreateOptions{})
			assert.NilError(t, e)
			h.observe()
			assert.NilError(t, h.run("test/edge"))
			assert.Assert(t, !h.status("edge").IsReady())
			actual, e := h.clients.Kube.CoreV1().Services("test").Get(ctx, "edge", metav1.GetOptions{})
			assert.NilError(t, e)
			assert.DeepEqual(t, actual.Spec, svc.Spec)
			assert.NilError(t, h.clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Delete(ctx, "edge", metav1.DeleteOptions{}))
			h.observe()
			assert.NilError(t, h.run("test/edge"))
			if owner != "old-access" {
				_, e = h.clients.Kube.CoreV1().Services("test").Get(ctx, "edge", metav1.GetOptions{})
				assert.NilError(t, e)
			}
		})
	}
}

func TestSharedCertificateRetargetAndRemoval(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	a := testAccess("local")
	a.Spec.Issuer = "issuer"
	a.Spec.Certificate = "shared"
	b := a.DeepCopy()
	b.Name = "other"
	b.UID = "other-uid"
	h.put(a)
	h.put(b)
	// Neither Access scope authors Certificate intent, even before observations
	// advance. The one target scope then aggregates both requests in one write.
	assert.NilError(t, h.run(a.Key()))
	assert.NilError(t, h.run(b.Key()))
	_, e := h.clients.Skupper.SkupperV2alpha1().Certificates("test").Get(ctx, "shared", metav1.GetOptions{})
	assert.Assert(t, apierrors.IsNotFound(e))
	h.settle(a.Name)
	h.settle(b.Name)
	h.noWrites(a.Name)
	h.noWrites(b.Name)
	c, e := h.clients.Skupper.SkupperV2alpha1().Certificates("test").Get(ctx, "shared", metav1.GetOptions{})
	assert.NilError(t, e)
	assert.Equal(t, c.Annotations["internal.skupper.io/secured-access-certificate"], "true")
	assert.Equal(t, c.Spec.Subject, "edge")
	assert.DeepEqual(t, c.Spec.Hosts, []string{"edge", "edge.test", "other", "other.test"})
	assert.Equal(t, len(c.OwnerReferences), 2)
	b = h.status(b.Name)
	b.Spec.Certificate = "retarget"
	_, e = h.clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Update(ctx, b, metav1.UpdateOptions{})
	assert.NilError(t, e)
	h.settle(b.Name)
	c, e = h.clients.Skupper.SkupperV2alpha1().Certificates("test").Get(ctx, "shared", metav1.GetOptions{})
	assert.NilError(t, e)
	assert.DeepEqual(t, c.Spec.Hosts, []string{"edge", "edge.test"})
	assert.Equal(t, len(c.OwnerReferences), 1)
	assert.NilError(t, h.clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Delete(ctx, b.Name, metav1.DeleteOptions{}))
	h = newHarness(t, h.clients)
	h.observe()
	assert.NilError(t, h.run("test/"+b.Name))
	assert.NilError(t, h.runCertificates())
	_, e = h.clients.Skupper.SkupperV2alpha1().Certificates("test").Get(ctx, "retarget", metav1.GetOptions{})
	assert.Assert(t, e != nil)
	h.observe()
	h.noWrites(a.Name)
	a = h.status(a.Name)
	a.Spec.Issuer = ""
	_, e = h.clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Update(ctx, a, metav1.UpdateOptions{})
	assert.NilError(t, e)
	h.settle(a.Name)
	c, e = h.clients.Skupper.SkupperV2alpha1().Certificates("test").Get(ctx, "shared", metav1.GetOptions{})
	assert.Assert(t, apierrors.IsNotFound(e))
}

func TestProviderRemoval(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	sa := testAccess("contour-http-proxy")
	h.put(sa)
	h.settle(sa.Name)
	h.noWrites(sa.Name)
	current := h.status(sa.Name)
	current.Spec.Ports = current.Spec.Ports[:1]
	_, err := h.clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Update(ctx, current, metav1.UpdateOptions{})
	assert.NilError(t, err)
	h.settle(sa.Name)
	os, err := h.clients.Dynamic.Resource(httpProxyResource).Namespace("test").List(ctx, metav1.ListOptions{})
	assert.NilError(t, err)
	assert.Equal(t, len(os.Items), 1)
	current = h.status(sa.Name)
	current.Spec.AccessType = "local"
	_, err = h.clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Update(ctx, current, metav1.UpdateOptions{})
	assert.NilError(t, err)
	h.settle(sa.Name)
	os, err = h.clients.Dynamic.Resource(httpProxyResource).Namespace("test").List(ctx, metav1.ListOptions{})
	assert.NilError(t, err)
	assert.Equal(t, len(os.Items), 0)
	// A surviving output is independently sufficient for deletion after restart.
	assert.NilError(t, h.clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Delete(ctx, sa.Name, metav1.DeleteOptions{}))
	h = newHarness(t, h.clients)
	h.observe()
	assert.NilError(t, h.run(sa.Key()))
	svcs, err := h.clients.Kube.CoreV1().Services("test").List(ctx, metav1.ListOptions{})
	assert.NilError(t, err)
	assert.Equal(t, len(svcs.Items), 0)
}

func TestProviderNameCamping(t *testing.T) {
	ctx := context.Background()
	for _, provider := range []string{"route", "contour-http-proxy", "gateway"} {
		t.Run(provider, func(t *testing.T) {
			h := newHarness(t, nil)
			sa := testAccess(provider)
			sa.Spec.Ports = sa.Spec.Ports[:1]
			h.put(sa)
			var original runtime.Object
			gvr := httpProxyResource
			switch provider {
			case "route":
				route := desiredRouteForPort(sa, sa.Spec.Ports[0])
				route.OwnerReferences = nil
				_, err := h.clients.Route.RouteV1().Routes("test").Create(ctx, route, metav1.CreateOptions{})
				assert.NilError(t, err)
				original = route
			case "contour-http-proxy", "gateway":
				obj := desiredTLSRoute(sa, sa.Spec.Ports[0], "controller", "gateway.example")
				if provider == "contour-http-proxy" {
					obj.SetGroupVersionKind(httpProxyGVK)
					obj.Object["spec"] = map[string]interface{}{}
					proxy := desiredHttpProxies("test.proxy.example", sa)[0]
					assert.NilError(t, proxy.writeToContourProxy(obj))
				} else {
					gvr = resource.TlsRouteResource()
					_, err := h.clients.Dynamic.Resource(resource.GatewayResource()).Namespace("controller").Create(ctx, desiredGateway("controller", "skupper-controller", testConfig()), metav1.CreateOptions{})
					assert.NilError(t, err)
				}
				obj.SetOwnerReferences(nil)
				_, err := h.clients.Dynamic.Resource(gvr).Namespace("test").Create(ctx, obj, metav1.CreateOptions{})
				assert.NilError(t, err)
				original = obj
			}
			h.observe()
			assert.NilError(t, h.run(sa.Key()))
			assert.Assert(t, strings.Contains(h.status(sa.Name).Status.Message, "conflicting ownership"))
			assert.NilError(t, h.clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Delete(ctx, sa.Name, metav1.DeleteOptions{}))
			h = newHarness(t, h.clients)
			h.observe()
			assert.NilError(t, h.run(sa.Key()))
			if provider == "route" {
				actual, err := h.clients.Route.RouteV1().Routes("test").Get(ctx, "edge-alpha", metav1.GetOptions{})
				assert.NilError(t, err)
				assert.DeepEqual(t, actual, original)
			} else {
				actual, err := h.clients.Dynamic.Resource(gvr).Namespace("test").Get(ctx, "edge-alpha", metav1.GetOptions{})
				assert.NilError(t, err)
				assert.DeepEqual(t, actual, original)
			}
		})
	}
}

func TestEnsureReplacementBeforeObservation(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	sa := testAccess("local")
	first := []metav1.OwnerReference{{APIVersion: "skupper.io/v2alpha1", Kind: "Site", Name: "first", UID: "first-uid"}}
	second := []metav1.OwnerReference{{APIVersion: "skupper.io/v2alpha1", Kind: "Site", Name: "second", UID: "second-uid"}}
	assert.NilError(t, h.m.Ensure("test", "edge", sa.Spec, map[string]string{"a": "one"}, first))
	sa.Spec.Ports = sa.Spec.Ports[:1]
	assert.NilError(t, h.m.Ensure("test", "edge", sa.Spec, map[string]string{"b": "two"}, second))
	actual, e := h.clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Get(ctx, "edge", metav1.GetOptions{})
	assert.NilError(t, e)
	assert.DeepEqual(t, actual.Spec, sa.Spec)
	assert.DeepEqual(t, actual.OwnerReferences, second)
	assert.Equal(t, actual.Annotations["a"], "one")
	assert.Equal(t, actual.Annotations["b"], "two")
	assert.NilError(t, h.m.Delete("test", "edge"))
	assert.NilError(t, h.m.Delete("test", "edge"))
}

// readFromContourProxy tests also exercise these fixtures through its public
// serialized representation rather than any remembered manager state.
func httpProxyWithContent(name, namespace string, content map[string]interface{}) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: runtime.DeepCopyJSON(content)}
	o.SetName(name)
	o.SetNamespace(namespace)
	return o
}

func TestAccessScopeErrors(t *testing.T) {
	for _, scenario := range []string{"ready", "validation", "output failure", "status failure"} {
		t.Run(scenario, func(t *testing.T) {
			h := newHarness(t, nil)
			sa := testAccess("local")
			if scenario == "validation" {
				sa.Spec.AccessType = "unsupported"
			}
			h.put(sa)
			failure := errors.New("injected API failure")
			if scenario == "output failure" {
				h.clients.Kube.(*k8sfake.Clientset).PrependReactor("create", "services", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, failure })
			}
			if scenario == "status failure" {
				h.clients.Skupper.(*skupperfake.Clientset).PrependReactor("update", "securedaccesses", func(a k8stesting.Action) (bool, runtime.Object, error) {
					if a.GetSubresource() != "status" {
						return false, nil, nil
					}
					return true, nil, failure
				})
			}
			err := h.run(sa.Key())
			if scenario == "output failure" || scenario == "status failure" {
				assert.Assert(t, errors.Is(err, failure), "only API failures request retry")
			} else {
				assert.NilError(t, err)
			}
			if scenario == "ready" {
				h.settle(sa.Name)
				h.noWrites(sa.Name)
			}
		})
	}
}

func TestGatewayRejectsUnprovenLegacyOwnership(t *testing.T) {
	// Independent copy of the API server's baseline Apply field set. A matching
	// spec or manager without these paths is insufficient for legacy adoption.
	legacyFields := `{"f:spec":{"f:gatewayClassName":{},"f:listeners":{"k:{\"name\":\"tls\"}":{".":{},"f:allowedRoutes":{"f:namespaces":{"f:from":{}}},"f:name":{},"f:port":{},"f:protocol":{},"f:tls":{"f:mode":{}}}}}}`
	for _, tc := range []struct {
		name   string
		change func(*unstructured.Unstructured)
	}{
		{"no history", func(g *unstructured.Unstructured) { g.SetManagedFields(nil) }},
		{"other manager", func(g *unstructured.Unstructured) {
			fields := g.GetManagedFields()
			fields[0].Manager = "other-controller"
			g.SetManagedFields(fields)
		}},
		{"update is not apply", func(g *unstructured.Unstructured) {
			fields := g.GetManagedFields()
			fields[0].Operation = metav1.ManagedFieldsOperationUpdate
			g.SetManagedFields(fields)
		}},
		{"other API version", func(g *unstructured.Unstructured) {
			fields := g.GetManagedFields()
			fields[0].APIVersion = "gateway.networking.k8s.io/v1beta1"
			g.SetManagedFields(fields)
		}},
		{"status ownership only", func(g *unstructured.Unstructured) {
			fields := g.GetManagedFields()
			fields[0].Subresource = "status"
			g.SetManagedFields(fields)
		}},
		{"class only", func(g *unstructured.Unstructured) {
			fields := g.GetManagedFields()
			fields[0].FieldsV1.Raw = []byte(`{"f:spec":{"f:gatewayClassName":{}}}`)
			g.SetManagedFields(fields)
		}},
		{"listener only", func(g *unstructured.Unstructured) {
			fields := g.GetManagedFields()
			fields[0].FieldsV1.Raw = []byte(strings.Replace(legacyFields, `"f:gatewayClassName":{},`, "", 1))
			g.SetManagedFields(fields)
		}},
		{"partial listener", func(g *unstructured.Unstructured) {
			fields := g.GetManagedFields()
			fields[0].FieldsV1.Raw = []byte(strings.Replace(legacyFields, `"f:protocol":{},`, "", 1))
			g.SetManagedFields(fields)
		}},
		{"other listener", func(g *unstructured.Unstructured) {
			fields := g.GetManagedFields()
			fields[0].FieldsV1.Raw = []byte(strings.ReplaceAll(legacyFields, "tls", "other"))
			g.SetManagedFields(fields)
		}},
		{"malformed history", func(g *unstructured.Unstructured) {
			fields := g.GetManagedFields()
			fields[0].FieldsV1.Raw = []byte(`{"not-a-field-path":{}}`)
			g.SetManagedFields(fields)
		}},
		{"foreign claim", func(g *unstructured.Unstructured) {
			g.SetAnnotations(map[string]string{gatewayOwner: "other-controller"})
		}},
		{"empty explicit claim", func(g *unstructured.Unstructured) { g.SetAnnotations(map[string]string{gatewayOwner: ""}) }},
		{"owner reference", func(g *unstructured.Unstructured) {
			g.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "foreign", UID: "foreign-uid"}})
		}},
		{"deleting", func(g *unstructured.Unstructured) { now := metav1.Now(); g.SetDeletionTimestamp(&now) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			gateway := desiredGateway("controller", "skupper-controller", testConfig())
			gateway.SetAnnotations(nil)
			gateway.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "skupper-controller", Operation: metav1.ManagedFieldsOperationApply, APIVersion: "gateway.networking.k8s.io/v1", FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(legacyFields)}}})
			tc.change(gateway)
			_, err := h.clients.Dynamic.Resource(resource.GatewayResource()).Namespace("controller").Create(context.Background(), gateway, metav1.CreateOptions{})
			assert.NilError(t, err)
			h.put(testAccess("gateway"))
			h.clearActions()
			assert.ErrorContains(t, h.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}), "conflicting ownership")
			assert.Equal(t, len(h.mutations()), 0)
			assert.NilError(t, h.run("test/edge"))
			assert.Assert(t, !h.status("edge").IsReady())
			observed, err := h.clients.Dynamic.Resource(resource.GatewayResource()).Namespace("controller").Get(context.Background(), "skupper", metav1.GetOptions{})
			assert.NilError(t, err)
			assert.DeepEqual(t, observed.Object, gateway.Object)
		})
	}
}

func TestAggregateAndGatewayErrors(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)
	sa := testAccess("local")
	sa.Spec.Issuer, sa.Spec.Certificate = "issuer", "shared"
	h.put(sa)
	certificates := h.clients.Skupper.SkupperV2alpha1().Certificates("test")
	event := watchers.ResourceChange{Key: "test/shared"}
	assert.NilError(t, h.m.certificates.Handle(event))
	_, err := certificates.Get(ctx, "shared", metav1.GetOptions{})
	assert.NilError(t, err)
	h.observe()
	assert.NilError(t, h.m.certificates.Handle(event))

	assert.Assert(t, h.m.certificates.Handle(watchers.ResourceChange{Key: "bad/key/format"}) != nil)

	// The dynamic fake cannot create via Apply; the failed request must return
	// an error for retry, never report a successful realization.
	assert.Assert(t, h.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}) != nil)

	assert.NilError(t, h.clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Delete(ctx, sa.Name, metav1.DeleteOptions{}))
	h.observe()
	assert.NilError(t, h.m.certificates.Handle(event))
	_, err = certificates.Get(ctx, "shared", metav1.GetOptions{})
	assert.Assert(t, apierrors.IsNotFound(err))

	// A planning refusal has no API action but still returns its error.
	h.put(sa)
	foreign := &saapi.Certificate{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "test"}}
	_, err = certificates.Create(ctx, foreign, metav1.CreateOptions{})
	assert.NilError(t, err)
	h.observe()
	assert.Assert(t, h.m.certificates.Handle(event) != nil)
}
