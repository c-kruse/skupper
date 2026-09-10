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
	"k8s.io/client-go/util/retry"
)

// StatusPublisher publishes observations for this pod, without leader election.
// ConfigSync supplies immutable snapshots after each attempted configuration sync.
type StatusPublisher struct {
	cli     *internalclient.KubeClient
	mu      sync.Mutex
	desired *qdr.RouterConfig
	adaptor qdr.AdaptorConfig
	next    chan struct{}
	limiter *rate.Limiter
	podUID  string
}

func NewStatusPublisher(cli *internalclient.KubeClient) *StatusPublisher {
	return &StatusPublisher{
		cli: cli, next: make(chan struct{}, 1),
		podUID: os.Getenv("SKUPPER_POD_UID"),
		// Allow startup observations to converge quickly, then sustain at most
		// one attempt every five seconds.
		limiter: rate.NewLimiter(rate.Every(5*time.Second), 3),
	}
}

func (p *StatusPublisher) Notify() {
	select {
	case p.next <- struct{}{}:
	default:
	}
}

func (p *StatusPublisher) ConfigUpdated(cm *corev1.ConfigMap) {
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
	p.mu.Lock()
	p.desired, p.adaptor = &desired, adaptor
	p.mu.Unlock()
	p.Notify()
}

func (p *StatusPublisher) Run(ctx context.Context) {
	podName := os.Getenv("SKUPPER_POD_NAME")
	if podName == "" || p.podUID == "" {
		slog.Error("Cannot publish router status without SKUPPER_POD_NAME and SKUPPER_POD_UID")
		return
	}
	p.publishLoop(ctx, podName)
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
		desired, adaptor := p.desired, p.adaptor
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
		builder := status.Builder{Client: agent, Hostname: hostname}
		doc, err := builder.Build(ctx, desired, adaptor, index)
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
	pod, err := p.cli.Kube.CoreV1().Pods(p.cli.Namespace).Get(ctx, doc.Router.Hostname, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if pod.DeletionTimestamp != nil || string(pod.UID) != p.podUID {
		return nil
	}
	doc.Router.PodUID = string(pod.UID)
	payload, err := status.Encode(doc)
	if err != nil {
		return err
	}
	if len(payload) > 1024*1024 {
		return fmt.Errorf("compressed router status exceeds 1 MiB")
	}
	owner := metav1.OwnerReference{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID}
	client := p.cli.Kube.CoreV1().ConfigMaps(p.cli.Namespace)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		cm, err := client.Get(ctx, pod.Name+"-status", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if err := ctx.Err(); err != nil {
				return err
			}
			_, err = client.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name: pod.Name + "-status", Namespace: p.cli.Namespace,
				Labels:          map[string]string{status.ConfigMapLabel: ""},
				OwnerReferences: []metav1.OwnerReference{owner},
			}, BinaryData: map[string][]byte{status.DataKey: payload}}, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		if bytes.Equal(cm.BinaryData[status.DataKey], payload) && len(cm.OwnerReferences) == 1 && cm.OwnerReferences[0] == owner {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		cm = cm.DeepCopy()
		cm.OwnerReferences = []metav1.OwnerReference{owner}
		if cm.BinaryData == nil {
			cm.BinaryData = map[string][]byte{}
		}
		cm.BinaryData[status.DataKey] = payload
		_, err = client.Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
}
