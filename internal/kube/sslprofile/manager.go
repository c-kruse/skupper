package sslprofile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strconv"

	"github.com/skupperproject/skupper/internal/qdr"
	"github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

type sslProfile struct {
	Name               string
	Ordinal            uint64
	OldestValidOrdinal uint64
	SecretContentSum   [32]byte
	HasSecret          bool

	BasePath string
	CAOnly   bool
}

type sslProfileMap map[string]*sslProfile

func (pm sslProfileMap) Apply(config *qdr.RouterConfig) bool {
	change := false

	found := make([]string, 0, len(config.SslProfiles))
	for profileName := range config.SslProfiles {
		found = append(found, profileName)
	}

	for profileName, desired := range pm {
		existing, ok := config.SslProfiles[profileName]
		if !ok {
			// add
			continue
		}
		updateQDRProfile := func(context *sslProfile, profile *qdr.SslProfile) bool {
			return false
		}

	}
	return change
}

type Manager struct {
	logger  *slog.Logger
	secrets v1.SecretInterface

	profiles      sslProfileMap
	configurePath string
}

func toQdrProfile(profile *sslProfile) qdr.SslProfile {
	result := qdr.SslProfile{
		Name:               profile.Name,
		CaCertFile:         path.Join(profile.BasePath, profile.Name, "ca.crt"),
		Ordinal:            profile.Ordinal,
		OldestValidOrdinal: profile.OldestValidOrdinal,
	}
	if !profile.CAOnly {
		result.CertFile = path.Join(profile.BasePath, profile.Name, "tls.crt")
		result.PrivateKeyFile = path.Join(profile.BasePath, profile.Name, "tls.key")
	}
	return result
}

func NewManager(secrets v1.SecretInterface, logger *slog.Logger) *Manager {
	return &Manager{
		logger:   logger,
		secrets:  secrets,
		profiles: make(sslProfileMap),
	}
}

func (m *Manager) Chain(update qdr.ConfigUpdate) qdr.ConfigUpdate {
	return configUpdateChain{update, m.profiles}
}

func (m *Manager) Secret(key string, secret *corev1.Secret) (bool, error) {
	if secret == nil {
		return false, nil
	}
	name := secret.ObjectMeta.Name
	profile, ok := m.profiles[name]
	if !ok {
		return false, nil
	}
	if !profile.HasSecret {
		profile.HasSecret = true
		updateSecretChecksum(secret, &profile.SecretContentSum)
	}
	changed := false
	if updateSecretChecksum(secret, &profile.SecretContentSum) {
		profile.Ordinal += 1
		changed = true
	}
	pv := getPriorValidity(secret)
	if nextOldest := profile.Ordinal - pv; pv <= profile.Ordinal && nextOldest > profile.OldestValidOrdinal {
		profile.OldestValidOrdinal = nextOldest
		changed = true
	}
	if updateSecretAnnotations(secret, profile) {
		if _, err := m.secrets.Update(context.TODO(), secret, metav1.UpdateOptions{}); err != nil {
			return false, fmt.Errorf("error updating sslProfile secret anntations: %s", err)
		}
	}
	if !changed {
		return false, nil
	}
	m.logger.Info("SslProfile Secret Changed",
		slog.String("name", name),
		slog.Int("next_ordinal", int(profile.Ordinal)),
	)
	return true, nil
}

func (m *Manager) Link(key string, link *v2alpha1.Link) error
func (m *Manager) RouterAccess(key string, link *v2alpha1.RouterAccess) error
func (m *Manager) Listener(key string, link *v2alpha1.Listener) error
func (m *Manager) Connector(key string, link *v2alpha1.Connector) error

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
func updateSecretAnnotations(secret *corev1.Secret, state *sslProfile) bool {
	expected := fmt.Sprint(state.Ordinal)
	val := secret.Annotations["internal.skupper.io/ssl-profile-ordinal"]
	if val == expected {
		return true
	}
	secret.Annotations["internal.skupper.io/ssl-profile-ordinal"] = expected
	return true
}

type configUpdateChain []qdr.ConfigUpdate

func (c configUpdateChain) Apply(config *qdr.RouterConfig) bool {
	anyChange := false
	for _, u := range c {
		change := u.Apply(config)
		anyChange = anyChange || change
	}
	return anyChange
}
