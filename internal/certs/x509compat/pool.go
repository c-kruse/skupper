package x509compat

import (
	"crypto/x509"
	"encoding/pem"
)

// CertPoolFromPEM returns a new x509.CertPool loaded with all of the
// certificates from pemCerts. Returns an error when any of the certificates
// cannot be parsed.
// Use as an alternative to x509.CertPool.AppendCertsFromPEM() when
// x509compat.ParseCertificate is needed.
func CertPoolFromPEM(pemCerts []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	var block *pem.Block
	for len(pemCerts) > 0 {
		block, pemCerts = pem.Decode(pemCerts)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			continue
		}
		cert, err := ParseCertificate(block.Bytes)
		if err != nil {
			return pool, err
		}
		pool.AddCert(cert)
	}
	return pool, nil
}
