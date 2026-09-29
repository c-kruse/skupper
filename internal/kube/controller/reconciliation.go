package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"

	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	"github.com/skupperproject/skupper/internal/kube/reconcile"
	"github.com/skupperproject/skupper/internal/routercontrol"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	skupperinformers "github.com/skupperproject/skupper/pkg/generated/client/informers/externalversions"
)

const allocationConfigMapName = "skupper-controller-allocations"

// ObservationSource returns an immutable observation view. A missing target is
// unknown, not a complete empty report.
type ObservationSource interface {
	Snapshot(namespace string, evaluationTime time.Time) map[routercontrol.TargetIdentity]reconcile.Observation
}

// NamespaceController owns shared caches and the leader-only namespace queue.
// StartCaches and WaitForCacheSync are safe for standbys and perform no writes.
type NamespaceController struct {
	clients                internalclient.Clients
	controllerID           string
	coreFactory            informers.SharedInformerFactory
	skupperFactory         skupperinformers.SharedInformerFactory
	informers              namespaceInformers
	queue                  *reconcile.Queue
	observations           ObservationSource
	requireExplicitControl bool
	synced                 atomic.Bool
}

type NamespaceControllerOptions struct {
	WatchNamespace         string
	ControllerID           string
	RequireExplicitControl bool
	Workers                int
}

type namespaceInformers struct {
	namespaces        cache.SharedIndexInformer
	configMaps        cache.SharedIndexInformer
	pods              cache.SharedIndexInformer
	services          cache.SharedIndexInformer
	secrets           cache.SharedIndexInformer
	sites             cache.SharedIndexInformer
	listeners         cache.SharedIndexInformer
	multiKeyListeners cache.SharedIndexInformer
	connectors        cache.SharedIndexInformer
	links             cache.SharedIndexInformer
	routerAccesses    cache.SharedIndexInformer
	certificates      cache.SharedIndexInformer
	securedAccesses   cache.SharedIndexInformer
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
	coreFactory := informers.NewSharedInformerFactoryWithOptions(clients.GetKubeClient(), 5*time.Minute, informers.WithNamespace(options.WatchNamespace))
	skupperFactory := skupperinformers.NewSharedInformerFactoryWithOptions(clients.GetSkupperClient(), 5*time.Minute, skupperinformers.WithNamespace(options.WatchNamespace))
	crs := skupperFactory.Skupper().V2alpha1()
	c := &NamespaceController{clients: clients, controllerID: options.ControllerID, coreFactory: coreFactory, skupperFactory: skupperFactory, observations: observations, requireExplicitControl: options.RequireExplicitControl}
	c.informers = namespaceInformers{
		namespaces: coreFactory.Core().V1().Namespaces().Informer(), configMaps: coreFactory.Core().V1().ConfigMaps().Informer(), pods: coreFactory.Core().V1().Pods().Informer(), services: coreFactory.Core().V1().Services().Informer(), secrets: coreFactory.Core().V1().Secrets().Informer(),
		sites: crs.Sites().Informer(), listeners: crs.Listeners().Informer(), multiKeyListeners: crs.MultiKeyListeners().Informer(), connectors: crs.Connectors().Informer(), links: crs.Links().Informer(), routerAccesses: crs.RouterAccesses().Informer(), certificates: crs.Certificates().Informer(), securedAccesses: crs.SecuredAccesses().Informer(), attached: crs.AttachedConnectors().Informer(), bindings: crs.AttachedConnectorBindings().Informer(),
	}
	planner := reconcile.PublicationPlanner{Allocations: c, Publisher: publisher}
	c.queue = reconcile.NewQueue("namespace-reconciliation", options.Workers, reconcile.NamespaceReconciler{Collector: c, Deriver: reconcile.NamespaceDeriver{}, Planner: planner, Executor: reconcile.Executor{}})
	if err := c.registerInvalidations(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *NamespaceController) StartCaches(ctx context.Context) {
	c.coreFactory.Start(ctx.Done())
	c.skupperFactory.Start(ctx.Done())
}

func (c *NamespaceController) WaitForCacheSync(ctx context.Context) error {
	for kind, ok := range c.coreFactory.WaitForCacheSync(ctx.Done()) {
		if !ok {
			return fmt.Errorf("core informer %v did not synchronize", kind)
		}
	}
	for kind, ok := range c.skupperFactory.WaitForCacheSync(ctx.Done()) {
		if !ok {
			return fmt.Errorf("Skupper informer %v did not synchronize", kind)
		}
	}
	c.synced.Store(true)
	return nil
}

func (c *NamespaceController) CachesSynced() bool { return c.synced.Load() }

func (c *NamespaceController) RunLeader(ctx context.Context) error {
	if !c.CachesSynced() {
		return fmt.Errorf("cannot run namespace reconciliation before caches synchronize")
	}
	c.enqueueAll()
	c.queue.RunLeader(ctx)
	return ctx.Err()
}

func (c *NamespaceController) registerInvalidations() error {
	local := []cache.SharedIndexInformer{c.informers.namespaces, c.informers.configMaps, c.informers.services, c.informers.secrets, c.informers.sites, c.informers.listeners, c.informers.multiKeyListeners, c.informers.connectors, c.informers.links, c.informers.routerAccesses, c.informers.certificates, c.informers.securedAccesses}
	for _, informer := range local {
		if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: c.invalidateObject, UpdateFunc: func(old, current interface{}) { c.invalidateObject(old); c.invalidateObject(current) }, DeleteFunc: c.invalidateObject}); err != nil {
			return err
		}
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

func objectFromEvent(value interface{}) metav1.Object {
	if tombstone, ok := value.(cache.DeletedFinalStateUnknown); ok {
		value = tombstone.Obj
	}
	object, _ := value.(metav1.Object)
	return object
}

func (c *NamespaceController) invalidateObject(value interface{}) {
	if object := objectFromEvent(value); object != nil {
		c.queue.Add(object.GetNamespace())
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
	for _, informer := range []cache.SharedIndexInformer{c.informers.configMaps, c.informers.sites, c.informers.listeners, c.informers.multiKeyListeners, c.informers.connectors, c.informers.links, c.informers.routerAccesses, c.informers.certificates, c.informers.securedAccesses, c.informers.attached, c.informers.bindings} {
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
	snapshot := reconcile.Snapshot{Namespace: reconcile.NamespaceIdentity{Name: namespace, UID: ns.UID}, EvaluationTime: evaluationTime, Assignment: c.assignment(namespace), Sites: listNamespace[*skupperv2alpha1.Site](c.informers.sites, namespace), Listeners: listNamespace[*skupperv2alpha1.Listener](c.informers.listeners, namespace), MultiKeyListeners: listNamespace[*skupperv2alpha1.MultiKeyListener](c.informers.multiKeyListeners, namespace), Connectors: listNamespace[*skupperv2alpha1.Connector](c.informers.connectors, namespace), Links: listNamespace[*skupperv2alpha1.Link](c.informers.links, namespace), RouterAccesses: listNamespace[*skupperv2alpha1.RouterAccess](c.informers.routerAccesses, namespace), Certificates: listNamespace[*skupperv2alpha1.Certificate](c.informers.certificates, namespace), SecuredAccesses: listNamespace[*skupperv2alpha1.SecuredAccess](c.informers.securedAccesses, namespace), Bindings: listNamespace[*skupperv2alpha1.AttachedConnectorBinding](c.informers.bindings, namespace), Services: listNamespace[*corev1.Service](c.informers.services, namespace), Secrets: listNamespace[*corev1.Secret](c.informers.secrets, namespace), Allocations: c.allocations(namespace)}
	sources := map[string]bool{namespace: true}
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
	}
	if c.observations != nil {
		snapshot.Observations = c.observations.Snapshot(namespace, evaluationTime)
	} else {
		snapshot.Observations = map[routercontrol.TargetIdentity]reconcile.Observation{}
	}
	return snapshot, nil
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
	controller := value.(*corev1.ConfigMap).Data[controllerSettingKey]
	if !strings.Contains(controller, "/") && controller != "" {
		controller = namespace + "/" + controller
	}
	return reconcile.Assignment{Controller: controller, Controlled: controller == c.controllerID}
}

func (c *NamespaceController) allocations(namespace string) reconcile.AllocationState {
	result := reconcile.AllocationState{Ports: map[string]int{}}
	value, exists, _ := c.informers.configMaps.GetStore().GetByKey(namespace + "/" + allocationConfigMapName)
	if !exists {
		return result
	}
	config := value.(*corev1.ConfigMap)
	result.SiteUID = types.UID(config.Data["siteUID"])
	_ = json.Unmarshal([]byte(config.Data["ports"]), &result.Ports)
	return result
}

func (c *NamespaceController) CommitAllocations(ctx context.Context, namespace reconcile.NamespaceIdentity, allocations reconcile.AllocationState) error {
	currentNamespace, err := c.clients.GetKubeClient().CoreV1().Namespaces().Get(ctx, namespace.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if currentNamespace.UID != namespace.UID {
		return reconcile.SupersededError{Reason: "namespace UID changed"}
	}
	encoded, err := json.Marshal(allocations.Ports)
	if err != nil {
		return err
	}
	configMaps := c.clients.GetKubeClient().CoreV1().ConfigMaps(namespace.Name)
	current, err := configMaps.Get(ctx, allocationConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = configMaps.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: allocationConfigMapName, Labels: map[string]string{"internal.skupper.io/allocation-state": "true"}}, Data: map[string]string{"version": "1", "siteUID": string(allocations.SiteUID), "ports": string(encoded)}}, metav1.CreateOptions{})
		return classifyWriteError(err)
	}
	if err != nil {
		return err
	}
	if current.Data["siteUID"] != "" && current.Data["siteUID"] != string(allocations.SiteUID) {
		return reconcile.SupersededError{Reason: "allocation record belongs to another Site UID"}
	}
	current.Data = map[string]string{"version": "1", "siteUID": string(allocations.SiteUID), "ports": string(encoded)}
	_, err = configMaps.Update(ctx, current, metav1.UpdateOptions{})
	return classifyWriteError(err)
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
