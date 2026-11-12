//go:build integration

package controller

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	"github.com/skupperproject/skupper/internal/kube/resource"
	"github.com/skupperproject/skupper/internal/kube/securedaccess"
	"github.com/skupperproject/skupper/internal/kube/watchers"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	"gotest.tools/v3/assert"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

type routingTransport struct {
	next            http.RoundTripper
	patches, writes *atomic.Int64
}

func (r routingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == "PATCH" {
		r.patches.Add(1)
	}
	switch req.Method {
	case "POST", "PUT", "PATCH", "DELETE":
		r.writes.Add(1)
	}
	return r.next.RoundTrip(req)
}

// Same official Gateway API v1.3.0 CRDs as securedaccess's SSA suite. Exercise
// production controller registration and the queue, not a replica of routing.
func TestRealAPIRoutingConvergence(t *testing.T) {
	crds := os.Getenv("GATEWAY_API_CRDS")
	if crds == "" {
		t.Fatal("GATEWAY_API_CRDS must contain official v1.3.0 Gateway/TLSRoute CRDs")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd", "bases"), crds}, ErrorIfCRDPathMissing: true}
	env.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1")
	cfg, err := env.Start()
	assert.NilError(t, err)
	t.Cleanup(func() { assert.NilError(t, env.Stop()) })
	var patches, writes atomic.Int64
	cfg.WrapTransport = func(next http.RoundTripper) http.RoundTripper { return routingTransport{next, &patches, &writes} }
	clients, err := internalclient.NewClientFromRestConfig(cfg, "controller")
	assert.NilError(t, err)
	ctx := context.Background()
	for _, name := range []string{"controller", "test", "other"} {
		_, err := clients.Kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
		assert.NilError(t, err)
	}
	config := &securedaccess.Config{EnabledAccessTypes: []string{"local", "gateway"}, GatewayClass: "test-gateway", GatewayDomain: "gateway.example", GatewayPort: 7443}
	metrics := &routingMetrics{}
	c, stop := routingController(t, clients, config, watchers.WithMetricsProvider(metrics), func(p *watchers.EventProcessor) { p.SetResync(2 * time.Second); p.SetResyncShort(2 * time.Second) })
	gateways := clients.Dynamic.Resource(resource.GatewayResource()).Namespace("controller")
	routingUntil(t, c, "explicit startup creates Gateway with no Access", func() bool {
		_, err := gateways.Get(ctx, "skupper", metav1.GetOptions{})
		return err == nil
	})
	for _, namespace := range []string{"test", "other"} {
		_, err = clients.Skupper.SkupperV2alpha1().SecuredAccesses(namespace).Create(ctx, &skupperv2alpha1.SecuredAccess{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "edge"},
			Spec:       skupperv2alpha1.SecuredAccessSpec{AccessType: "gateway", Selector: map[string]string{"app": "backend"}, Ports: []skupperv2alpha1.SecuredAccessPort{{Name: "alpha", Port: 8080, TargetPort: 8181}, {Name: "beta", Port: 9090, TargetPort: 9191}}},
		}, metav1.CreateOptions{})
		assert.NilError(t, err)
	}
	routingUntil(t, c, "cross-namespace consumers realize from synced inventories", func() bool {
		for _, namespace := range []string{"test", "other"} {
			current, err := clients.Skupper.SkupperV2alpha1().SecuredAccesses(namespace).Get(ctx, "edge", metav1.GetOptions{})
			if err != nil || !current.IsReady() {
				return false
			}
		}
		return true
	})
	quiet := func() int64 {
		last, changed := writes.Load(), time.Now()
		routingUntil(t, c, "bounded informer feedback followed by quiet interval", func() bool {
			now := writes.Load()
			if now != last {
				last, changed = now, time.Now()
			}
			return time.Since(changed) >= 500*time.Millisecond
		})
		return last
	}
	versions := func() map[string]string {
		result := map[string]string{}
		gateway, err := gateways.Get(ctx, "skupper", metav1.GetOptions{})
		assert.NilError(t, err)
		result["Gateway"] = gateway.GetResourceVersion()
		for _, namespace := range []string{"test", "other"} {
			routes, err := clients.Dynamic.Resource(resource.TlsRouteResource()).Namespace(namespace).List(ctx, metav1.ListOptions{})
			assert.NilError(t, err)
			assert.Equal(t, len(routes.Items), 2)
			for _, route := range routes.Items {
				result[namespace+"/"+route.GetName()] = route.GetResourceVersion()
			}
			current, err := clients.Skupper.SkupperV2alpha1().SecuredAccesses(namespace).Get(ctx, "edge", metav1.GetOptions{})
			assert.NilError(t, err)
			assert.Assert(t, current.IsReady())
			result[namespace+"/Access"] = current.ResourceVersion
		}
		return result
	}
	last := quiet()
	lastPatches := patches.Load()
	before := versions()
	for i := 0; i < 3; i++ {
		notificationsBefore := metrics.accessNotifications.Load()
		accessBefore, gatewayBefore := metrics.access.Load(), metrics.gateway.Load()
		// Prove real informer/resync delivery and both queued scope families
		// execute even when successful receipts suppress all settled writes.
		routingUntil(t, c, "resync reaches resource callbacks and queued scopes", func() bool {
			return metrics.accessNotifications.Load() > notificationsBefore && metrics.access.Load() > accessBefore && metrics.gateway.Load() > gatewayBefore
		})
		next := quiet()
		assert.Equal(t, next, last, "settled resync must attempt zero mutations")
		assert.Equal(t, patches.Load(), lastPatches, "settled SSA must use success receipts")
		assert.DeepEqual(t, versions(), before)
		t.Logf("resync %d: Access callbacks=%d Access scopes=%d Gateway scopes=%d mutation attempts=0 PATCH attempts=0 changed stored RVs=0; >=500ms quiet", i+1, metrics.accessNotifications.Load()-notificationsBefore, metrics.access.Load()-accessBefore, metrics.gateway.Load()-gatewayBefore)
	}
	// A foreign Gateway is deliberately outside the named watch.
	foreign, err := gateways.Get(ctx, "skupper", metav1.GetOptions{})
	assert.NilError(t, err)
	foreign.SetName("unrelated")
	foreign.SetResourceVersion("")
	foreign.SetUID("")
	foreign.SetManagedFields(nil)
	_, err = gateways.Create(ctx, foreign, metav1.CreateOptions{})
	assert.NilError(t, err)
	end := time.Now().Add(200 * time.Millisecond)
	accessBefore, gatewayBefore, patchBefore := metrics.access.Load(), metrics.gateway.Load(), patches.Load()
	for time.Now().Before(end) {
		c.eventProcessor.TestProcessAll()
		time.Sleep(10 * time.Millisecond)
	}
	assert.Equal(t, metrics.access.Load(), accessBefore)
	assert.Equal(t, metrics.gateway.Load(), gatewayBefore)
	assert.Equal(t, patches.Load(), patchBefore)
	assert.Equal(t, len(c.accessCaches.Gateways.List()), 1)
	// Singleton deletion still wakes its sole writer and both namespaces.
	original, err := gateways.Get(ctx, "skupper", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.NilError(t, gateways.Delete(ctx, "skupper", metav1.DeleteOptions{}))
	routingUntil(t, c, "named Gateway delete recreates singleton", func() bool {
		current, err := gateways.Get(ctx, "skupper", metav1.GetOptions{})
		return err == nil && current.GetUID() != original.GetUID()
	})
	quiet()
	// Persist legacy-labeled output, delete its definition while stopped, then
	// reconstruct cleanup exclusively from the fresh unfiltered inventories.
	stop()
	tlsRoutes := clients.Dynamic.Resource(resource.TlsRouteResource()).Namespace("test")
	_, err = tlsRoutes.Patch(ctx, "edge-alpha", types.MergePatchType, []byte(`{"metadata":{"labels":{"internal.skupper.io/secured-access":null,"internal.skupper.io/securedaccess":"edge"}}}`), metav1.PatchOptions{})
	assert.NilError(t, err)
	assert.NilError(t, clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Delete(ctx, "edge", metav1.DeleteOptions{}))
	c, _ = routingController(t, clients, config)
	routingUntil(t, c, "restart cleans old-label outputs with Access absent", func() bool {
		list, err := tlsRoutes.List(ctx, metav1.ListOptions{})
		if err != nil || len(list.Items) != 0 {
			return false
		}
		_, err = clients.Kube.CoreV1().Services("test").Get(ctx, "edge", metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	})
	remaining, err := clients.Dynamic.Resource(resource.TlsRouteResource()).Namespace("other").List(ctx, metav1.ListOptions{})
	assert.NilError(t, err)
	assert.Equal(t, len(remaining.Items), 2, "cleanup must not cross scope namespaces")
}
