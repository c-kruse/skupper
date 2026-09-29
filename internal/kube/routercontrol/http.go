package routercontrol

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"golang.org/x/time/rate"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	maxEnrollmentBody               = 24 * 1024
	DefaultEnrollmentRequestTimeout = 15 * time.Second
)

type EnrollRequest struct {
	CSR []byte `json:"csr"`
}

type EnrollResponse struct {
	Identity    Identity `json:"identity"`
	Certificate [][]byte `json:"certificate"`
	NotAfter    string   `json:"notAfter"`
}

// EnrollmentHandler is the only HTTP surface that accepts a bearer token.
func EnrollmentHandler(enroller *Enroller) http.Handler {
	limiter := rate.NewLimiter(10, 20)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if !limiter.Allow() {
			http.Error(w, "enrollment rate exceeded", http.StatusTooManyRequests)
			return
		}
		var request EnrollRequest
		if r.ContentLength > maxEnrollmentBody || decodeJSONBody(r.Body, maxEnrollmentBody, &request) != nil || len(request.CSR) > 16*1024 {
			http.Error(w, "invalid enrollment request", http.StatusBadRequest)
			return
		}
		enrollment, err := enroller.Enroll(r.Context(), token, request.CSR)
		if err != nil {
			// Deliberately avoid reflecting details that distinguish live identities.
			http.Error(w, "enrollment denied", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(EnrollResponse{Identity: enrollment.Identity, Certificate: enrollment.Certificate, NotAfter: enrollment.NotAfter.UTC().Format("2006-01-02T15:04:05.999999999Z")})
	})
}

type EnrollmentClient struct {
	URL            string
	TokenPath      string
	HTTP           *http.Client
	RequestTimeout time.Duration
}

func NewEnrollmentClient(enrollmentURL, tokenPath, publicCAPath, serverName string) (*EnrollmentClient, error) {
	if err := validateEnrollmentURL(enrollmentURL); err != nil {
		return nil, err
	}
	if serverName == "" {
		return nil, fmt.Errorf("router-control TLS server name is required")
	}
	caData, err := os.ReadFile(publicCAPath)
	if err != nil {
		return nil, fmt.Errorf("read router-control server trust: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caData) {
		return nil, fmt.Errorf("router-control server trust contains no certificates")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: serverName}
	return &EnrollmentClient{URL: enrollmentURL, TokenPath: tokenPath, HTTP: &http.Client{Transport: transport}, RequestTimeout: DefaultEnrollmentRequestTimeout}, nil
}

// Enroll generates a fresh private key and keeps it in the returned in-memory
// credential. Only the CSR is sent to the controller.
func (c *EnrollmentClient) Enroll(ctx context.Context) (*ClientCredential, error) {
	if err := validateEnrollmentURL(c.URL); err != nil {
		return nil, err
	}
	token, err := os.ReadFile(c.TokenPath)
	if err != nil {
		return nil, fmt.Errorf("read projected enrollment token: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(EnrollRequest{CSR: csr})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	request.Header.Set("Content-Type", "application/json")
	client := *http.DefaultClient
	if c.HTTP != nil {
		client = *c.HTTP
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return fmt.Errorf("enrollment redirects are not allowed")
	}
	client.Timeout = c.RequestTimeout
	if client.Timeout <= 0 {
		client.Timeout = DefaultEnrollmentRequestTimeout
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("enrollment request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("enrollment denied with HTTP status %d", response.StatusCode)
	}
	var enrolled EnrollResponse
	if response.ContentLength > maxEnrollmentBody {
		return nil, fmt.Errorf("enrollment response exceeds size limit")
	}
	if err := decodeJSONBody(response.Body, maxEnrollmentBody, &enrolled); err != nil {
		return nil, fmt.Errorf("decode enrollment response: %w", err)
	}
	if len(enrolled.Certificate) < 2 {
		return nil, fmt.Errorf("enrollment response has no certificate chain")
	}
	leaf, err := x509.ParseCertificate(enrolled.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse enrolled certificate: %w", err)
	}
	if err := validateEnrollmentExpiry(enrolled.NotAfter, leaf); err != nil {
		return nil, err
	}
	identity, err := identityFromCertificate(leaf)
	if err != nil || identity != enrolled.Identity {
		return nil, fmt.Errorf("enrollment response identity does not match signed certificate")
	}
	if err := verifyKey(leaf, key); err != nil {
		return nil, fmt.Errorf("enrolled certificate: %w", err)
	}
	issuer, err := x509.ParseCertificate(enrolled.Certificate[1])
	if err != nil {
		return nil, fmt.Errorf("parse enrolled issuer: %w", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(issuer)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return nil, fmt.Errorf("verify enrolled certificate chain: %w", err)
	}
	return &ClientCredential{Identity: identity, Certificate: enrolled.Certificate, PrivateKey: key, Leaf: leaf}, nil
}

func validateEnrollmentExpiry(encoded string, leaf *x509.Certificate) error {
	notAfter, err := time.Parse(time.RFC3339Nano, encoded)
	if err != nil {
		return fmt.Errorf("parse enrollment certificate expiry: %w", err)
	}
	if !notAfter.Equal(leaf.NotAfter) {
		return fmt.Errorf("enrollment response expiry does not match signed certificate")
	}
	return nil
}

func (c *ClientCredential) TLSCertificate() tls.Certificate {
	return tls.Certificate{Certificate: c.Certificate, PrivateKey: c.PrivateKey, Leaf: c.Leaf}
}

func validateEnrollmentURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("enrollment URL must be an HTTPS URL without user information")
	}
	return nil
}

func decodeJSONBody(reader io.Reader, maximum int64, destination any) error {
	data, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > maximum {
		return fmt.Errorf("body exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("body contains trailing JSON value")
		}
		return fmt.Errorf("body contains trailing data: %w", err)
	}
	return nil
}
