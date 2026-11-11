package securedaccess

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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
func (c *testContext) UID() string                 { return "controller-uid" }
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
	return &saapi.SecuredAccess{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "edge", UID: "access-uid", Generation: 1}, Spec: saapi.SecuredAccessSpec{AccessType: kind, Selector: map[string]string{"app": "backend"}, Ports: []saapi.SecuredAccessPort{{Name: "alpha", Port: 8080, TargetPort: 8181}, {Name: "beta", Port: 9090, TargetPort: 9191}}}}
}
func testConfig() Config {
	return Config{EnabledAccessTypes: []string{"local", "nodeport", "loadbalancer", "route", "ingress", "ingress-nginx", "contour-http-proxy", "gateway"}, ClusterHost: "cluster.example", IngressDomain: "ingress.example", HttpProxyDomain: "proxy.example", GatewayClass: "test-gateway", GatewayDomain: "gateway.example", GatewayPort: 7443}
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
	m := NewSecuredAccessManager(clients, nil, ptrConfig(testConfig()), &testContext{policy: labels.NewLabelsAndAnnotations("controller")})
	w := NewSecuredAccessResourceWatcher(m)
	h := &scopeHarness{t: t, m: m, clients: clients, stores: map[string]cache.Store{}}
	w.serviceWatcher = testWatcher[*corev1.Service]("Service", h.stores)
	w.ingressWatcher = testWatcher[*networkingv1.Ingress]("Ingress", h.stores)
	w.routeWatcher = testWatcher[*routev1.Route]("Route", h.stores)
	w.securedAccessWatcher = testWatcher[*saapi.SecuredAccess]("SecuredAccess", h.stores)
	w.certificateWatcher = testWatcher[*saapi.Certificate]("Certificate", h.stores)
	w.httpProxyWatcher = testWatcher[*unstructured.Unstructured]("HTTPProxy", h.stores)
	w.tlsRouteWatcher = testWatcher[*unstructured.Unstructured]("TLSRoute", h.stores)
	w.gatewayWatcher = testWatcher[*unstructured.Unstructured]("Gateway", h.stores)
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
	for _, sa := range h.m.watcher.securedAccessWatcher.List() {
		if target := certificateTarget(sa); target != "" {
			keys[sa.Namespace+"/"+target] = true
		}
	}
	for _, c := range h.m.watcher.certificateWatcher.List() {
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
func (h *scopeHarness) settle(name string) {
	h.t.Helper()
	for i := 0; i < 3; i++ {
		h.observe()
		assert.NilError(h.t, h.run("test/"+name))
		h.observe()
		assert.NilError(h.t, h.runCertificates())
	}
	h.observe()
}
func (h *scopeHarness) noWrites(name string) {
	h.t.Helper()
	k := h.clients.Kube.(*k8sfake.Clientset)
	k.ClearActions()
	before := h.status(name).DeepCopy()
	assert.NilError(h.t, h.run("test/"+name))
	assert.DeepEqual(h.t, h.status(name), before)
	for _, a := range k.Actions() {
		assert.Assert(h.t, a.GetVerb() == "get" || a.GetVerb() == "list", "unexpected mutation: %v", a)
	}
	s, err := h.m.snapshot("test/" + name)
	assert.NilError(h.t, err)
	p := planAccess(s)
	assert.Equal(h.t, len(p.actions), 0)
	results, err := h.m.executeAccess(p)
	assert.NilError(h.t, err)
	assert.Equal(h.t, len(results), 0)
	assert.NilError(h.t, h.runCertificates())
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
				expected = []saapi.Endpoint{{Name: "alpha", Host: "10.20.30.40", Port: "8080"}, {Name: "beta", Host: "10.20.30.40", Port: "9090"}, {Name: "alpha", Host: "lb.example", Port: "8080"}, {Name: "beta", Host: "lb.example", Port: "9090"}}
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

func TestScopeFailuresRetryAndOwnership(t *testing.T) {
	ctx := context.Background()
	t.Run("Route API unavailable", func(t *testing.T) {
		h := newHarness(t, nil)
		h.m.watcher.routeWatcher = nil
		h.put(testAccess("route"))
		h.settle("edge")
		assert.Assert(t, !h.status("edge").IsReady())
		assert.Equal(t, h.status("edge").Status.Message, "unsupported access type")
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

func TestProviderRemovalAndPurePlan(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	sa := testAccess("contour-http-proxy")
	h.put(sa)
	h.settle(sa.Name)
	s, err := h.m.snapshot(sa.Key())
	assert.NilError(t, err)
	before := s.proxies["edge-alpha"].DeepCopy()
	p := planAccess(s)
	assert.Equal(t, len(p.actions), 0)
	assert.DeepEqual(t, s.proxies["edge-alpha"], before)
	current := h.status(sa.Name)
	current.Spec.Ports = current.Spec.Ports[:1]
	_, err = h.clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Update(ctx, current, metav1.UpdateOptions{})
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

func captureScopeLogs(h *scopeHarness) (*bytes.Buffer, func() []map[string]any) {
	var output bytes.Buffer
	h.m.logger = slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})).With("component", "kube.securedaccess.manager")
	return &output, func() []map[string]any {
		var records []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
			if line == "" {
				continue
			}
			var record map[string]any
			assert.NilError(h.t, json.Unmarshal([]byte(line), &record))
			records = append(records, record)
		}
		return records
	}
}

func TestAccessScopeLogging(t *testing.T) {
	for _, scenario := range []string{"ready", "validation", "output failure", "status failure", "confirmed response"} {
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
			if scenario == "status failure" || scenario == "confirmed response" {
				h.clients.Skupper.(*skupperfake.Clientset).PrependReactor("update", "securedaccesses", func(a k8stesting.Action) (bool, runtime.Object, error) {
					if a.GetSubresource() != "status" {
						return false, nil, nil
					}
					if scenario == "status failure" {
						return true, nil, failure
					}
					written := a.(k8stesting.UpdateAction).GetObject().(*saapi.SecuredAccess).DeepCopy()
					written.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionUnknown, Reason: "Admission", Message: "confirmed response", ObservedGeneration: 7}}
					return true, written, nil
				})
			}
			output, logs := captureScopeLogs(h)
			err := h.run(sa.Key())
			failed := scenario == "output failure" || scenario == "status failure"
			assert.Equal(t, err != nil, failed)
			records := logs()
			assert.Equal(t, len(records), 3)
			for _, record := range records {
				assert.Equal(t, record["component"], "kube.securedaccess.manager")
				assert.Equal(t, record["scope"], "SecuredAccess")
				assert.Equal(t, record["key"], "test/edge")
			}
			status := records[1]
			assert.Equal(t, status["action"], "status")
			assert.Equal(t, status["kind"], "SecuredAccess")
			assert.Equal(t, status["namespace"], "test")
			assert.Equal(t, status["name"], "edge")
			if scenario == "status failure" {
				assert.Equal(t, status["level"], "ERROR")
				assert.Equal(t, status["msg"], "Failed to update SecuredAccess test/edge status")
				_, present := status["conditions"]
				assert.Assert(t, !present)
			} else {
				assert.Equal(t, status["level"], "INFO")
				assert.Equal(t, status["outcome"], "succeeded")
				assert.Assert(t, strings.Contains(status["msg"].(string), "Ready="))
				conditions := status["conditions"].([]any)
				if scenario == "confirmed response" {
					assert.Equal(t, len(conditions), 1)
					assert.Equal(t, conditions[0].(map[string]any)["status"], "Unknown")
					assert.Equal(t, conditions[0].(map[string]any)["observed_generation"], float64(7))
					assert.Assert(t, strings.Contains(status["msg"].(string), "confirmed response"))
				} else if scenario != "ready" {
					assert.Assert(t, strings.Contains(status["msg"].(string), "Configured=False (Error)"))
					assert.Assert(t, strings.Contains(status["msg"].(string), "Ready=False"))
				}
			}
			if scenario == "output failure" {
				assert.Equal(t, records[0]["level"], "ERROR")
				assert.Equal(t, records[0]["msg"], "Failed to create Service test/edge")
			}
			summary := records[2]
			assert.Equal(t, summary["level"], "DEBUG")
			assert.Equal(t, summary["phase"], "execute")
			assert.Equal(t, summary["actions"], float64(2))
			assert.Equal(t, summary["unchanged_actions"], float64(0))
			if failed {
				assert.Equal(t, summary["outcome"], "failed")
				assert.Equal(t, summary["failed_actions"], float64(1))
			} else {
				assert.Equal(t, summary["outcome"], "succeeded")
				assert.Equal(t, summary["failed_actions"], float64(0))
			}
			if scenario == "ready" {
				h.settle(sa.Name)
				output.Reset()
				assert.NilError(t, h.run(sa.Key()))
				records = logs()
				assert.Equal(t, len(records), 1)
				assert.Equal(t, records[0]["level"], "DEBUG")
				assert.Equal(t, records[0]["actions"], float64(0))
			}
		})
	}
}

func TestAggregateAndGatewayLogging(t *testing.T) {
	h := newHarness(t, nil)
	sa := testAccess("local")
	sa.Spec.Issuer, sa.Spec.Certificate = "issuer", "shared"
	h.put(sa)
	output, logs := captureScopeLogs(h)
	event := watchers.ResourceChange{Key: "test/shared"}
	assert.NilError(t, h.m.certificates.Handle(event))
	records := logs()
	assert.Equal(t, records[0]["msg"], "Created Certificate test/shared")
	assert.Equal(t, records[0]["scope"], "SecuredAccessCertificate")
	assert.Equal(t, records[0]["namespace"], "test")
	h.observe()
	output.Reset()
	assert.NilError(t, h.m.certificates.Handle(event))
	assert.Equal(t, len(logs()), 1)
	assert.Equal(t, logs()[0]["actions"], float64(0))

	output.Reset()
	assert.Assert(t, h.m.certificates.Handle(watchers.ResourceChange{Key: "bad/key/format"}) != nil)
	assert.Equal(t, logs()[0]["phase"], "key")
	assert.Equal(t, logs()[0]["outcome"], "failed")
	assert.Equal(t, logs()[0]["failed_actions"], float64(0))

	output.Reset()
	// The dynamic fake cannot create via Apply; the failed request must still
	// describe Apply rather than promise a Create or a successful realization.
	assert.Assert(t, h.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}) != nil)
	assert.Equal(t, logs()[0]["msg"], "Failed to apply Gateway controller/skupper")
	assert.Equal(t, logs()[0]["level"], "ERROR")
	assert.Equal(t, logs()[0]["action"], "create")
	assert.Equal(t, logs()[0]["scope"], "Gateway")

	assert.NilError(t, h.clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Delete(context.Background(), sa.Name, metav1.DeleteOptions{}))
	h.observe()
	output.Reset()
	assert.NilError(t, h.m.certificates.Handle(event))
	assert.Equal(t, logs()[0]["msg"], "Requested deletion of Certificate test/shared")

	// A planning refusal has no failed API action, but must still produce a
	// failed summary at the correct phase and return the error for the processor.
	h.put(sa)
	_, err := h.clients.Skupper.SkupperV2alpha1().Certificates("test").Create(context.Background(), &saapi.Certificate{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "test"}}, metav1.CreateOptions{})
	assert.NilError(t, err)
	h.observe()
	output.Reset()
	assert.Assert(t, h.m.certificates.Handle(event) != nil)
	assert.Equal(t, len(logs()), 1)
	assert.Equal(t, logs()[0]["level"], "DEBUG")
	assert.Equal(t, logs()[0]["phase"], "plan")
	assert.Equal(t, logs()[0]["actions"], float64(0))
	assert.Equal(t, logs()[0]["failed_actions"], float64(0))
	assert.Equal(t, logs()[0]["outcome"], "failed")
}
