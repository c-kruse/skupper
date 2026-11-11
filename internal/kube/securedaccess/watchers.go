package securedaccess

import (
	"errors"

	routev1 "github.com/openshift/api/route/v1"
	"github.com/skupperproject/skupper/internal/kube/watchers"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"
)

type SecuredAccessResourceWatcher struct {
	accessMgr                                         *SecuredAccessManager
	processor                                         *watchers.EventProcessor
	serviceWatcher                                    *watchers.ServiceWatcher
	routeWatcher                                      *watchers.RouteWatcher
	ingressWatcher                                    *watchers.IngressWatcher
	httpProxyWatcher, tlsRouteWatcher, gatewayWatcher *watchers.DynamicWatcher
	securedAccessWatcher                              *watchers.SecuredAccessWatcher
	certificateWatcher                                *watchers.CertificateWatcher
}

func NewSecuredAccessResourceWatcher(m *SecuredAccessManager) *SecuredAccessResourceWatcher {
	w := &SecuredAccessResourceWatcher{accessMgr: m}
	m.watcher = w
	return w
}

func (w *SecuredAccessResourceWatcher) WatchResources(p *watchers.EventProcessor, namespace string) {
	w.processor = p
	// Unfiltered inventories detect name camping and discover legacy outputs even
	// when labels have drifted (including the old securedaccess TLSRoute label).
	w.serviceWatcher = p.WatchServices(nil, namespace, func(key string, _ *corev1.Service) error { return w.resourceChanged(key) })
	w.ingressWatcher = p.WatchIngresses(nil, namespace, func(key string, _ *networkingv1.Ingress) error { return w.resourceChanged(key) })
	w.routeWatcher = p.WatchRoutes(nil, namespace, func(key string, _ *routev1.Route) error { return w.resourceChanged(key) })
	w.httpProxyWatcher = p.WatchContourHttpProxies(nil, namespace, func(key string, _ *unstructured.Unstructured) error { return w.resourceChanged(key) })
	w.tlsRouteWatcher = p.WatchTlsRoutes(nil, namespace, func(key string, _ *unstructured.Unstructured) error { return w.resourceChanged(key) })
	w.certificateWatcher = p.WatchCertificates(namespace, func(key string, _ *skupperv2alpha1.Certificate) error { return w.resourceChanged(key) })
}

func (w *SecuredAccessResourceWatcher) WatchGateway(p *watchers.EventProcessor, namespace string) {
	if w.accessMgr.config.isEnabled(ACCESS_TYPE_GATEWAY) {
		p.Enqueue(w.accessMgr.gateway, namespace+"/skupper")
	}
	w.gatewayWatcher = p.WatchGateways(func(options *metav1.ListOptions) {
		options.FieldSelector = "metadata.name=skupper"
	}, namespace, func(_ string, _ *unstructured.Unstructured) error {
		if w.accessMgr.config.isEnabled(ACCESS_TYPE_GATEWAY) {
			p.Enqueue(w.accessMgr.gateway, namespace+"/skupper")
		}
		w.EnqueueNamespace("")
		return nil
	})
}

func (w *SecuredAccessResourceWatcher) WatchSecuredAccesses(p *watchers.EventProcessor, namespace string, handler watchers.SecuredAccessHandler) {
	w.processor = p
	w.securedAccessWatcher = p.WatchSecuredAccesses(namespace, func(key string, sa *skupperv2alpha1.SecuredAccess) error {
		ns, _, err := cache.SplitMetaNamespaceKey(key)
		if err != nil || !w.accessMgr.isControlled(ns) {
			return err
		}
		p.Enqueue(w.accessMgr, key)
		w.EnqueueNamespace(ns)
		if handler != nil {
			return errors.Join(err, handler(key, sa))
		}
		return err
	})
}

func (w *SecuredAccessResourceWatcher) resourceChanged(key string) error {
	ns, _, err := cache.SplitMetaNamespaceKey(key)
	if err == nil {
		w.EnqueueNamespace(ns)
	}
	return err
}

// EnqueueNamespace closes dependencies for observed definitions and surviving
// outputs, on initial list, deletion and periodic informer resync alike. It is
// intentionally conservative while Site/Grant still use resource callbacks.
func (w *SecuredAccessResourceWatcher) EnqueueNamespace(namespace string) {
	if w.processor == nil || w.securedAccessWatcher == nil {
		return
	}
	for _, sa := range w.securedAccessWatcher.List() {
		if (namespace == "" || sa.Namespace == namespace) && w.accessMgr.isControlled(sa.Namespace) {
			w.processor.Enqueue(w.accessMgr, sa.Key())
			if target := certificateTarget(sa); target != "" {
				w.processor.Enqueue(w.accessMgr.certificates, sa.Namespace+"/"+target)
			}
		}
	}
	for _, obj := range w.inventory() {
		if (namespace == "" || obj.GetNamespace() == namespace) && w.accessMgr.isControlled(obj.GetNamespace()) {
			for _, ref := range obj.GetOwnerReferences() {
				if accessReference(ref) {
					w.processor.Enqueue(w.accessMgr, obj.GetNamespace()+"/"+ref.Name)
					if _, ok := obj.(*skupperv2alpha1.Certificate); ok {
						// Old targets survive retargeting, issuer removal and
						// definition deletion; their own key routes cleanup.
						w.processor.Enqueue(w.accessMgr.certificates, obj.GetNamespace()+"/"+obj.GetName())
					}
				}
			}
		}
	}
}

func (w *SecuredAccessResourceWatcher) inventory() []metav1.Object {
	var result []metav1.Object
	if w.serviceWatcher != nil {
		for _, o := range w.serviceWatcher.List() {
			result = append(result, o)
		}
	}
	if w.ingressWatcher != nil {
		for _, o := range w.ingressWatcher.List() {
			result = append(result, o)
		}
	}
	if w.routeWatcher != nil {
		for _, o := range w.routeWatcher.List() {
			result = append(result, o)
		}
	}
	if w.httpProxyWatcher != nil {
		for _, o := range w.httpProxyWatcher.List() {
			result = append(result, o)
		}
	}
	if w.tlsRouteWatcher != nil {
		for _, o := range w.tlsRouteWatcher.List() {
			result = append(result, o)
		}
	}
	if w.certificateWatcher != nil {
		for _, o := range w.certificateWatcher.List() {
			result = append(result, o)
		}
	}
	return result
}
