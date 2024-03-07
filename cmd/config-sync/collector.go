package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/c-kruse/vanflow/eventsource"
	"github.com/c-kruse/vanflow/session"
	"github.com/skupperproject/skupper/api/types"
	"github.com/skupperproject/skupper/client"
	"github.com/skupperproject/skupper/pkg/config"
	"github.com/skupperproject/skupper/pkg/kube"
	"github.com/skupperproject/skupper/pkg/status"
	"github.com/skupperproject/skupper/pkg/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

func updateLockOwner(lockname, namespace string, owner *metav1.OwnerReference, cli *client.VanClient) error {
	current, err := cli.KubeClient.CoreV1().ConfigMaps(namespace).Get(context.TODO(), lockname, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if owner != nil {
		current.ObjectMeta.OwnerReferences = []metav1.OwnerReference{
			*owner,
		}
	}
	_, err = cli.KubeClient.CoreV1().ConfigMaps(namespace).Update(context.TODO(), current, metav1.UpdateOptions{})
	return err
}

func siteCollector(ctx context.Context, cli *client.VanClient) {
	siteData := map[string]string{}
	platform := config.GetPlatform()
	if platform != types.PlatformKubernetes {
		return
	}
	current, err := kube.GetDeployment(types.TransportDeploymentName, cli.Namespace, cli.KubeClient)
	if err != nil {
		log.Fatal("Failed to get transport deployment", err.Error())
	}
	owner := kube.GetDeploymentOwnerReference(current)

	existing, err := kube.NewConfigMap(types.NetworkStatusConfigMapName, &siteData, nil, nil, &owner, cli.Namespace, cli.KubeClient)
	if err != nil && existing == nil {
		log.Fatal("Failed to create site status config map ", err.Error())
	}

	err = updateLockOwner(types.SiteLeaderLockName, cli.Namespace, &owner, cli)
	if err != nil {
		log.Println("Update lock error", err.Error())
	}

	factory := session.NewContainerFactory("amqp://localhost:5672", session.ContainerConfig{
		ContainerID: "configsync-collector-" + utils.RandomId(16),
	})
	statusCollector := status.NewFlowStatusCollector(factory)
	go fetchCollectorHints(statusCollector, cli)

	kubeHandler := status.NewKubeHandler(cli.Namespace, cli.KubeClient)
	kubeHandler.Start(ctx)
	statusCollector.Run(ctx, kubeHandler.Handle)
	log.Printf("FlowStatusCollector shut down")
}

// fetchCollectorHints attempts to guess the local controller and router event
// source IDs and push them to the status collector in order to accelerate
// startup time.
func fetchCollectorHints(collector status.StatusCollector, cli *client.VanClient) {
	podname, _ := os.Hostname()
	var prospectRouterID string
	if len(podname) >= 5 {
		prospectRouterID = fmt.Sprintf("%s:0", podname[len(podname)-5:])
	}
	var siteID string
	cm, err := kube.WaitConfigMapCreated(types.SiteConfigMapName, cli.Namespace, cli.KubeClient, 5*time.Second, 250*time.Millisecond)
	if err != nil {
		log.Printf("COLLECTOR: failed to get skupper-site ConfigMap. Proceeding without Site ID. %s\n", err)
	} else if cm != nil {
		siteID = string(cm.ObjectMeta.UID)
	}
	if siteID != "" {
		source := eventsource.Info{
			ID:      siteID,
			Version: 1,
			Type:    "CONTROLLER",
			Address: "mc/sfe." + siteID,
			Direct:  "sfe." + siteID,
		}
		collector.HintEventSource(source)
		log.Printf("COLLECTOR: sent hint for event source %v", source)
	}
	if prospectRouterID != "" {
		source := eventsource.Info{
			ID:      prospectRouterID,
			Version: 1,
			Type:    "ROUTER",
			Address: "mc/sfe." + prospectRouterID,
			Direct:  "sfe." + prospectRouterID,
		}
		collector.HintEventSource(source)
		log.Printf("COLLECTOR: sent hint for event source %v", source)
	}
}

func runLeaderElection(lock *resourcelock.ConfigMapLock, ctx context.Context, id string, cli *client.VanClient) {
	begin := time.Now()
	var (
		leaderCtx context.Context
		cancel    context.CancelCauseFunc
	)
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
				leaderCtx, cancel = context.WithCancelCause(ctx)
				siteCollector(leaderCtx, cli)
			},
			OnStoppedLeading: func() {
				if cancel == nil { //shouldn't happen
					return
				}
				// No longer the leader, transition to inactive
				cancel(errors.New("lost leader election"))
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

func startCollector(cli *client.VanClient) {
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
		Client: cli.KubeClient.CoreV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: podname,
		},
	}

	runLeaderElection(cmLock, ctx, podname, cli)
}
