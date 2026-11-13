package controller

import (
	routev1 "github.com/openshift/api/route/v1"
	"github.com/skupperproject/skupper/internal/kube/grants"
	"github.com/skupperproject/skupper/internal/kube/securedaccess"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"
)

// registerRouting owns static intake and resource-to-scope routing. Informers
// enqueue resource callbacks; these callbacks read CURRENT caches and enqueue
// stable domain handlers on the same serial EventProcessor. Equal-RV resyncs
// remain renewal and eventual policy/control propagation opportunities.
func (c *Controller) registerRouting(config *Config) {
	p := c.eventProcessor
	namespace := config.WatchNamespace
	// Transitional legacy Site consumers. Keep filtered membership semantics,
	// recovery's second Pod sync, remembered mappings and dynamic Pod lifetimes.
	c.siteWatcher = p.WatchSites(namespace, filter(c, c.checkSite))
	c.listenerWatcher = p.WatchListeners(namespace, filter(c, c.checkListener))
	c.multiKeyListenerWatcher = p.WatchMultiKeyListeners(namespace, filter(c, c.checkMultiKeyListener))
	p.WatchServices(listenerServices(), namespace, filter(c, c.checkListenerService))
	c.serviceWatcher = p.WatchServices(sansSkupperListenerServices(), namespace, filter(c, c.checkObservedService))
	c.connectorWatcher = p.WatchConnectors(namespace, filter(c, c.checkConnector))
	c.linkAccessWatcher = p.WatchRouterAccesses(namespace, filter(c, c.checkRouterAccess))
	c.attachedConnectorWatcher = p.WatchAttachedConnectors(namespace, filter(c, c.checkAttachedConnector))
	c.attachedConnectorBindingWatcher = p.WatchAttachedConnectorBindings(namespace, filter(c, c.checkAttachedConnectorBinding))
	p.WatchLinks(namespace, filter(c, c.checkLink))
	c.networkStatusWatcher = p.WatchConfigMaps(skupperNetworkStatus(), namespace, filter(c, c.networkStatusUpdate))
	p.WatchConfigMaps(skupperRouterConfig(), namespace, filter(c, c.routerConfigUpdate))
	p.WatchAccessTokens(namespace, filter(c, c.checkAccessToken))
	p.WatchPods("skupper.io/component=router,skupper.io/type=site", namespace, filter(c, c.routerPodEvent))
	c.siteSizingWatcher = p.WatchConfigMaps(skupperSiteSizingConfig(), config.Namespace, filter(c, c.siteSizing.Update))
	c.namespaces.watch(p, namespace)
	c.labellingWatcher = p.WatchConfigMaps(labelling(), namespace, c.labelling.Update)

	c.certificateWatcher = p.WatchCertificates(namespace, c.certificateChanged)
	secrets := p.WatchAllSecrets(namespace, c.secretChanged)
	c.certMgr.BindCaches(c.certificateWatcher, secrets)

	// Unfiltered inventories preserve foreign same-name blockers and surviving
	// outputs with drifted labels, including legacy TLSRoute labels.
	w := &securedaccess.ResourceCaches{Certificates: c.certificateWatcher}
	c.accessCaches = w
	w.Services = p.WatchServices(nil, namespace, c.accessServiceChanged)
	w.Ingresses = p.WatchIngresses(nil, namespace, c.accessIngressChanged)
	w.Routes = p.WatchRoutes(nil, namespace, c.accessRouteChanged)
	w.HTTPProxies = p.WatchContourHttpProxies(nil, namespace, c.accessDynamicResourceChanged)
	w.TLSRoutes = p.WatchTlsRoutes(nil, namespace, c.accessDynamicResourceChanged)
	w.Accesses = p.WatchSecuredAccesses(namespace, c.accessChanged)
	// Only this named watch and startup wake the singleton writer. Access and
	// provider notifications must not reintroduce shared-Gateway apply fanout.
	c.enqueueGateway()
	w.Gateways = p.WatchGateways(skupperGateway, config.Namespace, c.gatewayChanged)
	c.accessMgr.BindCaches(w)

	// Transitional Grants registration retains its internal named subscriptions,
	// synchronous authoring and server lifetime until Grants migrate to scopes.
	c.startGrantServer = grants.Initialise(p, config.Namespace, namespace, config.GrantConfig, c.generateLinkConfig, c.IsControlled)
	p.WatchConfigMaps(skupperLogConfig(), config.Namespace, c.logConfigUpdate)
}

func skupperGateway(options *metav1.ListOptions) {
	options.FieldSelector = "metadata.name=skupper"
}

func (c *Controller) enqueueGateway() {
	if c.accessMgr.IsValidAccessType(securedaccess.ACCESS_TYPE_GATEWAY) {
		c.eventProcessor.Enqueue(c.accessMgr.GatewayHandler(), c.Namespace()+"/skupper")
	}
}

func (c *Controller) gatewayChanged(_ string, _ *unstructured.Unstructured) error {
	c.enqueueGateway()
	c.enqueueAccessNamespace("")
	return nil
}

func (c *Controller) certificateChanged(key string, current *skupperv2alpha1.Certificate) error {
	// Authoring acknowledgment must precede scope work and stay on this queue,
	// alongside legacy Site's synchronous Ensure calls, not informer goroutines.
	c.certMgr.ObserveAuthored(key, current)
	c.eventProcessor.Enqueue(c.certMgr, key)
	return c.accessResourceChanged(key)
}

func (c *Controller) secretChanged(key string, _ *corev1.Secret) error {
	// This gate belongs only to the Certificate consumer. Legacy Site/Grant
	// Secret consumers keep their own subscriptions and authorization checks.
	namespace, _, err := cache.SplitMetaNamespaceKey(key)
	if err != nil || !c.IsControlled(namespace) {
		return err
	}
	c.eventProcessor.Enqueue(c.certMgr, key)
	for _, cert := range c.certificateWatcher.List() {
		if !cert.Spec.Signing && cert.Namespace+"/"+cert.Spec.Ca == key {
			c.eventProcessor.Enqueue(c.certMgr, cert.Key())
		}
	}
	return nil
}

func (c *Controller) accessChanged(key string, current *skupperv2alpha1.SecuredAccess) error {
	namespace, _, err := cache.SplitMetaNamespaceKey(key)
	if err != nil || !c.IsControlled(namespace) {
		return err
	}
	// An absent definition cannot be found by inventory. Always route its key.
	c.eventProcessor.Enqueue(c.accessMgr, key)
	c.enqueueAccessNamespace(namespace)
	// Transitional legacy endpoint/RouterAccess propagation, including nil on
	// deletion and returned errors so the resource callback itself is retried.
	return c.checkSecuredAccess(key, current)
}

func (c *Controller) accessServiceChanged(key string, _ *corev1.Service) error {
	return c.accessResourceChanged(key)
}

func (c *Controller) accessIngressChanged(key string, _ *networkingv1.Ingress) error {
	return c.accessResourceChanged(key)
}

func (c *Controller) accessRouteChanged(key string, _ *routev1.Route) error {
	return c.accessResourceChanged(key)
}

func (c *Controller) accessDynamicResourceChanged(key string, _ *unstructured.Unstructured) error {
	return c.accessResourceChanged(key)
}

func (c *Controller) accessResourceChanged(key string) error {
	namespace, _, err := cache.SplitMetaNamespaceKey(key)
	if err == nil {
		c.enqueueAccessNamespace(namespace)
	}
	return err
}

// enqueueAccessNamespace reconstructs dependencies from current definitions and
// surviving outputs on initial list, deletion and resync. Deleted definitions do
// not resync; after retry exhaustion liveness needs a surviving input/output or
// a fresh notification. This is not durable retry state.
func (c *Controller) enqueueAccessNamespace(namespace string) {
	w := c.accessCaches
	for _, sa := range w.Accesses.List() {
		if (namespace == "" || sa.Namespace == namespace) && c.IsControlled(sa.Namespace) {
			c.eventProcessor.Enqueue(c.accessMgr, sa.Key())
			if target := securedaccess.CertificateTarget(sa); target != "" {
				c.eventProcessor.Enqueue(c.accessMgr.CertificateHandler(), sa.Namespace+"/"+target)
			}
		}
	}
	for _, obj := range c.accessInventory() {
		if (namespace == "" || obj.GetNamespace() == namespace) && c.IsControlled(obj.GetNamespace()) {
			for _, ref := range obj.GetOwnerReferences() {
				if ref.APIVersion == "skupper.io/v2alpha1" && ref.Kind == "SecuredAccess" {
					c.eventProcessor.Enqueue(c.accessMgr, obj.GetNamespace()+"/"+ref.Name)
					if _, ok := obj.(*skupperv2alpha1.Certificate); ok {
						// Old targets survive retargeting, issuer removal and deletion.
						c.eventProcessor.Enqueue(c.accessMgr.CertificateHandler(), obj.GetNamespace()+"/"+obj.GetName())
					}
				}
			}
		}
	}
}

func (c *Controller) accessInventory() []metav1.Object {
	w := c.accessCaches
	var result []metav1.Object
	if w.Services != nil {
		for _, o := range w.Services.List() {
			result = append(result, o)
		}
	}
	if w.Ingresses != nil {
		for _, o := range w.Ingresses.List() {
			result = append(result, o)
		}
	}
	if w.Routes != nil {
		for _, o := range w.Routes.List() {
			result = append(result, o)
		}
	}
	if w.HTTPProxies != nil {
		for _, o := range w.HTTPProxies.List() {
			result = append(result, o)
		}
	}
	if w.TLSRoutes != nil {
		for _, o := range w.TLSRoutes.List() {
			result = append(result, o)
		}
	}
	for _, o := range w.Certificates.List() {
		result = append(result, o)
	}
	return result
}
