package client

import (
	"context"

	"github.com/c-kruse/skupper/pkg/qdr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/c-kruse/skupper/api/types"
	kubeqdr "github.com/c-kruse/skupper/pkg/kube/qdr"
)

// ConnectorInspect VAN connector instance
func (cli *VanClient) ConnectorInspect(ctx context.Context, name string) (*types.LinkStatus, error) {
	current, err := cli.getRouterConfig(ctx, "")
	if err != nil {
		return nil, err
	}
	secret, err := cli.KubeClient.CoreV1().Secrets(cli.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	connections, _ := kubeqdr.GetConnections(cli.Namespace, cli.KubeClient, cli.RestConfig)
	link := qdr.GetLinkStatus(secret, current.IsEdge(), connections)
	return &link, nil
}
