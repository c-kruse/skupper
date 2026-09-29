package adaptor

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/skupperproject/skupper/internal/routercontrol"
)

func credentialData(t *testing.T, serial int64) map[string][]byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "traffic"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	private := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return map[string][]byte{"ca.crt": cert, "tls.crt": cert, "tls.key": private}
}

func TestCredentialRotationChangesRealizationWithoutIntent(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "traffic", Namespace: "test"}, Data: credentialData(t, 1)}
	client := fake.NewSimpleClientset(secret)
	provider := NewSecretCredentialProvider(client.CoreV1().Secrets("test"), t.TempDir())
	binding := routercontrol.CredentialBinding{ID: "credential", Provider: routercontrol.CredentialProviderKubernetesSecret, Reference: "traffic", Usages: []string{routercontrol.CredentialUsageClientAuth}}
	first, err := provider.Resolve(context.Background(), binding)
	if err != nil {
		t.Fatal(err)
	}
	updated := secret.DeepCopy()
	updated.Data = credentialData(t, 2)
	updated.ResourceVersion = ""
	if _, err := client.CoreV1().Secrets("test").Update(context.Background(), updated, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	second, err := provider.Resolve(context.Background(), binding)
	if err != nil {
		t.Fatal(err)
	}
	if first.RealizationID == second.RealizationID {
		t.Fatal("credential rotation did not change realization")
	}
	if _, err := os.Stat(first.Profile.PrivateKeyFile); err != nil {
		t.Fatalf("old credential retired while still potentially consumed: %v", err)
	}
}

func TestInvalidRotationDoesNotDowngradeMaterial(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "traffic", Namespace: "test"}, Data: credentialData(t, 1)}
	client := fake.NewSimpleClientset(secret)
	provider := NewSecretCredentialProvider(client.CoreV1().Secrets("test"), t.TempDir())
	binding := routercontrol.CredentialBinding{ID: "credential", Provider: routercontrol.CredentialProviderKubernetesSecret, Reference: "traffic", Usages: []string{routercontrol.CredentialUsageClientAuth}}
	valid, err := provider.Resolve(context.Background(), binding)
	if err != nil {
		t.Fatal(err)
	}
	updated := secret.DeepCopy()
	updated.Data["tls.key"] = []byte("not a key")
	updated.ResourceVersion = ""
	if _, err := client.CoreV1().Secrets("test").Update(context.Background(), updated, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Resolve(context.Background(), binding); err == nil {
		t.Fatal("invalid key rotation was accepted")
	}
	if _, err := os.Stat(valid.Profile.PrivateKeyFile); err != nil {
		t.Fatalf("valid material removed after invalid rotation: %v", err)
	}
}

func TestSharedProxyAndTLSCredentialUsesExactV1Contract(t *testing.T) {
	data := credentialData(t, 3)
	data["host"] = []byte("proxy.example")
	data["port"] = []byte("3128")
	data["username"] = []byte("user")
	data["password"] = []byte("private")
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "test"}, Data: data}
	client := fake.NewSimpleClientset(secret)
	provider := NewSecretCredentialProvider(client.CoreV1().Secrets("test"), t.TempDir())
	binding := routercontrol.CredentialBinding{ID: "shared", Provider: routercontrol.CredentialProviderKubernetesSecret, Reference: "shared", Usages: []string{routercontrol.CredentialUsageTrust, routercontrol.CredentialUsageClientAuth, routercontrol.CredentialUsageProxy}}
	got, err := provider.Resolve(context.Background(), binding)
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile.PrivateKeyFile == "" || got.ProxyProfile == nil || got.ProxyProfile.Password != "private" {
		t.Fatalf("shared TLS/proxy realization incomplete: %#v", got)
	}
	binding.Provider = "kubernetes"
	if _, err := provider.Resolve(context.Background(), binding); err == nil {
		t.Fatal("alternate provider spelling was accepted")
	}
	binding.Provider = routercontrol.CredentialProviderKubernetesSecret
	binding.Usages = []string{"client"}
	if _, err := provider.Resolve(context.Background(), binding); err == nil {
		t.Fatal("alternate usage spelling was accepted")
	}
}

func TestTrustOnlyCredentialRequiresCAWithoutClientKeypair(t *testing.T) {
	data := credentialData(t, 4)
	delete(data, "tls.crt")
	delete(data, "tls.key")
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "trust", Namespace: "test"}, Data: data}
	client := fake.NewSimpleClientset(secret)
	provider := NewSecretCredentialProvider(client.CoreV1().Secrets("test"), t.TempDir())
	binding := routercontrol.CredentialBinding{ID: "trust", Provider: routercontrol.CredentialProviderKubernetesSecret, Reference: "trust", Usages: []string{routercontrol.CredentialUsageTrust}}
	got, err := provider.Resolve(context.Background(), binding)
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile.CaCertFile == "" || got.Profile.CertFile != "" || got.Profile.PrivateKeyFile != "" {
		t.Fatalf("trust-only credential required or emitted a client keypair: %#v", got.Profile)
	}
}

func TestCredentialMaterializationIsIdempotentAcrossProviders(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "traffic", Namespace: "test"}, Data: credentialData(t, 5)}
	client := fake.NewSimpleClientset(secret)
	root := t.TempDir()
	binding := routercontrol.CredentialBinding{ID: "credential", Provider: routercontrol.CredentialProviderKubernetesSecret, Reference: "traffic", Usages: []string{routercontrol.CredentialUsageClientAuth}}
	initRealization, err := NewSecretCredentialProvider(client.CoreV1().Secrets("test"), root).Resolve(context.Background(), binding)
	if err != nil {
		t.Fatal(err)
	}
	sidecarRealization, err := NewSecretCredentialProvider(client.CoreV1().Secrets("test"), root).Resolve(context.Background(), binding)
	if err != nil {
		t.Fatalf("fresh sidecar provider could not reuse init material: %v", err)
	}
	if sidecarRealization.RealizationID != initRealization.RealizationID || sidecarRealization.Profile.PrivateKeyFile != initRealization.Profile.PrivateKeyFile {
		t.Fatalf("providers disagreed on materialized revision: init=%#v sidecar=%#v", initRealization, sidecarRealization)
	}
}

func TestCredentialMaterializationRejectsCorruptExistingRevision(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "traffic", Namespace: "test"}, Data: credentialData(t, 6)}
	client := fake.NewSimpleClientset(secret)
	root := t.TempDir()
	binding := routercontrol.CredentialBinding{ID: "credential", Provider: routercontrol.CredentialProviderKubernetesSecret, Reference: "traffic", Usages: []string{routercontrol.CredentialUsageClientAuth}}
	first, err := NewSecretCredentialProvider(client.CoreV1().Secrets("test"), root).Resolve(context.Background(), binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first.Profile.PrivateKeyFile, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSecretCredentialProvider(client.CoreV1().Secrets("test"), root).Resolve(context.Background(), binding); err == nil {
		t.Fatal("fresh provider accepted corrupt existing credential revision")
	}
}
