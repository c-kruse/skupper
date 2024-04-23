package main

import (
	"context"
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
)

const v2ConfigMapName = "skupper-network-status-v2"

func siteCollectorV2(ctx context.Context, cli *client.VanClient) {
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

	existing, err := kube.NewConfigMap(v2ConfigMapName, &siteData, nil, nil, &owner, cli.Namespace, cli.KubeClient)
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

	kubeHandler := status.NewKubeHandler(cli.KubeClient.CoreV1().ConfigMaps(cli.Namespace), v2ConfigMapName)

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
