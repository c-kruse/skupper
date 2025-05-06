package adaptor

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	paths "path"
	"reflect"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

type SslProfileSyncer struct {
	profiles map[string]*SslProfile
	path     string
}

func newSslProfileSyncer(path string) *SslProfileSyncer {
	return &SslProfileSyncer{
		profiles: map[string]*SslProfile{},
		path:     path,
	}
}

func (s SslProfileSyncer) get(profile string, ordinal uint64) (*SslProfile, bool) {
	secret := profile
	if strings.HasSuffix(profile, "-profile") {
		secret = strings.TrimSuffix(profile, "-profile")
	}
	if current, ok := s.profiles[secret]; ok {
		doSync := current.ordinal < ordinal
		current.ordinal = ordinal
		return current, doSync
	}
	target := &SslProfile{
		name:    secret,
		path:    paths.Join(s.path, profile),
		ordinal: ordinal,
	}
	s.profiles[secret] = target
	return target, true
}

func (s SslProfileSyncer) bySecretName(secret string) (*SslProfile, bool) {
	current, ok := s.profiles[secret]
	return current, ok
}

type SslProfile struct {
	name        string
	ordinal     uint64
	prevOrdinal *uint64
	path        string
	secret      *corev1.Secret
}

func (s *SslProfile) isCurrent() bool {
	return s.prevOrdinal != nil && *s.prevOrdinal >= s.ordinal
}

func (s *SslProfile) sync(secret *corev1.Secret) (error, bool) {
	if s.secret != nil && reflect.DeepEqual(s.secret.Data, secret.Data) &&
		s.isCurrent() {
		return nil, false
	}
	ord, err := ordinalFromSecret(secret)
	if err != nil {
		return err, false
	}
	if ord < s.ordinal {
		return fmt.Errorf("secret %q has ssl-profile-ordinal %d but want %d", secret.Name, ord, s.ordinal), false
	}
	var sync bool
	if err, _ = writeSecretToPath(secret, s.path); err != nil {
		return err, false
	}
	s.secret = secret
	if !s.isCurrent() && ord >= s.ordinal {
		sync = true
	}
	s.prevOrdinal = &ord
	return err, sync
}

func writeSecretToPath(secret *corev1.Secret, path string) (error, bool) {
	wrote := false
	if err := mkdir(path); err != nil {
		return err, false
	}
	for key, value := range secret.Data {
		certFileName := paths.Join(path, key)
		if content, err := os.ReadFile(certFileName); err == nil {
			if bytes.Equal(content, value) {
				continue
			}
		}
		if err := os.WriteFile(certFileName, value, 0777); err != nil {
			return err, wrote
		}
		wrote = true
	}
	return nil, wrote
}

func mkdir(path string) error {
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		err = os.Mkdir(path, 0777)
		if err != nil {
			return err
		}
	}
	return nil
}

func ordinalFromSecret(secret *corev1.Secret) (uint64, error) {
	if secret == nil || secret.ObjectMeta.Annotations == nil {
		return 0, errors.New("secret missing ssl-profile-ordinal annotation")
	}
	ordStr, found := secret.ObjectMeta.Annotations["internal.skupper.io/ssl-profile-ordinal"]
	if !found {
		return 0, fmt.Errorf("secret %q missing ssl-profile-ordinal annotation", secret.Name)
	}
	parsed, err := strconv.ParseUint(ordStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("malformed ssl-profile-ordinal annotation value on secret %q", secret.Name)
	}
	return parsed, nil
}
