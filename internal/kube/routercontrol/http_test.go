package routercontrol

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestEnrollmentClientRequiresHTTPS(t *testing.T) {
	if _, err := NewEnrollmentClient("http://controller.example/enroll", "token", "ca", "controller.example"); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("insecure constructor error = %v", err)
	}
	client := &EnrollmentClient{URL: "http://controller.example/enroll", TokenPath: "does-not-exist"}
	if _, err := client.Enroll(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("insecure direct client error = %v", err)
	}
}

func TestEnrollmentClientDoesNotFollowRedirects(t *testing.T) {
	var redirected atomic.Bool
	destination := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Store(true)
	}))
	defer destination.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	tokenPath := writeTestToken(t)
	client := &EnrollmentClient{URL: source.URL, TokenPath: tokenPath, HTTP: source.Client(), RequestTimeout: time.Second}
	if _, err := client.Enroll(context.Background()); err == nil || !strings.Contains(err.Error(), "redirects are not allowed") {
		t.Fatalf("redirect error = %v", err)
	}
	if redirected.Load() {
		t.Fatal("enrollment bearer token was sent to redirect destination")
	}
}

func TestEnrollmentClientAppliesRequestTimeout(t *testing.T) {
	tokenPath := writeTestToken(t)
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	client := &EnrollmentClient{URL: "https://controller.example/enroll", TokenPath: tokenPath, HTTP: &http.Client{Transport: transport}, RequestTimeout: 20 * time.Millisecond}
	started := time.Now()
	if _, err := client.Enroll(context.Background()); err == nil {
		t.Fatal("timed-out enrollment succeeded")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("enrollment request timeout took %v", elapsed)
	}
}

func TestEnrollmentClientReloadsProjectedTokenForEveryAttempt(t *testing.T) {
	tokenPath := writeTestToken(t)
	var headers []string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		headers = append(headers, request.Header.Get("Authorization"))
		return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("denied")), Header: make(http.Header)}, nil
	})
	client := &EnrollmentClient{URL: "https://controller.example/enroll", TokenPath: tokenPath, HTTP: &http.Client{Transport: transport}, RequestTimeout: time.Second}
	if _, err := client.Enroll(context.Background()); err == nil {
		t.Fatal("first denied enrollment unexpectedly succeeded")
	}
	if err := os.WriteFile(tokenPath, []byte("rotated-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Enroll(context.Background()); err == nil {
		t.Fatal("second denied enrollment unexpectedly succeeded")
	}
	want := []string{"Bearer sensitive-token", "Bearer rotated-token"}
	if len(headers) != len(want) || headers[0] != want[0] || headers[1] != want[1] {
		t.Fatalf("enrollment authorization headers = %q, want %q", headers, want)
	}
}

func TestEnrollmentResponseExpiryMustMatchLeaf(t *testing.T) {
	leaf := &x509.Certificate{NotAfter: time.Date(2026, 9, 29, 12, 15, 0, 0, time.UTC)}
	if err := validateEnrollmentExpiry(leaf.NotAfter.Format(time.RFC3339Nano), leaf); err != nil {
		t.Fatalf("matching expiry rejected: %v", err)
	}
	if err := validateEnrollmentExpiry(leaf.NotAfter.Add(time.Second).Format(time.RFC3339Nano), leaf); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched expiry error = %v", err)
	}
	if err := validateEnrollmentExpiry("not-a-time", leaf); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("invalid expiry error = %v", err)
	}
}

func TestEnrollmentClientRejectsExpiryMetadataMismatch(t *testing.T) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var enrollment EnrollRequest
		if err := json.NewDecoder(request.Body).Decode(&enrollment); err != nil {
			t.Fatal(err)
		}
		csr, err := x509.ParseCertificateRequest(enrollment.CSR)
		if err != nil {
			t.Fatal(err)
		}
		leafTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(10 * time.Minute), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
		leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, csr.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(EnrollResponse{Certificate: [][]byte{leafDER, caDER}, NotAfter: leafTemplate.NotAfter.Add(time.Second).Format(time.RFC3339Nano)})
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})
	client := &EnrollmentClient{URL: "https://controller.example/enroll", TokenPath: writeTestToken(t), HTTP: &http.Client{Transport: transport}, RequestTimeout: time.Second}
	if _, err := client.Enroll(context.Background()); err == nil || !strings.Contains(err.Error(), "expiry does not match") {
		t.Fatalf("mismatched enrollment expiry error = %v", err)
	}
}

func TestDecodeJSONBodyRejectsOversizeAndTrailingValues(t *testing.T) {
	validBody := func(trailing string) []byte { return append([]byte{'{', '}'}, trailing...) }
	for _, test := range []struct {
		name string
		body []byte
		max  int64
	}{
		{name: "oversized", body: []byte(`{"csr":"123456"}`), max: 8},
		{name: "trailing value", body: []byte(`{"csr":""} {}`), max: 64},
		{name: "trailing garbage", body: []byte(`{"csr":""} garbage`), max: 64},
		{name: "valid JSON then value", body: validBody(" {}"), max: 64},
		{name: "valid JSON then garbage", body: validBody(" garbage"), max: 64},
	} {
		t.Run(test.name, func(t *testing.T) {
			var request EnrollRequest
			if err := decodeJSONBody(bytes.NewReader(test.body), test.max, &request); err == nil {
				t.Fatal("invalid enrollment body was accepted")
			}
		})
	}
}

func TestEnrollmentHandlerRejectsOversizedAndTrailingBodies(t *testing.T) {
	validBody := append([]byte{'{', '}'}, []byte(" {}")...)
	for _, body := range [][]byte{bytes.Repeat([]byte{'x'}, maxEnrollmentBody+1), validBody} {
		request := httptest.NewRequest(http.MethodPost, "https://controller.example/enroll", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		EnrollmentHandler(nil).ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid body status = %d, want %d", response.Code, http.StatusBadRequest)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func writeTestToken(t *testing.T) string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "token-")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := io.WriteString(file, "sensitive-token"); err != nil {
		t.Fatal(err)
	}
	return file.Name()
}
