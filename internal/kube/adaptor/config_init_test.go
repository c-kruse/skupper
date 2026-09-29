package adaptor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/skupperproject/skupper/internal/qdr"
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

func TestFileRestartConfigStoreUpdatesAndRemovesRoutes(t *testing.T) {
	directory := t.TempDir()
	store := FileRestartConfigStore{Directory: directory}
	withRoute := *basicConfig()
	name := ownedNamePrefix + "route"
	withRoute.Bridges.TcpConnectors[name] = qdr.TcpEndpoint{Name: name, Host: "backend", Port: "8080", Address: "orders"}
	if err := store.Persist(withRoute); err != nil {
		t.Fatal(err)
	}
	withoutRoute := *basicConfig()
	if err := store.Persist(withoutRoute); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "skrouterd.json"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := qdr.UnmarshalRouterConfig(string(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Bridges.TcpConnectors) != 0 || string(parsed.Metadata.Mode) != string(qdr.ModeInterior) {
		t.Fatalf("restart config did not reflect complete route removal: %#v", parsed)
	}
}

func TestFileRestartConfigStoreDoesNotRewriteEquivalentConfig(t *testing.T) {
	directory := t.TempDir()
	store := FileRestartConfigStore{Directory: directory}
	config := *basicConfig()
	if err := store.Persist(config); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(directory, "skrouterd.json")
	before, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Persist(config); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("equivalent restart config was atomically rewritten")
	}
}
