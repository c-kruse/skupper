//go:build integration

package securedaccess

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	"github.com/skupperproject/skupper/internal/kube/resource"
	"github.com/skupperproject/skupper/internal/kube/site/labels"
	"github.com/skupperproject/skupper/internal/kube/watchers"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	"gotest.tools/v3/assert"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

type mutationTransport struct {
	next           http.RoundTripper
	writes         *atomic.Int64
	openAPIReads   *atomic.Int64
	gatewayPatches *atomic.Int64
}

func (r mutationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasPrefix(req.URL.Path, "/openapi/") {
		r.openAPIReads.Add(1)
	}
	if req.Method == "PATCH" && strings.Contains(req.URL.Path, "/gateways/") {
		r.gatewayPatches.Add(1)
	}
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
	var writes, openAPIReads, gatewayPatches atomic.Int64
	cfg.WrapTransport = func(next http.RoundTripper) http.RoundTripper {
		return mutationTransport{next: next, writes: &writes, openAPIReads: &openAPIReads, gatewayPatches: &gatewayPatches}
	}
	clients, err := internalclient.NewClientFromRestConfig(cfg, "controller")
	assert.NilError(t, err)
	ctx := context.Background()
	for _, ns := range []string{"test", "controller"} {
		_, err = clients.Kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
		assert.NilError(t, err)
	}
	// Settled SSA still sends requests, but must not change any stored revision.
	assertUnchanged := func(t *testing.T, h *scopeHarness, requests int64, run func() error) {
		t.Helper()
		versions := func() map[string]string {
			h.observe()
			result := map[string]string{}
			for kind, store := range h.stores {
				for _, object := range store.List() {
					metadata := object.(metav1.Object)
					result[kind+"/"+metadata.GetNamespace()+"/"+metadata.GetName()] = metadata.GetResourceVersion()
				}
			}
			return result
		}
		beforeVersions, before := versions(), writes.Load()
		assert.NilError(t, run())
		assert.Equal(t, writes.Load()-before, requests, "attempted API mutations, including unchanged SSA")
		assert.DeepEqual(t, versions(), beforeVersions)
	}
	h := newHarness(t, clients)
	t.Run("Gateway without metadata policy", func(t *testing.T) {
		bare := newHarness(t, clients)
		gateways := clients.Dynamic.Resource(resource.GatewayResource()).Namespace("controller")
		assert.NilError(t, bare.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}))
		t.Cleanup(func() { assert.NilError(t, gateways.Delete(ctx, "skupper", metav1.DeleteOptions{})) })
		beforeInitial, err := gateways.Get(ctx, "skupper", metav1.GetOptions{})
		assert.NilError(t, err)
		// A successful create already establishes a receipt. No follow-up
		// apply is required just to persist read-time CRD status defaults.
		bare.observe()
		assert.NilError(t, bare.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}))
		afterInitial, err := gateways.Get(ctx, "skupper", metav1.GetOptions{})
		assert.NilError(t, err)
		assert.DeepEqual(t, beforeInitial.Object["spec"], afterInitial.Object["spec"])
		for i := 0; i < 3; i++ {
			assertUnchanged(t, bare, 0, func() error { return bare.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}) })
		}
		bare = newHarness(t, clients)
		// Restart loses only the optimization. The first apply can persist
		// read-time defaults; record its returned revision, then omit requests.
		beforeRestart := writes.Load()
		bare.observe()
		assert.NilError(t, bare.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}))
		assert.Equal(t, writes.Load()-beforeRestart, int64(1))
		assertUnchanged(t, bare, 0, func() error { return bare.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}) })
		// Even a status-only RV change invalidates the receipt. Apply exactly
		// once for each new revision, then remain request-free again.
		for i := range 2 {
			patch := []byte(fmt.Sprintf(`{"status":{"addresses":[{"type":"Hostname","value":"status-%d.example"}]}}`, i))
			_, err := gateways.Patch(ctx, "skupper", types.MergePatchType, patch, metav1.PatchOptions{}, "status")
			assert.NilError(t, err)
			bare.observe()
			before := gatewayPatches.Load()
			assert.NilError(t, bare.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}))
			assert.Equal(t, gatewayPatches.Load()-before, int64(1))
			assertUnchanged(t, bare, 0, func() error { return bare.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}) })
		}
		bare.m.applyCache.Clear()
		assertUnchanged(t, bare, 1, func() error { return bare.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}) })
		assertUnchanged(t, bare, 0, func() error { return bare.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}) })
		external := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway", "metadata": map[string]interface{}{"name": "skupper", "namespace": "controller"}}}
		for _, labels := range []map[string]string{{"external": "first"}, {"external": "second"}, nil} {
			external.SetLabels(labels)
			data, err := json.Marshal(external)
			assert.NilError(t, err)
			actual, err := gateways.Patch(ctx, "skupper", types.ApplyPatchType, data, metav1.PatchOptions{FieldManager: "external-metadata"})
			assert.NilError(t, err, "empty authored metadata must not claim the entire labels map")
			assert.Equal(t, actual.GetLabels()["external"], labels["external"])
			assertUnchanged(t, bare, 1, func() error { return bare.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}) })
			assertUnchanged(t, bare, 0, func() error { return bare.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}) })
		}
		output, logs := captureScopeLogs(bare)
		assertUnchanged(t, bare, 0, func() error { return bare.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}) })
		records := logs()
		assert.Equal(t, len(records), 1)
		assert.Equal(t, records[0]["msg"], "Reconciled Gateway scope")
		assert.Equal(t, records[0]["level"], "DEBUG")
		assert.Equal(t, records[0]["actions"], float64(0))
		assert.Equal(t, records[0]["unchanged_actions"], float64(0))
		assert.Equal(t, records[0]["changed_actions"], float64(0))
		assert.Equal(t, records[0]["failed_actions"], float64(0))
		// Equal values can still acquire ownership, changing managedFields/RV.
		external.SetLabels(map[string]string{"equal-policy": "same"})
		data, err := json.Marshal(external)
		assert.NilError(t, err)
		before, err := gateways.Patch(ctx, "skupper", types.ApplyPatchType, data, metav1.PatchOptions{FieldManager: "external-metadata"})
		assert.NilError(t, err)
		assert.NilError(t, bare.m.context.(*testContext).policy.Update("controller/equal-policy", &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "controller", Name: "equal-policy", Labels: map[string]string{"skupper.io/label-template": "true", "equal-policy": "same"}}, Data: map[string]string{"kind": "Gateway"}}))
		bare.observe()
		output.Reset()
		assert.NilError(t, bare.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}))
		after, err := gateways.Get(ctx, "skupper", metav1.GetOptions{})
		assert.NilError(t, err)
		assert.Assert(t, before.GetResourceVersion() != after.GetResourceVersion())
		assert.DeepEqual(t, before.Object["spec"], after.Object["spec"])
		assert.DeepEqual(t, before.GetLabels(), after.GetLabels())
		records = logs()
		assert.Equal(t, records[0]["msg"], "Applied Gateway controller/skupper")
		assert.Equal(t, records[0]["level"], "INFO")
		assert.Equal(t, records[1]["changed_actions"], float64(1))
		assert.Equal(t, records[1]["unchanged_actions"], float64(0))
		assertUnchanged(t, bare, 0, func() error { return bare.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}) })
	})
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
	_, err = h.m.applyOutput(accessAction{verb: "create", kind: "Gateway", object: desired})
	assert.Assert(t, apierrors.IsInvalid(err))
	untouched, err := clients.Dynamic.Resource(resource.GatewayResource()).Namespace("controller").Get(ctx, "occupied", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.Equal(t, untouched.GetAnnotations()[gatewayOwner], "unrelated-controller")
	t.Run("CRD create guards reject existing objects even with equal values", func(t *testing.T) {
		for _, kind := range []string{"Gateway", "TLSRoute"} {
			for i, manager := range []string{"skupper-controller", "external-controller"} {
				gvr := resource.GatewayResource()
				configuration := desiredGateway("test", "skupper-controller", testConfig())
				if kind == "TLSRoute" {
					gvr = resource.TlsRouteResource()
					sa := testAccess("gateway")
					configuration = desiredTLSRoute(sa, sa.Spec.Ports[0], "controller", "gateway.example")
				}
				configuration.SetName(fmt.Sprintf("create-guard-%d", i))
				client := clients.Dynamic.Resource(gvr).Namespace("test")
				data, err := json.Marshal(configuration)
				assert.NilError(t, err)
				_, err = client.Patch(ctx, configuration.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{FieldManager: manager})
				assert.NilError(t, err)
				t.Cleanup(func() {
					assert.NilError(t, client.Delete(ctx, configuration.GetName(), metav1.DeleteOptions{}))
				})
				before, err := client.Get(ctx, configuration.GetName(), metav1.GetOptions{})
				assert.NilError(t, err)
				// Equal values avoid an SSA ownership conflict hiding a broken
				// create guard. New metadata tests a write without such a conflict.
				for _, changed := range []bool{false, true} {
					desired := configuration.DeepCopy()
					if changed {
						annotations := desired.GetAnnotations()
						annotations["example.com/create-guard"] = "must-not-be-written"
						desired.SetAnnotations(annotations)
					}
					_, err := h.m.applyOutput(accessAction{verb: "create", kind: kind, object: desired})
					assert.Assert(t, apierrors.IsInvalid(err), "%s manager=%s changed=%t: %v", kind, manager, changed, err)
					assert.ErrorContains(t, err, "metadata.resourceVersion")
					t.Logf("%s manager=%s added-metadata=%t rejected: %v", kind, manager, changed, err)
					after, err := client.Get(ctx, configuration.GetName(), metav1.GetOptions{})
					assert.NilError(t, err)
					assert.DeepEqual(t, after, before)
				}
			}
		}
	})
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
		// Cold caches or changed revisions require one successful evaluation.
		h.observe()
		assert.NilError(t, h.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}))
		for _, name := range []string{"nodeport", "ingress", "gateway"} {
			assert.NilError(t, h.run("test/"+name))
		}
		for i := 0; i < 4; i++ {
			assertUnchanged(t, h, 0, func() error {
				for _, name := range []string{"nodeport", "ingress", "gateway"} {
					assert.NilError(t, h.run("test/"+name))
					current := h.status(name)
					assert.NilError(t, h.m.Ensure("test", name, current.Spec, nil, current.OwnerReferences))
				}
				assert.NilError(t, h.runCertificates())
				return h.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"})
			})
		}
	}
	assertSettled(h)
	h = newHarness(t, clients)
	h.m.context.(*testContext).policy = policy.Snapshot("test")
	h.observe()
	assertSettled(h)
	assert.Equal(t, h.status("ingress").Labels["priority"], "final")
	t.Log("defaulted Service/Ingress/TLSRoute/Gateway: stable stored revisions across repeated SSA and restart")
	t.Run("TLSRoute success receipts follow status revisions and cache lifetime", func(t *testing.T) {
		routes := clients.Dynamic.Resource(resource.TlsRouteResource()).Namespace("test")
		run := func() error { return h.run("test/gateway") }
		for i := range 2 {
			patch := []byte(fmt.Sprintf(`{"status":{"parents":[{"parentRef":{"name":"skupper","namespace":"controller"},"controllerName":"example.com/test-controller","conditions":[{"type":"Accepted","status":"True","reason":"Accepted","message":"status-%d","observedGeneration":1,"lastTransitionTime":"2026-01-01T00:00:00Z"}]}]}}`, i))
			_, err := routes.Patch(ctx, "gateway-alpha", types.MergePatchType, patch, metav1.PatchOptions{}, "status")
			assert.NilError(t, err)
			h.observe()
			before := writes.Load()
			assert.NilError(t, run())
			assert.Equal(t, writes.Load()-before, int64(1), "only the revised TLSRoute must reapply")
			assertUnchanged(t, h, 0, run)
		}
		h.m.applyCache.Clear()
		assertUnchanged(t, h, 2, run)
		assertUnchanged(t, h, 0, run)
		fresh := newHarness(t, clients)
		fresh.m.context.(*testContext).policy = h.m.context.(*testContext).policy.Snapshot("test")
		assertUnchanged(t, fresh, 2, func() error { return fresh.run("test/gateway") })
		assertUnchanged(t, fresh, 0, func() error { return fresh.run("test/gateway") })
	})
	policy = h.m.context.(*testContext).policy
	for _, kind := range []string{"Gateway", "TLSRoute"} {
		t.Run(kind+" external metadata", func(t *testing.T) {
			apiVersion, namespace, name, templateKind := "gateway.networking.k8s.io/v1", "controller", "skupper", "Gateway"
			gvr := resource.GatewayResource()
			requests := int64(1)
			run := func() error { return h.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}) }
			if kind == "TLSRoute" {
				apiVersion, namespace, name, templateKind = "gateway.networking.k8s.io/v1alpha2", "test", "gateway-alpha", "TlsRoute"
				gvr = resource.TlsRouteResource()
				run = func() error { return h.run("test/gateway") }
			}
			objects := clients.Dynamic.Resource(gvr).Namespace(namespace)
			external := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": apiVersion, "kind": kind}}
			external.SetName(name)
			external.SetNamespace(namespace)
			external.SetLabels(map[string]string{"external": "first", "select": "enabled", "equal-policy-value": "retained"})
			external.SetAnnotations(map[string]string{"external": "first"})
			if kind == "Gateway" {
				external.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "external-owner", UID: "external-owner-uid"}})
			}
			applyExternal := func() {
				data, err := json.Marshal(external)
				assert.NilError(t, err)
				_, err = objects.Patch(ctx, name, types.ApplyPatchType, data, metav1.PatchOptions{FieldManager: "external-metadata"})
				assert.NilError(t, err, "external metadata must remain independently writable without force")
			}
			applyExternal()
			assertUnchanged(t, h, requests, run)
			// Policy selectors must see external labels and earlier template
			// outputs. Equal-valued keys are authored too, not just value diffs.
			for _, cm := range []*corev1.ConfigMap{
				{ObjectMeta: metav1.ObjectMeta{Name: "metadata-a", Namespace: "controller", Labels: map[string]string{"skupper.io/label-template": "true", "policy-stage": "first", "equal-policy-value": "retained"}}, Data: map[string]string{"kind": templateKind, "name": name, "labelSelector": "select=enabled"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "metadata-b", Namespace: "controller", Annotations: map[string]string{"policy-output": "retained"}, Labels: map[string]string{"skupper.io/label-template": "true"}}, Data: map[string]string{"kind": templateKind, "name": name, "labelSelector": "policy-stage=first"}},
			} {
				assert.NilError(t, policy.Update("controller/"+cm.Name, cm))
			}
			h.observe()
			assert.NilError(t, run())
			actual, err := objects.Get(ctx, name, metav1.GetOptions{})
			assert.NilError(t, err)
			assert.Equal(t, actual.GetLabels()["policy-stage"], "first")
			assert.Equal(t, actual.GetAnnotations()["policy-output"], "retained")
			external.SetLabels(map[string]string{"external": "second", "select": "enabled"})
			external.SetAnnotations(map[string]string{"external": "second"})
			if kind == "Gateway" {
				refs := external.GetOwnerReferences()
				refs[0].Name = "external-owner-updated"
				external.SetOwnerReferences(refs)
			}
			applyExternal()
			assert.NilError(t, policy.Update("controller/metadata-a", nil))
			assert.NilError(t, policy.Update("controller/metadata-b", nil))
			assertUnchanged(t, h, requests, run)
			if kind == "Gateway" {
				h.m.config.GatewayPort = 8443
			} else {
				sa := h.status("gateway")
				sa.Spec.Ports[0].Port = 8082
				_, err := clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Update(ctx, sa, metav1.UpdateOptions{})
				assert.NilError(t, err)
			}
			h.observe()
			assert.NilError(t, run())
			external.SetLabels(nil)
			external.SetAnnotations(nil)
			external.SetOwnerReferences(nil)
			applyExternal()
			actual, err = objects.Get(ctx, name, metav1.GetOptions{})
			assert.NilError(t, err)
			assert.Equal(t, actual.GetLabels()["external"], "")
			assert.Equal(t, actual.GetAnnotations()["external"], "")
			assert.Equal(t, actual.GetLabels()["policy-stage"], "first")
			assert.Equal(t, actual.GetLabels()["equal-policy-value"], "retained")
			assert.Equal(t, actual.GetAnnotations()["policy-output"], "retained")
			if kind == "Gateway" {
				assert.Equal(t, len(actual.GetOwnerReferences()), 0)
				h.m.config.GatewayPort = 7443
			}
			h.observe()
			assert.NilError(t, run())
			assertUnchanged(t, h, 0, run)
		})
	}
	t.Run("stale SSA plans cannot be reported unchanged", func(t *testing.T) {
		for _, kind := range []string{"Gateway", "TLSRoute"} {
			h.observe()
			var actions []accessAction
			if kind == "Gateway" {
				gateway, err := gateways.Get(ctx, "skupper", metav1.GetOptions{})
				assert.NilError(t, err)
				actions, err = planGateway(gateway, desiredGateway("controller", "skupper-controller", h.m.config), policy)
				assert.NilError(t, err)
			} else {
				snapshot, err := h.m.snapshot("test/gateway")
				assert.NilError(t, err)
				actions = planAccess(snapshot).actions
			}
			tested := false
			for _, action := range actions {
				if action.kind != kind {
					continue
				}
				gvr := resource.GatewayResource()
				if kind == "TLSRoute" {
					gvr = resource.TlsRouteResource()
				}
				object := action.object.(*unstructured.Unstructured)
				_, err := clients.Dynamic.Resource(gvr).Namespace(object.GetNamespace()).Patch(ctx, object.GetName(), types.MergePatchType, []byte(`{"metadata":{"annotations":{"external-race":"changed"}}}`), metav1.PatchOptions{})
				assert.NilError(t, err)
				_, err = h.m.applyOutput(action)
				assert.Assert(t, apierrors.IsConflict(err))
				tested = true
				break
			}
			assert.Assert(t, tested, "expected a planned %s apply", kind)
		}
	})
	// A separate SSA manager may add a disjoint listener. It is neither drift
	// to repair nor part of our apply payload, even when our listener changes.
	extraListener := []byte(`{"apiVersion":"gateway.networking.k8s.io/v1","kind":"Gateway","metadata":{"name":"skupper","namespace":"controller"},"spec":{"listeners":[{"name":"extra","protocol":"HTTP","port":8080}]}}`)
	_, err = gateways.Patch(ctx, "skupper", types.ApplyPatchType, extraListener, metav1.PatchOptions{FieldManager: "external-controller"})
	assert.NilError(t, err)
	assertUnchanged(t, h, 1, func() error { return h.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}) })
	for i := 0; i < 4; i++ {
		assertUnchanged(t, h, 0, func() error { return h.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}) })
	}
	h.m.config.GatewayPort = 8443
	h.observe()
	assert.NilError(t, h.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}))
	withExtra, err := gateways.Get(ctx, "skupper", metav1.GetOptions{})
	assert.NilError(t, err)
	withExtraListeners, _, err := unstructured.NestedSlice(withExtra.Object, "spec", "listeners")
	assert.NilError(t, err)
	assert.Equal(t, len(withExtraListeners), 2)
	for _, listener := range withExtraListeners {
		l := listener.(map[string]interface{})
		if l["name"] == "tls" {
			assert.Equal(t, l["port"], int64(8443))
		} else {
			assert.Equal(t, l["name"], "extra")
			assert.Equal(t, l["port"], int64(8080))
		}
	}
	// Changing the external listener without force proves our real update did
	// not accidentally acquire its fields through a merged apply payload.
	extraListener = []byte(`{"apiVersion":"gateway.networking.k8s.io/v1","kind":"Gateway","metadata":{"name":"skupper","namespace":"controller"},"spec":{"listeners":[{"name":"extra","protocol":"HTTP","port":8081}]}}`)
	_, err = gateways.Patch(ctx, "skupper", types.ApplyPatchType, extraListener, metav1.PatchOptions{FieldManager: "external-controller"})
	assert.NilError(t, err)
	h.m.config.GatewayPort = 7443
	h.observe()
	assert.NilError(t, h.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}))
	h = newHarness(t, clients)
	h.m.context.(*testContext).policy = policy.Snapshot("test")
	h.observe()
	assertSettled(h)
	// The other manager can remove its listener; no deletion or replacement of
	// the Gateway is required to regain the original single-listener state.
	_, err = gateways.Patch(ctx, "skupper", types.ApplyPatchType, []byte(`{"apiVersion":"gateway.networking.k8s.io/v1","kind":"Gateway","metadata":{"name":"skupper","namespace":"controller"}}`), metav1.PatchOptions{FieldManager: "external-controller"})
	assert.NilError(t, err)
	h.observe()
	assertSettled(h)
	t.Log("independently owned Gateway listener: stable revisions across repeated SSA/restart, no ownership acquisition during updates")
	// A stale deletion cannot remove a different object at the same name.
	services := clients.Kube.CoreV1().Services("test")
	old, err := services.Create(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "delete-race"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8080}}}}, metav1.CreateOptions{})
	assert.NilError(t, err)
	assert.NilError(t, services.Delete(ctx, old.Name, metav1.DeleteOptions{}))
	replacement, err := services.Create(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: old.Name}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 9090}}}}, metav1.CreateOptions{})
	assert.NilError(t, err)
	_, err = h.m.executeAction(accessAction{verb: "delete", kind: "Service", object: old})
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
	for range 2 {
		h.observe()
		err = h.run("test/gateway")
		assert.Assert(t, apierrors.IsConflict(err), "failed TLSRoute applies must remain retryable")
		assert.Assert(t, !h.status("gateway").IsReady())
	}
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
	h.observe()
	assertSettled(h)
	// Non-forcing SSA applies to the singleton too, not only its TLSRoutes.
	patch = []byte(`{"apiVersion":"gateway.networking.k8s.io/v1","kind":"Gateway","metadata":{"name":"skupper","namespace":"controller"},"spec":{"listeners":[{"name":"tls","port":8555}]}}`)
	_, err = gateways.Patch(ctx, "skupper", types.ApplyPatchType, patch, metav1.PatchOptions{FieldManager: "external-controller", Force: &force})
	assert.NilError(t, err)
	h.observe()
	beforeFailedApplies := gatewayPatches.Load()
	for range 2 {
		err = h.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"})
		assert.Assert(t, apierrors.IsConflict(err))
	}
	assert.Equal(t, gatewayPatches.Load()-beforeFailedApplies, int64(2), "failed applies must never establish a receipt")
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
	_, err = h.m.executeAction(*certificateAction)
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

	t.Run("schema changes need no controller discovery or restart", func(t *testing.T) {
		h := newHarness(t, clients)
		h.observe()
		assert.NilError(t, h.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}))
		sa := testAccess("gateway")
		sa.Name, sa.UID = "schema-access", ""
		h.put(sa)
		h.settle(sa.Name)
		definitions := clients.Dynamic.Resource(schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"})
		for _, gvr := range []schema.GroupVersionResource{resource.GatewayResource(), resource.TlsRouteResource()} {
			definition, err := definitions.Get(ctx, gvr.Resource+"."+gvr.Group, metav1.GetOptions{})
			assert.NilError(t, err)
			versions, _, err := unstructured.NestedSlice(definition.Object, "spec", "versions")
			assert.NilError(t, err)
			for _, v := range versions {
				version := v.(map[string]interface{})
				assert.NilError(t, unstructured.SetNestedMap(version, map[string]interface{}{"type": "string"}, "schema", "openAPIV3Schema", "properties", "spec", "properties", "reviewExtension"))
			}
			assert.NilError(t, unstructured.SetNestedSlice(definition.Object, versions, "spec", "versions"))
			_, err = definitions.Update(ctx, definition, metav1.UpdateOptions{})
			assert.NilError(t, err)
			namespace, name := "controller", "skupper"
			if gvr.Resource == "tlsroutes" {
				namespace, name = "test", "schema-access-alpha"
			}
			objects := clients.Dynamic.Resource(gvr).Namespace(namespace)
			// Wait only for the API to accept/store the new field. The controller
			// does not discover or cache OpenAPI schemas.
			assert.NilError(t, wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(context.Context) (bool, error) {
				object, err := objects.Patch(ctx, name, types.MergePatchType, []byte(`{"spec":{"reviewExtension":"external"}}`), metav1.PatchOptions{})
				if err != nil {
					return false, err
				}
				value, _, _ := unstructured.NestedString(object.Object, "spec", "reviewExtension")
				return value == "external", nil
			}))
		}
		h.m.config.GatewayPort = 9443
		sa = h.status(sa.Name)
		sa.Spec.Ports[0].Port = 8188
		_, err := clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Update(ctx, sa, metav1.UpdateOptions{})
		assert.NilError(t, err)
		h.observe()
		assert.NilError(t, h.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}))
		assert.NilError(t, h.run("test/schema-access"))
		assert.Assert(t, h.status(sa.Name).IsReady())
		gateway, err := clients.Dynamic.Resource(resource.GatewayResource()).Namespace("controller").Get(ctx, "skupper", metav1.GetOptions{})
		assert.NilError(t, err)
		listeners, _, _ := unstructured.NestedSlice(gateway.Object, "spec", "listeners")
		assert.Equal(t, listeners[0].(map[string]interface{})["port"], int64(9443))
		route, err := clients.Dynamic.Resource(resource.TlsRouteResource()).Namespace("test").Get(ctx, "schema-access-alpha", metav1.GetOptions{})
		assert.NilError(t, err)
		rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
		backends := rules[0].(map[string]interface{})["backendRefs"].([]interface{})
		assert.Equal(t, backends[0].(map[string]interface{})["port"], int64(8188))
		for _, object := range []*unstructured.Unstructured{gateway, route} {
			value, _, _ := unstructured.NestedString(object.Object, "spec", "reviewExtension")
			assert.Equal(t, value, "external")
		}
		for i := 0; i < 3; i++ {
			assertUnchanged(t, h, 0, func() error {
				assert.NilError(t, h.m.gateway.Handle(watchers.ResourceChange{Key: "controller/skupper"}))
				return h.run("test/schema-access")
			})
		}
	})
	t.Run("real informers converge between periodic resyncs", func(t *testing.T) {
		m := NewSecuredAccessManager(clients, ptrConfig(testConfig()), &testContext{policy: labels.NewLabelsAndAnnotations("controller")})
		w := NewSecuredAccessResourceWatcher(m)
		p := watchers.NewEventProcessor("AccessSSAIntegration", clients)
		p.SetResync(2 * time.Second)
		p.SetResyncShort(2 * time.Second)
		w.WatchResources(p, "")
		var accessCallbacks atomic.Int64
		w.WatchSecuredAccesses(p, "", func(key string, _ *skupperv2alpha1.SecuredAccess) error {
			if key == "test/watch-access" {
				accessCallbacks.Add(1)
			}
			return nil
		})
		w.WatchGateway(p, "controller")
		stop := make(chan struct{})
		t.Cleanup(func() { close(stop); p.Stop() })
		p.StartWatchers(stop)
		assert.Assert(t, p.WaitForCacheSync(stop))
		// The earlier "occupied" Gateway exists in this namespace too. It must
		// not enter the shared-Gateway watch or wake either scope.
		gateways := w.gatewayWatcher.List()
		assert.Equal(t, len(gateways), 1)
		assert.Equal(t, gateways[0].GetName(), "skupper")
		p.Start(stop)
		sa := testAccess("gateway")
		sa.Name, sa.UID = "watch-access", ""
		_, err := clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Create(ctx, sa, metav1.CreateOptions{})
		assert.NilError(t, err)
		assert.NilError(t, wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 30*time.Second, true, func(context.Context) (bool, error) {
			current, err := clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Get(ctx, sa.Name, metav1.GetOptions{})
			return err == nil && current.IsReady(), err
		}))
		// No manual Handle/observe calls: API watch events and real informer
		// resyncs must drain rather than continuously feeding their own writes.
		quiet := func(after int64) int64 {
			last, changed := writes.Load(), time.Now()
			assert.NilError(t, wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 30*time.Second, true, func(context.Context) (bool, error) {
				now := writes.Load()
				if now != last {
					last, changed = now, time.Now()
				}
				return now > after && time.Since(changed) >= 500*time.Millisecond, nil
			}))
			return last
		}
		last := quiet(-1)
		versions := func() map[string]string {
			result := map[string]string{}
			gateway, err := clients.Dynamic.Resource(resource.GatewayResource()).Namespace("controller").Get(ctx, "skupper", metav1.GetOptions{})
			assert.NilError(t, err)
			result["Gateway"] = gateway.GetResourceVersion()
			routes, err := clients.Dynamic.Resource(resource.TlsRouteResource()).Namespace("test").List(ctx, metav1.ListOptions{})
			assert.NilError(t, err)
			for _, route := range routes.Items {
				result[route.GetName()] = route.GetResourceVersion()
			}
			current, err := clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Get(ctx, sa.Name, metav1.GetOptions{})
			assert.NilError(t, err)
			assert.Assert(t, current.IsReady())
			result["SecuredAccess"] = current.ResourceVersion
			return result
		}
		before := versions()
		beforeGatewayPatches := gatewayPatches.Load()
		for i := 0; i < 3; i++ {
			previousCallbacks := accessCallbacks.Load()
			assert.NilError(t, wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true, func(context.Context) (bool, error) {
				return accessCallbacks.Load() > previousCallbacks, nil
			}))
			next := quiet(-1) // Wait for queued scopes without requiring a write.
			assert.Equal(t, next, last, "settled resync must make zero mutation requests")
			t.Logf("periodic resync %d: zero mutation requests, followed by a quiet interval", i+1)
			assert.DeepEqual(t, versions(), before)
			assert.Equal(t, gatewayPatches.Load(), beforeGatewayPatches, "settled Gateway resyncs must use success receipts")
		}
		// Deletion of the shared Gateway itself must still recreate it without
		// an Access or output event to wake its scope.
		gatewayClient := clients.Dynamic.Resource(resource.GatewayResource()).Namespace("controller")
		original, err := gatewayClient.Get(ctx, "skupper", metav1.GetOptions{})
		assert.NilError(t, err)
		assert.NilError(t, gatewayClient.Delete(ctx, "skupper", metav1.DeleteOptions{}))
		assert.NilError(t, wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 30*time.Second, true, func(context.Context) (bool, error) {
			current, err := gatewayClient.Get(ctx, "skupper", metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return err == nil && current.GetUID() != original.GetUID() && current.GetAnnotations()[gatewayOwner] == m.context.Name(), err
		}))
		quiet(-1) // Drain recreation's dependent Access work before API teardown.
	})
	assert.Equal(t, openAPIReads.Load(), int64(0), "reconciliation must not require OpenAPI discovery")
}
