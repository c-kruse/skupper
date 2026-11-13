//go:build integration

package securedaccess

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	"github.com/skupperproject/skupper/internal/kube/resource"
	"github.com/skupperproject/skupper/internal/kube/watchers"
	"gotest.tools/v3/assert"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

type mutationTransport struct {
	next   http.RoundTripper
	writes *atomic.Int64
}

func (r mutationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.Method {
	case "POST", "PUT", "PATCH", "DELETE":
		r.writes.Add(1)
	}
	return r.next.RoundTrip(req)
}

// Uses the official Gateway API schemas, not a permissive imitation. Supply
// GATEWAY_API_CRDS with the Gateway and experimental TLSRoute CRDs from
// kubernetes-sigs/gateway-api's config/crd directory at tag v1.3.0.
func TestRealAPIAccessGateway(t *testing.T) {
	crds := os.Getenv("GATEWAY_API_CRDS")
	if crds == "" {
		t.Fatal("GATEWAY_API_CRDS must name the directory containing the v1.3.0 Gateway/TLSRoute CRDs")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd", "bases"), crds}, ErrorIfCRDPathMissing: true}
	env.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1")
	cfg, err := env.Start()
	assert.NilError(t, err)
	t.Cleanup(func() { assert.NilError(t, env.Stop()) })
	var writes atomic.Int64
	cfg.WrapTransport = func(next http.RoundTripper) http.RoundTripper { return mutationTransport{next: next, writes: &writes} }
	clients, err := internalclient.NewClientFromRestConfig(cfg, "controller")
	assert.NilError(t, err)
	ctx := context.Background()
	for _, ns := range []string{"test", "controller"} {
		_, err = clients.Kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
		assert.NilError(t, err)
	}
	h := newHarness(t, clients)
	policy := h.m.context.(*testContext).policy
	// Earlier templates change the value transiently; selectors see those
	// changes, but the final lexical winner is already settled on the server.
	for _, name := range []string{"z-final", "a-first"} {
		values := map[string]string{"skupper.io/label-template": "true", "stage": "first", "priority": "intermediate"}
		data := map[string]string{}
		if name == "z-final" {
			values["priority"] = "final"
			data["labelSelector"] = "stage=first"
		}
		assert.NilError(t, policy.Update("controller/"+name, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "controller", Labels: values}, Data: data}))
	}
	// A create plan must not adopt an object that appeared after its snapshot,
	// even with the same SSA manager and identical spec.
	occupied := desiredGateway("controller", "unrelated-controller", testConfig())
	occupied.SetName("occupied")
	occupiedData, err := json.Marshal(occupied)
	assert.NilError(t, err)
	_, err = clients.Dynamic.Resource(resource.GatewayResource()).Namespace("controller").Patch(ctx, "occupied", types.ApplyPatchType, occupiedData, metav1.PatchOptions{FieldManager: "skupper-controller"})
	assert.NilError(t, err)
	desired := desiredGateway("controller", "skupper-controller", testConfig())
	desired.SetName("occupied")
	err = h.m.executeAction(accessAction{verb: "create", kind: "Gateway", object: desired})
	assert.Assert(t, apierrors.IsInvalid(err))
	untouched, err := clients.Dynamic.Resource(resource.GatewayResource()).Namespace("controller").Get(ctx, "occupied", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.Equal(t, untouched.GetAnnotations()[gatewayOwner], "unrelated-controller")
	// Metadata policy may decorate an authorized Gateway, not transfer the
	// controller's authority. Check both creation and an actual metadata update.
	authorityPolicy := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "gateway-authority", Namespace: "controller", Labels: map[string]string{"skupper.io/label-template": "true"}, Annotations: map[string]string{gatewayOwner: "policy-override", "decoration": "created"}}, Data: map[string]string{"kind": "Gateway", "name": "skupper"}}
	assert.NilError(t, policy.Update("controller/gateway-authority", authorityPolicy))
	assert.NilError(t, h.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}))
	gateways := clients.Dynamic.Resource(resource.GatewayResource()).Namespace("controller")
	createdGateway, err := gateways.Get(ctx, "skupper", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.Equal(t, createdGateway.GetAnnotations()[gatewayOwner], "skupper-controller")
	assert.Equal(t, createdGateway.GetAnnotations()["decoration"], "created")
	authorityPolicy = authorityPolicy.DeepCopy()
	authorityPolicy.Annotations["decoration"] = "updated"
	assert.NilError(t, policy.Update("controller/gateway-authority", authorityPolicy))
	h.observe()
	assert.NilError(t, h.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}))
	updatedGateway, err := gateways.Get(ctx, "skupper", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.Equal(t, updatedGateway.GetUID(), createdGateway.GetUID())
	assert.Equal(t, updatedGateway.GetAnnotations()[gatewayOwner], "skupper-controller")
	assert.Equal(t, updatedGateway.GetAnnotations()["decoration"], "updated")
	for _, kind := range []string{"nodeport", "ingress", "gateway"} {
		sa := testAccess(kind)
		sa.Name = kind
		sa.UID = ""
		sa.Spec.Issuer = "issuer"
		h.put(sa)
		h.settle(sa.Name)
		assert.Assert(t, h.status(sa.Name).IsReady())
	}
	h.observe()
	assert.NilError(t, h.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}))
	h.observe()
	assertSettled := func(h *scopeHarness) {
		t.Helper()
		before := writes.Load()
		for i := 0; i < 4; i++ {
			for _, name := range []string{"nodeport", "ingress", "gateway"} {
				assert.NilError(t, h.run("test/"+name))
				current := h.status(name)
				assert.NilError(t, h.m.Ensure("test", name, current.Spec, nil, current.OwnerReferences))
			}
			assert.NilError(t, h.runCertificates())
			assert.NilError(t, h.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}))
		}
		assert.Equal(t, writes.Load()-before, int64(0), "settled HTTP mutations, including status/SSA")
	}
	assertSettled(h)
	h = newHarness(t, clients)
	h.m.context.(*testContext).policy = policy.Snapshot("test")
	h.observe()
	assertSettled(h)
	assert.Equal(t, h.status("ingress").Labels["priority"], "final")
	t.Log("defaulted Service/Ingress/TLSRoute/Gateway: zero HTTP mutations across repeated scopes and restart")
	// A stale deletion cannot remove a different object at the same name.
	services := clients.Kube.CoreV1().Services("test")
	old, err := services.Create(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "delete-race"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8080}}}}, metav1.CreateOptions{})
	assert.NilError(t, err)
	assert.NilError(t, services.Delete(ctx, old.Name, metav1.DeleteOptions{}))
	replacement, err := services.Create(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: old.Name}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 9090}}}}, metav1.CreateOptions{})
	assert.NilError(t, err)
	err = h.m.executeAction(accessAction{verb: "delete", kind: "Service", object: old})
	assert.Assert(t, apierrors.IsConflict(err))
	untouchedService, err := services.Get(ctx, old.Name, metav1.GetOptions{})
	assert.NilError(t, err)
	assert.Equal(t, untouchedService.UID, replacement.UID)
	assert.Equal(t, untouchedService.Spec.Ports[0].Port, int32(9090))

	// Port removal preserves allocations and clears obsolete provider outputs.
	svc, err := clients.Kube.CoreV1().Services("test").Get(ctx, "nodeport", metav1.GetOptions{})
	assert.NilError(t, err)
	allocated := svc.Spec.Ports[0].NodePort
	sa := h.status("nodeport")
	sa.Spec.Ports = sa.Spec.Ports[:1]
	sa.Spec.Ports[0].TargetPort = 8282
	_, err = clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Update(ctx, sa, metav1.UpdateOptions{})
	assert.NilError(t, err)
	h.settle("nodeport")
	svc, err = clients.Kube.CoreV1().Services("test").Get(ctx, "nodeport", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.Equal(t, len(svc.Spec.Ports), 1)
	assert.Equal(t, svc.Spec.Ports[0].NodePort, allocated)
	assert.Equal(t, svc.Spec.Ports[0].TargetPort.IntVal, int32(8282))

	// API resourceVersion rejects stale plans. This is not a fake reactor.
	sa = h.status("nodeport")
	sa.Spec.Selector = map[string]string{"app": "updated"}
	_, err = clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Update(ctx, sa, metav1.UpdateOptions{})
	assert.NilError(t, err)
	h.observe()
	snapshot, err := h.m.snapshot("test/nodeport")
	assert.NilError(t, err)
	plan := planAccess(snapshot)
	svc.Labels["external"] = "preserved"
	_, err = clients.Kube.CoreV1().Services("test").Update(ctx, svc, metav1.UpdateOptions{})
	assert.NilError(t, err)
	_, err = h.m.executeAccess(plan)
	assert.Assert(t, apierrors.IsConflict(err))
	h.settle("nodeport")
	t.Log("real stale Service update returned 409 and recovered")

	// Exercise SSA removal with a real list-map schema and field ownership.
	tls := clients.Dynamic.Resource(resource.TlsRouteResource()).Namespace("test")
	drift := desiredTLSRoute(h.status("gateway"), h.status("gateway").Spec.Ports[0], "controller", "gateway.example")
	drift.Object["spec"].(map[string]interface{})["hostnames"] = []interface{}{"stale.example", "extra.example"}
	driftData, err := json.Marshal(drift)
	assert.NilError(t, err)
	_, err = tls.Patch(ctx, drift.GetName(), types.ApplyPatchType, driftData, metav1.PatchOptions{FieldManager: "skupper-controller"})
	assert.NilError(t, err)
	h.settle("gateway")
	obj, err := tls.Get(ctx, "gateway-alpha", metav1.GetOptions{})
	assert.NilError(t, err)
	hosts, _, err := unstructured.NestedStringSlice(obj.Object, "spec", "hostnames")
	assert.NilError(t, err)
	assert.DeepEqual(t, hosts, []string{"gateway-alpha.test.gateway.example"})
	// A different SSA manager's changed hostname must not be forced away.
	patch := []byte(`{"apiVersion":"gateway.networking.k8s.io/v1alpha2","kind":"TLSRoute","metadata":{"name":"gateway-alpha","namespace":"test"},"spec":{"hostnames":["external.example"]}}`)
	force := true
	_, err = tls.Patch(ctx, "gateway-alpha", types.ApplyPatchType, patch, metav1.PatchOptions{FieldManager: "external-controller", Force: &force})
	assert.NilError(t, err)
	h.observe()
	err = h.run("test/gateway")
	assert.Assert(t, apierrors.IsConflict(err))
	assert.Assert(t, !h.status("gateway").IsReady())
	assert.NilError(t, tls.Delete(ctx, "gateway-alpha", metav1.DeleteOptions{}))
	h.settle("gateway")
	t.Log("TLSRoute SSA removed stale hosts, rejected ownership conflict, recovered after deletion")

	// Shared Gateway drift and foreign ownership, independent of Access writers.
	staleConfig := testConfig()
	staleConfig.GatewayPort = 9443
	staleGateway := desiredGateway("controller", "skupper-controller", staleConfig)
	staleListeners, _, _ := unstructured.NestedSlice(staleGateway.Object, "spec", "listeners")
	extra := staleListeners[0].(map[string]interface{})
	extra["name"] = "obsolete"
	extra["port"] = int64(9555)
	assert.NilError(t, unstructured.SetNestedSlice(staleGateway.Object, append(staleGateway.Object["spec"].(map[string]interface{})["listeners"].([]interface{}), extra), "spec", "listeners"))
	driftData, err = json.Marshal(staleGateway)
	assert.NilError(t, err)
	_, err = gateways.Patch(ctx, "skupper", types.ApplyPatchType, driftData, metav1.PatchOptions{FieldManager: "skupper-controller"})
	assert.NilError(t, err)
	h.observe()
	assert.NilError(t, h.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}))
	h.observe()
	gw, err := gateways.Get(ctx, "skupper", metav1.GetOptions{})
	assert.NilError(t, err)
	listeners, _, _ := unstructured.NestedSlice(gw.Object, "spec", "listeners")
	assert.Equal(t, len(listeners), 1)
	assert.Equal(t, listeners[0].(map[string]interface{})["port"], int64(7443))
	// Non-forcing SSA applies to the singleton too, not only its TLSRoutes.
	patch = []byte(`{"apiVersion":"gateway.networking.k8s.io/v1","kind":"Gateway","metadata":{"name":"skupper","namespace":"controller"},"spec":{"listeners":[{"name":"tls","port":8555}]}}`)
	_, err = gateways.Patch(ctx, "skupper", types.ApplyPatchType, patch, metav1.PatchOptions{FieldManager: "external-controller", Force: &force})
	assert.NilError(t, err)
	h.observe()
	err = h.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"})
	assert.Assert(t, apierrors.IsConflict(err))
	assert.NilError(t, gateways.Delete(ctx, "skupper", metav1.DeleteOptions{}))
	h.observe()
	assert.NilError(t, h.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}))
	gw, err = gateways.Get(ctx, "skupper", metav1.GetOptions{})
	assert.NilError(t, err)
	annotations := gw.GetAnnotations()
	delete(annotations, gatewayOwner)
	gw.SetAnnotations(annotations)
	_, err = gateways.Update(ctx, gw, metav1.UpdateOptions{})
	assert.NilError(t, err)
	h.observe()
	before := writes.Load()
	err = h.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"})
	assert.ErrorContains(t, err, "conflicting ownership")
	assert.Equal(t, writes.Load(), before)
	assert.NilError(t, h.run("test/gateway"))
	assert.Assert(t, !h.status("gateway").IsReady())
	annotations[gatewayOwner] = "skupper-controller"
	gw, err = gateways.Get(ctx, "skupper", metav1.GetOptions{})
	assert.NilError(t, err)
	gw.SetAnnotations(annotations)
	_, err = gateways.Update(ctx, gw, metav1.UpdateOptions{})
	assert.NilError(t, err)
	h.observe()
	assert.NilError(t, h.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}))
	h.settle("gateway")
	t.Log("shared Gateway drift repaired; unmarked legacy Gateway left untouched")

	// Previously owned optional fields are removed, not merged indefinitely.
	sa = h.status("ingress")
	sa.Spec.Settings = map[string]string{SettingIngressClassName: "temporary"}
	_, err = clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Update(ctx, sa, metav1.UpdateOptions{})
	assert.NilError(t, err)
	h.settle("ingress")
	sa = h.status("ingress")
	sa.Spec.Settings = nil
	_, err = clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Update(ctx, sa, metav1.UpdateOptions{})
	assert.NilError(t, err)
	h.settle("ingress")
	ing, err := clients.Kube.NetworkingV1().Ingresses("test").Get(ctx, "ingress", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.Assert(t, ing.Spec.IngressClassName == nil)
	h.observe()
	assertSettled(h)

	// HA exposures share one aggregate writer, not competing per-Access writes.
	for _, name := range []string{"ha-a", "ha-b"} {
		access := testAccess("local")
		access.Name, access.UID = name, ""
		access.Spec.Issuer, access.Spec.Certificate = "issuer", "ha-shared"
		h.put(access)
		h.settle(name)
	}
	certificates := clients.Skupper.SkupperV2alpha1().Certificates("test")
	shared, err := certificates.Get(ctx, "ha-shared", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.DeepEqual(t, shared.Spec.Hosts, []string{"ha-a", "ha-a.test", "ha-b", "ha-b.test"})
	assert.Equal(t, len(shared.OwnerReferences), 2)
	assert.Equal(t, shared.Spec.Subject, "ha-a")
	h = newHarness(t, clients)
	h.m.context.(*testContext).policy = policy.Snapshot("test")
	h.observe()
	before = writes.Load()
	for i := 0; i < 3; i++ {
		assert.NilError(t, h.run("test/ha-b"))
		assert.NilError(t, h.run("test/ha-a"))
		assert.NilError(t, h.runCertificates())
	}
	assert.Equal(t, writes.Load()-before, int64(0), "HA aggregate must remain HTTP write-free after restart")
	// Removal changes the aggregate. A concurrent metadata update must reject
	// the stale plan rather than overwrite the other writer's changes.
	assert.NilError(t, clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Delete(ctx, "ha-a", metav1.DeleteOptions{}))
	h.observe()
	certificateSnapshot, err := h.m.certificates.snapshot("test/ha-shared")
	assert.NilError(t, err)
	certificateAction, err := planAccessCertificate(certificateSnapshot)
	assert.NilError(t, err)
	assert.Assert(t, certificateAction != nil)
	shared.Labels["external"] = "preserved"
	_, err = certificates.Update(ctx, shared, metav1.UpdateOptions{})
	assert.NilError(t, err)
	err = h.m.executeAction(*certificateAction)
	assert.Assert(t, apierrors.IsConflict(err))
	h.observe()
	assert.NilError(t, h.runCertificates())
	shared, err = certificates.Get(ctx, "ha-shared", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.DeepEqual(t, shared.Spec.Hosts, []string{"ha-b", "ha-b.test"})
	assert.Equal(t, shared.Spec.Subject, "ha-a", "removing the first owner must not change the established subject")
	assert.Equal(t, shared.Labels["external"], "preserved")
	assert.Equal(t, len(shared.OwnerReferences), 1)
	assert.NilError(t, clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Delete(ctx, "ha-b", metav1.DeleteOptions{}))
	h = newHarness(t, clients)
	h.m.context.(*testContext).policy = policy.Snapshot("test")
	h.observe()
	assert.NilError(t, h.runCertificates())
	_, err = certificates.Get(ctx, "ha-shared", metav1.GetOptions{})
	assert.Assert(t, apierrors.IsNotFound(err))
	t.Log("HA shared Certificate: stable aggregate/subject, zero settled HTTP writes, real 409 retry, surviving-output restart cleanup")

	// Failed/preconditioned removal can be rediscovered from surviving outputs
	// without a definition or any state carried over from the previous manager.
	assert.NilError(t, clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Delete(ctx, "gateway", metav1.DeleteOptions{}))
	h = newHarness(t, clients)
	h.m.context.(*testContext).policy = policy.Snapshot("test")
	h.observe()
	assert.NilError(t, h.run("test/gateway"))
	assert.NilError(t, h.runCertificates())
	remaining, err := tls.List(ctx, metav1.ListOptions{})
	assert.NilError(t, err)
	assert.Equal(t, len(remaining.Items), 0)
	_, err = clients.Kube.CoreV1().Services("test").Get(ctx, "gateway", metav1.GetOptions{})
	assert.Assert(t, apierrors.IsNotFound(err))
	_, err = clients.Skupper.SkupperV2alpha1().Certificates("test").Get(ctx, "gateway", metav1.GetOptions{})
	assert.Assert(t, apierrors.IsNotFound(err))
	t.Log("restart after definition removal cleaned Service, TLSRoutes and Certificate intent")
}
