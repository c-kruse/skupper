package adaptor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/skupperproject/skupper/internal/qdr"
	"github.com/skupperproject/skupper/internal/routercontrol"
)

type InitialIntentSource interface {
	FetchInitialIntent(context.Context) (routercontrol.RouterIntent, routercontrol.Digest, error)
}

type TrafficCredentialResolver interface {
	Resolve(context.Context, routercontrol.CredentialBinding) (CredentialRealization, error)
}

type RestartConfigStore interface {
	Persist(qdr.RouterConfig) error
}

type FileRestartConfigStore struct {
	Directory string
}

// Persist atomically replaces the complete router startup configuration. A
// semantic comparison avoids rewriting an unchanged file despite map ordering
// in the JSON representation.
func (s FileRestartConfigStore) Persist(config qdr.RouterConfig) error {
	value, err := qdr.MarshalRouterConfig(config)
	if err != nil {
		return err
	}
	name := filepath.Join(s.Directory, "skrouterd.json")
	current, err := os.ReadFile(name)
	if err == nil && qdr.RouterConfigEquals(string(current), value) {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return atomicWrite(name, []byte(value), 0600)
}

// InitialiseFromIntent is the config-init path. It fetches a complete intent,
// materializes traffic credentials, and atomically writes the normal router
// startup file. It cannot and does not report Applied before router startup.
func InitialiseFromIntent(ctx context.Context, source InitialIntentSource, credentials TrafficCredentialResolver, path string) error {
	intent, expectedDigest, err := source.FetchInitialIntent(ctx)
	if err != nil {
		return err
	}
	digest, err := DigestIntent(intent)
	if err != nil {
		return err
	}
	if digest != expectedDigest {
		return fmt.Errorf("initial intent digest %q, computed %q", expectedDigest, digest)
	}
	realizations := map[routercontrol.ResourceID]CredentialRealization{}
	for _, binding := range intent.CredentialBindings {
		realization, err := credentials.Resolve(ctx, binding)
		if err != nil {
			return fmt.Errorf("resolve credential %q: %w", binding.ID, err)
		}
		realizations[binding.ID] = realization
	}
	compiled, err := CompileIntent(intent, realizations)
	if err != nil {
		return err
	}
	return (FileRestartConfigStore{Directory: path}).Persist(compiled.Config)
}

func atomicWrite(name string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(name), ".skrouterd-")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, name)
}
