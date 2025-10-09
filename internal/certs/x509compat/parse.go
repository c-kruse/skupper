package x509compat

import (
	"crypto/x509"
	"errors"
	"slices"
)

// ParseCertificate parses a single certificate from the given ASN.1 DER data.
//
// Before Skupper 2.1.3 Skupper created Certificates with malformed SAN
// DNSNames for most Certificates used for signing or client auth.
// ParseCertificate wraps x509.ParseCertificate, returning an error
// implementing ErrorMalformedSAN for these specific malformed Certificates
// independent of go standard library validations introduced in go1.24.8 and
// go1.25.2.
func ParseCertificate(der []byte) (*x509.Certificate, error) {
	const errMalformedDNSNames = "x509: SAN dNSName is malformed"
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		if msg := err.Error(); msg == errMalformedDNSNames {
			return cert, malformedError{err}
		}
		return cert, err
	}
	if slices.Contains(cert.DNSNames, "") {
		err = malformedError{errors.New("x509compat: SAN dNSName malformed")}
	}
	return cert, err
}

// ErrorMalformedSAN indicates a special case where a malformed x509
// Certificate contained an invalid SAN DNS entry.
type ErrorMalformedSAN interface {
	error
	MalformedSANs()
}

type malformedError struct {
	error
}

func (malformedError) MalformedSANs() {}
