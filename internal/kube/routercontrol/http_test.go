package routercontrol

import (
	"bytes"
	"context"
	"io"
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
