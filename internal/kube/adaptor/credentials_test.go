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
	binding := routercontrol.CredentialBinding{ID: "credential", Provider: "kubernetes-secret", Reference: "traffic", Usages: []string{"client-auth"}}
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
	binding := routercontrol.CredentialBinding{ID: "credential", Provider: "kubernetes-secret", Reference: "traffic", Usages: []string{"client-auth"}}
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
