package adaptor

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path"
	paths "path"
	"sort"
	"strconv"
	"strings"

	"github.com/skupperproject/skupper/internal/qdr"
	corev1 "k8s.io/api/core/v1"
)

const ordinalAnnotationKey = "skupper.io/ordinal"

type profileState struct {
	ProfileName string
	SecretName  string
	Dir         string
	Latest      uint64
	Oldest      uint64
	LatestSum   []byte
	Versions    []uint64
}

func (p *profileState) SslProfile() (qdr.SslProfile, bool) {
	profile := qdr.SslProfile{
		Name:               p.ProfileName,
		Ordinal:            p.Latest,
		OldestValidOrdinal: p.Oldest,
		CaCertFile:         paths.Join(p.Dir, "ca.crt"),
		CertFile:           paths.Join(p.Dir, "tls.crt"),
		PrivateKeyFile:     paths.Join(p.Dir, "tls.key"),
	}
	ready := p.LatestSum != nil
	return profile, ready
}

func (p *profileState) Advance(next, oldest uint64, sum []byte) {
	if next > p.Latest || (sum != nil && next == p.Latest && p.LatestSum == nil) {
		p.Versions = append(p.Versions, next)
		p.Latest = next
	}
	if oldest > p.Oldest {
		p.Oldest = oldest
		idx := 0
		for i, version := range p.Versions {
			if version >= oldest {
				idx = i
				break
			}
		}
		p.Versions = p.Versions[idx:]
	}
	p.LatestSum = sum
}

func (p *profileState) Push(profile qdr.SslProfile) (bool, error) {
	switch {
	case profile.OldestValidOrdinal > profile.Ordinal:
		return false, fmt.Errorf("oldest valid ordinal cannot be advanced past current %d", profile.Ordinal)
	case profile.Ordinal <= p.Latest:
		return false, nil // ignore configured SslProfiles with ordinal less than latest - forward only
	default:
		p.Advance(profile.Ordinal, profile.OldestValidOrdinal, nil)
		return true, nil
	}
}

type sslProfileManager struct {
	basePath             string
	profiles             map[string]*profileState
	secretProfileMapping map[string]string
}

func newSslProfileManager(basePath string) sslProfileManager {
	return sslProfileManager{
		basePath:             basePath,
		profiles:             make(map[string]*profileState),
		secretProfileMapping: make(map[string]string),
	}
}

func (m sslProfileManager) ProfileName(secretName string) string {
	if profileName, ok := m.secretProfileMapping[secretName]; ok {
		return profileName
	}
	return secretName
}

func (m sslProfileManager) Profiles() map[string]qdr.SslProfile {
	profiles := make(map[string]qdr.SslProfile, len(m.profiles))
	for _, profile := range m.profiles {
		if profile, ok := profile.SslProfile(); ok {
			profiles[profile.Name] = profile
		}
	}
	return profiles
}

func (m sslProfileManager) Register(profile qdr.SslProfile) (state profileState, dirty bool, err error) {
	if state, ok := m.profiles[profile.Name]; ok {
		dirty, err := state.Push(profile)
		return *state, dirty, err
	}
	secretName := strings.TrimSuffix(profile.Name, "-profile")
	if secretName != profile.Name {
		m.secretProfileMapping[secretName] = profile.Name
	}
	state = profileState{
		ProfileName: profile.Name,
		SecretName:  secretName,
		Dir:         path.Join(m.basePath, profile.Name),
		Latest:      profile.Ordinal,
		Oldest:      profile.OldestValidOrdinal,
	}
	m.profiles[profile.Name] = &state
	return state, true, nil
}

type sslProfileUpdateHandler func(effectiveProfile qdr.SslProfile) error

type writeResult struct {
	State          profileState
	ContentRefresh bool
	OrdinalAdvance bool
}

func (s sslProfileManager) WriteLatest(secret *corev1.Secret, applyEffectiveProfile sslProfileUpdateHandler) (writeResult, error) {
	var result writeResult
	profileName := s.ProfileName(secret.Name)
	checksum := secretDataChecksum(secret)
	state, ok := s.profiles[profileName]
	if !ok {
		return result, nil
	}
	result.State = *state
	ord, _, err := secretOrdinal(secret)
	if err != nil {
		return result, err
	}
	if ord < state.Latest {
		return result, fmt.Errorf("invalid ordinal value %d less than current latest %d", ord, state.Latest)
	}
	result.ContentRefresh = !bytes.Equal(state.LatestSum, checksum)
	result.OrdinalAdvance = ord > state.Latest || (ord == state.Latest && state.LatestSum == nil)
	if !result.ContentRefresh && !result.OrdinalAdvance {
		return result, nil
	}
	if err := writeSecretToPath(secret, state.Dir); err != nil {
		return result, err
	}
	if applyEffectiveProfile != nil {
		profile, _ := state.SslProfile()
		profile.Ordinal = ord
		err := applyEffectiveProfile(profile)
		if err != nil {
			return result, err
		}
	}
	state.Advance(ord, 0, checksum)
	result.State = *state
	return result, nil
}

func secretOrdinal(secret *corev1.Secret) (uint64, bool, error) {
	val, ok := secret.Annotations[ordinalAnnotationKey]
	if !ok {
		return 0, false, nil
	}
	ord, err := strconv.ParseUint(val, 10, 64)
	if err != nil {
		return 0, true, fmt.Errorf("unexpected value parsing ordinal annotation %q: %s", val, err)
	}
	return ord, true, nil

}

func writeSecretToPath(secret *corev1.Secret, path string) error {
	if err := mkdir(path); err != nil {
		return err
	}
	for key, value := range secret.Data {
		certFileName := paths.Join(path, key)
		if err := os.WriteFile(certFileName, value, 0777); err != nil {
			return err
		}
	}
	return nil
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

func secretDataChecksum(secret *corev1.Secret) []byte {
	checksum := sha256.New()
	keys := make([]string, 0, len(secret.Data))
	for key := range secret.Data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		checksum.Write([]byte(key))
		checksum.Write(secret.Data[key])
	}
	return checksum.Sum(nil)
}
