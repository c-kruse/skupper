package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/c-kruse/vanflow/session"
	"github.com/skupperproject/skupper/api/types"
	"github.com/skupperproject/skupper/client"
	"github.com/skupperproject/skupper/pkg/config"
	"github.com/skupperproject/skupper/pkg/flow"
	"github.com/skupperproject/skupper/pkg/kube"
	"github.com/skupperproject/skupper/pkg/network"
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

func siteCollector(stopCh <-chan struct{}, cli *client.VanClient) {
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

	cf := session.NewContainerFactory("amqp://localhost:5672", session.ContainerConfig{
		ContainerID: "configsync-collector-" + utils.RandomId(16),
	})
	tc := status.NewFlowStatusCollector(cf)
	var (
		netUpdateCt       int
		firstStatusUpdate bool
		startTime         time.Time = time.Now()
	)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer cancel()
		<-stopCh
		log.Printf("FlowStatusCollector shutting down")
	}()
	tc.Run(ctx, func(info network.NetworkStatusInfo) {
		ctx, cancel := context.WithTimeout(ctx, time.Second*5)
		defer cancel()
		bs, err := json.Marshal(info)
		if err != nil {
			log.Printf("FlowStatusCollector error decoding network status info: %s", err)
			return
		}
		networkStatus := string(bs)
		current, err := cli.KubeClient.CoreV1().ConfigMaps(cli.Namespace).Get(ctx, types.NetworkStatusConfigMapName, metav1.GetOptions{})
		if err != nil {
			log.Printf("FlowStatusCollector error retrieveing configmap: %s", err)
		}
		current.Data = map[string]string{"NetworkStatus": networkStatus}
		_, err = cli.KubeClient.CoreV1().ConfigMaps(cli.Namespace).Update(ctx, current, metav1.UpdateOptions{})
		if err != nil {
			log.Printf("FlowStatusCollector error decoding network status info: %s", err)
		} else {
			netUpdateCt++
		}
		if !firstStatusUpdate && len(info.SiteStatus) > 0 && len(info.SiteStatus[0].RouterStatus) > 0 {
			firstStatusUpdate = true
			log.Printf("FlowStatusCollector: First functional network status update written after %s and %d updates\n", time.Since(startTime), netUpdateCt)
		}
		log.Printf("FlowStatusCollector Updated skupper-network-status: %s\n", networkStatus)
	})
}

// primeBeacons attempts to guess the router and service-controller vanflow IDs
// to pass to the flow collector in order to accelerate startup time.
func primeBeacons(fc *flow.FlowCollector, cli *client.VanClient) {
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
	log.Printf("COLLECTOR: Priming site with expected beacons for '%s' and '%s'\n", prospectRouterID, siteID)
	fc.PrimeSiteBeacons(siteID, prospectRouterID)
}

func runLeaderElection(lock *resourcelock.ConfigMapLock, ctx context.Context, id string, cli *client.VanClient) {
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
