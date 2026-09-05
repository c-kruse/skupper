package mgmt_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	amqp "github.com/Azure/go-amqp"
	"github.com/skupperproject/skupper/pkg/skrouter/config"
	"github.com/skupperproject/skupper/pkg/skrouter/mgmt"
	"github.com/skupperproject/skupper/pkg/skrouter/mgmt/entities"
	"github.com/skupperproject/skupper/pkg/skrouter/reconcile"
)

//go:embed entities/skrouter.json
var vendoredSchema []byte

func TestRouterIntegration(t *testing.T) {
	image := os.Getenv("SKUPPER_ROUTER_IMAGE")
	if image == "" {
		t.Skip("set SKUPPER_ROUTER_IMAGE to run against a real skrouterd")
	}

	port := startRouter(t, image)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client := mgmt.NewClient(func(ctx context.Context) (*amqp.Conn, error) {
		return amqp.Dial(ctx, "amqp://127.0.0.1:"+strconv.Itoa(port), nil)
	}, mgmt.WithRequestTimeout(5*time.Second))
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer closeCancel()
		if err := client.Close(closeCtx); err != nil {
			t.Errorf("close management client: %v", err)
		}
	})
	target := client.Local()

	connections, err := mgmt.Query[entities.Connection](ctx, target, mgmt.Fields(
		entities.ConnectionIdentity,
		entities.ConnectionAdminStatus,
	))
	if err != nil {
		t.Fatalf("query connections: %v", err)
	}
	if len(connections) == 0 || connections[0].Identity == "" {
		t.Fatalf("query connections returned no identifiable management connection: %#v", connections)
	}
	updated, err := mgmt.Update[entities.Connection](ctx, target, mgmt.ByIdentity(connections[0].Identity), mgmt.Partial[entities.Connection]{
		Value:  entities.Connection{AdminStatus: entities.AdminStatusEnabled},
		Fields: mgmt.Fields(entities.ConnectionAdminStatus),
	})
	if err != nil {
		t.Fatalf("update connection: %v", err)
	}
	if updated.AdminStatus != entities.AdminStatusEnabled {
		t.Fatalf("updated connection admin status = %q, want %q", updated.AdminStatus, entities.AdminStatusEnabled)
	}

	connectorName := "skrouter-mgmt-integration"
	connector, err := mgmt.Create[entities.TcpConnector](ctx, target, mgmt.Partial[entities.TcpConnector]{
		Value: entities.TcpConnector{
			Name:    connectorName,
			Type:    entities.TcpConnectorType.Name,
			Address: "integration.test",
			Host:    "127.0.0.1",
			Port:    "1",
		},
		Fields: mgmt.Fields(
			entities.TcpConnectorName,
			entities.TcpConnectorTypeField,
			entities.TcpConnectorAddress,
			entities.TcpConnectorHost,
			entities.TcpConnectorPort,
		),
	})
	if err != nil {
		t.Fatalf("create tcpConnector: %v", err)
	}
	if connector.Name != connectorName {
		t.Fatalf("created tcpConnector name = %q, want %q", connector.Name, connectorName)
	}
	if err := mgmt.Delete[entities.TcpConnector](ctx, target, mgmt.ByName(connectorName)); err != nil {
		t.Fatalf("delete tcpConnector: %v", err)
	}

	t.Run("reconcile", func(t *testing.T) {
		doc, err := config.Parse([]byte(`[["tcpConnector",{"name":"reconcile-test","address":"test","host":"127.0.0.1","port":"1","verifyHostname":false}]]`))
		if err != nil {
			t.Fatal(err)
		}
		opts := reconcile.Options[entities.TcpConnector]{Owned: func(e entities.TcpConnector) bool { return e.Name == "reconcile-test" }}
		for _, action := range []string{"create", "unchanged", "replace", "delete"} {
			if action == "replace" {
				doc.TcpConnectors[0].Value.Port = "2"
			}
			if action == "delete" {
				doc.TcpConnectors = nil
			}
			report, err := reconcile.Sync[entities.TcpConnector](ctx, target, doc.TcpConnectors, opts)
			if err != nil {
				t.Fatalf("%s: %v", action, err)
			}
			count := map[string]int{"create": report.Created, "unchanged": report.Unchanged, "replace": report.Replaced, "delete": report.Deleted}[action]
			if count != 1 {
				t.Fatalf("%s: %+v", action, report)
			}
		}
		// Ordinal exercises an in-place update, including an explicit zero.
		profiles := []mgmt.Partial[entities.SslProfile]{{Value: entities.SslProfile{Name: "reconcile-profile", Ordinal: 1}, Fields: mgmt.Fields(entities.SslProfileName, entities.SslProfileOrdinal)}}
		profileOpts := reconcile.Options[entities.SslProfile]{Owned: func(e entities.SslProfile) bool { return e.Name == "reconcile-profile" }}
		if _, err := reconcile.Sync[entities.SslProfile](ctx, target, profiles, profileOpts); err != nil {
			t.Fatal(err)
		}
		profiles[0].Value.Ordinal = 0
		report, err := reconcile.Sync[entities.SslProfile](ctx, target, profiles, profileOpts)
		if err != nil || report.Updated != 1 {
			t.Fatalf("update: %+v %v", report, err)
		}
		report, err = reconcile.Sync[entities.SslProfile](ctx, target, profiles, profileOpts)
		if err != nil || report.Unchanged != 1 {
			t.Fatalf("after update: %+v %v", report, err)
		}
		if _, err := reconcile.Sync[entities.SslProfile](ctx, target, nil, profileOpts); err != nil {
			t.Fatal(err)
		}
	})

	response, err := target.Do(ctx, &mgmt.Request{Operation: "GET-JSON-SCHEMA", Ref: mgmt.ByIdentity("self")})
	if err != nil {
		t.Fatalf("GET-JSON-SCHEMA: %v", err)
	}
	liveSchema, ok := mgmt.AsString(response.Body)
	if !ok {
		t.Fatalf("GET-JSON-SCHEMA body is %T, want string", response.Body)
	}
	want := schemaProjectionJSON(t, vendoredSchema, true)
	got := schemaProjectionJSON(t, []byte(liveSchema), false)
	if !bytes.Equal(got, want) {
		t.Fatalf("live schema projection differs from vendored entities/skrouter.json (live sha256 %x, vendored sha256 %x)", sha256.Sum256(got), sha256.Sum256(want))
	}
}

func startRouter(t *testing.T, image string) int {
	t.Helper()
	engine, err := exec.LookPath("podman")
	if err != nil {
		engine, err = exec.LookPath("docker")
		if err != nil {
			t.Fatal("SKUPPER_ROUTER_IMAGE is set, but neither podman nor docker is installed")
		}
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate router port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release router port: %v", err)
	}

	configPath := filepath.Join(t.TempDir(), "skrouterd.json")
	doc := config.Document{
		Router:    mgmt.Partial[entities.Router]{Value: entities.Router{Id: "mgmt-integration", Mode: entities.RouterMode("standalone")}, Fields: mgmt.Fields(entities.RouterId, entities.RouterModeField)},
		Listeners: []mgmt.Partial[entities.Listener]{{Value: entities.Listener{Host: "127.0.0.1", Port: strconv.Itoa(port), SaslMechanisms: "ANONYMOUS"}, Fields: mgmt.Fields(entities.ListenerHost, entities.ListenerPort, entities.ListenerAuthenticatePeer, entities.ListenerSaslMechanisms)}},
	}
	data, err := doc.Marshal()
	if err != nil {
		t.Fatalf("marshal router config: %v", err)
	}
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		t.Fatalf("write router config: %v", err)
	}

	name := fmt.Sprintf("skrouter-mgmt-integration-%d-%d", os.Getpid(), port)
	args := []string{"run", "--rm", "--name", name, "--network", "host"}
	if filepath.Base(engine) == "podman" {
		args = append(args, "--cgroup-manager=cgroupfs")
	}
	args = append(args, "-v", configPath+":/tmp/skrouterd.json:ro", image, "skrouterd", "-c", "/tmp/skrouterd.json")
	routerCtx, stopRouter := context.WithCancel(context.Background())
	cmd := exec.CommandContext(routerCtx, engine, args...)
	var logs bytes.Buffer
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		stopRouter()
		t.Fatalf("start router container: %v", err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = exec.CommandContext(stopCtx, engine, "stop", "--time", "1", name).Run()
		stopRouter()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("router container did not stop")
		}
		if t.Failed() {
			t.Logf("router container output:\n%s", strings.TrimSpace(logs.String()))
		}
	})

	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		connection, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 100*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return port
		}
		select {
		case <-done:
			t.Fatalf("router container exited before becoming ready: %v\n%s", waitErr, strings.TrimSpace(logs.String()))
		case <-deadline.C:
			t.Fatalf("router did not listen on port %d within 20s", port)
		case <-ticker.C:
		}
	}
}

type schemaDocument struct {
	Prefix      string                  `json:"prefix"`
	EntityTypes map[string]schemaEntity `json:"entityTypes"`
}

type schemaEntity struct {
	Attributes         map[string]map[string]any `json:"attributes"`
	Description        string                    `json:"description"`
	Extends            string                    `json:"extends"`
	FullyQualifiedType string                    `json:"fullyQualifiedType"`
	Operations         []string                  `json:"operations"`
	Singleton          bool                      `json:"singleton"`
}

type projectedSchema struct {
	Prefix      string                     `json:"prefix"`
	EntityTypes map[string]projectedEntity `json:"entityTypes"`
}

type projectedEntity struct {
	Attributes  map[string]map[string]any `json:"attributes"`
	Description string                    `json:"description"`
	Operations  []string                  `json:"operations,omitempty"`
	Singleton   bool                      `json:"singleton,omitempty"`
}

func schemaProjectionJSON(t *testing.T, input []byte, resolveInheritance bool) []byte {
	t.Helper()
	var document schemaDocument
	if err := json.Unmarshal(input, &document); err != nil {
		t.Fatalf("decode schema JSON: %v", err)
	}
	projection := projectedSchema{
		Prefix:      document.Prefix,
		EntityTypes: make(map[string]projectedEntity, len(document.EntityTypes)),
	}
	if resolveInheritance {
		cache := make(map[string]projectedEntity, len(document.EntityTypes))
		visiting := make(map[string]bool, len(document.EntityTypes))
		var resolve func(string) projectedEntity
		resolve = func(name string) projectedEntity {
			if entity, found := cache[name]; found {
				return entity
			}
			if visiting[name] {
				t.Fatalf("schema inheritance cycle at %q", name)
			}
			source, found := document.EntityTypes[name]
			if !found {
				t.Fatalf("schema entity %q not found", name)
			}
			visiting[name] = true
			entity := projectEntity(source)
			if source.Extends != "" {
				parent := resolve(source.Extends)
				for attribute, definition := range parent.Attributes {
					if _, overridden := entity.Attributes[attribute]; !overridden {
						entity.Attributes[attribute] = definition
					}
				}
				entity.Operations = appendUnique(entity.Operations, parent.Operations...)
			}
			delete(entity.Attributes, "type")
			visiting[name] = false
			cache[name] = entity
			return entity
		}
		for name := range document.EntityTypes {
			projection.EntityTypes[document.Prefix+"."+name] = resolve(name)
		}
	} else {
		for name, source := range document.EntityTypes {
			wireName := source.FullyQualifiedType
			if wireName == "" {
				wireName = document.Prefix + "." + name
			}
			entity := projectEntity(source)
			delete(entity.Attributes, "type")
			projection.EntityTypes[wireName] = entity
		}
	}
	result, err := json.Marshal(projection)
	if err != nil {
		t.Fatalf("encode schema projection: %v", err)
	}
	return result
}

func projectEntity(source schemaEntity) projectedEntity {
	attributes := make(map[string]map[string]any, len(source.Attributes))
	for name, definition := range source.Attributes {
		normalized := make(map[string]any, len(definition))
		for key, value := range definition {
			switch key {
			case "create", "hidden", "update":
				continue
			}
			if !schemaZero(value) {
				normalized[key] = value
			}
		}
		attributes[name] = normalized
	}
	return projectedEntity{
		Attributes:  attributes,
		Description: source.Description,
		Operations:  appendUnique(nil, source.Operations...),
		Singleton:   source.Singleton,
	}
}

func appendUnique(existing []string, values ...string) []string {
	for _, value := range values {
		found := false
		for _, candidate := range existing {
			if candidate == value {
				found = true
				break
			}
		}
		if !found {
			existing = append(existing, value)
		}
	}
	return existing
}

func schemaZero(value any) bool {
	switch value := value.(type) {
	case nil:
		return true
	case bool:
		return !value
	case float64:
		return value == 0
	case string:
		return value == ""
	case []any:
		return len(value) == 0
	case map[string]any:
		return len(value) == 0
	default:
		return false
	}
}
