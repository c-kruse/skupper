package securedaccess

import (
	"fmt"
	"time"

	"github.com/skupperproject/skupper/internal/kube/site/labels"
	"github.com/skupperproject/skupper/internal/kube/watchers"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"
)

const gatewayOwner = "internal.skupper.io/gateway-controller"

// gatewayScope is the sole shared-Gateway writer. Access scopes only consume
// its cached status; no remembered domain or pending-Access list is needed.
type gatewayScope struct{ manager *SecuredAccessManager }

func (g *gatewayScope) Kind() string                              { return "GatewayScope" }
func (g *gatewayScope) Describe(e watchers.ResourceChange) string { return g.Kind() + " " + e.Key }
func (g *gatewayScope) Handle(e watchers.ResourceChange) (err error) {
	m := g.manager
	phase := "key"
	var results []accessResult
	defer func() { m.logResults("Gateway", e.Key, phase, results, err) }()
	ns, name, err := cache.SplitMetaNamespaceKey(e.Key)
	if err != nil || !m.isControlled(ns) {
		return err
	}
	if !m.config.isEnabled(ACCESS_TYPE_GATEWAY) || m.context == nil || ns != m.context.Namespace() || name != "skupper" {
		return nil
	}
	phase = "snapshot"
	if m.watcher.Gateways == nil {
		return fmt.Errorf("Gateway API is unavailable")
	}
	observed, err := m.watcher.Gateways.Get(e.Key)
	if err != nil {
		return err
	}
	observed = observed.DeepCopy()
	desired := desiredGateway(ns, m.context.Name(), m.config)
	policy := m.context.SnapshotMetadata(ns)
	phase = "plan"
	actions, err := planGateway(observed, desired, policy)
	if err != nil {
		return err
	}
	phase = "execute"
	for _, a := range actions {
		start := time.Now()
		var unchanged bool
		unchanged, err = m.executeAction(a)
		results = append(results, accessResult{verb: a.verb, kind: a.kind, namespace: ns, name: name, duration: time.Since(start), err: err, unchanged: unchanged})
		if err != nil {
			break
		}
	}
	return err
}

func planGateway(observed, desired *unstructured.Unstructured, policy *labels.LabelsAndAnnotations) ([]accessAction, error) {
	authority := desired.GetAnnotations()[gatewayOwner]
	if observed != nil {
		// Old Gateways had no durable owner metadata. Matching name/spec or an
		// SSA manager is not adoption authority: require explicit operator opt-in.
		if observed.GetAnnotations()[gatewayOwner] != desired.GetAnnotations()[gatewayOwner] {
			return nil, fmt.Errorf("Gateway %q has conflicting ownership; legacy adoption requires %s=%s", observed.GetNamespace()+"/"+observed.GetName(), gatewayOwner, desired.GetAnnotations()[gatewayOwner])
		}
		if observed.GetDeletionTimestamp() != nil {
			return nil, nil
		}
	}
	configuration, err := applyConfiguration(observed, desired, policy, "Gateway")
	if err != nil {
		return nil, err
	}
	annotations := configuration.GetAnnotations()
	annotations[gatewayOwner] = authority
	configuration.SetAnnotations(annotations)
	verb := "create"
	if observed != nil {
		verb = "update"
	}
	return []accessAction{{verb: verb, kind: "Gateway", object: configuration}}, nil
}

func desiredGateway(namespace, controller string, c Config) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway",
		"metadata": map[string]interface{}{"name": "skupper", "namespace": namespace, "annotations": map[string]interface{}{gatewayOwner: controller}},
		"spec":     map[string]interface{}{"gatewayClassName": c.GatewayClass, "listeners": []interface{}{map[string]interface{}{"name": "tls", "protocol": "TLS", "port": int64(c.GatewayPort), "tls": map[string]interface{}{"mode": "Passthrough"}, "allowedRoutes": map[string]interface{}{"namespaces": map[string]interface{}{"from": "All"}}}}},
	}}
}

func desiredTLSRoute(sa *skupperv2alpha1.SecuredAccess, p skupperv2alpha1.SecuredAccessPort, gatewayNamespace, domain string) *unstructured.Unstructured {
	name := sa.Name + "-" + p.Name
	d := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "gateway.networking.k8s.io/v1alpha2", "kind": "TLSRoute",
		"spec": map[string]interface{}{
			"parentRefs": []interface{}{map[string]interface{}{"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": "skupper", "namespace": gatewayNamespace, "sectionName": "tls"}},
			"hostnames":  []interface{}{qualify(name+"."+sa.Namespace, domain)},
			"rules":      []interface{}{map[string]interface{}{"backendRefs": []interface{}{map[string]interface{}{"group": "", "kind": "Service", "name": sa.Name, "namespace": sa.Namespace, "port": int64(p.Port), "weight": int64(1)}}}},
		},
	}}
	d.SetName(name)
	d.SetNamespace(sa.Namespace)
	d.SetOwnerReferences(ownerReferences(sa))
	d.SetLabels(map[string]string{accessLabel: "true"})
	d.SetAnnotations(map[string]string{"internal.skupper.io/controlled": "true"})
	return d
}

func getBaseDomain(obj *unstructured.Unstructured) string {
	addresses, _, _ := unstructured.NestedSlice(obj.Object, "status", "addresses")
	for _, kind := range []string{"Hostname", "IPAddress"} {
		for _, a := range addresses {
			if address, ok := a.(map[string]interface{}); ok {
				value, _, _ := unstructured.NestedString(address, "value")
				t, _, _ := unstructured.NestedString(address, "type")
				if value != "" && t == kind {
					if kind == "IPAddress" {
						return value + ".nip.io"
					}
					return value
				}
			}
		}
	}
	return ""
}
