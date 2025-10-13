package x509compat

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"testing"
	"time"
)

var dnsNamesMalformed []string = []string{""}

func TestParseCertificate(t *testing.T) {
	t.Run("valid certificate", func(t *testing.T) {
		_, err := ParseCertificate(FixtureCert(t, nil))
		if err != nil {
			t.Errorf("unexpected error: %s", err)
		}
	})
	t.Run("certificate with empty DNSName", func(t *testing.T) {
		_, err := ParseCertificate(FixtureCert(t, dnsNamesMalformed))
		if err == nil {
			t.Errorf("expected MalformedSANs")
		} else if !errors.Is(err, ErrorMalformedSAN{}) {
			t.Errorf("got error %s of type %T but expected MalformedSANs", err, err)
		}
		t.Logf("parse error: %s", err)
	})
}

// FixtureCert returns the ANS.1 DER encoded x509 Certificate
func FixtureCert(t *testing.T, dnsNames []string) []byte {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("failed to generate key: %s", err)
	}
	notBefore := time.Now()
	notAfter := notBefore.Add(time.Hour)
	template := x509.Certificate{
		Subject: pkix.Name{
			CommonName: "fixture",
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,

		DNSNames: dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, priv.Public(), priv)
	if err != nil {
		t.Fatalf("failed to create certificate: %s", err)
	}
	return der
}
