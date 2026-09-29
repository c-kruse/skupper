package adaptor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/skupperproject/skupper/internal/routercontrol"
)

type staticIntentSource struct {
	intent routercontrol.RouterIntent
	digest routercontrol.Digest
}

func (s staticIntentSource) FetchInitialIntent(context.Context) (routercontrol.RouterIntent, routercontrol.Digest, error) {
	return s.intent, s.digest, nil
}

type noCredentials struct{}

func (noCredentials) Resolve(context.Context, routercontrol.CredentialBinding) (CredentialRealization, error) {
	panic("unexpected credential")
}

func TestInitialiseFromIntentWritesProtectedStartupFile(t *testing.T) {
	intent := testIntent()
	digest, _ := DigestIntent(intent)
	directory := t.TempDir()
	if err := InitialiseFromIntent(context.Background(), staticIntentSource{intent, digest}, noCredentials{}, directory); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(directory, "skrouterd.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("startup config mode %o, want 0600", info.Mode().Perm())
	}
}

func TestInitialiseFromIntentRejectsCorruptDigest(t *testing.T) {
	err := InitialiseFromIntent(context.Background(), staticIntentSource{testIntent(), "wrong"}, noCredentials{}, t.TempDir())
	if err == nil {
		t.Fatal("corrupt initial intent was written")
	}
}
