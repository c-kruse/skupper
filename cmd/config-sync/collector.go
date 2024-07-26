package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"time"

	"github.com/skupperproject/skupper/api/types"
	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	"github.com/skupperproject/skupper/pkg/config"
	"github.com/skupperproject/skupper/pkg/kube"
	kubeflow "github.com/skupperproject/skupper/pkg/kube/flow"
	"github.com/skupperproject/skupper/pkg/vanflow"
	"github.com/skupperproject/skupper/pkg/vanflow/session"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1informer "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

func updateLockOwner(lockname, namespace string, owner *metav1.OwnerReference, cli *internalclient.KubeClient) error {
	current, err := cli.Kube.CoreV1().ConfigMaps(namespace).Get(context.TODO(), lockname, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if owner != nil {
		current.ObjectMeta.OwnerReferences = []metav1.OwnerReference{
			*owner,
		}
	}
	_, err = cli.Kube.CoreV1().ConfigMaps(namespace).Update(context.TODO(), current, metav1.UpdateOptions{})
	return err
}

func siteCollector(stopCh <-chan struct{}, cli *internalclient.KubeClient) {
	siteData := map[string]string{}
	platform := config.GetPlatform()
	if platform != types.PlatformKubernetes {
		return
	}
	current, err := kube.GetDeployment(deploymentName(), cli.Namespace, cli.Kube)
	if err != nil {
		log.Fatal("Failed to get transport deployment", err.Error())
	}
	owner := kube.GetDeploymentOwnerReference(current)

	existing, err := kube.NewConfigMap(types.NetworkStatusConfigMapName, &siteData, nil, nil, &owner, cli.Namespace, cli.Kube)
	if err != nil && existing == nil {
		log.Fatal("Failed to create site status config map ", err.Error())
	}

	err = updateLockOwner(types.SiteLeaderLockName, cli.Namespace, &owner, cli)
	if err != nil {
		log.Println("Update lock error", err.Error())
	}

	factory := session.NewContainerFactory("amqp://localhost:5672", session.ContainerConfig{ContainerID: "kube-flow-collector"})
	statusSync := kubeflow.NewStatusSync(factory, nil, cli.Kube.CoreV1().ConfigMaps(cli.Namespace), types.NetworkStatusConfigMapName)
	go statusSync.Run(context.Background())

}

func startFlowController(stopCh <-chan struct{}, cli *internalclient.KubeClient) error {
	siteId := os.Getenv("SKUPPER_SITE_ID")

	deployment, err := kube.GetDeployment(deploymentName(), cli.Namespace, cli.Kube)
	if err != nil {
		log.Fatal("Failed to get transport deployment", err.Error())
	}

	informer := corev1informer.NewPodInformer(cli.Kube, cli.Namespace, time.Minute*5, cache.Indexers{})
	platform := "kubernetes"
	fc := kubeflow.NewController(kubeflow.ControllerConfig{
		Factory:  session.NewContainerFactory("amqp://localhost:5672", session.ContainerConfig{ContainerID: "kube-flow-controller"}),
		Informer: informer,
		Site: vanflow.SiteRecord{
			BaseRecord: vanflow.NewBase(siteId, deployment.ObjectMeta.CreationTimestamp.Time),
			Name:       &deployment.Name,
			Namespace:  &cli.Namespace,
			Platform:   &platform,
		},
	})
	go informer.Run(stopCh)
	//TODO: should watching nodes be optional or should we attempt to determine if we have permissions first?
	//kubeflow.WatchNodes(controller, cli.Namespace, flowController.UpdateHost)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-stopCh
		cancel()
	}()
	go func() {
		defer cancel()
		fc.Run(ctx)
		if ctx.Err() == nil {
			slog.Error("kube flow controller unexpectedly quit")
		}
	}()
	return nil
}

func runLeaderElection(lock *resourcelock.ConfigMapLock, ctx context.Context, id string, cli *internalclient.KubeClient) {
	begin := time.Now()
	var stopCh chan struct{}
	podname, _ := os.Hostname()
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(c context.Context) {
				log.Printf("COLLECTOR: Leader %s starting site collection after %s\n", podname, time.Since(begin))
				stopCh = make(chan struct{})
				siteCollector(stopCh, cli)
				if err := startFlowController(stopCh, cli); err != nil {
					log.Printf("COLLECTOR: Failed to start controller for emitting site events: %s", err)
				}
			},
			OnStoppedLeading: func() {
				// No longer the leader, transition to inactive
				close(stopCh)
			},
			OnNewLeader: func(current_id string) {
				if current_id == id {
					// Remain as the leader
					return
				}
				log.Printf("COLLECTOR: New leader for site collection is %s\n", current_id)
			},
		},
	})
}

func startCollector(cli *internalclient.KubeClient) {
	lockname := types.SiteLeaderLockName
	namespace := cli.Namespace
	podname, _ := os.Hostname()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmLock := &resourcelock.ConfigMapLock{
		ConfigMapMeta: metav1.ObjectMeta{
			Name:      lockname,
			Namespace: namespace,
		},
		Client: cli.Kube.CoreV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: podname,
		},
	}

	runLeaderElection(cmLock, ctx, podname, cli)
}

func deploymentName() string {
	deployment := os.Getenv("SKUPPER_ROUTER_DEPLOYMENT")
	if deployment == "" {
		return types.TransportDeploymentName
	}
	return deployment

}
