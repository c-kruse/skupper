package site

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	"github.com/skupperproject/skupper/internal/qdr"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type sslProfileState struct {
	Name               string
	Ordinal            uint64
	OldestValidOrdinal uint64
	SecretContentSum   [32]byte
	HasSecret          bool
}

type sslProfileContext interface {
	watchSecrets(handler internalclient.SecretHandler) *internalclient.SecretWatcher
	updateRouterConfig(update qdr.ConfigUpdate) error
}

type sslProfileManager struct {
	namespace string
	client    kubernetes.Interface
	logger    *slog.Logger

	stopCh   chan struct{}
	context  sslProfileContext
	profiles map[string]*sslProfileState
	watcher  *internalclient.SecretWatcher
}

func newSslProfileManager(namespace string, client kubernetes.Interface) *sslProfileManager {
	manager := &sslProfileManager{
		namespace: namespace,
		client:    client,
		profiles:  map[string]*sslProfileState{},
		logger: slog.New(slog.Default().Handler()).With(
			slog.String("component", "kube.site.sslProfileManager"),
			slog.String("namespace", namespace),
		),
	}
	return manager
}

func (m *sslProfileManager) Init(context sslProfileContext, config *qdr.RouterConfig) {
	m.context = context
	m.stopCh = make(chan struct{})
	m.watcher = m.context.watchSecrets(m.handler)
	m.watcher.Start(m.stopCh)
	m.profiles = make(map[string]*sslProfileState)
	if config == nil {
		return
	}
	m.Reload(config)
}

func (m *sslProfileManager) secretName(profileName string) string {
	secretName := profileName
	if strings.HasSuffix(profileName, "-profile") {
		secretName = strings.TrimSuffix(profileName, "-profile")
	}
	return secretName
}

func (m *sslProfileManager) Stop() {
	close(m.stopCh)
}

func (m *sslProfileManager) Reload(config *qdr.RouterConfig) {
	for _, profile := range config.SslProfiles {
		secretName := m.secretName(profile.Name)
		_, ok := m.profiles[secretName]
		if !ok {
			m.profiles[secretName] = &sslProfileState{
				Name:               profile.Name,
				Ordinal:            profile.Ordinal,
				OldestValidOrdinal: profile.OldestValidOrdinal,
			}
		}
		key := fmt.Sprintf("%s/%s", m.namespace, secretName)
		secret, err := m.watcher.Get(key)
		if err == nil && secret != nil {
			m.handler(key, secret)
		} else {
		}
	}
}

func (m *sslProfileManager) handler(key string, secret *corev1.Secret) error {
	if secret == nil {
		return nil
	}
	name := secret.ObjectMeta.Name
	state, ok := m.profiles[name]
	if !ok {
		return nil
	}
	if !state.HasSecret {
		state.HasSecret = true
		updateSecretChecksum(secret, &state.SecretContentSum)
	}
	changed := false
	if updateSecretChecksum(secret, &state.SecretContentSum) {
		state.Ordinal += 1
		changed = true
	}
	pv := getPriorValidity(secret)
	if nextOldest := state.Ordinal - pv; pv <= state.Ordinal && nextOldest > state.OldestValidOrdinal {
		state.OldestValidOrdinal = nextOldest
		changed = true
	}

	if updateSecretAnnotations(secret, state) {
		m.logger.Debug("Updating ssl-profile-ordinal secret", slog.String("secret", name), slog.Uint64("nextOrdinal", state.Ordinal))
		if _, err := m.client.CoreV1().Secrets(m.namespace).Update(context.TODO(), secret, v1.UpdateOptions{}); err != nil {
			return fmt.Errorf("error updating sslProfile secret anntations: %s", err)
		}
	}
	if !changed {
		return nil
	}
	m.logger.Info("SslProfile Secret Changed",
		slog.String("name", name),
		slog.Int("next_ordinal", int(state.Ordinal)),
	)
	return m.context.updateRouterConfig(m)
}

func (m *sslProfileManager) Apply(config *qdr.RouterConfig) bool {
	haschange := false
	for key, profile := range config.SslProfiles {
		desired, ok := m.profiles[m.secretName(profile.Name)]
		if !ok {
			continue
		}
		if desired.Ordinal != profile.Ordinal {
			haschange = true
			profile.Ordinal = desired.Ordinal
			config.SslProfiles[key] = profile
		}
		if desired.OldestValidOrdinal != profile.OldestValidOrdinal {
			haschange = true
			profile.OldestValidOrdinal = desired.OldestValidOrdinal
			config.SslProfiles[key] = profile
		}
	}
	return haschange
}

func getPriorValidity(secret *corev1.Secret) uint64 {
	result := uint64(2)
	if secret.ObjectMeta.Annotations == nil {
		return result
	}
	pvr, ok := secret.ObjectMeta.Annotations["skupper.io/prior-valid-revisions"]
	if !ok {
		return result
	}
	parsed, err := strconv.ParseUint(pvr, 10, 64)
	if err != nil {
		return result
	}
	return parsed
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

func updateSecretAnnotations(secret *corev1.Secret, state *sslProfileState) bool {
	expected := strconv.FormatInt(int64(state.Ordinal), 10)
	if secret.Annotations == nil {
		secret.Annotations = make(map[string]string)
	}
	val := secret.Annotations["internal.skupper.io/ssl-profile-ordinal"]
	if val == expected {
		return false
	}
	secret.Annotations["internal.skupper.io/ssl-profile-ordinal"] = expected
	return true
}
