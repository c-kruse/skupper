package secrets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strconv"
	"sync"

	"github.com/skupperproject/skupper/internal/qdr"
	"github.com/skupperproject/skupper/internal/sslprofile"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
)

var _ sslprofile.Provider = (*Manager)(nil)

type SecretsCache interface {
	Get(key string) (*corev1.Secret, error)
	List() []*corev1.Secret
}

type SecretsCacheFactory func(stopCh <-chan struct{}, handler func(string, *corev1.Secret) error) SecretsCache
type Callback func(secretName string)

type sslProfile struct {
	Name               string
	Ordinal            uint64
	OldestValidOrdinal uint64

	SecretContentSum [32]byte
	HasSecret        bool

	CAOnly bool
}

type Manager struct {
	logger        *slog.Logger
	namespace     string
	secrets       v1.SecretInterface
	cache         SecretsCache
	callback      Callback
	configurePath string

	profiles         map[string]*sslProfile
	profilesBySecret map[string]string

	cleanup func()
}

func NewManager(factory SecretsCacheFactory, client v1.SecretInterface, changeHandler Callback, namespace string, logger *slog.Logger) *Manager {
	stopCh := make(chan struct{})
	manager := &Manager{
		cleanup:          sync.OnceFunc(func() { close(stopCh) }),
		callback:         changeHandler,
		logger:           logger,
		secrets:          client,
		namespace:        namespace,
		configurePath:    "/etc/skupper-router-certs",
		profiles:         make(map[string]*sslProfile),
		profilesBySecret: make(map[string]string),
	}
	manager.cache = factory(stopCh, manager.handleEvent)
	return manager
}

func (m *Manager) doCallback(name string) {
	if m.callback == nil {
		return
	}
	m.callback(name)
}

func (m *Manager) handleEvent(key string, secret *corev1.Secret) error {
	if secret == nil {
		_, name, _ := cache.SplitMetaNamespaceKey(key)
		profileName, ok := m.profilesBySecret[name]
		if !ok {
			return nil
		}
		profile := m.profiles[profileName]
		if profile == nil {
			return nil
		}
		profile.HasSecret = false
		profile.SecretContentSum = [32]byte{}
		m.doCallback(name)
		return nil
	}
	name := secret.Name
	profileName, ok := m.profilesBySecret[name]
	if !ok {
		return m.clearSecretAnnotations(secret)
	}
	profile, ok := m.profiles[profileName]
	if !ok {
		delete(m.profilesBySecret, name)
		return nil
	}
	changed := false
	if !profile.HasSecret {
		changed = true
		profile.HasSecret = true
		updateSecretChecksum(secret, &profile.SecretContentSum)
	}
	if updateSecretChecksum(secret, &profile.SecretContentSum) {
		profile.Ordinal += 1
		changed = true
	}
	pv := getPriorValidity(secret)
	if nextOldest := profile.Ordinal - pv; pv <= profile.Ordinal && nextOldest > profile.OldestValidOrdinal {
		profile.OldestValidOrdinal = nextOldest
		changed = true
	}
	updated, err := updateSecret(secret, secretContext{
		Ordinal:     profile.Ordinal,
		ProfileName: profileName,
	})
	if err != nil {
		return fmt.Errorf("error updating secret annotation value: %s", err)
	}
	if updated {
		if _, err := m.secrets.Update(context.TODO(), secret, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("error updating sslProfile secret annotations: %s", err)
		}
	}
	if changed {
		m.logger.Info("SslProfile Secret Changed",
			slog.String("profile_name", profileName),
			slog.String("secret_name", name),
			slog.Int("next_ordinal", int(profile.Ordinal)),
		)
		m.doCallback(name)
	}
	return nil
}

func (m *Manager) Stop() {
	m.cleanup()
}
func (m *Manager) Recover(routerConfig *qdr.RouterConfig) {
	for profileName, profile := range routerConfig.SslProfiles {
		m.logger.Info("Recovered profile", slog.String("name", profileName), slog.Uint64("ordinal", profile.Ordinal))
		m.profiles[profileName] = &sslProfile{
			Name:               profileName,
			Ordinal:            profile.Ordinal,
			OldestValidOrdinal: profile.OldestValidOrdinal,
			CAOnly:             profile.PrivateKeyFile == "",
		}
	}
}

func (m *Manager) Apply(current *qdr.RouterConfig) bool {
	change := false

	found := make(map[string]struct{}, len(current.SslProfiles))
	for profileName := range current.SslProfiles {
		if _, ok := m.profiles[profileName]; ok {
			found[profileName] = struct{}{}
		}
	}

	for profileName, desired := range m.profiles {
		if !desired.HasSecret {
			continue
		}
		existing, ok := current.SslProfiles[profileName]
		if !ok {
			change = true
			current.SslProfiles[profileName] = m.toQdrProfile(desired)
			continue
		}
		if updateQDRProfile(desired, &existing) {
			change = true
			current.SslProfiles[profileName] = existing
		}
		delete(found, profileName)
	}

	for unwanted := range found {
		change = true
		delete(current.SslProfiles, unwanted)
	}
	return change
}

func (m *Manager) Get(tlsCredentials string, opts sslprofile.Opts) (string, error) {
	if opts.NamingStrategy == nil {
		opts.NamingStrategy = sslprofile.UseDefaultName
	}
	profileName := opts.NamingStrategy.TLSCredentialsToProfile(tlsCredentials)

	if _, ok := m.profiles[profileName]; !ok {
		m.logger.Info("Get for unknown profile", slog.String("name", profileName))
		m.profiles[profileName] = &sslProfile{
			Name:   profileName,
			CAOnly: opts.CAOnly,
		}
	}
	m.profilesBySecret[tlsCredentials] = profileName
	return profileName, m.ensureSecretProfile(profileName, tlsCredentials, opts.CAOnly)
}

func (m *Manager) ensureSecretProfile(profileName, tlsCredentials string, caOnly bool) error {
	targetKey := cacheKey(m.namespace, tlsCredentials)
	target, err := m.cache.Get(targetKey)
	if err != nil {
		return fmt.Errorf("error getting tlsCredentials Secret: %s", err)
	}
	if target == nil {
		return fmt.Errorf("missing tlsCredentials Secret %q", tlsCredentials)
	}
	if profile, ok := m.profiles[profileName]; ok {
		profile.CAOnly = caOnly
		if profile.HasSecret {
			m.logger.Info("Found secret for profile", slog.String("name", profileName))
			return nil
		}
	}
	m.logger.Info("Rechecking secret", slog.String("name", target.Name))
	return m.handleEvent(targetKey, target)
}

func (m *Manager) clearSecretAnnotations(secret *corev1.Secret) error {
	_, found, _ := fromSecret(secret)
	if !found {
		return nil
	}
	delete(secret.Annotations, annotationKeySslProfileContext)
	if _, err := m.secrets.Update(context.TODO(), secret, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("error clearing sslProfile secret annotations: %s", err)
	}

	return nil
}
func cacheKey(ns, name string) string {
	return ns + "/" + name
}

// updateSecretChecksum updates a sha256 checksum with a Secret's Data fields.
// Returns true when updated.
func updateSecretChecksum(secret *corev1.Secret, checksum *[32]byte) bool {
	var tmp [32]byte
	hash := sha256.New()
	keys := make([]string, 0, len(secret.Data))
	for key := range secret.Data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(hash, ":%s:%d:", key, len(secret.Data[key]))
		hash.Write(secret.Data[key])
	}
	hash.Sum(tmp[:0])
	if bytes.Equal(checksum[:], tmp[:]) {
		return false
	}
	copy(checksum[:], tmp[:])
	return true
}

func getPriorValidity(secret *corev1.Secret) uint64 {
	result := uint64(2)
	if secret.ObjectMeta.Annotations == nil {
		return result
	}
	pvr, ok := secret.ObjectMeta.Annotations[AnnotationKeyTlsPriorValidRevisions]
	if !ok {
		return result
	}
	parsed, err := strconv.ParseUint(pvr, 10, 64)
	if err != nil {
		return result
	}
	return parsed
}
func (m *Manager) toQdrProfile(profile *sslProfile) qdr.SslProfile {
	result := qdr.SslProfile{
		Name:               profile.Name,
		CaCertFile:         path.Join(m.configurePath, profile.Name, "ca.crt"),
		Ordinal:            profile.Ordinal,
		OldestValidOrdinal: profile.OldestValidOrdinal,
	}
	if !profile.CAOnly {
		result.CertFile = path.Join(m.configurePath, profile.Name, "tls.crt")
		result.PrivateKeyFile = path.Join(m.configurePath, profile.Name, "tls.key")
	}
	return result
}

func updateQDRProfile(context *sslProfile, profile *qdr.SslProfile) bool {
	hasChange := false
	if profile.Ordinal != context.Ordinal {
		hasChange = true
		profile.Ordinal = context.Ordinal
	}
	if profile.OldestValidOrdinal != context.OldestValidOrdinal {
		hasChange = true
		profile.OldestValidOrdinal = context.OldestValidOrdinal
	}
	return hasChange
}
