package routercontrol

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	serverCACertKey = "server-ca.crt"
	serverCAKeyKey  = "server-ca.key"
	serverCertKey   = "server.crt"
	serverKeyKey    = "server.key"
	clientCACertKey = "client-ca.crt"
	clientCAKeyKey  = "client-ca.key"
)

type Installation struct {
	Name         string
	ServerCA     *x509.Certificate
	ServerCAData []byte
	ServerCert   *x509.Certificate
	ServerKey    crypto.Signer
	ClientCA     *x509.Certificate
	ClientCAData []byte
	ClientCAKey  crypto.Signer
}

// EnsureInstallation loads installation-wide trust, creating it once if absent.
// A create race is resolved by loading the winning Secret. Existing malformed or
// expired material is never silently replaced because doing so would split trust.
func EnsureInstallation(ctx context.Context, client kubernetes.Interface, namespace, secretName, installation string, serverDNSNames []string, now time.Time) (*Installation, error) {
	if installation == "" || len(serverDNSNames) == 0 {
		return nil, fmt.Errorf("installation identity and server DNS names are required")
	}
	secrets := client.CoreV1().Secrets(namespace)
	secret, err := secrets.Get(ctx, secretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		secret, err = newInstallationSecret(secretName, installation, serverDNSNames, now)
		if err != nil {
			return nil, err
		}
		secret, err = secrets.Create(ctx, secret, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			secret, err = secrets.Get(ctx, secretName, metav1.GetOptions{})
		}
	}
	if err != nil {
		return nil, fmt.Errorf("load router-control installation Secret: %w", err)
	}
	return parseInstallation(secret, installation, serverDNSNames, now)
}

func newInstallationSecret(name, installation string, dnsNames []string, now time.Time) (*corev1.Secret, error) {
	serverCA, serverCAKey, err := newCA("skupper router-control server CA", now)
	if err != nil {
		return nil, err
	}
	clientCA, clientCAKey, err := newCA("skupper router-control client CA", now)
	if err != nil {
		return nil, err
	}
	serverCert, serverKey, err := newServerCertificate(installation, dnsNames, serverCA, serverCAKey, now)
	if err != nil {
		return nil, err
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			serverCACertKey: certPEM(serverCA),
			serverCAKeyKey:  keyPEM(serverCAKey),
			serverCertKey:   certPEM(serverCert),
			serverKeyKey:    keyPEM(serverKey),
			clientCACertKey: certPEM(clientCA),
			clientCAKeyKey:  keyPEM(clientCAKey),
		},
	}, nil
}

func parseInstallation(secret *corev1.Secret, name string, dnsNames []string, now time.Time) (*Installation, error) {
	serverCA, err := parseCertificate(secret.Data[serverCACertKey])
	if err != nil {
		return nil, fmt.Errorf("server CA: %w", err)
	}
	serverCAKey, err := parsePrivateKey(secret.Data[serverCAKeyKey])
	if err != nil {
		return nil, fmt.Errorf("server CA key: %w", err)
	}
	serverCert, err := parseCertificate(secret.Data[serverCertKey])
	if err != nil {
		return nil, fmt.Errorf("server certificate: %w", err)
	}
	serverKey, err := parsePrivateKey(secret.Data[serverKeyKey])
	if err != nil {
		return nil, fmt.Errorf("server key: %w", err)
	}
	clientCA, err := parseCertificate(secret.Data[clientCACertKey])
	if err != nil {
		return nil, fmt.Errorf("client CA: %w", err)
	}
	clientCAKey, err := parsePrivateKey(secret.Data[clientCAKeyKey])
	if err != nil {
		return nil, fmt.Errorf("client CA key: %w", err)
	}
	if !serverCA.IsCA || !clientCA.IsCA {
		return nil, fmt.Errorf("installation Secret contains a non-CA issuer")
	}
	if err := verifyKey(serverCA, serverCAKey); err != nil {
		return nil, fmt.Errorf("server CA: %w", err)
	}
	if err := verifyKey(serverCert, serverKey); err != nil {
		return nil, fmt.Errorf("server certificate: %w", err)
	}
	if err := verifyKey(clientCA, clientCAKey); err != nil {
		return nil, fmt.Errorf("client CA: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(serverCA)
	for _, dnsName := range dnsNames {
		if _, err := serverCert.Verify(x509.VerifyOptions{Roots: pool, DNSName: dnsName, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			return nil, fmt.Errorf("server certificate is not valid for %q: %w", dnsName, err)
		}
	}
	if now.Before(clientCA.NotBefore) || !now.Before(clientCA.NotAfter) {
		return nil, fmt.Errorf("client CA is not currently valid")
	}
	return &Installation{Name: name, ServerCA: serverCA, ServerCAData: secret.Data[serverCACertKey], ServerCert: serverCert, ServerKey: serverKey, ClientCA: clientCA, ClientCAData: secret.Data[clientCACertKey], ClientCAKey: clientCAKey}, nil
}

func newCA(commonName string, now time.Time) (*x509.Certificate, crypto.Signer, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serialNumber, err := serial()
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{SerialNumber: serialNumber, Subject: pkix.Name{CommonName: commonName}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(5 * 365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return cert, key, err
}

func newServerCertificate(installation string, dnsNames []string, ca *x509.Certificate, caKey crypto.Signer, now time.Time) (*x509.Certificate, crypto.Signer, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serialNumber, err := serial()
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{SerialNumber: serialNumber, Subject: pkix.Name{CommonName: installation}, DNSNames: append([]string(nil), dnsNames...), NotBefore: now.Add(-time.Minute), NotAfter: minTime(now.Add(365*24*time.Hour), ca.NotAfter), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, key.Public(), caKey)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return cert, key, err
}

func parseCertificate(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("missing PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

func parsePrivateKey(data []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("missing PKCS#8 private key")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("private key cannot sign")
	}
	return signer, nil
}

func verifyKey(cert *x509.Certificate, key crypto.Signer) error {
	certKey, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil {
		return err
	}
	privateKey, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		return err
	}
	if string(certKey) != string(privateKey) {
		return fmt.Errorf("certificate and private key do not match")
	}
	return nil
}

func certPEM(cert *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

func keyPEM(key crypto.Signer) []byte {
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func serial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
