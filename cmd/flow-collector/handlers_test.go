package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/skupperproject/skupper/pkg/utils"
)

func TestBasic(t *testing.T) {
	configuredUsers := map[string]string{
		"test-user": "plaintext-password",
		"admin":     "p@ssword!",
	}
	tmpDir := t.TempDir()
	userCreate := func(usr, pwd string) {
		userFile, err := os.Create(filepath.Join(tmpDir, usr))
		if err != nil {
			t.Fatal(err)
		}
		defer userFile.Close()
		userFile.Write([]byte(pwd))
	}
	for usr, pwd := range configuredUsers {
		userCreate(usr, pwd)
	}

	done := make(chan struct{})
	defer close(done)
	handlerUnderTest, err := newBasicAuthHandler(tmpDir, done)
	if err != nil {
		t.Fatal("unexpected error", err)
	}

	tstSrv := httptest.NewTLSServer(handlerUnderTest.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Write([]byte("OK"))
	}))
	defer tstSrv.Close()
	client := tstSrv.Client()
	assertStatusCode := func(expected int, req *http.Request) {
		t.Helper()
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != expected {
			t.Fatalf("expected http %d: got %d", expected, resp.StatusCode)
		}
	}
	unauthenticated, _ := http.NewRequest(http.MethodGet, tstSrv.URL, nil)
	assertStatusCode(401, unauthenticated)

	incorrectPass, _ := http.NewRequest(http.MethodGet, tstSrv.URL, nil)
	incorrectPass.SetBasicAuth("test-user", "X"+configuredUsers["test-user"])
	assertStatusCode(401, incorrectPass)

	incorrectUser, _ := http.NewRequest(http.MethodGet, tstSrv.URL, nil)
	incorrectUser.SetBasicAuth("test-user-x", configuredUsers["test-user"])
	assertStatusCode(401, incorrectPass)

	mixedUserPass, _ := http.NewRequest(http.MethodGet, tstSrv.URL, nil)
	mixedUserPass.SetBasicAuth("admin", configuredUsers["test-user"])
	assertStatusCode(401, mixedUserPass)

	for usr, pwd := range configuredUsers {
		req, _ := http.NewRequest(http.MethodGet, tstSrv.URL, nil)
		req.SetBasicAuth(usr, pwd)
		assertStatusCode(200, req)
	}

	userDNE, _ := http.NewRequest(http.MethodGet, tstSrv.URL, nil)
	userDNE.SetBasicAuth("newuser", configuredUsers["password"])
	assertStatusCode(401, userDNE)

	userCreate("newuser", "password")
	handlerUnderTest.OnCreate("")
	utils.Retry(time.Millisecond*2, 500, func() (bool, error) {
		req, _ := http.NewRequest(http.MethodGet, tstSrv.URL, nil)
		req.SetBasicAuth("newuser", "password")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode == 200, nil
	})
}

func FuzzBasic(f *testing.F) {
	const (
		tUser     = "skupper"
		tPassword = "P@ssword!"
	)
	basic := &basicAuthHandler{
		users: map[string]string{tUser: tPassword},
	}
	f.Add(tUser, tPassword)
	f.Add(tPassword, tUser)
	f.Add(tUser, "")
	f.Add("", tPassword)
	f.Fuzz(func(t *testing.T, user, password string) {
		expected := user == tUser && password == tPassword
		out := basic.check(user, password)
		if expected != out {
			t.Errorf("%q:%q does not match %q:%q", user, password, tUser, tPassword)
		}
	})
}
