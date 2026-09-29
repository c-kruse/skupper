package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"

	"github.com/skupperproject/skupper/internal/kube/certificates"
	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	"github.com/skupperproject/skupper/internal/kube/reconcile"
	"github.com/skupperproject/skupper/internal/kube/securedaccess"
	sitelabels "github.com/skupperproject/skupper/internal/kube/site/labels"
	siteresources "github.com/skupperproject/skupper/internal/kube/site/resources"
	"github.com/skupperproject/skupper/internal/kube/site/sizing"
	"github.com/skupperproject/skupper/internal/routercontrol"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	skupperinformers "github.com/skupperproject/skupper/pkg/generated/client/informers/externalversions"
)

const allocationConfigMapName = "skupper-controller-allocations"

// ObservationSource returns an immutable observation view. A missing target is
// unknown, not a complete empty report.
type ObservationSource interface {
	Snapshot(namespace string, evaluationTime time.Time) map[routercontrol.TargetIdentity][]reconcile.Observation
}

// NamespaceController owns shared caches and the leader-only namespace queue.
// StartCaches and WaitForCacheSync are safe for standbys and perform no writes.
type NamespaceController struct {
	clients                internalclient.Clients
	controllerID           string
	coreFactory            informers.SharedInformerFactory
	configurationFactory   informers.SharedInformerFactory
	skupperFactory         skupperinformers.SharedInformerFactory
	informers              namespaceInformers
	queue                  *reconcile.Queue
	observations           ObservationSource
	requireExplicitControl bool
	bootstrap              reconcile.RouterControlBootstrap
	disableSecurityContext bool
	sizing                 SiteSizing
	labelling              siteresources.Labelling
	controllerNamespace    string
	defaultAccessType      string
	clusterHost            string
	accessConfig           reconcile.AccessConfig
	sizingUpdater          func(string, *corev1.ConfigMap) error
	labellingUpdater       func(string, *corev1.ConfigMap) error
	configurationConfigMap cache.SharedIndexInformer
	configurationHandlers  []cache.ResourceEventHandlerRegistration
	bootstrapMu            sync.RWMutex
	leaderRunning          bool
	synced                 atomic.Bool
}

type SiteSizing interface {
	GetSizing(*skupperv2alpha1.Site) (sizing.Sizing, error)
}

type NamespaceControllerOptions struct {
	WatchNamespace         string
	ControllerID           string
	RequireExplicitControl bool
	Workers                int
	Bootstrap              reconcile.RouterControlBootstrap
	DisableSecurityContext bool
	Sizing                 SiteSizing
	Labelling              siteresources.Labelling
	DefaultAccessType      string
	ClusterHost            string
	SecuredAccess          *securedaccess.Config
}

type namespaceInformers struct {
	namespaces        cache.SharedIndexInformer
	configMaps        cache.SharedIndexInformer
	pods              cache.SharedIndexInformer
	services          cache.SharedIndexInformer
	ingresses         cache.SharedIndexInformer
	secrets           cache.SharedIndexInformer
	sites             cache.SharedIndexInformer
	listeners         cache.SharedIndexInformer
	multiKeyListeners cache.SharedIndexInformer
	connectors        cache.SharedIndexInformer
	links             cache.SharedIndexInformer
	routerAccesses    cache.SharedIndexInformer
	certificates      cache.SharedIndexInformer
	securedAccesses   cache.SharedIndexInformer
	routes            cache.SharedIndexInformer
	attached          cache.SharedIndexInformer
	bindings          cache.SharedIndexInformer
}

func NewNamespaceController(clients internalclient.Clients, options NamespaceControllerOptions, publisher routercontrol.IntentPublisher, observations ObservationSource) (*NamespaceController, error) {
	if publisher == nil {
		return nil, fmt.Errorf("intent publisher is required")
	}
	if options.ControllerID == "" {
		return nil, fmt.Errorf("stable controller ID is required")
	}
	if err := options.Bootstrap.Validate(); err != nil {
		return nil, err
	}
	if options.SecuredAccess != nil {
		if err := options.SecuredAccess.Verify(); err != nil {
			return nil, fmt.Errorf("secured access config: %w", err)
		}
	}
	coreFactory := informers.NewSharedInformerFactoryWithOptions(clients.GetKubeClient(), 5*time.Minute, informers.WithNamespace(options.WatchNamespace))
	skupperFactory := skupperinformers.NewSharedInformerFactoryWithOptions(clients.GetSkupperClient(), 5*time.Minute, skupperinformers.WithNamespace(options.WatchNamespace))
	crs := skupperFactory.Skupper().V2alpha1()
	controllerNamespace := options.ControllerID
	if separator := strings.IndexByte(controllerNamespace, '/'); separator >= 0 {
		controllerNamespace = controllerNamespace[:separator]
	} else if options.WatchNamespace != "" {
		controllerNamespace = options.WatchNamespace
	}
	accessConfig := reconcile.AccessConfig{}
	if options.SecuredAccess != nil {
		accessConfig = reconcile.AccessConfig{EnabledTypes: append([]string(nil), options.SecuredAccess.EnabledAccessTypes...), DefaultType: options.SecuredAccess.DefaultAccessType, ClusterHost: options.SecuredAccess.ClusterHost, IngressDomain: options.SecuredAccess.IngressDomain, IngressClassName: options.SecuredAccess.IngressClassName, HTTPProxyDomain: options.SecuredAccess.HttpProxyDomain, GatewayPort: options.SecuredAccess.GatewayPort, GatewayClass: options.SecuredAccess.GatewayClass, GatewayDomain: options.SecuredAccess.GatewayDomain}
	}
	c := &NamespaceController{clients: clients, controllerID: options.ControllerID, coreFactory: coreFactory, skupperFactory: skupperFactory, observations: observations, requireExplicitControl: options.RequireExplicitControl, bootstrap: options.Bootstrap, disableSecurityContext: options.DisableSecurityContext, sizing: options.Sizing, labelling: options.Labelling, controllerNamespace: controllerNamespace, defaultAccessType: options.DefaultAccessType, clusterHost: options.ClusterHost, accessConfig: accessConfig}
	if c.sizing == nil {
		registry := sizing.NewRegistry()
		c.sizing = registry
		c.sizingUpdater = registry.Update
	}
	if c.labelling == nil {
		registry := sitelabels.NewLabelsAndAnnotations(controllerNamespace)
		c.labelling = registry
		c.labellingUpdater = registry.Update
	}
	if options.WatchNamespace != "" && options.WatchNamespace != controllerNamespace {
		c.configurationFactory = informers.NewSharedInformerFactoryWithOptions(clients.GetKubeClient(), 5*time.Minute, informers.WithNamespace(controllerNamespace))
		c.configurationConfigMap = c.configurationFactory.Core().V1().ConfigMaps().Informer()
	}
	c.informers = namespaceInformers{
		namespaces: coreFactory.Core().V1().Namespaces().Informer(), configMaps: coreFactory.Core().V1().ConfigMaps().Informer(), pods: coreFactory.Core().V1().Pods().Informer(), services: coreFactory.Core().V1().Services().Informer(), secrets: coreFactory.Core().V1().Secrets().Informer(), ingresses: coreFactory.Networking().V1().Ingresses().Informer(),
		sites: crs.Sites().Informer(), listeners: crs.Listeners().Informer(), multiKeyListeners: crs.MultiKeyListeners().Informer(), connectors: crs.Connectors().Informer(), links: crs.Links().Informer(), routerAccesses: crs.RouterAccesses().Informer(), certificates: crs.Certificates().Informer(), securedAccesses: crs.SecuredAccesses().Informer(), attached: crs.AttachedConnectors().Informer(), bindings: crs.AttachedConnectorBindings().Informer(),
	}
	if routeClient := clients.GetRouteClient(); routeClient != nil {
		c.informers.routes = cache.NewSharedIndexInformer(&cache.ListWatch{ListFunc: func(listOptions metav1.ListOptions) (runtime.Object, error) {
			return routeClient.Routes(options.WatchNamespace).List(context.Background(), listOptions)
		}, WatchFunc: func(listOptions metav1.ListOptions) (watch.Interface, error) {
			return routeClient.Routes(options.WatchNamespace).Watch(context.Background(), listOptions)
		}}, &routev1.Route{}, 5*time.Minute, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	}
	planner := reconcile.PublicationPlanner{Allocations: c, Publisher: publisher, Validator: c.verifySite}
	accesses := reconcile.AccessPlanner{Next: planner, Ensurer: c}
	workloads := reconcile.WorkloadPlanner{Next: accesses, Ensurer: c}
	c.queue = reconcile.NewQueue("namespace-reconciliation", options.Workers, reconcile.NamespaceReconciler{Collector: c, Deriver: reconcile.NamespaceDeriver{}, Planner: reconcile.StatusPlanner{Next: workloads, Writer: c}, Executor: reconcile.Executor{}})
	if err := c.registerInvalidations(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *NamespaceController) StartCaches(ctx context.Context) {
	c.coreFactory.Start(ctx.Done())
	if c.configurationFactory != nil {
		c.configurationFactory.Start(ctx.Done())
	}
	c.skupperFactory.Start(ctx.Done())
	if c.informers.routes != nil {
		go c.informers.routes.Run(ctx.Done())
	}
}

func (c *NamespaceController) WaitForCacheSync(ctx context.Context) error {
	for kind, ok := range c.coreFactory.WaitForCacheSync(ctx.Done()) {
		if !ok {
			return fmt.Errorf("core informer %v did not synchronize", kind)
		}
	}
	if c.configurationFactory != nil {
		for kind, ok := range c.configurationFactory.WaitForCacheSync(ctx.Done()) {
			if !ok {
				return fmt.Errorf("configuration informer %v did not synchronize", kind)
			}
		}
	}
	for kind, ok := range c.skupperFactory.WaitForCacheSync(ctx.Done()) {
		if !ok {
			return fmt.Errorf("Skupper informer %v did not synchronize", kind)
		}
	}
	if c.informers.routes != nil && !cache.WaitForCacheSync(ctx.Done(), c.informers.routes.HasSynced) {
		return fmt.Errorf("Route informer did not synchronize")
	}
	for _, handler := range c.configurationHandlers {
		if !cache.WaitForCacheSync(ctx.Done(), handler.HasSynced) {
			return fmt.Errorf("configuration event handler did not synchronize")
		}
	}
	c.synced.Store(true)
	return nil
}

func (c *NamespaceController) CachesSynced() bool { return c.synced.Load() }

// SetRouterControlCA installs the public server trust used by router workloads.
// Prepare must call it after cache synchronization and before RunLeader. The
// bytes are copied and are never exposed back to callers.
func (c *NamespaceController) SetRouterControlCA(publicPEM []byte) error {
	if len(publicPEM) == 0 {
		return fmt.Errorf("router-control public CA must not be empty")
	}
	c.bootstrapMu.Lock()
	defer c.bootstrapMu.Unlock()
	if c.leaderRunning {
		return fmt.Errorf("cannot change router-control public CA while leader reconciliation is running")
	}
	c.bootstrap.PublicCA = append(c.bootstrap.PublicCA[:0], publicPEM...)
	return nil
}

// IsControlled reads only the synchronized assignment cache. It is suitable for
// enrollment/session authorization callbacks and never performs an API request.
func (c *NamespaceController) IsControlled(namespace string) bool {
	return c.CachesSynced() && c.assignment(namespace).Controlled
}

// ActiveSite exposes the cache-owned active Site for enrollment authorization
// and the separate UID-keyed AccessGrant workflow. It never performs effects.
func (c *NamespaceController) ActiveSite(namespace string) (*skupperv2alpha1.Site, bool) {
	if !c.CachesSynced() || !c.assignment(namespace).Controlled {
		return nil, false
	}
	sites := listNamespace[*skupperv2alpha1.Site](c.informers.sites, namespace)
	allocation, err := c.allocations(namespace, c.namespaceUID(namespace), listNamespace[*skupperv2alpha1.RouterAccess](c.informers.routerAccesses, namespace))
	if err == nil && allocation.SiteUID != "" {
		for _, site := range sites {
			if site.UID == allocation.SiteUID {
				return site, true
			}
		}
		return nil, false
	}
	if len(sites) == 1 {
		return sites[0], true
	}
	return nil, false
}

func (c *NamespaceController) namespaceUID(namespace string) types.UID {
	value, exists, _ := c.informers.namespaces.GetStore().GetByKey(namespace)
	if !exists {
		return ""
	}
	return value.(*corev1.Namespace).UID
}

// InvalidateNamespaces is the sink for authenticated observation, session,
// authorization, and expiry changes. Repeated namespaces are deduplicated by the
// queue; source namespaces affected by attached definitions should be included.
func (c *NamespaceController) InvalidateNamespaces(namespaces ...string) {
	for _, namespace := range namespaces {
		c.queue.Add(namespace)
	}
}

func (c *NamespaceController) RunLeader(ctx context.Context) error {
	if !c.CachesSynced() {
		return fmt.Errorf("cannot run namespace reconciliation before caches synchronize")
	}
	c.bootstrapMu.Lock()
	if len(c.bootstrap.PublicCA) == 0 {
		c.bootstrapMu.Unlock()
		return fmt.Errorf("cannot run namespace reconciliation without router-control public CA")
	}
	if c.leaderRunning {
		c.bootstrapMu.Unlock()
		return fmt.Errorf("namespace reconciliation is already running")
	}
	c.leaderRunning = true
	c.bootstrapMu.Unlock()
	defer func() {
		c.bootstrapMu.Lock()
		c.leaderRunning = false
		c.bootstrapMu.Unlock()
	}()
	c.enqueueAll()
	c.queue.RunLeader(ctx)
	return ctx.Err()
}

func (c *NamespaceController) registerInvalidations() error {
	local := []cache.SharedIndexInformer{c.informers.namespaces, c.informers.services, c.informers.secrets, c.informers.ingresses, c.informers.sites, c.informers.listeners, c.informers.multiKeyListeners, c.informers.connectors, c.informers.links, c.informers.routerAccesses, c.informers.certificates, c.informers.securedAccesses}
	if c.informers.routes != nil {
		local = append(local, c.informers.routes)
	}
	for _, informer := range local {
		if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: c.invalidateObject, UpdateFunc: func(old, current interface{}) { c.invalidateObject(old); c.invalidateObject(current) }, DeleteFunc: c.invalidateObject}); err != nil {
			return err
		}
	}
	handler, err := c.informers.configMaps.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: c.configurationAdded, UpdateFunc: c.configurationUpdated, DeleteFunc: c.configurationDeleted})
	if err != nil {
		return err
	}
	c.configurationHandlers = append(c.configurationHandlers, handler)
	if c.configurationConfigMap != nil {
		handler, err := c.configurationConfigMap.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: c.configurationAdded, UpdateFunc: c.configurationUpdated, DeleteFunc: c.configurationDeleted})
		if err != nil {
			return err
		}
		c.configurationHandlers = append(c.configurationHandlers, handler)
	}
	if _, err := c.informers.attached.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: c.invalidateAttached, UpdateFunc: func(old, current interface{}) { c.invalidateAttached(old); c.invalidateAttached(current) }, DeleteFunc: c.invalidateAttached}); err != nil {
		return err
	}
	if _, err := c.informers.bindings.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: c.invalidateBinding, UpdateFunc: func(old, current interface{}) { c.invalidateBinding(old); c.invalidateBinding(current) }, DeleteFunc: c.invalidateBinding}); err != nil {
		return err
	}
	if _, err := c.informers.pods.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: c.invalidatePod, UpdateFunc: func(old, current interface{}) { c.invalidatePod(old); c.invalidatePod(current) }, DeleteFunc: c.invalidatePod}); err != nil {
		return err
	}
	return nil
}

func (c *NamespaceController) configurationAdded(value interface{}) {
	if config, ok := eventObject(value).(*corev1.ConfigMap); ok {
		c.updateConfiguration(config, false)
		if configurationAffectsAll(config, c.controllerNamespace) {
			c.invalidateAllSiteNamespaces()
		}
	}
	c.invalidateObject(value)
}

func (c *NamespaceController) configurationUpdated(old, current interface{}) {
	oldConfig, _ := eventObject(old).(*corev1.ConfigMap)
	currentConfig, _ := eventObject(current).(*corev1.ConfigMap)
	if currentConfig != nil {
		c.updateConfiguration(currentConfig, false)
	}
	c.invalidateObject(old)
	c.invalidateObject(current)
	if configurationAffectsAll(oldConfig, c.controllerNamespace) || configurationAffectsAll(currentConfig, c.controllerNamespace) {
		c.invalidateAllSiteNamespaces()
	}
}

func (c *NamespaceController) configurationDeleted(value interface{}) {
	if config, ok := eventObject(value).(*corev1.ConfigMap); ok {
		c.updateConfiguration(config, true)
		if configurationAffectsAll(config, c.controllerNamespace) {
			c.invalidateAllSiteNamespaces()
		}
	}
	c.invalidateObject(value)
}

func (c *NamespaceController) updateConfiguration(config *corev1.ConfigMap, deleted bool) {
	if config == nil {
		return
	}
	key := config.Namespace + "/" + config.Name
	value := config
	if deleted {
		value = nil
	}
	if c.sizingUpdater != nil && config.Namespace == c.controllerNamespace {
		_ = c.sizingUpdater(key, value)
	}
	if c.labellingUpdater != nil {
		_ = c.labellingUpdater(key, value)
	}
}

func configurationAffectsAll(config *corev1.ConfigMap, controllerNamespace string) bool {
	if config == nil || config.Namespace != controllerNamespace {
		return false
	}
	_, sizingConfig := config.Labels[sizing.SiteSizingLabel]
	_, labelTemplate := config.Labels["skupper.io/label-template"]
	return sizingConfig || labelTemplate
}

func (c *NamespaceController) invalidateAllSiteNamespaces() {
	for _, value := range c.informers.sites.GetStore().List() {
		c.queue.Add(value.(*skupperv2alpha1.Site).Namespace)
	}
}

func objectFromEvent(value interface{}) metav1.Object {
	if tombstone, ok := value.(cache.DeletedFinalStateUnknown); ok {
		value = tombstone.Obj
	}
	object, _ := value.(metav1.Object)
	return object
}

func (c *NamespaceController) invalidateObject(value interface{}) {
	if object := objectFromEvent(value); object != nil {
		namespace := object.GetNamespace()
		if namespace == "" {
			if _, ok := eventObject(value).(*corev1.Namespace); ok {
				namespace = object.GetName()
			}
		}
		c.queue.Add(namespace)
		if config, ok := eventObject(value).(*corev1.ConfigMap); ok && config.Name == namespaceConfigName {
			for _, candidate := range c.informers.attached.GetStore().List() {
				definition := candidate.(*skupperv2alpha1.AttachedConnector)
				if definition.Namespace == namespace {
					c.queue.Add(definition.Spec.SiteNamespace)
				}
				if definition.Spec.SiteNamespace == namespace {
					c.queue.Add(definition.Namespace)
				}
			}
		}
	}
}

func (c *NamespaceController) invalidateAttached(value interface{}) {
	if object := objectFromEvent(value); object != nil {
		c.queue.Add(object.GetNamespace())
		if definition, ok := eventObject(value).(*skupperv2alpha1.AttachedConnector); ok {
			c.queue.Add(definition.Spec.SiteNamespace)
		}
	}
}

func (c *NamespaceController) invalidateBinding(value interface{}) {
	if object := objectFromEvent(value); object != nil {
		c.queue.Add(object.GetNamespace())
		if binding, ok := eventObject(value).(*skupperv2alpha1.AttachedConnectorBinding); ok {
			c.queue.Add(binding.Spec.ConnectorNamespace)
		}
	}
}

func (c *NamespaceController) invalidatePod(value interface{}) {
	pod, _ := eventObject(value).(*corev1.Pod)
	if pod == nil {
		return
	}
	c.queue.Add(pod.Namespace)
	for _, value := range c.informers.attached.GetStore().List() {
		definition := value.(*skupperv2alpha1.AttachedConnector)
		if definition.Namespace == pod.Namespace {
			c.queue.Add(definition.Spec.SiteNamespace)
		}
	}
}

func eventObject(value interface{}) interface{} {
	if tombstone, ok := value.(cache.DeletedFinalStateUnknown); ok {
		return tombstone.Obj
	}
	return value
}

func (c *NamespaceController) enqueueAll() {
	informers := []cache.SharedIndexInformer{c.informers.configMaps, c.informers.sites, c.informers.listeners, c.informers.multiKeyListeners, c.informers.connectors, c.informers.links, c.informers.routerAccesses, c.informers.certificates, c.informers.securedAccesses, c.informers.attached, c.informers.bindings}
	if c.informers.routes != nil {
		informers = append(informers, c.informers.routes)
	}
	for _, informer := range informers {
		for _, value := range informer.GetStore().List() {
			c.invalidateObject(value)
		}
	}
	for _, value := range c.informers.attached.GetStore().List() {
		c.invalidateAttached(value)
	}
}

func (c *NamespaceController) Collect(ctx context.Context, namespace string) (reconcile.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return reconcile.Snapshot{}, err
	}
	namespaceObject, exists, err := c.informers.namespaces.GetStore().GetByKey(namespace)
	if err != nil {
		return reconcile.Snapshot{}, err
	}
	if !exists {
		return reconcile.Snapshot{Namespace: reconcile.NamespaceIdentity{Name: namespace}}, nil
	}
	ns := namespaceObject.(*corev1.Namespace)
	evaluationTime := time.Now()
	c.bootstrapMu.RLock()
	bootstrap := c.bootstrap
	bootstrap.PublicCA = append([]byte(nil), c.bootstrap.PublicCA...)
	c.bootstrapMu.RUnlock()
	snapshot := reconcile.Snapshot{Namespace: reconcile.NamespaceIdentity{Name: namespace, UID: ns.UID}, EvaluationTime: evaluationTime, Assignment: c.assignment(namespace), Sites: listNamespace[*skupperv2alpha1.Site](c.informers.sites, namespace), Listeners: listNamespace[*skupperv2alpha1.Listener](c.informers.listeners, namespace), MultiKeyListeners: listNamespace[*skupperv2alpha1.MultiKeyListener](c.informers.multiKeyListeners, namespace), Connectors: listNamespace[*skupperv2alpha1.Connector](c.informers.connectors, namespace), Links: listNamespace[*skupperv2alpha1.Link](c.informers.links, namespace), RouterAccesses: listNamespace[*skupperv2alpha1.RouterAccess](c.informers.routerAccesses, namespace), Certificates: listNamespace[*skupperv2alpha1.Certificate](c.informers.certificates, namespace), SecuredAccesses: listNamespace[*skupperv2alpha1.SecuredAccess](c.informers.securedAccesses, namespace), Bindings: listNamespace[*skupperv2alpha1.AttachedConnectorBinding](c.informers.bindings, namespace), Services: listNamespace[*corev1.Service](c.informers.services, namespace), Ingresses: listNamespace[*networkingv1.Ingress](c.informers.ingresses, namespace), Secrets: listNamespace[*corev1.Secret](c.informers.secrets, namespace), Bootstrap: bootstrap}
	if c.informers.routes != nil {
		snapshot.Routes = listNamespace[*routev1.Route](c.informers.routes, namespace)
	}
	snapshot.ClusterHost = c.clusterHost
	snapshot.AccessConfig = c.accessConfig
	snapshot.AccessConfig.EnabledTypes = append([]string(nil), c.accessConfig.EnabledTypes...)
	if snapshot.AccessConfig.ClusterHost != "" {
		snapshot.ClusterHost = snapshot.AccessConfig.ClusterHost
	}
	if c.defaultAccessType != "" {
		snapshot.DefaultAccessType = c.defaultAccessType
	} else if snapshot.AccessConfig.DefaultType != "" {
		snapshot.DefaultAccessType = snapshot.AccessConfig.DefaultType
	} else if accessTypeEnabled(snapshot.AccessConfig.EnabledTypes, "route") && c.clients.GetRouteClient() != nil {
		snapshot.DefaultAccessType = "route"
	} else if accessTypeEnabled(snapshot.AccessConfig.EnabledTypes, "loadbalancer") {
		snapshot.DefaultAccessType = "loadbalancer"
	} else if len(snapshot.AccessConfig.EnabledTypes) > 0 {
		snapshot.DefaultAccessType = snapshot.AccessConfig.EnabledTypes[0]
	} else if c.clients.GetRouteClient() != nil {
		snapshot.DefaultAccessType = "route"
	} else {
		snapshot.DefaultAccessType = "loadbalancer"
	}
	snapshot.Allocations, err = c.allocations(namespace, ns.UID, snapshot.RouterAccesses)
	if err != nil {
		return reconcile.Snapshot{}, fmt.Errorf("collect allocations: %w", err)
	}
	sources := map[string]bool{namespace: true}
	snapshot.SourceNamespaces = map[string]types.UID{namespace: ns.UID}
	for _, value := range c.informers.attached.GetStore().List() {
		definition := value.(*skupperv2alpha1.AttachedConnector)
		if definition.Namespace == namespace || definition.Spec.SiteNamespace == namespace {
			snapshot.Attached = append(snapshot.Attached, definition.DeepCopy())
		}
		if definition.Spec.SiteNamespace == namespace {
			sources[definition.Namespace] = true
		}
	}
	for source := range sources {
		snapshot.Pods = append(snapshot.Pods, listNamespace[*corev1.Pod](c.informers.pods, source)...)
		if value, exists, _ := c.informers.namespaces.GetStore().GetByKey(source); exists {
			snapshot.SourceNamespaces[source] = value.(*corev1.Namespace).UID
		}
	}
	if c.observations != nil {
		snapshot.Observations = c.observations.Snapshot(namespace, evaluationTime)
	} else {
		snapshot.Observations = map[routercontrol.TargetIdentity][]reconcile.Observation{}
	}
	return snapshot, nil
}

func accessTypeEnabled(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func listNamespace[T runtime.Object](informer cache.SharedIndexInformer, namespace string) []T {
	values, _ := informer.GetIndexer().ByIndex(cache.NamespaceIndex, namespace)
	result := make([]T, 0, len(values))
	for _, value := range values {
		result = append(result, value.(T).DeepCopyObject().(T))
	}
	sort.Slice(result, func(i, j int) bool { return objectKey(result[i]) < objectKey(result[j]) })
	return result
}

func objectKey(value runtime.Object) string {
	object, _ := value.(metav1.Object)
	return object.GetNamespace() + "/" + object.GetName()
}

func (c *NamespaceController) assignment(namespace string) reconcile.Assignment {
	value, exists, _ := c.informers.configMaps.GetStore().GetByKey(namespace + "/" + namespaceConfigName)
	if !exists {
		return reconcile.Assignment{Controller: c.controllerID, Controlled: !c.requireExplicitControl}
	}
	config := value.(*corev1.ConfigMap)
	controller := assignedController(config, namespace, c.controllerID, c.requireExplicitControl)
	return reconcile.Assignment{Controller: controller, Controlled: ControlsNamespace(config, namespace, c.controllerID, c.requireExplicitControl)}
}

// ControlsNamespace is the shared cached/live authorization rule. A missing
// ConfigMap or controller key is automatic only when explicit control is not
// required; an explicitly empty controller value is unassigned.
func ControlsNamespace(config *corev1.ConfigMap, namespace, controllerID string, requireExplicit bool) bool {
	return assignedController(config, namespace, controllerID, requireExplicit) == controllerID
}

func assignedController(config *corev1.ConfigMap, namespace, controllerID string, requireExplicit bool) string {
	if config == nil {
		if requireExplicit {
			return ""
		}
		return controllerID
	}
	controller, assigned := config.Data[controllerSettingKey]
	if !assigned {
		if requireExplicit {
			return ""
		}
		return controllerID
	}
	if !strings.Contains(controller, "/") {
		controller = namespace + "/" + controller
	}
	return controller
}

func (c *NamespaceController) allocations(namespace string, namespaceUID types.UID, accesses []*skupperv2alpha1.RouterAccess) (reconcile.AllocationState, error) {
	result := reconcile.AllocationState{Ports: map[string]int{}}
	value, exists, _ := c.informers.configMaps.GetStore().GetByKey(namespace + "/" + allocationConfigMapName)
	if !exists {
		return result, nil
	}
	config := value.(*corev1.ConfigMap)
	if config.Data["version"] != "1" {
		return result, fmt.Errorf("unsupported allocation record version %q", config.Data["version"])
	}
	if config.Data["namespaceUID"] != string(namespaceUID) {
		return result, fmt.Errorf("allocation record namespace UID %q does not match %q", config.Data["namespaceUID"], namespaceUID)
	}
	if config.Data["siteUID"] == "" {
		return result, fmt.Errorf("allocation record has no Site UID")
	}
	result.SiteUID = types.UID(config.Data["siteUID"])
	result.ResourceVersion = config.ResourceVersion
	if err := json.Unmarshal([]byte(config.Data["ports"]), &result.Ports); err != nil {
		return result, fmt.Errorf("invalid allocation ports: %w", err)
	}
	reserved := map[int]bool{45671: true, 55671: true, 5671: true, 5672: true, 9090: true}
	for _, access := range accesses {
		for _, role := range access.Spec.Roles {
			port := int(role.GetPort())
			if port < 1 || port > 65535 {
				return result, fmt.Errorf("RouterAccess %s/%s role %s has invalid port %d", access.Namespace, access.Name, role.Name, port)
			}
			reserved[port] = true
		}
	}
	used := map[int]string{}
	for key, port := range result.Ports {
		if port < 1024 || port > 65535 {
			return result, fmt.Errorf("allocation %q has invalid port %d", key, port)
		}
		if reserved[port] {
			return result, fmt.Errorf("allocation %q uses reserved port %d", key, port)
		}
		if previous := used[port]; previous != "" {
			return result, fmt.Errorf("allocations %q and %q both use port %d", previous, key, port)
		}
		used[port] = key
	}
	return result, nil
}

func (c *NamespaceController) CommitAllocations(ctx context.Context, namespace reconcile.NamespaceIdentity, site *skupperv2alpha1.Site, allocations reconcile.AllocationState) error {
	if err := c.verifySite(ctx, namespace, site); err != nil {
		return err
	}
	encoded, err := json.Marshal(allocations.Ports)
	if err != nil {
		return err
	}
	configMaps := c.clients.GetKubeClient().CoreV1().ConfigMaps(namespace.Name)
	current, err := configMaps.Get(ctx, allocationConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if allocations.ResourceVersion != "" {
			return reconcile.SupersededError{Reason: "allocation record disappeared"}
		}
		_, err = configMaps.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: allocationConfigMapName, Labels: map[string]string{"internal.skupper.io/allocation-state": "true"}}, Data: map[string]string{"version": "1", "namespaceUID": string(namespace.UID), "siteUID": string(allocations.SiteUID), "ports": string(encoded)}}, metav1.CreateOptions{})
		return classifyWriteError(err)
	}
	if err != nil {
		return err
	}
	if current.Data["siteUID"] != "" && current.Data["siteUID"] != string(allocations.SiteUID) {
		return reconcile.SupersededError{Reason: "allocation record belongs to another Site UID"}
	}
	if allocations.ResourceVersion == "" || current.ResourceVersion != allocations.ResourceVersion {
		return reconcile.SupersededError{Reason: "allocation record changed after snapshot"}
	}
	if current.Data["namespaceUID"] != string(namespace.UID) {
		return reconcile.SupersededError{Reason: "allocation record belongs to another namespace UID"}
	}
	desiredData := map[string]string{"version": "1", "namespaceUID": string(namespace.UID), "siteUID": string(allocations.SiteUID), "ports": string(encoded)}
	if reflect.DeepEqual(current.Data, desiredData) {
		return nil
	}
	current.Data = desiredData
	_, err = configMaps.Update(ctx, current, metav1.UpdateOptions{})
	return classifyWriteError(err)
}

func (c *NamespaceController) EnsureRouterControlCA(ctx context.Context, namespace reconcile.NamespaceIdentity, site *skupperv2alpha1.Site, bootstrap reconcile.RouterControlBootstrap) error {
	if len(bootstrap.PublicCA) == 0 {
		return fmt.Errorf("router-control public CA must not be empty")
	}
	if err := c.verifySite(ctx, namespace, site); err != nil {
		return err
	}
	controller, block := true, true
	desired := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: bootstrap.CABundleConfigMap, Namespace: namespace.Name, OwnerReferences: []metav1.OwnerReference{{APIVersion: skupperv2alpha1.SchemeGroupVersion.String(), Kind: "Site", Name: site.Name, UID: site.UID, Controller: &controller, BlockOwnerDeletion: &block}}}, Data: map[string]string{bootstrap.CABundleKey: string(bootstrap.PublicCA)}}
	configMaps := c.clients.GetKubeClient().CoreV1().ConfigMaps(namespace.Name)
	current, err := configMaps.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = configMaps.Create(ctx, desired, metav1.CreateOptions{})
		return classifyWriteError(err)
	}
	if err != nil {
		return classifyWriteError(err)
	}
	if !metav1.IsControlledBy(current, site) {
		return fmt.Errorf("router-control CA ConfigMap %s/%s is not controlled by Site UID %s", namespace.Name, desired.Name, site.UID)
	}
	if reflect.DeepEqual(current.Data, desired.Data) {
		return nil
	}
	desired.ResourceVersion = current.ResourceVersion
	_, err = configMaps.Update(ctx, desired, metav1.UpdateOptions{})
	return classifyWriteError(err)
}

func (c *NamespaceController) EnsureRouterPrerequisites(ctx context.Context, namespace reconcile.NamespaceIdentity, site *skupperv2alpha1.Site, serviceAccount *corev1.ServiceAccount, role *rbacv1.Role, roleBinding *rbacv1.RoleBinding) error {
	if err := c.verifySite(ctx, namespace, site); err != nil {
		return err
	}
	if serviceAccount == nil || role == nil || roleBinding == nil {
		return c.retireRouterPrerequisites(ctx, namespace, site)
	}
	serviceAccounts := c.clients.GetKubeClient().CoreV1().ServiceAccounts(namespace.Name)
	currentServiceAccount, err := serviceAccounts.Get(ctx, serviceAccount.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if err := c.verifySite(ctx, namespace, site); err != nil {
			return err
		}
		_, err = serviceAccounts.Create(ctx, serviceAccount, metav1.CreateOptions{})
	} else if err == nil {
		if !metav1.IsControlledBy(currentServiceAccount, site) {
			return fmt.Errorf("router ServiceAccount %s/%s is not controlled by Site UID %s", namespace.Name, serviceAccount.Name, site.UID)
		}
		if !reflect.DeepEqual(currentServiceAccount.OwnerReferences, serviceAccount.OwnerReferences) {
			if err := c.verifySite(ctx, namespace, site); err != nil {
				return err
			}
			currentServiceAccount.OwnerReferences = serviceAccount.OwnerReferences
			_, err = serviceAccounts.Update(ctx, currentServiceAccount, metav1.UpdateOptions{})
		}
	}
	if err != nil {
		return classifyWriteError(err)
	}

	roles := c.clients.GetKubeClient().RbacV1().Roles(namespace.Name)
	currentRole, err := roles.Get(ctx, role.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if err := c.verifySite(ctx, namespace, site); err != nil {
			return err
		}
		_, err = roles.Create(ctx, role, metav1.CreateOptions{})
	} else if err == nil {
		if !metav1.IsControlledBy(currentRole, site) {
			return fmt.Errorf("router Role %s/%s is not controlled by Site UID %s", namespace.Name, role.Name, site.UID)
		}
		if !reflect.DeepEqual(currentRole.OwnerReferences, role.OwnerReferences) || !reflect.DeepEqual(currentRole.Rules, role.Rules) {
			if err := c.verifySite(ctx, namespace, site); err != nil {
				return err
			}
			role.ResourceVersion = currentRole.ResourceVersion
			_, err = roles.Update(ctx, role, metav1.UpdateOptions{})
		}
	}
	if err != nil {
		return classifyWriteError(err)
	}

	bindings := c.clients.GetKubeClient().RbacV1().RoleBindings(namespace.Name)
	currentBinding, err := bindings.Get(ctx, roleBinding.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if err := c.verifySite(ctx, namespace, site); err != nil {
			return err
		}
		_, err = bindings.Create(ctx, roleBinding, metav1.CreateOptions{})
	} else if err == nil {
		if !metav1.IsControlledBy(currentBinding, site) {
			return fmt.Errorf("router RoleBinding %s/%s is not controlled by Site UID %s", namespace.Name, roleBinding.Name, site.UID)
		}
		if !reflect.DeepEqual(currentBinding.OwnerReferences, roleBinding.OwnerReferences) || !reflect.DeepEqual(currentBinding.Subjects, roleBinding.Subjects) || !reflect.DeepEqual(currentBinding.RoleRef, roleBinding.RoleRef) {
			if currentBinding.RoleRef != roleBinding.RoleRef {
				return fmt.Errorf("router RoleBinding %s/%s has an incompatible roleRef", namespace.Name, roleBinding.Name)
			}
			if err := c.verifySite(ctx, namespace, site); err != nil {
				return err
			}
			roleBinding.ResourceVersion = currentBinding.ResourceVersion
			_, err = bindings.Update(ctx, roleBinding, metav1.UpdateOptions{})
		}
	}
	return classifyWriteError(err)
}

func (c *NamespaceController) retireRouterPrerequisites(ctx context.Context, namespace reconcile.NamespaceIdentity, site *skupperv2alpha1.Site) error {
	bindings := c.clients.GetKubeClient().RbacV1().RoleBindings(namespace.Name)
	if current, err := bindings.Get(ctx, "skupper-router", metav1.GetOptions{}); err == nil && metav1.IsControlledBy(current, site) {
		if err := c.verifySite(ctx, namespace, site); err != nil {
			return err
		}
		if err := bindings.Delete(ctx, current.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &current.UID}}); err != nil && !apierrors.IsNotFound(err) {
			return classifyWriteError(err)
		}
	} else if err != nil && !apierrors.IsNotFound(err) {
		return classifyWriteError(err)
	}
	roles := c.clients.GetKubeClient().RbacV1().Roles(namespace.Name)
	if current, err := roles.Get(ctx, "skupper-router", metav1.GetOptions{}); err == nil && metav1.IsControlledBy(current, site) {
		if err := c.verifySite(ctx, namespace, site); err != nil {
			return err
		}
		if err := roles.Delete(ctx, current.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &current.UID}}); err != nil && !apierrors.IsNotFound(err) {
			return classifyWriteError(err)
		}
	} else if err != nil && !apierrors.IsNotFound(err) {
		return classifyWriteError(err)
	}
	serviceAccounts := c.clients.GetKubeClient().CoreV1().ServiceAccounts(namespace.Name)
	if current, err := serviceAccounts.Get(ctx, "skupper-router", metav1.GetOptions{}); err == nil && metav1.IsControlledBy(current, site) {
		if err := c.verifySite(ctx, namespace, site); err != nil {
			return err
		}
		if err := serviceAccounts.Delete(ctx, current.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &current.UID}}); err != nil && !apierrors.IsNotFound(err) {
			return classifyWriteError(err)
		}
	} else if err != nil && !apierrors.IsNotFound(err) {
		return classifyWriteError(err)
	}
	return nil
}

func (c *NamespaceController) EnsureSite(ctx context.Context, namespace reconcile.NamespaceIdentity, site *skupperv2alpha1.Site, groups []string, bootstrap reconcile.RouterControlBootstrap) error {
	if err := c.verifySite(ctx, namespace, site); err != nil {
		return err
	}
	if err := c.validateWorkloadOwnership(ctx, site, groups); err != nil {
		return err
	}
	config := siteresources.RouterControlConfig{NamespaceUID: string(namespace.UID), SiteUID: string(site.UID), EnrollmentURL: bootstrap.EnrollmentURL, ControlAddress: bootstrap.ControlAddress, TLSServerName: bootstrap.TLSServerName, TokenAudience: bootstrap.TokenAudience, TokenPath: bootstrap.TokenPath, CABundleConfigMap: bootstrap.CABundleConfigMap, CABundleKey: bootstrap.CABundleKey, CABundlePath: bootstrap.CABundlePath}
	siteSizing := sizing.Sizing{}
	if c.sizing != nil {
		var err error
		siteSizing, err = c.sizing.GetSizing(site)
		if err != nil {
			return err
		}
	}
	for _, group := range groups {
		if err := siteresources.ApplyWithRouterControl(c.clients, ctx, site, group, siteSizing, c.labelling, c.disableSecurityContext, config); err != nil {
			return err
		}
	}
	return c.retireSiteWorkloads(ctx, site, groups)
}

func (c *NamespaceController) EnsureListenerServices(ctx context.Context, namespace reconcile.NamespaceIdentity, site *skupperv2alpha1.Site, services []*corev1.Service) error {
	names := make([]string, 0, len(services))
	for _, service := range services {
		names = append(names, service.Name)
		if err := c.EnsureListenerService(ctx, namespace, site, service); err != nil {
			return err
		}
	}
	return c.RetireListenerServices(ctx, namespace, site, names)
}

func (c *NamespaceController) EnsureListenerService(ctx context.Context, namespace reconcile.NamespaceIdentity, site *skupperv2alpha1.Site, desired *corev1.Service) error {
	if err := c.verifySite(ctx, namespace, site); err != nil {
		return err
	}
	client := c.clients.GetKubeClient().CoreV1().Services(namespace.Name)
	current, err := client.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := client.Create(ctx, desired, metav1.CreateOptions{}); err != nil {
			return classifyWriteError(err)
		}
		return nil
	}
	if err != nil {
		return classifyWriteError(err)
	}
	if !ownedByUID(current.OwnerReferences, site.UID) || current.Annotations["internal.skupper.io/controlled"] != "true" {
		return fmt.Errorf("Listener Service %s/%s is not owned by Site UID %s", namespace.Name, desired.Name, site.UID)
	}
	desired.ResourceVersion = current.ResourceVersion
	desired.Spec.ClusterIP = current.Spec.ClusterIP
	desired.Spec.ClusterIPs = append([]string(nil), current.Spec.ClusterIPs...)
	desired.Spec.IPFamilies = append([]corev1.IPFamily(nil), current.Spec.IPFamilies...)
	desired.Spec.IPFamilyPolicy = current.Spec.IPFamilyPolicy
	if reflect.DeepEqual(current.Labels, desired.Labels) && reflect.DeepEqual(current.Annotations, desired.Annotations) && reflect.DeepEqual(current.OwnerReferences, desired.OwnerReferences) && reflect.DeepEqual(current.Spec, desired.Spec) {
		return nil
	}
	_, err = client.Update(ctx, desired, metav1.UpdateOptions{})
	return classifyWriteError(err)
}

func (c *NamespaceController) RetireListenerServices(ctx context.Context, namespace reconcile.NamespaceIdentity, site *skupperv2alpha1.Site, names []string) error {
	if err := c.verifySite(ctx, namespace, site); err != nil {
		return err
	}
	desiredNames := map[string]bool{}
	for _, name := range names {
		desiredNames[name] = true
	}
	client := c.clients.GetKubeClient().CoreV1().Services(namespace.Name)
	current, err := client.List(ctx, metav1.ListOptions{LabelSelector: "internal.skupper.io/listener=true"})
	if err != nil {
		return classifyWriteError(err)
	}
	for i := range current.Items {
		service := &current.Items[i]
		if desiredNames[service.Name] || service.Annotations["internal.skupper.io/controlled"] != "true" || !ownedByUID(service.OwnerReferences, site.UID) {
			continue
		}
		if err := client.Delete(ctx, service.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &service.UID}}); err != nil && !apierrors.IsNotFound(err) {
			return classifyWriteError(err)
		}
	}
	return nil
}

func (c *NamespaceController) EnsureAccessComposition(ctx context.Context, namespace reconcile.NamespaceIdentity, site *skupperv2alpha1.Site, generated *skupperv2alpha1.RouterAccess, secured []*skupperv2alpha1.SecuredAccess, desiredCertificates []*skupperv2alpha1.Certificate, services []*corev1.Service, routes []*routev1.Route, ingresses []*networkingv1.Ingress, snapshotRouterAccesses []*skupperv2alpha1.RouterAccess, snapshotSecuredAccesses []*skupperv2alpha1.SecuredAccess, snapshotCertificates []*skupperv2alpha1.Certificate, _ []*corev1.Secret, evaluationTime time.Time) error {
	if err := c.verifySite(ctx, namespace, site); err != nil {
		return err
	}
	effectErrors := []error{}
	if site != nil {
		if err := c.ensureGeneratedRouterAccess(ctx, namespace, site, generated); err != nil {
			effectErrors = append(effectErrors, fmt.Errorf("ensure generated RouterAccess: %w", err))
		}
	}
	if err := c.ensureSecuredAccesses(ctx, namespace, site, secured, snapshotRouterAccesses); err != nil {
		effectErrors = append(effectErrors, fmt.Errorf("ensure generated SecuredAccess resources: %w", err))
	}
	if err := c.ensureAccessServices(ctx, namespace, site, services, snapshotSecuredAccesses); err != nil {
		effectErrors = append(effectErrors, fmt.Errorf("ensure SecuredAccess Services: %w", err))
	}
	if err := c.ensureAccessRoutes(ctx, namespace, site, routes, snapshotSecuredAccesses); err != nil {
		effectErrors = append(effectErrors, fmt.Errorf("ensure SecuredAccess Routes: %w", err))
	}
	if err := c.ensureAccessIngresses(ctx, namespace, site, ingresses, snapshotSecuredAccesses); err != nil {
		effectErrors = append(effectErrors, fmt.Errorf("ensure SecuredAccess Ingresses: %w", err))
	}
	applied, err := c.ensureCertificates(ctx, namespace, site, desiredCertificates, snapshotSecuredAccesses)
	if err != nil {
		effectErrors = append(effectErrors, fmt.Errorf("ensure generated Certificates: %w", err))
	}
	byName := map[string]*skupperv2alpha1.Certificate{}
	for _, certificate := range snapshotCertificates {
		byName[certificate.Name] = certificate
	}
	for _, certificate := range applied {
		byName[certificate.Name] = certificate
	}
	availableSecrets := map[string]*corev1.Secret{}
	for _, signing := range []bool{true, false} {
		names := make([]string, 0, len(byName))
		for name, certificate := range byName {
			if certificate.Spec.Signing == signing {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			secret, err := c.ensureCertificateSecret(ctx, namespace, site, byName[name], availableSecrets[byName[name].Spec.Ca], evaluationTime)
			if err != nil {
				effectErrors = append(effectErrors, fmt.Errorf("ensure Certificate Secret %s: %w", name, err))
				continue
			}
			availableSecrets[name] = secret
		}
	}
	return errors.Join(effectErrors...)
}

func (c *NamespaceController) ensureAccessIngresses(ctx context.Context, namespace reconcile.NamespaceIdentity, site *skupperv2alpha1.Site, desired []*networkingv1.Ingress, expected []*skupperv2alpha1.SecuredAccess) error {
	api := c.clients.GetKubeClient().NetworkingV1().Ingresses(namespace.Name)
	names := map[string]bool{}
	var failures []error
	for _, value := range desired {
		names[value.Name] = true
		if err := c.verifyAccessOwners(ctx, namespace, site, value.OwnerReferences, nil, expected); err != nil {
			failures = append(failures, fmt.Errorf("Ingress %s: %w", value.Name, err))
			continue
		}
		current, err := api.Get(ctx, value.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if err := c.verifyAccessOwners(ctx, namespace, site, value.OwnerReferences, nil, expected); err != nil {
				failures = append(failures, fmt.Errorf("Ingress %s: %w", value.Name, err))
				continue
			}
			_, err = api.Create(ctx, value, metav1.CreateOptions{})
		} else if err == nil {
			ownerUID := value.OwnerReferences[0].UID
			if current.Annotations["internal.skupper.io/controlled"] != "true" || !hasOwnerUID(current.OwnerReferences, ownerUID) {
				failures = append(failures, fmt.Errorf("Ingress %s/%s is not owned by SecuredAccess UID %s", namespace.Name, value.Name, ownerUID))
				continue
			}
			if reflect.DeepEqual(current.Spec, value.Spec) && reflect.DeepEqual(current.Labels, value.Labels) && reflect.DeepEqual(current.Annotations, value.Annotations) && reflect.DeepEqual(current.OwnerReferences, value.OwnerReferences) {
				continue
			}
			value.ResourceVersion = current.ResourceVersion
			if err := c.verifyAccessOwners(ctx, namespace, site, value.OwnerReferences, nil, expected); err != nil {
				failures = append(failures, fmt.Errorf("Ingress %s: %w", value.Name, err))
				continue
			}
			_, err = api.Update(ctx, value, metav1.UpdateOptions{})
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("Ingress %s: %w", value.Name, classifyWriteError(err)))
		}
	}
	current, err := api.List(ctx, metav1.ListOptions{LabelSelector: "internal.skupper.io/secured-access=true"})
	if err != nil {
		return errors.Join(append(failures, classifyWriteError(err))...)
	}
	for i := range current.Items {
		value := &current.Items[i]
		if names[value.Name] || value.Annotations["internal.skupper.io/controlled"] != "true" {
			continue
		}
		owner := metav1.GetControllerOf(value)
		if owner == nil || owner.Kind != "SecuredAccess" {
			continue
		}
		parent := expectedSecuredAccess(expected, owner)
		if parent == nil {
			continue
		}
		if err := c.verifySecuredAccess(ctx, namespace, parent); err != nil {
			failures = append(failures, fmt.Errorf("retire Ingress %s: %w", value.Name, err))
			continue
		}
		if err := api.Delete(ctx, value.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &value.UID}}); err != nil && !apierrors.IsNotFound(err) {
			failures = append(failures, fmt.Errorf("retire Ingress %s: %w", value.Name, classifyWriteError(err)))
		}
	}
	return errors.Join(failures...)
}

func (c *NamespaceController) ensureAccessRoutes(ctx context.Context, namespace reconcile.NamespaceIdentity, site *skupperv2alpha1.Site, desired []*routev1.Route, expectedSecuredAccesses []*skupperv2alpha1.SecuredAccess) error {
	api := c.clients.GetRouteClient()
	if api == nil {
		if len(desired) > 0 {
			return fmt.Errorf("Route access is not available")
		}
		return nil
	}
	routes := api.Routes(namespace.Name)
	names := map[string]bool{}
	var failures []error
	for _, value := range desired {
		names[value.Name] = true
		if err := c.verifyAccessOwners(ctx, namespace, site, value.OwnerReferences, nil, expectedSecuredAccesses); err != nil {
			failures = append(failures, fmt.Errorf("Route %s: %w", value.Name, err))
			continue
		}
		current, err := routes.Get(ctx, value.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if err := c.verifyAccessOwners(ctx, namespace, site, value.OwnerReferences, nil, expectedSecuredAccesses); err != nil {
				failures = append(failures, fmt.Errorf("Route %s: %w", value.Name, err))
				continue
			}
			if _, err = routes.Create(ctx, value, metav1.CreateOptions{}); err != nil {
				failures = append(failures, fmt.Errorf("Route %s: %w", value.Name, classifyWriteError(err)))
			}
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("Route %s: %w", value.Name, classifyWriteError(err)))
			continue
		}
		ownerUID := value.OwnerReferences[0].UID
		if current.Annotations["internal.skupper.io/controlled"] != "true" || !hasOwnerUID(current.OwnerReferences, ownerUID) {
			failures = append(failures, fmt.Errorf("Route %s/%s is not owned by SecuredAccess UID %s", namespace.Name, value.Name, ownerUID))
			continue
		}
		if value.Spec.Host == "" {
			value.Spec.Host = current.Spec.Host
		}
		if reflect.DeepEqual(current.Spec, value.Spec) && reflect.DeepEqual(current.Labels, value.Labels) && reflect.DeepEqual(current.Annotations, value.Annotations) && reflect.DeepEqual(current.OwnerReferences, value.OwnerReferences) {
			continue
		}
		value.ResourceVersion = current.ResourceVersion
		if err := c.verifyAccessOwners(ctx, namespace, site, value.OwnerReferences, nil, expectedSecuredAccesses); err != nil {
			failures = append(failures, fmt.Errorf("Route %s: %w", value.Name, err))
			continue
		}
		if _, err = routes.Update(ctx, value, metav1.UpdateOptions{}); err != nil {
			failures = append(failures, fmt.Errorf("Route %s: %w", value.Name, classifyWriteError(err)))
		}
	}
	current, err := routes.List(ctx, metav1.ListOptions{LabelSelector: "internal.skupper.io/secured-access=true"})
	if err != nil {
		return errors.Join(append(failures, classifyWriteError(err))...)
	}
	for i := range current.Items {
		value := &current.Items[i]
		if names[value.Name] || value.Annotations["internal.skupper.io/controlled"] != "true" {
			continue
		}
		owner := metav1.GetControllerOf(value)
		if owner == nil || owner.Kind != "SecuredAccess" {
			continue
		}
		parent := expectedSecuredAccess(expectedSecuredAccesses, owner)
		if parent == nil {
			continue
		}
		if err := c.verifySecuredAccess(ctx, namespace, parent); err != nil {
			failures = append(failures, fmt.Errorf("retire Route %s: %w", value.Name, err))
			continue
		}
		if err := routes.Delete(ctx, value.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &value.UID}}); err != nil && !apierrors.IsNotFound(err) {
			failures = append(failures, fmt.Errorf("retire Route %s: %w", value.Name, classifyWriteError(err)))
		}
	}
	return errors.Join(failures...)
}

func (c *NamespaceController) ensureGeneratedRouterAccess(ctx context.Context, namespace reconcile.NamespaceIdentity, site *skupperv2alpha1.Site, desired *skupperv2alpha1.RouterAccess) error {
	api := c.clients.GetSkupperClient().SkupperV2alpha1().RouterAccesses(namespace.Name)
	current, err := api.Get(ctx, "skupper-router", metav1.GetOptions{})
	if desired == nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return classifyWriteError(err)
		}
		if current.Annotations["internal.skupper.io/controlled"] != "true" || !ownedByUID(current.OwnerReferences, site.UID) {
			return nil
		}
		if err := c.verifySite(ctx, namespace, site); err != nil {
			return err
		}
		return classifyWriteError(api.Delete(ctx, current.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &current.UID}}))
	}
	if apierrors.IsNotFound(err) {
		if err := c.verifySite(ctx, namespace, site); err != nil {
			return err
		}
		_, err = api.Create(ctx, desired, metav1.CreateOptions{})
		return classifyWriteError(err)
	}
	if err != nil {
		return classifyWriteError(err)
	}
	if current.Annotations["internal.skupper.io/controlled"] != "true" || !ownedByUID(current.OwnerReferences, site.UID) {
		return fmt.Errorf("default RouterAccess %s/%s is not owned by Site UID %s", namespace.Name, current.Name, site.UID)
	}
	if reflect.DeepEqual(current.Spec, desired.Spec) && reflect.DeepEqual(current.OwnerReferences, desired.OwnerReferences) && reflect.DeepEqual(current.Annotations, desired.Annotations) {
		return nil
	}
	desired.ResourceVersion = current.ResourceVersion
	if err := c.verifySite(ctx, namespace, site); err != nil {
		return err
	}
	_, err = api.Update(ctx, desired, metav1.UpdateOptions{})
	return classifyWriteError(err)
}

func (c *NamespaceController) ensureSecuredAccesses(ctx context.Context, namespace reconcile.NamespaceIdentity, site *skupperv2alpha1.Site, desired []*skupperv2alpha1.SecuredAccess, expectedRouterAccesses []*skupperv2alpha1.RouterAccess) error {
	api := c.clients.GetSkupperClient().SkupperV2alpha1().SecuredAccesses(namespace.Name)
	names := map[string]bool{}
	var failures []error
	for _, value := range desired {
		names[value.Name] = true
		if err := c.verifyAccessOwners(ctx, namespace, site, value.OwnerReferences, expectedRouterAccesses, nil); err != nil {
			failures = append(failures, fmt.Errorf("SecuredAccess %s: %w", value.Name, err))
			continue
		}
		current, err := api.Get(ctx, value.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if err := c.verifyAccessOwners(ctx, namespace, site, value.OwnerReferences, expectedRouterAccesses, nil); err != nil {
				failures = append(failures, fmt.Errorf("SecuredAccess %s: %w", value.Name, err))
				continue
			}
			if _, err = api.Create(ctx, value, metav1.CreateOptions{}); err != nil {
				failures = append(failures, fmt.Errorf("SecuredAccess %s: %w", value.Name, classifyWriteError(err)))
			}
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("SecuredAccess %s: %w", value.Name, classifyWriteError(err)))
			continue
		}
		ownerUID := value.OwnerReferences[0].UID
		if current.Annotations["internal.skupper.io/controlled"] != "true" || !hasOwnerUID(current.OwnerReferences, ownerUID) {
			failures = append(failures, fmt.Errorf("generated SecuredAccess %s/%s is not owned by RouterAccess UID %s", namespace.Name, value.Name, ownerUID))
			continue
		}
		if reflect.DeepEqual(current.Spec, value.Spec) && reflect.DeepEqual(current.OwnerReferences, value.OwnerReferences) && reflect.DeepEqual(current.Annotations, value.Annotations) {
			continue
		}
		value.ResourceVersion = current.ResourceVersion
		if err := c.verifyAccessOwners(ctx, namespace, site, value.OwnerReferences, expectedRouterAccesses, nil); err != nil {
			failures = append(failures, fmt.Errorf("SecuredAccess %s: %w", value.Name, err))
			continue
		}
		if _, err = api.Update(ctx, value, metav1.UpdateOptions{}); err != nil {
			failures = append(failures, fmt.Errorf("SecuredAccess %s: %w", value.Name, classifyWriteError(err)))
		}
	}
	current, err := api.List(ctx, metav1.ListOptions{})
	if err != nil {
		return errors.Join(append(failures, classifyWriteError(err))...)
	}
	for i := range current.Items {
		value := &current.Items[i]
		if names[value.Name] || value.Annotations["internal.skupper.io/controlled"] != "true" || value.Annotations["internal.skupper.io/routeraccess"] == "" {
			continue
		}
		owner := metav1.GetControllerOf(value)
		if owner == nil || owner.Kind != "RouterAccess" {
			continue
		}
		parent := expectedRouterAccess(expectedRouterAccesses, owner)
		if parent == nil {
			continue
		}
		if err := c.verifyRouterAccess(ctx, namespace, parent); err != nil {
			failures = append(failures, fmt.Errorf("retire SecuredAccess %s: %w", value.Name, err))
			continue
		}
		if err := api.Delete(ctx, value.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &value.UID}}); err != nil && !apierrors.IsNotFound(err) {
			failures = append(failures, fmt.Errorf("retire SecuredAccess %s: %w", value.Name, classifyWriteError(err)))
		}
	}
	return errors.Join(failures...)
}

func (c *NamespaceController) ensureAccessServices(ctx context.Context, namespace reconcile.NamespaceIdentity, site *skupperv2alpha1.Site, desired []*corev1.Service, expectedSecuredAccesses []*skupperv2alpha1.SecuredAccess) error {
	api := c.clients.GetKubeClient().CoreV1().Services(namespace.Name)
	names := map[string]bool{}
	var failures []error
	for _, value := range desired {
		names[value.Name] = true
		if err := c.verifyAccessOwners(ctx, namespace, site, value.OwnerReferences, nil, expectedSecuredAccesses); err != nil {
			failures = append(failures, fmt.Errorf("Service %s: %w", value.Name, err))
			continue
		}
		current, err := api.Get(ctx, value.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if err := c.verifyAccessOwners(ctx, namespace, site, value.OwnerReferences, nil, expectedSecuredAccesses); err != nil {
				failures = append(failures, fmt.Errorf("Service %s: %w", value.Name, err))
				continue
			}
			if _, err = api.Create(ctx, value, metav1.CreateOptions{}); err != nil {
				failures = append(failures, fmt.Errorf("Service %s: %w", value.Name, classifyWriteError(err)))
			}
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("Service %s: %w", value.Name, classifyWriteError(err)))
			continue
		}
		ownerUID := value.OwnerReferences[0].UID
		if current.Annotations["internal.skupper.io/controlled"] != "true" || !hasOwnerUID(current.OwnerReferences, ownerUID) {
			failures = append(failures, fmt.Errorf("SecuredAccess Service %s/%s is not owned by SecuredAccess UID %s", namespace.Name, value.Name, ownerUID))
			continue
		}
		value.ResourceVersion = current.ResourceVersion
		value.Spec.ClusterIP = current.Spec.ClusterIP
		value.Spec.ClusterIPs = append([]string(nil), current.Spec.ClusterIPs...)
		value.Spec.IPFamilies = append([]corev1.IPFamily(nil), current.Spec.IPFamilies...)
		value.Spec.IPFamilyPolicy = current.Spec.IPFamilyPolicy
		value.Spec.HealthCheckNodePort = current.Spec.HealthCheckNodePort
		value.Spec.ExternalTrafficPolicy = current.Spec.ExternalTrafficPolicy
		value.Spec.AllocateLoadBalancerNodePorts = current.Spec.AllocateLoadBalancerNodePorts
		value.Spec.LoadBalancerClass = current.Spec.LoadBalancerClass
		value.Spec.SessionAffinityConfig = current.Spec.SessionAffinityConfig
		value.Spec.TrafficDistribution = current.Spec.TrafficDistribution
		for index := range value.Spec.Ports {
			for _, actual := range current.Spec.Ports {
				if actual.Name == value.Spec.Ports[index].Name && value.Spec.Ports[index].NodePort == 0 {
					value.Spec.Ports[index].NodePort = actual.NodePort
				}
			}
		}
		if reflect.DeepEqual(current.Spec, value.Spec) && reflect.DeepEqual(current.Labels, value.Labels) && reflect.DeepEqual(current.Annotations, value.Annotations) && reflect.DeepEqual(current.OwnerReferences, value.OwnerReferences) {
			continue
		}
		if err := c.verifyAccessOwners(ctx, namespace, site, value.OwnerReferences, nil, expectedSecuredAccesses); err != nil {
			failures = append(failures, fmt.Errorf("Service %s: %w", value.Name, err))
			continue
		}
		if _, err = api.Update(ctx, value, metav1.UpdateOptions{}); err != nil {
			failures = append(failures, fmt.Errorf("Service %s: %w", value.Name, classifyWriteError(err)))
		}
	}
	current, err := api.List(ctx, metav1.ListOptions{LabelSelector: "internal.skupper.io/secured-access=true"})
	if err != nil {
		return errors.Join(append(failures, classifyWriteError(err))...)
	}
	for i := range current.Items {
		value := &current.Items[i]
		if names[value.Name] || value.Annotations["internal.skupper.io/controlled"] != "true" {
			continue
		}
		owner := metav1.GetControllerOf(value)
		if owner == nil || owner.Kind != "SecuredAccess" {
			continue
		}
		parent := expectedSecuredAccess(expectedSecuredAccesses, owner)
		if parent == nil {
			continue
		}
		if err := c.verifySecuredAccess(ctx, namespace, parent); err != nil {
			failures = append(failures, fmt.Errorf("retire Service %s: %w", value.Name, err))
			continue
		}
		if err := api.Delete(ctx, value.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &value.UID}}); err != nil && !apierrors.IsNotFound(err) {
			failures = append(failures, fmt.Errorf("retire Service %s: %w", value.Name, classifyWriteError(err)))
		}
	}
	return errors.Join(failures...)
}

func (c *NamespaceController) ensureCertificates(ctx context.Context, namespace reconcile.NamespaceIdentity, site *skupperv2alpha1.Site, desired []*skupperv2alpha1.Certificate, expectedSecuredAccesses []*skupperv2alpha1.SecuredAccess) ([]*skupperv2alpha1.Certificate, error) {
	api := c.clients.GetSkupperClient().SkupperV2alpha1().Certificates(namespace.Name)
	result := make([]*skupperv2alpha1.Certificate, 0, len(desired))
	var failures []error
	for _, value := range desired {
		if err := c.verifyAccessOwners(ctx, namespace, site, value.OwnerReferences, nil, expectedSecuredAccesses); err != nil {
			failures = append(failures, fmt.Errorf("Certificate %s: %w", value.Name, err))
			continue
		}
		current, err := api.Get(ctx, value.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if err := c.verifyAccessOwners(ctx, namespace, site, value.OwnerReferences, nil, expectedSecuredAccesses); err != nil {
				failures = append(failures, fmt.Errorf("Certificate %s: %w", value.Name, err))
				continue
			}
			created, err := api.Create(ctx, value, metav1.CreateOptions{})
			if err != nil {
				failures = append(failures, fmt.Errorf("Certificate %s: %w", value.Name, classifyWriteError(err)))
				continue
			}
			result = append(result, created)
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("Certificate %s: %w", value.Name, classifyWriteError(err)))
			continue
		}
		if current.Annotations["internal.skupper.io/controlled"] != "true" && current.Labels["internal.skupper.io/certificate"] != "true" {
			failures = append(failures, fmt.Errorf("Certificate %s/%s exists but is not controlled by Skupper", namespace.Name, value.Name))
			continue
		}
		if !ownerSetsOverlap(current.OwnerReferences, value.OwnerReferences) {
			failures = append(failures, fmt.Errorf("Certificate %s/%s is controlled by different resource identities", namespace.Name, value.Name))
			continue
		}
		if reflect.DeepEqual(current.Spec, value.Spec) && reflect.DeepEqual(current.OwnerReferences, value.OwnerReferences) && reflect.DeepEqual(current.Annotations, value.Annotations) {
			result = append(result, current)
			continue
		}
		value.ResourceVersion = current.ResourceVersion
		if err := c.verifyAccessOwners(ctx, namespace, site, value.OwnerReferences, nil, expectedSecuredAccesses); err != nil {
			failures = append(failures, fmt.Errorf("Certificate %s: %w", value.Name, err))
			continue
		}
		updated, err := api.Update(ctx, value, metav1.UpdateOptions{})
		if err != nil {
			failures = append(failures, fmt.Errorf("Certificate %s: %w", value.Name, classifyWriteError(err)))
			continue
		}
		result = append(result, updated)
	}
	return result, errors.Join(failures...)
}

func (c *NamespaceController) ensureCertificateSecret(ctx context.Context, namespace reconcile.NamespaceIdentity, site *skupperv2alpha1.Site, expected *skupperv2alpha1.Certificate, issuer *corev1.Secret, evaluationTime time.Time) (*corev1.Secret, error) {
	certificate, err := c.clients.GetSkupperClient().SkupperV2alpha1().Certificates(namespace.Name).Get(ctx, expected.Name, metav1.GetOptions{})
	if err != nil {
		return nil, classifyWriteError(err)
	}
	if certificate.UID != expected.UID || certificate.ResourceVersion != expected.ResourceVersion || certificate.Generation != expected.Generation || certificate.DeletionTimestamp != nil {
		return nil, reconcile.SupersededError{Reason: "Certificate changed or is deleting"}
	}
	secrets := c.clients.GetKubeClient().CoreV1().Secrets(namespace.Name)
	current, err := secrets.Get(ctx, certificate.Name, metav1.GetOptions{})
	if err == nil && certificates.SecretCorrectAt(certificate, current, evaluationTime) {
		return current, nil
	}
	missing := apierrors.IsNotFound(err)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, classifyWriteError(err)
	}
	if !missing && !certificates.SecretControlled(current) {
		return nil, fmt.Errorf("certificate Secret %s/%s exists but is not controlled by Skupper", namespace.Name, certificate.Name)
	}
	if !missing && !hasOwnerUID(current.OwnerReferences, certificate.UID) {
		return nil, fmt.Errorf("certificate Secret %s/%s is not owned by Certificate UID %s", namespace.Name, certificate.Name, certificate.UID)
	}
	if !certificate.Spec.Signing && issuer == nil {
		issuer, err = secrets.Get(ctx, certificate.Spec.Ca, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get issuer Secret %s/%s: %w", namespace.Name, certificate.Spec.Ca, err)
		}
	}
	desired, err := certificates.GenerateSecret(certificate, issuer)
	if err != nil {
		return nil, err
	}
	if err := c.verifySite(ctx, namespace, site); err != nil {
		return nil, err
	}
	if err := c.verifyCertificate(ctx, namespace, expected); err != nil {
		return nil, err
	}
	if missing {
		created, err := secrets.Create(ctx, desired, metav1.CreateOptions{})
		return created, classifyWriteError(err)
	}
	desired.ResourceVersion = current.ResourceVersion
	desired.Labels = current.Labels
	for key, value := range current.Annotations {
		if _, exists := desired.Annotations[key]; !exists {
			desired.Annotations[key] = value
		}
	}
	updated, err := secrets.Update(ctx, desired, metav1.UpdateOptions{})
	return updated, classifyWriteError(err)
}

func hasOwnerUID(owners []metav1.OwnerReference, uid types.UID) bool {
	for _, owner := range owners {
		if owner.UID == uid {
			return true
		}
	}
	return false
}

func ownerSetsOverlap(first, second []metav1.OwnerReference) bool {
	for _, left := range first {
		for _, right := range second {
			if left.UID != "" && left.UID == right.UID && left.Kind == right.Kind && left.APIVersion == right.APIVersion {
				return true
			}
		}
	}
	return false
}

func expectedRouterAccess(values []*skupperv2alpha1.RouterAccess, owner *metav1.OwnerReference) *skupperv2alpha1.RouterAccess {
	for _, value := range values {
		if value.Name == owner.Name && value.UID == owner.UID {
			return value
		}
	}
	return nil
}

func expectedSecuredAccess(values []*skupperv2alpha1.SecuredAccess, owner *metav1.OwnerReference) *skupperv2alpha1.SecuredAccess {
	for _, value := range values {
		if value.Name == owner.Name && value.UID == owner.UID {
			return value
		}
	}
	return nil
}

func (c *NamespaceController) verifyRouterAccess(ctx context.Context, namespace reconcile.NamespaceIdentity, expected *skupperv2alpha1.RouterAccess) error {
	current, err := c.clients.GetSkupperClient().SkupperV2alpha1().RouterAccesses(namespace.Name).Get(ctx, expected.Name, metav1.GetOptions{})
	if err != nil {
		return classifyWriteError(err)
	}
	if current.UID != expected.UID || current.ResourceVersion != expected.ResourceVersion || current.Generation != expected.Generation || current.DeletionTimestamp != nil {
		return reconcile.SupersededError{Reason: "RouterAccess changed or is deleting"}
	}
	return nil
}

func (c *NamespaceController) verifySecuredAccess(ctx context.Context, namespace reconcile.NamespaceIdentity, expected *skupperv2alpha1.SecuredAccess) error {
	current, err := c.clients.GetSkupperClient().SkupperV2alpha1().SecuredAccesses(namespace.Name).Get(ctx, expected.Name, metav1.GetOptions{})
	if err != nil {
		return classifyWriteError(err)
	}
	if current.UID != expected.UID || current.ResourceVersion != expected.ResourceVersion || current.Generation != expected.Generation || current.DeletionTimestamp != nil {
		return reconcile.SupersededError{Reason: "SecuredAccess changed or is deleting"}
	}
	return nil
}

func (c *NamespaceController) verifyAccessOwners(ctx context.Context, namespace reconcile.NamespaceIdentity, site *skupperv2alpha1.Site, owners []metav1.OwnerReference, routerAccesses []*skupperv2alpha1.RouterAccess, securedAccesses []*skupperv2alpha1.SecuredAccess) error {
	verified := false
	for _, owner := range owners {
		switch owner.Kind {
		case "Site":
			if site == nil || owner.UID != site.UID {
				return reconcile.SupersededError{Reason: "Site owner changed"}
			}
			if err := c.verifySite(ctx, namespace, site); err != nil {
				return err
			}
			verified = true
		case "RouterAccess":
			matched := false
			for _, expected := range routerAccesses {
				if expected.Name == owner.Name && expected.UID == owner.UID {
					if err := c.verifyRouterAccess(ctx, namespace, expected); err != nil {
						return err
					}
					matched = true
					verified = true
					break
				}
			}
			if !matched {
				return reconcile.SupersededError{Reason: "RouterAccess owner changed"}
			}
		case "SecuredAccess":
			matched := false
			for _, expected := range securedAccesses {
				if expected.Name == owner.Name && expected.UID == owner.UID {
					if err := c.verifySecuredAccess(ctx, namespace, expected); err != nil {
						return err
					}
					matched = true
					verified = true
					break
				}
			}
			if !matched {
				return reconcile.SupersededError{Reason: "SecuredAccess owner changed"}
			}
		}
	}
	if verified {
		return nil
	}
	return reconcile.SupersededError{Reason: "access resource has no authoritative owner"}
}

func (c *NamespaceController) verifyCertificate(ctx context.Context, namespace reconcile.NamespaceIdentity, expected *skupperv2alpha1.Certificate) error {
	current, err := c.clients.GetSkupperClient().SkupperV2alpha1().Certificates(namespace.Name).Get(ctx, expected.Name, metav1.GetOptions{})
	if err != nil {
		return classifyWriteError(err)
	}
	if current.UID != expected.UID || current.ResourceVersion != expected.ResourceVersion || current.Generation != expected.Generation || current.DeletionTimestamp != nil {
		return reconcile.SupersededError{Reason: "Certificate changed or is deleting"}
	}
	return nil
}

func (c *NamespaceController) ApplyStatuses(ctx context.Context, namespace reconcile.NamespaceIdentity, projection reconcile.StatusProjection) error {
	if err := c.verifySite(ctx, namespace, projection.Owner); err != nil {
		return err
	}
	check := func(uid types.UID, resourceVersion string, generation int64, current metav1.Object) error {
		if current.GetUID() != uid || current.GetResourceVersion() != resourceVersion || current.GetGeneration() != generation || current.GetDeletionTimestamp() != nil {
			return reconcile.SupersededError{Reason: "status resource changed or is deleting"}
		}
		return nil
	}
	api := c.clients.GetSkupperClient().SkupperV2alpha1()
	for _, desired := range projection.Listeners {
		current, err := api.Listeners(namespace.Name).Get(ctx, desired.Name, metav1.GetOptions{})
		if err != nil {
			return classifyWriteError(err)
		}
		if err := check(desired.UID, desired.ResourceVersion, desired.Generation, current); err != nil {
			return err
		}
		desired.ResourceVersion = current.ResourceVersion
		if _, err := api.Listeners(namespace.Name).UpdateStatus(ctx, desired, metav1.UpdateOptions{}); err != nil {
			return classifyWriteError(err)
		}
	}
	for _, desired := range projection.MultiKey {
		current, err := api.MultiKeyListeners(namespace.Name).Get(ctx, desired.Name, metav1.GetOptions{})
		if err != nil {
			return classifyWriteError(err)
		}
		if err := check(desired.UID, desired.ResourceVersion, desired.Generation, current); err != nil {
			return err
		}
		desired.ResourceVersion = current.ResourceVersion
		if _, err := api.MultiKeyListeners(namespace.Name).UpdateStatus(ctx, desired, metav1.UpdateOptions{}); err != nil {
			return classifyWriteError(err)
		}
	}
	for _, desired := range projection.Connectors {
		current, err := api.Connectors(namespace.Name).Get(ctx, desired.Name, metav1.GetOptions{})
		if err != nil {
			return classifyWriteError(err)
		}
		if err := check(desired.UID, desired.ResourceVersion, desired.Generation, current); err != nil {
			return err
		}
		desired.ResourceVersion = current.ResourceVersion
		if _, err := api.Connectors(namespace.Name).UpdateStatus(ctx, desired, metav1.UpdateOptions{}); err != nil {
			return classifyWriteError(err)
		}
	}
	for _, desired := range projection.Links {
		current, err := api.Links(namespace.Name).Get(ctx, desired.Name, metav1.GetOptions{})
		if err != nil {
			return classifyWriteError(err)
		}
		if err := check(desired.UID, desired.ResourceVersion, desired.Generation, current); err != nil {
			return err
		}
		desired.ResourceVersion = current.ResourceVersion
		if _, err := api.Links(namespace.Name).UpdateStatus(ctx, desired, metav1.UpdateOptions{}); err != nil {
			return classifyWriteError(err)
		}
	}
	for _, desired := range projection.Accesses {
		current, err := api.RouterAccesses(namespace.Name).Get(ctx, desired.Name, metav1.GetOptions{})
		if err != nil {
			return classifyWriteError(err)
		}
		if err := check(desired.UID, desired.ResourceVersion, desired.Generation, current); err != nil {
			return err
		}
		desired.ResourceVersion = current.ResourceVersion
		if _, err := api.RouterAccesses(namespace.Name).UpdateStatus(ctx, desired, metav1.UpdateOptions{}); err != nil {
			return classifyWriteError(err)
		}
	}
	for _, desired := range projection.SecuredAccesses {
		current, err := api.SecuredAccesses(namespace.Name).Get(ctx, desired.Name, metav1.GetOptions{})
		if err != nil {
			return classifyWriteError(err)
		}
		if err := check(desired.UID, desired.ResourceVersion, desired.Generation, current); err != nil {
			return err
		}
		desired.ResourceVersion = current.ResourceVersion
		if _, err := api.SecuredAccesses(namespace.Name).UpdateStatus(ctx, desired, metav1.UpdateOptions{}); err != nil {
			return classifyWriteError(err)
		}
	}
	for _, desired := range projection.Certificates {
		current, err := api.Certificates(namespace.Name).Get(ctx, desired.Name, metav1.GetOptions{})
		if err != nil {
			return classifyWriteError(err)
		}
		if err := check(desired.UID, desired.ResourceVersion, desired.Generation, current); err != nil {
			return err
		}
		desired.ResourceVersion = current.ResourceVersion
		if _, err := api.Certificates(namespace.Name).UpdateStatus(ctx, desired, metav1.UpdateOptions{}); err != nil {
			return classifyWriteError(err)
		}
	}
	for _, desired := range projection.Bindings {
		current, err := api.AttachedConnectorBindings(namespace.Name).Get(ctx, desired.Name, metav1.GetOptions{})
		if err != nil {
			return classifyWriteError(err)
		}
		if err := check(desired.UID, desired.ResourceVersion, desired.Generation, current); err != nil {
			return err
		}
		desired.ResourceVersion = current.ResourceVersion
		if _, err := api.AttachedConnectorBindings(namespace.Name).UpdateStatus(ctx, desired, metav1.UpdateOptions{}); err != nil {
			return classifyWriteError(err)
		}
	}
	for _, desired := range projection.Attached {
		expectedNamespaceUID, found := projection.SourceNamespaces[desired.Namespace]
		if !found {
			return reconcile.SupersededError{Reason: "attached source namespace identity is missing"}
		}
		if err := c.verifyControlledNamespace(ctx, desired.Namespace, expectedNamespaceUID); err != nil {
			return err
		}
		current, err := api.AttachedConnectors(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
		if err != nil {
			return classifyWriteError(err)
		}
		if err := check(desired.UID, desired.ResourceVersion, desired.Generation, current); err != nil {
			return err
		}
		desired.ResourceVersion = current.ResourceVersion
		if _, err := api.AttachedConnectors(desired.Namespace).UpdateStatus(ctx, desired, metav1.UpdateOptions{}); err != nil {
			return classifyWriteError(err)
		}
	}
	for _, desired := range projection.Sites {
		current, err := api.Sites(namespace.Name).Get(ctx, desired.Name, metav1.GetOptions{})
		if err != nil {
			return classifyWriteError(err)
		}
		if err := check(desired.UID, desired.ResourceVersion, desired.Generation, current); err != nil {
			return err
		}
		desired.ResourceVersion = current.ResourceVersion
		if _, err := api.Sites(namespace.Name).UpdateStatus(ctx, desired, metav1.UpdateOptions{}); err != nil {
			return classifyWriteError(err)
		}
	}
	return nil
}

func (c *NamespaceController) verifyControlledNamespace(ctx context.Context, namespace string, expectedUID types.UID) error {
	current, err := c.clients.GetKubeClient().CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		return classifyWriteError(err)
	}
	if current.UID != expectedUID {
		return reconcile.SupersededError{Reason: "source namespace UID changed"}
	}
	config, err := c.clients.GetKubeClient().CoreV1().ConfigMaps(namespace).Get(ctx, namespaceConfigName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		config = nil
	} else if err != nil {
		return classifyWriteError(err)
	}
	if !ControlsNamespace(config, namespace, c.controllerID, c.requireExplicitControl) {
		return reconcile.SupersededError{Reason: "source namespace controller assignment changed"}
	}
	return nil
}

func (c *NamespaceController) validateWorkloadOwnership(ctx context.Context, site *skupperv2alpha1.Site, groups []string) error {
	for _, group := range groups {
		deployment, err := c.clients.GetKubeClient().AppsV1().Deployments(site.Namespace).Get(ctx, group, metav1.GetOptions{})
		if err == nil && !metav1.IsControlledBy(deployment, site) {
			return fmt.Errorf("router Deployment %s/%s is not controlled by Site UID %s", site.Namespace, group, site.UID)
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return classifyWriteError(err)
		}
	}
	service, err := c.clients.GetKubeClient().CoreV1().Services(site.Namespace).Get(ctx, "skupper-router-local", metav1.GetOptions{})
	if err == nil && !ownedByUID(service.OwnerReferences, site.UID) {
		return fmt.Errorf("router Service %s/%s is not owned by Site UID %s", site.Namespace, service.Name, site.UID)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return classifyWriteError(err)
	}
	return nil
}

func (c *NamespaceController) retireSiteWorkloads(ctx context.Context, site *skupperv2alpha1.Site, groups []string) error {
	desired := map[string]bool{}
	for _, group := range groups {
		desired[group] = true
	}
	deployments, err := c.clients.GetKubeClient().AppsV1().Deployments(site.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "application=skupper-router"})
	if err != nil {
		return classifyWriteError(err)
	}
	for i := range deployments.Items {
		deployment := &deployments.Items[i]
		if desired[deployment.Name] || !metav1.IsControlledBy(deployment, site) {
			continue
		}
		if err := c.clients.GetKubeClient().AppsV1().Deployments(site.Namespace).Delete(ctx, deployment.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &deployment.UID}}); err != nil && !apierrors.IsNotFound(err) {
			return classifyWriteError(err)
		}
	}
	return nil
}

func ownedByUID(owners []metav1.OwnerReference, uid types.UID) bool {
	for _, owner := range owners {
		if owner.UID == uid && owner.Kind == "Site" && owner.APIVersion == skupperv2alpha1.SchemeGroupVersion.String() {
			return true
		}
	}
	return false
}

func (c *NamespaceController) verifySite(ctx context.Context, namespace reconcile.NamespaceIdentity, expected *skupperv2alpha1.Site) error {
	if expected == nil {
		return c.verifyControlledNamespace(ctx, namespace.Name, namespace.UID)
	}
	currentNamespace, err := c.clients.GetKubeClient().CoreV1().Namespaces().Get(ctx, namespace.Name, metav1.GetOptions{})
	if err != nil {
		return classifyWriteError(err)
	}
	if currentNamespace.UID != namespace.UID {
		return reconcile.SupersededError{Reason: "namespace UID changed"}
	}
	config, err := c.clients.GetKubeClient().CoreV1().ConfigMaps(namespace.Name).Get(ctx, namespaceConfigName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		config = nil
	} else if err != nil {
		return classifyWriteError(err)
	}
	if !ControlsNamespace(config, namespace.Name, c.controllerID, c.requireExplicitControl) {
		return reconcile.SupersededError{Reason: "namespace controller assignment changed"}
	}
	current, err := c.clients.GetSkupperClient().SkupperV2alpha1().Sites(namespace.Name).Get(ctx, expected.Name, metav1.GetOptions{})
	if err != nil {
		return classifyWriteError(err)
	}
	if current.UID != expected.UID || current.ResourceVersion != expected.ResourceVersion || current.Generation != expected.Generation || current.DeletionTimestamp != nil {
		return reconcile.SupersededError{Reason: "active Site changed or is deleting"}
	}
	return nil
}

func classifyWriteError(err error) error {
	if err == nil || apierrors.IsConflict(err) || apierrors.IsInvalid(err) || apierrors.IsForbidden(err) {
		return err
	}
	if apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) {
		return reconcile.Ambiguous(err)
	}
	return err
}
