package status

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/rogpeppe/go-internal/lockedfile"
	"github.com/skupperproject/skupper/pkg/network"
)

const (
	defaultLocation = "/etc/skupper-network-status/skupper-network-status.json"
)

// NewPodmanHandler creates a status update handler that writes network status
// info as a json string to a file. networkStatusFile defaults to default
// location.
func NewPodmanHandler(networkStatusFile string) StatusUpdateHandler {
	if networkStatusFile == "" {
		networkStatusFile = defaultLocation
	}
	return &podmanHandler{
		filename: networkStatusFile,
	}
}

type podmanHandler struct {
	filename string
}

func (h *podmanHandler) Start(ctx context.Context) {
}

func (h *podmanHandler) writeUpdate(info network.NetworkStatusInfo) error {
	networkStatusLockFile := h.filename + ".lock"
	unlockFn, err := lockedfile.MutexAt(networkStatusLockFile).Lock()
	if err != nil {
		return fmt.Errorf("unable to unlock %s: %s", networkStatusLockFile, err)
	}
	defer unlockFn()
	defer func() {
		_ = os.Remove(networkStatusLockFile)
	}()
	f, err := os.Create(h.filename)
	if err != nil {
		return fmt.Errorf("failed to create file %s: %s", h.filename, err)
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(info); err != nil {
		return fmt.Errorf("error writing network status info: %s", err)
	}
	return nil
}

func (h *podmanHandler) Handle(info network.NetworkStatusInfo) {
	if err := h.writeUpdate(info); err != nil {
		slog.Error("StatusPodmanHandler failed to write network status info", slog.Any("error", err))
	}
}
