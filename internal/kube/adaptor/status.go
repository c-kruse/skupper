package adaptor

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	kubeqdr "github.com/skupperproject/skupper/internal/kube/qdr"
	"github.com/skupperproject/skupper/internal/qdr"
	"github.com/skupperproject/skupper/internal/routerstatus"
	"github.com/skupperproject/skupper/pkg/vanflow/session"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/retry"
)

// StatusPublisher publishes observations only while this pod owns its group's lease.
// ConfigSync supplies immutable snapshots after each attempted configuration sync.
type StatusPublisher struct {
	cli     *internalclient.KubeClient
	mu      sync.Mutex
	desired *qdr.RouterConfig
	adaptor qdr.AdaptorConfig
	applied status.Applied
	next    chan struct{}
	limiter *rate.Limiter
}

func NewStatusPublisher(cli *internalclient.KubeClient) *StatusPublisher {
	return &StatusPublisher{
		cli: cli, next: make(chan struct{}, 1),
		// Allow startup observations to converge quickly, then sustain at most
		// one attempt every five seconds. Keep the budget across leadership changes.
		limiter: rate.NewLimiter(rate.Every(5*time.Second), 3),
	}
}

func (p *StatusPublisher) Notify() {
	select {
	case p.next <- struct{}{}:
	default:
	}
}

func (p *StatusPublisher) ConfigUpdated(cm *corev1.ConfigMap, syncErr error) {
	data, err := kubeqdr.GetRouterConfigData(cm)
	if err != nil {
		slog.Error("Reading status configuration", "error", err)
		return
	}
	desired, err := qdr.UnmarshalRouterConfig(data)
	if err != nil {
		slog.Error("Parsing status configuration", "error", err)
		return
	}
	adaptor, err := kubeqdr.GetAdaptorConfigFromConfigMap(cm)
	if err != nil {
		slog.Error("Parsing adaptor configuration", "error", err)
		return
	}
	applied := status.Applied{ResourceVersion: cm.ResourceVersion}
	if syncErr != nil {
		applied.Error = syncErr.Error()
	}
	p.mu.Lock()
	p.desired, p.adaptor, p.applied = &desired, adaptor, applied
	p.mu.Unlock()
	p.Notify()
}

func (p *StatusPublisher) Run(ctx context.Context) {
	hostname, _ := os.Hostname()
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: deploymentName() + "-status-leader", Namespace: p.cli.Namespace},
		Client:     p.cli.Kube.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: hostname},
	}
	for ctx.Err() == nil {
		done := make(chan struct{})
		leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
			Lock: lock, LeaseDuration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(leaderCtx context.Context) { defer close(done); p.publishLoop(leaderCtx, hostname) },
				OnStoppedLeading: func() {},
			},
		})
		// The election cancels the callback context before returning. Wait for
		// management and API requests to stop before attempting leadership again.
		select {
		case <-done:
		case <-ctx.Done():
			return
		}
	}
}

func (p *StatusPublisher) publishLoop(ctx context.Context, hostname string) {
	index := status.NewNetworkIndex(session.NewContainerFactory("amqp://localhost:5672", session.ContainerConfig{ContainerID: "router-status-" + hostname}), p.Notify)
	go index.Run(ctx)
	var agent *qdr.Agent
	defer func() {
		if agent != nil {
			agent.Close()
		}
	}()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	p.Notify()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-p.next:
		}
		if err := p.limiter.Wait(ctx); err != nil {
			return
		}
		p.mu.Lock()
		desired, adaptor, applied := p.desired, p.adaptor, p.applied
		p.mu.Unlock()
		if desired == nil {
			continue
		}
		if agent == nil {
			var err error
			agent, err = qdr.ConnectTimeout("amqp://localhost:5672", nil, 5*time.Second)
			if err != nil {
				if agent != nil {
					agent.Close()
					agent = nil
				}
				slog.Error("Connecting for router status", "error", err)
				continue
			}
		}
		builder := status.Builder{Client: agent, Group: deploymentName(), Hostname: hostname}
		doc, err := builder.Build(ctx, desired, adaptor, applied, index)
		if err == nil {
			err = p.publish(ctx, &doc)
		} else {
			agent.Close()
			agent = nil
		}
		if err != nil && ctx.Err() == nil {
			slog.Error("Publishing router status", "error", err)
		}
	}
}

func (p *StatusPublisher) publish(ctx context.Context, doc *status.Document) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	payload, err := status.Encode(doc)
	if err != nil {
		return err
	}
	if len(payload) > 1024*1024 {
		return fmt.Errorf("compressed router status exceeds 1 MiB")
	}
	deployment, err := p.cli.Kube.AppsV1().Deployments(p.cli.Namespace).Get(ctx, doc.Group, metav1.GetOptions{})
	if err != nil {
		return err
	}
	owner := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: deployment.Name, UID: deployment.UID}
	client := p.cli.Kube.CoreV1().ConfigMaps(p.cli.Namespace)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		cm, err := client.Get(ctx, doc.Group+"-status", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if err := ctx.Err(); err != nil {
				return err
			}
			_, err = client.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name: doc.Group + "-status", Namespace: p.cli.Namespace,
				Labels:          map[string]string{status.ConfigMapLabel: "", status.GroupLabel: doc.Group},
				OwnerReferences: []metav1.OwnerReference{owner},
			}, BinaryData: map[string][]byte{status.DataKey: payload}}, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		if bytes.Equal(cm.BinaryData[status.DataKey], payload) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		cm = cm.DeepCopy()
		if cm.BinaryData == nil {
			cm.BinaryData = map[string][]byte{}
		}
		cm.BinaryData[status.DataKey] = payload
		_, err = client.Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
}
