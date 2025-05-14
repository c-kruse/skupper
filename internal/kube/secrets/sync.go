package secrets

import (
	"fmt"
	"log/slog"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/skupperproject/skupper/internal/qdr"
	corev1 "k8s.io/api/core/v1"
)

type syncContext struct {
	Prev   *secretContext
	Secret string

	Expect qdr.SslProfile
}

type Sync struct {
	logger   *slog.Logger
	cache    SecretsCache
	callback Callback

	mu       sync.Mutex
	profiles map[string]syncContext
	cleanup  func()
}

func NewSync(factory SecretsCacheFactory, callback Callback, logger *slog.Logger) *Sync {
	stopCh := make(chan struct{})
	sync := &Sync{
		cleanup:  sync.OnceFunc(func() { close(stopCh) }),
		logger:   logger,
		profiles: make(map[string]syncContext),
	}
	sync.cache = factory(stopCh, sync.handle)
	return sync
}

func (s *Sync) Stop() {
	s.cleanup()
}

func (m *Sync) doCallback(name string) {
	if m.callback == nil {
		return
	}
	m.callback(name)
}

func (s *Sync) Recover() {
	for _, secret := range s.cache.List() {
		s.handle(secret.Namespace+"/"+secret.Name, secret)
	}
}

func (s *Sync) handle(key string, secret *corev1.Secret) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if secret == nil {
		return nil
	}
	metadata, found, err := fromSecret(secret)
	if err != nil {
		return fmt.Errorf("failed to decode secret metadata: %s", err)
	}
	if !found {
		return nil
	}
	profileName := metadata.ProfileName
	var (
		expectProfile qdr.SslProfile
	)
	if context, ok := s.profiles[profileName]; ok {
		expectProfile = context.Expect
		if prev := context.Prev; prev != nil {
			if prev.Ordinal == metadata.Ordinal {
				return nil
			}
			if metadata.Ordinal < prev.Ordinal {
				s.logger.Info("Ignoring Secret update downgrading ordinal",
					slog.String("secret_name", key),
					slog.Uint64("want_ordinal", prev.Ordinal),
					slog.Uint64("have_ordinal", metadata.Ordinal),
				)
				return nil
			}
		}
	}

	if err := writeSslProfile(secret, expectProfile); err != nil {
		return err
	}

	prevContext := s.profiles[profileName]
	s.profiles[profileName] = syncContext{
		Prev:   &metadata,
		Secret: key,
		Expect: prevContext.Expect,
	}
	s.logger.Info("Wrote SslProfile contents",
		slog.String("profile_name", profileName),
		slog.String("secret_name", key),
		slog.Uint64("want_ordinal", prevContext.Expect.Ordinal),
		slog.Uint64("have_ordinal", metadata.Ordinal),
	)
	s.doCallback(profileName)
	return nil
}

func (s *Sync) Expect(profiles map[string]qdr.SslProfile) SyncDelta {
	s.mu.Lock()
	defer s.mu.Unlock()
	var delta SyncDelta
	found := make(map[string]struct{}, len(s.profiles))
	for profileName := range s.profiles {
		found[profileName] = struct{}{}
	}
	for profileName, qdrProfile := range profiles {
		delete(found, profileName)
		context := s.profiles[profileName]
		if context.Prev == nil { // secret not found
			delta.Missing = append(delta.Missing, profileName)
		} else if context.Prev.Ordinal < qdrProfile.Ordinal {
			if delta.PendingOrdinals == nil {
				delta.PendingOrdinals = make(map[string]OrdinalDelta)
			}
			delta.PendingOrdinals[profileName] = OrdinalDelta{
				SecretName: context.Secret,
				Expect:     qdrProfile.Ordinal,
				Current:    context.Prev.Ordinal,
			}
		}
		context.Expect = qdrProfile
		s.profiles[profileName] = context
	}
	for unreferenced := range found {
		delete(s.profiles, unreferenced)
	}
	return delta
}

type OrdinalDelta struct {
	SecretName string
	Expect     uint64
	Current    uint64
}
type SyncDelta struct {
	Missing         []string
	PendingOrdinals map[string]OrdinalDelta
}

func (d SyncDelta) Error() error {
	if d.Empty() {
		return nil
	}
	var parts []string
	for _, missing := range d.Missing {
		parts = append(parts, fmt.Sprintf("missing secret for profile %q", missing))
	}
	for pname, diff := range d.PendingOrdinals {
		parts = append(parts, fmt.Sprintf(
			"profile %q configured with ordinal %d, but secret %q has %d",
			pname,
			diff.Expect,
			diff.SecretName,
			diff.Current,
		))
	}
	return fmt.Errorf("secrets not synchronized with router config: %s", strings.Join(parts, ", "))
}

func (d SyncDelta) Empty() bool {
	return len(d.Missing) == 0 && len(d.PendingOrdinals) == 0
}

func writeSslProfile(secret *corev1.Secret, profile qdr.SslProfile) error {
	if profile.CaCertFile == "" {
		return fmt.Errorf("empty sslProfile %q", secret.Name)
	}
	baseName := path.Dir(profile.CaCertFile)
	if err := os.MkdirAll(baseName, 0777); err != nil {
		return fmt.Errorf("error making sslProfile certificates directory %q: %e", baseName, err)
	}

	if err := writeFile(profile.CaCertFile, secret.Data["ca.crt"], 0777); err != nil {
		return fmt.Errorf("error writing ca.crt: %e", err)
	}
	if err := writeFile(profile.CertFile, secret.Data["tls.crt"], 0777); err != nil {
		return fmt.Errorf("error writing tls.crt: %e", err)
	}
	if err := writeFile(profile.PrivateKeyFile, secret.Data["tls.key"], 0777); err != nil {
		return fmt.Errorf("error writing tls.key: %e", err)
	}
	return nil
}

func writeFile(path string, data []byte, perm os.FileMode) error {
	if path == "" {
		return nil
	}
	return os.WriteFile(path, data, perm)
}
