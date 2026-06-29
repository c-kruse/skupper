package adaptor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/skupperproject/skupper/api/types"
	"github.com/skupperproject/skupper/internal/config"
	"github.com/skupperproject/skupper/internal/flow"
	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	kubeflow "github.com/skupperproject/skupper/internal/kube/flow"
	"github.com/skupperproject/skupper/internal/network"
	"github.com/skupperproject/skupper/internal/version"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	applyconfigurationskupperv2alpha1 "github.com/skupperproject/skupper/pkg/generated/client/applyconfiguration/skupper/v2alpha1"
	skupperclient "github.com/skupperproject/skupper/pkg/generated/client/clientset/versioned"
	"github.com/skupperproject/skupper/pkg/vanflow"
	"github.com/skupperproject/skupper/pkg/vanflow/session"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1informer "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

func siteCollector(ctx context.Context, cli *internalclient.KubeClient) {
	platform := config.GetPlatform()
	if platform != types.PlatformKubernetes {
		return
	}
	current, err := cli.Kube.AppsV1().Deployments(cli.Namespace).Get(context.TODO(), deploymentName(), metav1.GetOptions{})
	if err != nil {
		slog.Error("Failed to get transport deployment", slog.Any("error", err))
		os.Exit(1)
	}
	if len(current.OwnerReferences) < 1 {
		slog.Error("transport deployment had no owner required to infer site name")
		os.Exit(1)
	}
	siteName := current.OwnerReferences[0].Name

	factory := session.NewContainerFactory("amqp://localhost:5672", session.ContainerConfig{ContainerID: "kube-flow-collector"})
	publishSiteStatus := func(ctx context.Context, info network.NetworkStatusInfo) error {
		return applySiteNetworkStatus(ctx, cli.GetSkupperClient(), cli.Namespace, siteName, info)
	}
	statusSync := flow.NewStatusSync(factory, nil, nil, "", flow.WithPublishHandler(publishSiteStatus))
	go statusSync.Run(ctx)
}

const fieldManagerNetworkStatus = "skupper-kube-adaptor"

func applySiteNetworkStatus(ctx context.Context, client skupperclient.Interface, namespace, siteName string, info network.NetworkStatusInfo) error {
	records := network.ExtractSiteRecords(info)
	status := applyconfigurationskupperv2alpha1.SiteStatus().
		WithSitesInNetwork(len(records))
	for _, record := range records {
		status.WithNetwork(siteRecordApplyConfiguration(record))
	}
	site := applyconfigurationskupperv2alpha1.Site(siteName, namespace).WithStatus(status)
	_, err := client.SkupperV2alpha1().Sites(namespace).ApplyStatus(ctx, site, metav1.ApplyOptions{
		FieldManager: fieldManagerNetworkStatus,
		Force:        true,
	})
	return err
}

func siteRecordApplyConfiguration(record skupperv2alpha1.SiteRecord) *applyconfigurationskupperv2alpha1.SiteRecordApplyConfiguration {
	rec := applyconfigurationskupperv2alpha1.SiteRecord().
		WithId(record.Id).
		WithName(record.Name).
		WithNamespace(record.Namespace).
		WithPlatform(record.Platform).
		WithVersion(record.Version)
	for _, link := range record.Links {
		rec.WithLinks(applyconfigurationskupperv2alpha1.LinkRecord().
			WithName(link.Name).
			WithRemoteSiteId(link.RemoteSiteId).
			WithRemoteSiteName(link.RemoteSiteName).
			WithOperational(link.Operational))
	}
	for _, service := range record.Services {
		svc := applyconfigurationskupperv2alpha1.ServiceRecord().
			WithRoutingKey(service.RoutingKey)
		if len(service.Connectors) > 0 {
			svc.WithConnectors(service.Connectors...)
		}
		if len(service.Listeners) > 0 {
			svc.WithListeners(service.Listeners...)
		}
		rec.WithServices(svc)
	}
	return rec
}

func startFlowController(ctx context.Context, cli *internalclient.KubeClient) error {
	deployment, err := cli.Kube.AppsV1().Deployments(cli.Namespace).Get(ctx, deploymentName(), metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get transport deployment: %s", err)
	}
	if len(deployment.OwnerReferences) < 1 {
		return fmt.Errorf("transport deployment had no owner required to infer site name and ID")
	}
	siteID := string(deployment.OwnerReferences[0].UID)
	siteName := deployment.OwnerReferences[0].Name

	informer := corev1informer.NewPodInformer(cli.Kube, cli.Namespace, time.Minute*5, cache.Indexers{})
	platform := "kubernetes"
	fc := kubeflow.NewController(kubeflow.ControllerConfig{
		Factory:  session.NewContainerFactory("amqp://localhost:5672", session.ContainerConfig{ContainerID: "kube-flow-controller"}),
		Informer: informer,
		Site: vanflow.SiteRecord{
			BaseRecord: vanflow.NewBase(siteID, deployment.ObjectMeta.CreationTimestamp.Time),
			Name:       &siteName,
			Namespace:  &cli.Namespace,
			Platform:   &platform,
			Version:    &version.Version,
			Provider:   &platform, //todo(ck) Not really correct. involved with nodes access (below)
		},
	})
	go informer.Run(ctx.Done())
	//TODO: should watching nodes be optional or should we attempt to determine if we have permissions first?
	//kubeflow.WatchNodes(controller, cli.Namespace, flowController.UpdateHost)
	go func() {
		fc.Run(ctx)
		if ctx.Err() == nil {
			slog.Error("kube flow controller unexpectedly quit")
		}
	}()
	return nil
}

func ensureStartFlowController(ctx context.Context, cli *internalclient.KubeClient) {
	go func() {
		b := backoff.NewExponentialBackOff()
		b.InitialInterval = time.Millisecond * 250
		b.MaxInterval = time.Second * 30
		b.MaxElapsedTime = 0
		b.Reset()

		attempt := 0
		err := backoff.RetryNotify(
			func() error {
				attempt++
				if err := startFlowController(ctx, cli); err != nil {
					return err
				}
				if attempt > 1 {
					slog.Info("COLLECTOR: Site flow controller started after retry", slog.Int("attempt", attempt))
				}
				return nil
			},
			backoff.WithContext(b, ctx),
			func(err error, d time.Duration) {
				if ctx.Err() != nil {
					return
				}
				slog.Error("COLLECTOR: Failed to start controller for emitting site events, retrying",
					slog.Any("error", err),
					slog.Duration("retryAfter", d),
				)
			},
		)
		if err != nil && ctx.Err() == nil {
			slog.Error("COLLECTOR: Stopped retrying site flow controller start", slog.Any("error", err))
		}
	}()
}

func runLeaderElection(lock *resourcelock.LeaseLock, id string, cli *internalclient.KubeClient) {
	var (
		mu              sync.Mutex
		leaderCtx       context.Context
		leaderCtxCancel func()
	)
	// attempt to run leader election forever
	strategy := backoff.NewExponentialBackOff(backoff.WithMaxElapsedTime(0))
	backoff.RetryNotify(func() error {
		leaderelection.RunOrDie(context.Background(), leaderelection.LeaderElectionConfig{
			Lock:            lock,
			ReleaseOnCancel: true,
			LeaseDuration:   15 * time.Second,
			RenewDeadline:   10 * time.Second,
			RetryPeriod:     2 * time.Second,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(ctx context.Context) {
					mu.Lock()
					defer mu.Unlock()
					leaderCtx, leaderCtxCancel = context.WithCancel(ctx)
					slog.Info("COLLECTOR: Became leader. Starting status sync and site controller", slog.Any("elapsedTime", strategy.GetElapsedTime()))
					siteCollector(leaderCtx, cli)
					ensureStartFlowController(leaderCtx, cli)
				},
				OnStoppedLeading: func() {
					slog.Info("COLLECTOR: Lost leader lock. Stopping status sync and site controller", slog.Any("elapsedTime", strategy.GetElapsedTime()))
					mu.Lock()
					defer mu.Unlock()
					if leaderCtxCancel == nil {
						return
					}
					leaderCtxCancel()
					leaderCtx, leaderCtxCancel = nil, nil
				},
				OnNewLeader: func(current_id string) {
					if current_id == id {
						// Remain as the leader
						return
					}
					slog.Info("COLLECTOR: New leader for site collection", slog.String("newLeader", current_id))
				},
			},
		})
		return fmt.Errorf("leader election died")
	},
		strategy,
		func(_ error, d time.Duration) {
			slog.Info("COLLECTOR: leader election failed. retrying after delay", slog.Any("delay", d))
		})
}

func StartCollector(cli *internalclient.KubeClient) {
	lockname := types.SiteLeaderLockName
	namespace := cli.Namespace
	podname, _ := os.Hostname()

	leaseLock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      lockname,
			Namespace: namespace,
		},
		Client: cli.Kube.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: podname,
		},
	}

	runLeaderElection(leaseLock, podname, cli)
}

func deploymentName() string {
	deployment := os.Getenv("SKUPPER_ROUTER_DEPLOYMENT")
	if deployment == "" {
		return types.TransportDeploymentName
	}
	return deployment

}

