package securedaccess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	routev1 "github.com/openshift/api/route/v1"
	"github.com/skupperproject/skupper/internal/kube/resource"
	"github.com/skupperproject/skupper/internal/kube/site/labels"
	"github.com/skupperproject/skupper/internal/kube/watchers"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
)

const accessLabel = "internal.skupper.io/secured-access"

type accessSnapshot struct {
	namespace, name    string
	access             *skupperv2alpha1.SecuredAccess
	service            *corev1.Service
	ingress            *networkingv1.Ingress
	routes             map[string]*routev1.Route
	proxies, tlsRoutes map[string]*unstructured.Unstructured
	certificate        accessCertificateSnapshot
	policy             *labels.LabelsAndAnnotations
	config             Config
	accessType         string
	routeAvailable     bool
	gateway            *unstructured.Unstructured
	gatewayNamespace   string
	gatewayController  string
}

// Actions contain final objects, never closures over mutable manager state.
// Status timestamps and API results are execution facts, not desired state.
type accessAction struct {
	verb, kind string
	object     runtime.Object
}
type accessPlan struct {
	actions       []accessAction
	access        *skupperv2alpha1.SecuredAccess
	endpoints     []skupperv2alpha1.Endpoint
	validationErr error
}
type accessResult struct {
	verb, kind, namespace, name string
	duration                    time.Duration
	err                         error
	conditions                  []metav1.Condition
}

func (m *SecuredAccessManager) Kind() string { return "AccessScope" }
func (m *SecuredAccessManager) Describe(e watchers.ResourceChange) string {
	return m.Kind() + " " + e.Key
}
func (m *SecuredAccessManager) Handle(e watchers.ResourceChange) (err error) {
	phase := "key"
	var results []accessResult
	defer func() { m.logResults("SecuredAccess", e.Key, phase, results, err) }()
	ns, _, err := cache.SplitMetaNamespaceKey(e.Key)
	if err != nil || !m.isControlled(ns) {
		return err
	}
	phase = "snapshot"
	s, err := m.snapshot(e.Key)
	if err != nil {
		return err
	}
	phase = "plan"
	p := planAccess(s)
	phase = "execute"
	results, err = m.executeAccess(p)
	return err
}

func (m *SecuredAccessManager) snapshot(key string) (accessSnapshot, error) {
	ns, name, err := cache.SplitMetaNamespaceKey(key)
	s := accessSnapshot{namespace: ns, name: name, config: m.config, routes: map[string]*routev1.Route{}, proxies: map[string]*unstructured.Unstructured{}, tlsRoutes: map[string]*unstructured.Unstructured{}}
	if err != nil {
		return s, err
	}
	w := m.watcher
	s.routeAvailable = w.routeWatcher != nil
	sa, err := w.securedAccessWatcher.Get(key)
	if err != nil {
		return s, err
	}
	s.access = sa.DeepCopy()
	if sa != nil {
		s.accessType = m.actualAccessType(sa)
		if target := certificateTarget(sa); target != "" {
			s.certificate, err = m.certificates.snapshot(ns + "/" + target)
			if err != nil {
				return s, err
			}
		}
	}
	// Every informer pointer crossing this boundary is copied, including metadata.
	for _, o := range w.inventory() {
		if o.GetNamespace() != ns {
			continue
		}
		switch o := o.(type) {
		case *corev1.Service:
			if o.Name == name {
				s.service = o.DeepCopy()
			}
		case *networkingv1.Ingress:
			if o.Name == name {
				s.ingress = o.DeepCopy()
			}
		case *routev1.Route:
			s.routes[o.Name] = o.DeepCopy()
		case *unstructured.Unstructured:
			if o.GetKind() == "HTTPProxy" {
				s.proxies[o.GetName()] = o.DeepCopy()
			} else if o.GetKind() == "TLSRoute" {
				s.tlsRoutes[o.GetName()] = o.DeepCopy()
			}
		}
	}
	if m.context != nil {
		s.policy = m.context.SnapshotMetadata(ns)
		s.gatewayNamespace = m.context.Namespace()
		s.gatewayController = m.context.Name()
	}
	if w.gatewayWatcher != nil {
		g, err := w.gatewayWatcher.Get(s.gatewayNamespace + "/skupper")
		if err != nil {
			return s, err
		}
		s.gateway = g.DeepCopy()
	}
	return s, nil
}

func exposureMetadata(sa *skupperv2alpha1.SecuredAccess, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: sa.Namespace, Name: name, OwnerReferences: ownerReferences(sa), Labels: map[string]string{accessLabel: "true"}, Annotations: map[string]string{"internal.skupper.io/controlled": "true"}}
}
func accessReference(ref metav1.OwnerReference) bool {
	return ref.APIVersion == "skupper.io/v2alpha1" && ref.Kind == "SecuredAccess"
}
func ownedByAccess(obj metav1.Object, name string, uid types.UID) bool {
	refs := obj.GetOwnerReferences()
	// Mixed owners are ambiguous for exclusive provider outputs. Certificate
	// contributions have a separate, deliberately multi-owner contract below.
	return len(refs) == 1 && accessReference(refs[0]) && refs[0].Name == name && refs[0].UID == uid && uid != ""
}
func outputForScope(obj metav1.Object, name string) bool {
	refs := obj.GetOwnerReferences()
	return len(refs) == 1 && accessReference(refs[0]) && refs[0].Name == name && refs[0].UID != ""
}

func applyPolicy(policy *labels.LabelsAndAnnotations, kind string, obj metav1.Object) {
	if policy == nil {
		return
	}
	// Preserve the spelling used by existing TLSRoute metadata templates.
	if kind == "TLSRoute" {
		kind = "TlsRoute"
	}
	metadata := metav1.ObjectMeta{Labels: obj.GetLabels(), Annotations: obj.GetAnnotations()}
	policy.SetObjectMetadata(obj.GetNamespace(), obj.GetName(), kind, &metadata)
	if len(metadata.Labels) > 0 || obj.GetLabels() != nil {
		obj.SetLabels(metadata.Labels)
	}
	if len(metadata.Annotations) > 0 || obj.GetAnnotations() != nil {
		obj.SetAnnotations(metadata.Annotations)
	}
}

// planOutput guards ownership before deriving a mutation. Metadata is evaluated
// from the observed object so selectors chain, then compared in its final form.
func (p *accessPlan) planOutput(s accessSnapshot, kind string, observed, desired runtime.Object) {
	dm, _ := meta.Accessor(desired)
	if observed != nil && !reflect.ValueOf(observed).IsNil() {
		om, _ := meta.Accessor(observed)
		if !ownedByAccess(om, s.name, s.access.UID) {
			p.validationErr = errors.Join(p.validationErr, fmt.Errorf("%s %q has conflicting ownership", kind, dm.GetName()))
			return
		}
		if om.GetDeletionTimestamp() != nil {
			return
		}
		// Preserve external metadata and API identity; only built-ins and policy
		// values belong to this writer. Removing a template retains old values,
		// matching the existing metadata registry contract.
		merged := observed.DeepCopyObject()
		mm, _ := meta.Accessor(merged)
		ls, as := mm.GetLabels(), mm.GetAnnotations()
		if ls == nil {
			ls = map[string]string{}
		}
		if as == nil {
			as = map[string]string{}
		}
		for k, v := range dm.GetLabels() {
			ls[k] = v
		}
		for k, v := range dm.GetAnnotations() {
			as[k] = v
		}
		mm.SetLabels(ls)
		mm.SetAnnotations(as)
		switch d := desired.(type) {
		case *corev1.Service:
			o := merged.(*corev1.Service)
			o.Spec.Selector = d.Spec.Selector
			o.Spec.Type = d.Spec.Type
			updatePorts(&o.Spec, s.access.Spec.Ports)
			if o.Spec.Type == corev1.ServiceTypeClusterIP {
				for i := range o.Spec.Ports {
					o.Spec.Ports[i].NodePort = 0
				}
				o.Spec.HealthCheckNodePort = 0
				o.Spec.ExternalTrafficPolicy = ""
			}
			if o.Spec.Type != corev1.ServiceTypeLoadBalancer {
				o.Spec.HealthCheckNodePort = 0
				o.Spec.AllocateLoadBalancerNodePorts = nil
				o.Spec.LoadBalancerClass = nil
			}
		case *networkingv1.Ingress:
			merged.(*networkingv1.Ingress).Spec = d.Spec
		case *routev1.Route:
			o := merged.(*routev1.Route)
			if !equivalentRoute(o, d) {
				host := o.Spec.Host
				o.Spec = d.Spec
				if d.Spec.Host == "" {
					o.Spec.Host = host
				}
			}
		case *unstructured.Unstructured:
			merged.(*unstructured.Unstructured).Object["spec"] = runtime.DeepCopyJSONValue(d.Object["spec"])
		}
		applyPolicy(s.policy, kind, mm)
		if !reflect.DeepEqual(observed, merged) {
			p.actions = append(p.actions, accessAction{verb: "update", kind: kind, object: merged})
		}
		return
	}
	applyPolicy(s.policy, kind, dm)
	p.actions = append(p.actions, accessAction{verb: "create", kind: kind, object: desired})
}

func planAccess(s accessSnapshot) accessPlan {
	p := accessPlan{access: s.access.DeepCopy(), validationErr: validateAccessCertificate(s.certificate)}
	sa := s.access
	if sa != nil && sa.DeletionTimestamp != nil {
		return accessPlan{}
	}
	desiredNames := map[string]bool{}
	if sa != nil {
		svc := &corev1.Service{ObjectMeta: exposureMetadata(sa, sa.Name), Spec: corev1.ServiceSpec{Selector: sa.Spec.Selector, Type: serviceType(s.accessType)}}
		updatePorts(&svc.Spec, sa.Spec.Ports)
		p.planOutput(s, "Service", s.service, svc)
		// Endpoints requiring allocations/status wait for ordinary observations.
		if !s.config.isEnabled(s.accessType) || (s.accessType == ACCESS_TYPE_ROUTE && !s.routeAvailable) {
			p.validationErr = errors.Join(p.validationErr, errors.New("unsupported access type"))
		} else {
			switch s.accessType {
			case ACCESS_TYPE_LOCAL:
				for _, port := range sa.Spec.Ports {
					p.endpoints = append(p.endpoints, skupperv2alpha1.Endpoint{Name: port.Name, Host: sa.Name + "." + sa.Namespace, Port: strconv.Itoa(port.Port)})
				}
			case ACCESS_TYPE_NODEPORT:
				if s.service != nil {
					for _, port := range s.service.Spec.Ports {
						if hasPort(sa, port.Name) && port.NodePort != 0 {
							p.endpoints = append(p.endpoints, skupperv2alpha1.Endpoint{Name: port.Name, Host: s.config.ClusterHost, Port: strconv.Itoa(int(port.NodePort))})
						}
					}
				}
			case ACCESS_TYPE_LOADBALANCER:
				if s.service != nil {
					for _, addr := range s.service.Status.LoadBalancer.Ingress {
						host := addr.IP
						if host == "" {
							host = addr.Hostname
						}
						if host == "" {
							continue
						}
						for _, port := range sa.Spec.Ports {
							p.endpoints = append(p.endpoints, skupperv2alpha1.Endpoint{Name: port.Name, Host: host, Port: strconv.Itoa(port.Port)})
						}
					}
				}
			case ACCESS_TYPE_INGRESS, ACCESS_TYPE_INGRESS_NGINX:
				domain := s.config.IngressDomain
				qualified := domain != ""
				if domain == "" && s.ingress != nil {
					domain = deduceDomainForIngressHosts(s.ingress)
					qualified = domain != ""
				}
				domain = qualify(sa.Namespace, domain)
				d := toIngress(domain, sa)
				if class := ingressClassNameForDesiredIngress(s.accessType == ACCESS_TYPE_INGRESS_NGINX, s.config.IngressClassName, sa.Spec.Settings); class != "" {
					d.Spec.IngressClassName = &class
				}
				if s.accessType == ACCESS_TYPE_INGRESS_NGINX {
					addNginxIngressAnnotations(d.Annotations)
				}
				p.planOutput(s, "Ingress", s.ingress, d)
				desiredNames["Ingress/"+d.Name] = true
				if qualified {
					for _, r := range d.Spec.Rules {
						p.endpoints = append(p.endpoints, skupperv2alpha1.Endpoint{Name: prefix(r.Host), Host: r.Host, Port: "443"})
					}
				}
			case ACCESS_TYPE_ROUTE:
				for _, port := range sa.Spec.Ports {
					d := desiredRouteForPort(sa, port)
					p.planOutput(s, "Route", s.routes[d.Name], d)
					desiredNames["Route/"+d.Name] = true
					if o := s.routes[d.Name]; o != nil {
						for _, r := range o.Status.Ingress {
							p.endpoints = append(p.endpoints, skupperv2alpha1.Endpoint{Name: port.Name, Host: r.Host, Port: "443"})
						}
					}
				}
			case ACCESS_TYPE_CONTOUR_HTTP_PROXY:
				for _, proxy := range desiredHttpProxies(qualify(sa.Namespace, s.config.HttpProxyDomain), sa) {
					d := &unstructured.Unstructured{}
					d.SetGroupVersionKind(httpProxyGVK)
					d.SetName(proxy.Name)
					d.SetNamespace(sa.Namespace)
					d.SetOwnerReferences(ownerReferences(sa))
					d.SetLabels(map[string]string{accessLabel: "true"})
					d.SetAnnotations(map[string]string{"internal.skupper.io/controlled": "true"})
					_ = proxy.writeToContourProxy(d)
					p.planOutput(s, "HTTPProxy", s.proxies[proxy.Name], d)
					desiredNames["HTTPProxy/"+proxy.Name] = true
					p.endpoints = append(p.endpoints, extractEndpoint(d))
				}
			case ACCESS_TYPE_GATEWAY:
				for _, port := range sa.Spec.Ports {
					desiredNames["TLSRoute/"+sa.Name+"-"+port.Name] = true
				}
				if s.gateway == nil || s.gateway.GetDeletionTimestamp() != nil {
					p.validationErr = errors.Join(p.validationErr, errors.New("Gateway not yet available"))
					break
				}
				if s.gatewayController == "" || s.gateway.GetAnnotations()[gatewayOwner] != s.gatewayController {
					p.validationErr = errors.Join(p.validationErr, errors.New("Gateway has conflicting ownership"))
					break
				}
				domain := s.config.GatewayDomain
				if domain == "" && s.gateway != nil {
					domain = getBaseDomain(s.gateway)
				}
				if domain == "" {
					p.validationErr = errors.Join(p.validationErr, errors.New("Gateway base domain not yet resolved"))
				} else {
					for _, port := range sa.Spec.Ports {
						d := desiredTLSRoute(sa, port, s.gatewayNamespace, domain)
						p.planOutput(s, "TLSRoute", s.tlsRoutes[d.GetName()], d)
						desiredNames["TLSRoute/"+d.GetName()] = true
						p.endpoints = append(p.endpoints, skupperv2alpha1.Endpoint{Name: port.Name, Host: qualify(d.GetName()+"."+sa.Namespace, domain), Port: strconv.Itoa(s.config.GatewayPort)})
					}
				}
			default:
				p.validationErr = errors.Join(p.validationErr, errors.New("unsupported access type"))
			}
		}
		// Never publish endpoints or certificate hosts of an ownership conflict.
		if p.validationErr != nil {
			p.endpoints = nil
		}
	}
	// Discovery is independent of a definition's existence and resource labels.
	var outputs []accessAction
	if s.service != nil && sa == nil {
		outputs = append(outputs, accessAction{kind: "Service", object: s.service})
	}
	if s.ingress != nil {
		outputs = append(outputs, accessAction{kind: "Ingress", object: s.ingress})
	}
	for _, o := range s.routes {
		outputs = append(outputs, accessAction{kind: "Route", object: o})
	}
	for _, o := range s.proxies {
		outputs = append(outputs, accessAction{kind: "HTTPProxy", object: o})
	}
	for _, o := range s.tlsRoutes {
		outputs = append(outputs, accessAction{kind: "TLSRoute", object: o})
	}
	sort.Slice(outputs, func(i, j int) bool {
		a, _ := meta.Accessor(outputs[i].object)
		b, _ := meta.Accessor(outputs[j].object)
		return outputs[i].kind+"/"+a.GetName() < outputs[j].kind+"/"+b.GetName()
	})
	for _, o := range outputs {
		om, _ := meta.Accessor(o.object)
		if !desiredNames[o.kind+"/"+om.GetName()] && outputForScope(om, s.name) && om.GetDeletionTimestamp() == nil {
			o.verb = "delete"
			p.actions = append(p.actions, o)
		}
	}
	if sa != nil {
		d := sa.DeepCopy()
		applyPolicy(s.policy, "SecuredAccess", d)
		if !reflect.DeepEqual(d, sa) {
			p.actions = append(p.actions, accessAction{verb: "update", kind: "SecuredAccess", object: d})
		}
	}
	return p
}

func (m *SecuredAccessManager) executeAccess(p accessPlan) ([]accessResult, error) {
	var results []accessResult
	var executionErr error
	for _, a := range p.actions {
		start := time.Now()
		err := m.executeAction(a)
		om, _ := meta.Accessor(a.object)
		results = append(results, accessResult{verb: a.verb, kind: a.kind, namespace: om.GetNamespace(), name: om.GetName(), duration: time.Since(start), err: err})
		executionErr = errors.Join(executionErr, err)
		if err != nil {
			break
		} // A failed action invalidates downstream assumptions.
		if sa, ok := a.object.(*skupperv2alpha1.SecuredAccess); ok {
			p.access = sa.DeepCopy()
		}
	}
	if p.access != nil {
		sa := p.access.DeepCopy()
		endpoints := p.endpoints
		if executionErr != nil {
			endpoints = nil
		}
		changed := sa.SetResolved(endpoints)
		changed = sa.SetConfigured(errors.Join(p.validationErr, executionErr)) || changed
		if changed {
			start := time.Now()
			written, err := m.clients.GetSkupperClient().SkupperV2alpha1().SecuredAccesses(sa.Namespace).UpdateStatus(context.Background(), sa, metav1.UpdateOptions{})
			result := accessResult{verb: "status", kind: "SecuredAccess", namespace: sa.Namespace, name: sa.Name, duration: time.Since(start), err: err}
			if err == nil {
				result.conditions = append([]metav1.Condition(nil), written.Status.Conditions...)
			}
			results = append(results, result)
			executionErr = errors.Join(executionErr, err)
		}
	}
	return results, executionErr
}

func (m *SecuredAccessManager) executeAction(a accessAction) error {
	ctx := context.Background()
	om, _ := meta.Accessor(a.object)
	ns, name := om.GetNamespace(), om.GetName()
	uid, rv := om.GetUID(), om.GetResourceVersion()
	del := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}
	var err error
	switch o := a.object.(type) {
	case *corev1.Service:
		c := m.clients.GetKubeClient().CoreV1().Services(ns)
		switch a.verb {
		case "create":
			_, err = c.Create(ctx, o, metav1.CreateOptions{})
		case "update":
			_, err = c.Update(ctx, o, metav1.UpdateOptions{})
		case "delete":
			err = c.Delete(ctx, name, del)
		}
	case *networkingv1.Ingress:
		c := m.clients.GetKubeClient().NetworkingV1().Ingresses(ns)
		switch a.verb {
		case "create":
			_, err = c.Create(ctx, o, metav1.CreateOptions{})
		case "update":
			_, err = c.Update(ctx, o, metav1.UpdateOptions{})
		case "delete":
			err = c.Delete(ctx, name, del)
		}
	case *routev1.Route:
		c := m.clients.GetRouteClient().Routes(ns)
		switch a.verb {
		case "create":
			_, err = c.Create(ctx, o, metav1.CreateOptions{})
		case "update":
			_, err = c.Update(ctx, o, metav1.UpdateOptions{})
		case "delete":
			err = c.Delete(ctx, name, del)
		}
	case *skupperv2alpha1.Certificate:
		c := m.clients.GetSkupperClient().SkupperV2alpha1().Certificates(ns)
		switch a.verb {
		case "create":
			_, err = c.Create(ctx, o, metav1.CreateOptions{})
		case "update":
			_, err = c.Update(ctx, o, metav1.UpdateOptions{})
		case "delete":
			err = c.Delete(ctx, name, del)
		}
	case *skupperv2alpha1.SecuredAccess:
		updated, e := m.clients.GetSkupperClient().SkupperV2alpha1().SecuredAccesses(ns).Update(ctx, o, metav1.UpdateOptions{})
		err = e
		if err == nil {
			*o = *updated
		}
	case *unstructured.Unstructured:
		gvr := resource.TlsRouteResource()
		if a.kind == "HTTPProxy" {
			gvr = httpProxyResource
		}
		if a.kind == "Gateway" {
			gvr = resource.GatewayResource()
		}
		c := m.clients.GetDynamicClient().Resource(gvr).Namespace(ns)
		if a.verb == "delete" {
			err = c.Delete(ctx, name, del)
		} else if a.kind == "HTTPProxy" {
			if a.verb == "create" {
				_, err = c.Create(ctx, o, metav1.CreateOptions{})
			} else {
				_, err = c.Update(ctx, o, metav1.UpdateOptions{})
			}
		} else {
			// Apply only a complete owned projection. Retain resourceVersion as a
			// precondition, and never force ownership conflicts. Version 0 permits
			// creation but is rejected on update: a resource appearing after the
			// snapshot cannot be silently adopted, even under the same SSA manager.
			if a.verb == "create" {
				rv = "0"
			}
			d := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": o.GetAPIVersion(), "kind": o.GetKind(), "spec": runtime.DeepCopyJSONValue(o.Object["spec"])}}
			d.SetName(name)
			d.SetNamespace(ns)
			d.SetResourceVersion(rv)
			d.SetLabels(o.GetLabels())
			d.SetAnnotations(o.GetAnnotations())
			d.SetOwnerReferences(o.GetOwnerReferences())
			data, e := json.Marshal(d)
			if e != nil {
				return e
			}
			_, err = c.Patch(ctx, name, types.ApplyPatchType, data, metav1.PatchOptions{FieldManager: "skupper-controller"})
		}
	default:
		return fmt.Errorf("unknown access action object %T", a.object)
	}
	if a.verb == "delete" && apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (m *SecuredAccessManager) logResults(scope, key, phase string, results []accessResult, err error) {
	failed := 0
	for _, r := range results {
		outcome := "succeeded"
		level := slog.LevelInfo
		if r.err != nil {
			outcome = "failed"
			level = slog.LevelError
			failed++
		}
		attributes := []any{"scope", scope, "key", key, "action", r.verb, "kind", r.kind, "namespace", r.namespace, "name", r.name, "outcome", outcome, "duration", r.duration}
		if r.err != nil {
			attributes = append(attributes, "error", r.err)
		}
		if r.verb == "status" && r.err == nil {
			conditions := make([]map[string]any, 0, len(r.conditions))
			for _, c := range r.conditions {
				conditions = append(conditions, map[string]any{"type": c.Type, "status": string(c.Status), "reason": c.Reason, "message": c.Message, "observed_generation": c.ObservedGeneration})
			}
			attributes = append(attributes, "conditions", conditions)
		}
		m.logger.Log(context.Background(), level, r.logMessage(), attributes...)
	}
	outcome := "succeeded"
	if err != nil {
		outcome = "failed"
	}
	m.logger.Debug("Reconciled "+scope+" scope", "scope", scope, "key", key, "phase", phase, "actions", len(results), "failed_actions", failed, "unchanged_actions", 0, "outcome", outcome)
}

func (r accessResult) logMessage() string {
	target := r.kind + " " + r.namespace + "/" + r.name
	operation, completed := r.verb, ""
	switch r.verb {
	case "create":
		completed = "Created"
	case "update":
		completed = "Updated"
	case "delete":
		completed = "Requested deletion of"
	case "status":
		operation, completed = "update", "Updated"
		target += " status"
	}
	if (r.kind == "TLSRoute" || r.kind == "Gateway") && (r.verb == "create" || r.verb == "update") {
		operation, completed = "apply", "Applied"
	}
	if r.err != nil {
		return "Failed to " + operation + " " + target
	}
	message := completed + " " + target
	if r.verb == "status" {
		var conditions []string
		for _, c := range r.conditions {
			condition := c.Type + "=" + string(c.Status) + " (" + c.Reason + ")"
			if c.Message != "" {
				condition += ": " + c.Message
			}
			conditions = append(conditions, condition)
		}
		if len(conditions) > 0 {
			message += ": " + strings.Join(conditions, "; ")
		}
	}
	return message
}
