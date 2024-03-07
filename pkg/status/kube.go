package status

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/skupperproject/skupper/api/types"
	"github.com/skupperproject/skupper/pkg/network"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

func NewKubeHandler(namespace string, client kubernetes.Interface) StatusUpdateHandler {
	return &kubeHandler{
		client:    client,
		namespace: namespace,
		hasNext:   make(chan struct{}, 1),
	}
}

type kubeHandler struct {
	client    kubernetes.Interface
	namespace string

	mu      sync.Mutex
	hasNext chan struct{}
	next    network.NetworkStatusInfo
}

func (h *kubeHandler) Start(ctx context.Context) {
	go func() {
		var (
			netUpdateCt       int
			firstStatusUpdate bool
			startTime         time.Time = time.Now()
		)
		for {
			select {
			case <-ctx.Done():
				return
			case <-h.hasNext:
				h.mu.Lock()
				info := h.next
				h.mu.Unlock()
				bs, err := json.Marshal(info)
				if err != nil {
					slog.Error("StatusKubeHandler unepxected error marshaling network info", slog.Any("error", err))
					continue
				}
				networkStatus := string(bs)
				data := map[string]string{"NetworkStatus": networkStatus}
				err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
					current, err := h.client.CoreV1().ConfigMaps(h.namespace).Get(ctx, types.NetworkStatusConfigMapName, metav1.GetOptions{})
					if err != nil {
						return err
					}
					current.Data = data
					_, err = h.client.CoreV1().ConfigMaps(h.namespace).Update(ctx, current, metav1.UpdateOptions{})
					return err
				})
				if err != nil {
					slog.Error("StatusKubeHandler unepxected error writing skupper-network-status update", slog.Any("error", err))
					select { // queue a retry
					case h.hasNext <- struct{}{}:
					default:
					}
					continue
				}
				netUpdateCt++
				if !firstStatusUpdate && len(info.SiteStatus) > 0 && len(info.SiteStatus[0].RouterStatus) > 0 {
					firstStatusUpdate = true
					slog.Info("StatusKubeHandler wrote first functional update",
						slog.Any("prevUpdates", netUpdateCt),
						slog.String("after", time.Since(startTime).String()),
					)
				}
			}
		}
	}()
}

func (h *kubeHandler) Handle(info network.NetworkStatusInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next = info
	select {
	case h.hasNext <- struct{}{}:
	default:
	}
}
