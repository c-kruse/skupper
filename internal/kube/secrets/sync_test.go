package secrets_test

import (
	"io"
	"log/slog"
	"os"
	"path"
	"testing"

	"github.com/skupperproject/skupper/internal/kube/secrets"
	"github.com/skupperproject/skupper/internal/qdr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSync(t *testing.T) {
	tmpdir := t.TempDir()
	tlog := slog.New(slog.NewTextHandler(io.Discard, nil))
	sCache := secretsCacheFactoryFixture(t, "testing")
	var callbackValues []string
	callback := func(profileName string) {
		callbackValues = append(callbackValues, profileName)
	}
	secretSync := secrets.NewSync(sCache.Factory, callback, tlog)

	const tlsProfKey = `internal.skupper.io/tls-profile-context`
	sCache.Secrets["testing/local"] = fixtureTlsSecret("local", "testing")
	sCache.Secrets["testing/remote"] = fixtureTlsSecret("remote", "testing")
	sCache.Secrets["testing/foo"] = fixtureTlsSecret("foo", "testing")
	sCache.Secrets["testing/bar"] = fixtureTlsSecret("bar", "testing")
	sCache.Secrets["testing/zip"] = fixtureTlsSecret("zip", "testing")

	sCache.Secrets["testing/local"].Annotations[tlsProfKey] = `[{"profileName": "local"}, {"profileName": "local-profile", "ordinal": 8}]`
	sCache.Secrets["testing/remote"].Annotations[tlsProfKey] = `[{"profileName": "remote", "ordinal": 22}]`
	sCache.Secrets["testing/foo"].Annotations[tlsProfKey] = `[{"profileName": "not-configured"}]`
	sCache.Secrets["testing/bar"].Annotations[tlsProfKey] = `[!bad json!}`

	// Test Secrets first
	secretSync.Recover()
	expected := map[string]qdr.SslProfile{
		"local":         fixtureSslProfile("local", tmpdir, 0, 0, false),
		"local-profile": fixtureSslProfile("local-profile", tmpdir, 8, 4, true),
		"remote":        fixtureSslProfile("remote", tmpdir, 2, 2, false),
	}
	delta := secretSync.Expect(expected)
	if !delta.Empty() {
		t.Errorf("expected all profiles to be resolved: %s", delta.Error())
	}

	expectedFiles := []string{
		path.Join(tmpdir, "local", "tls.key"),
		path.Join(tmpdir, "local", "tls.crt"),
		path.Join(tmpdir, "local", "ca.crt"),
		path.Join(tmpdir, "local-profile", "ca.crt"),
		path.Join(tmpdir, "remote", "tls.crt"),
	}
	for _, fname := range expectedFiles {
		if _, err := os.Stat(fname); err != nil {
			t.Errorf("expected file %s to be written: %s", fname, err)
		}
	}
	if _, err := os.Stat(path.Join(tmpdir, "local-profile", "tls.crt")); err == nil {
		t.Error("expected CA Only SslProfile to not write tls.crt")
	}

	// Test Profile First
	delta = secretSync.Expect(
		map[string]qdr.SslProfile{
			"local-ii": fixtureSslProfile("local-ii", tmpdir, 1, 1, false),
		},
	)
	if len(delta.Missing) != 1 {
		t.Errorf("expected missing profile local-ii: %s", delta.Error())
	}
	sCache.Secrets["testing/local-ii-secret"] = fixtureTlsSecret("local-ii-secret", "testing")
	sCache.Secrets["testing/local-ii-secret"].Annotations[tlsProfKey] = `[{"profileName": "local-ii", "ordinal": 0}]`
	if err := sCache.HandlerFn("testing/local-ii-secret", sCache.Secrets["testing/local-ii-secret"]); err != nil {
		t.Errorf("unexpected error handling new secret: %s", err)
	}
	delta = secretSync.Expect(
		map[string]qdr.SslProfile{
			"local-ii": fixtureSslProfile("local-ii", tmpdir, 1, 1, false),
		},
	)
	diff := delta.PendingOrdinals["local-ii"]
	expectDiff := secrets.OrdinalDelta{Expect: 1, Current: 0, SecretName: "testing/local-ii-secret"}
	if diff != expectDiff {
		t.Errorf("expected pending ordinal %v got %v", expectDiff, diff)
	}
	if len(callbackValues) > 0 {
		t.Errorf("unexpected callbacks: %s", callbackValues)
	}
	sCache.Secrets["testing/local-ii-secret"].Annotations[tlsProfKey] = `[{"profileName": "local-ii", "ordinal": 1}]`
	if err := sCache.HandlerFn("testing/local-ii-secret", sCache.Secrets["testing/local-ii-secret"]); err != nil {
		t.Errorf("unexpected error handling new secret: %s", err)
	}

	if len(callbackValues) != 1 || callbackValues[0] != "local-ii" {
		t.Errorf("expected one callback for local-ii profile: %s", callbackValues)
	}
	delta = secretSync.Expect(
		map[string]qdr.SslProfile{
			"local-ii": fixtureSslProfile("local-ii", tmpdir, 1, 1, false),
		},
	)
	if !delta.Empty() {
		t.Errorf("expected all profiles to be resolved: %s", delta.Error())
	}
}

func fixtureTlsSecret(name, namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: map[string]string{},
		},
		Data: map[string][]byte{
			"ca.crt":  []byte("ca.crt - " + name),
			"tls.crt": []byte("tls.crt - " + name),
			"tls.key": []byte("tls.key - " + name),
		},
	}
}

func fixtureSslProfile(name, baseDir string, ord, oldestOrd uint64, caonly bool) qdr.SslProfile {
	profile := qdr.SslProfile{
		Name:               name,
		CaCertFile:         path.Join(baseDir, name, "ca.crt"),
		Ordinal:            ord,
		OldestValidOrdinal: oldestOrd,
	}
	if !caonly {
		profile.CertFile = path.Join(baseDir, name, "tls.crt")
		profile.PrivateKeyFile = path.Join(baseDir, name, "tls.key")
	}
	return profile
}

func secretsCacheFactoryFixture(t *testing.T, ns string) *stubSecretsCache {
	t.Helper()
	return &stubSecretsCache{
		Secrets:   make(map[string]*corev1.Secret),
		Namespace: ns,
	}
}

type stubSecretsCache struct {
	Secrets   map[string]*corev1.Secret
	Namespace string
	HandlerFn func(string, *corev1.Secret) error
}

func (s *stubSecretsCache) Factory(stopCh <-chan struct{}, handler func(string, *corev1.Secret) error) secrets.SecretsCache {
	s.HandlerFn = handler
	return s
}

func (s stubSecretsCache) Get(key string) (*corev1.Secret, error) {
	return s.Secrets[key], nil
}
func (s stubSecretsCache) List() []*corev1.Secret {
	out := make([]*corev1.Secret, 0, len(s.Secrets))
	for _, secret := range s.Secrets {
		out = append(out, secret)
	}
	return out
}
