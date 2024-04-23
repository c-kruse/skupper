package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/skupperproject/skupper/api/types"
	"github.com/skupperproject/skupper/pkg/network"
	corev1 "k8s.io/api/core/v1"
	corev1informer "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	kubeConfig, err := clientcmd.NewDefaultClientConfigLoadingRules().Load()
	if err != nil {
		panic(err)
	}
	config := clientcmd.NewDefaultClientConfig(*kubeConfig, &clientcmd.ConfigOverrides{})
	namespace, _, err := config.Namespace()
	if err != nil {
		panic(err)
	}
	restcfg, err := config.ClientConfig()
	if err != nil {
		panic(err)
	}

	clientset, err := kubernetes.NewForConfig(restcfg)
	if err != nil {
		panic(err)
	}

	informer := corev1informer.NewFilteredConfigMapInformer(
		clientset,
		namespace,
		time.Second*30,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
		nil)

	prevDiff := "invalid"
	networkStatusKey := namespace + "/skupper-network-status"
	networkStatusV2Key := namespace + "/skupper-network-status-v2"
	reconcile := func() {
		cmobj, exists, err := informer.GetStore().GetByKey(networkStatusKey)
		if !exists || err != nil {
			log.Printf("cannot get cm: %v, exists: %v", err, exists)
			return
		}
		cm := cmobj.(*corev1.ConfigMap)
		cmobj, exists, err = informer.GetStore().GetByKey(networkStatusV2Key)
		if !exists || err != nil {
			log.Printf("cannot get cm: %v, exists: %v", err, exists)
			return
		}
		cmv2 := cmobj.(*corev1.ConfigMap)

		ns, err := network.UnmarshalSkupperStatus(cm.Data)
		if err != nil {
			log.Printf("cannot unmarshal cm: %v", err)
			return
		}
		nsv2, err := network.UnmarshalSkupperStatus(cmv2.Data)
		if err != nil {
			log.Printf("cannot unmarshal cm v2: %v", err)
			return
		}
		diff := network.Diff(*ns, *nsv2)
		if diff == prevDiff {
			return
		}
		if diff == "" && prevDiff != diff {
			log.Printf("IDENTICAL")
			prevDiff = diff
			return
		}
		log.Printf("DIFF: %s", diff)
		prevDiff = diff
	}

	informer.AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: func(obj any) bool {
			cm, ok := obj.(*corev1.ConfigMap)
			if !ok {
				return false
			}
			return strings.HasPrefix(cm.ObjectMeta.Name, types.NetworkStatusConfigMapName)
		},
		Handler: cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(_, _ interface{}) {
				reconcile()
			},
		},
	})
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(done)
	}()
	go informer.Run(done)
	cache.WaitForCacheSync(done, informer.HasSynced)
	log.Printf("watchers started")
	reconcile()
	<-done
}
