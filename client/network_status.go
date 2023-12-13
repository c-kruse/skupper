package client

import (
	"context"
	"fmt"
	"github.com/c-kruse/skupper/api/types"
	"github.com/c-kruse/skupper/pkg/domain/kube"
	k8s "github.com/c-kruse/skupper/pkg/kube"
	"github.com/c-kruse/skupper/pkg/network"
)

func (cli *VanClient) NetworkStatus(ctx context.Context) (*network.NetworkStatusInfo, error) {

	//Checking if the router has been deployed
	_, err := k8s.GetDeployment(types.TransportDeploymentName, cli.Namespace, cli.KubeClient)
	if err != nil {
		return nil, fmt.Errorf("Skupper is not installed: %s", err)
	}

	configmap, err := k8s.GetConfigMap(types.NetworkStatusConfigMapName, cli.Namespace, cli.KubeClient)
	if err != nil {
		return nil, err
	}

	vanInfo, err := network.UnmarshalSkupperStatus(configmap.Data)
	if err != nil {
		return nil, err
	}

	return vanInfo, nil
}

func (cli *VanClient) GetRemoteLinks(ctx context.Context, siteConfig *types.SiteConfig) ([]*network.RemoteLinkInfo, error) {
	cfg, err := cli.getRouterConfig(ctx, cli.Namespace)
	if err != nil {
		return nil, err
	}
	linkHander := kube.NewLinkHandlerKube(cli.Namespace, siteConfig, cfg, cli.KubeClient, cli.RestConfig)
	return linkHander.RemoteLinks(ctx)
}
