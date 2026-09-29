//go:build integration

package adaptor

import (
	"os"
	"testing"
	"time"

	"github.com/skupperproject/skupper/internal/qdr"
)

// SKUPPER_TEST_LOCAL_ROUTER must name a disposable router without an adaptor.
// This exercises real AMQP management acknowledgments and readback, which the
// in-memory LocalRouter fake cannot reproduce.
func TestLocalBridgeChangesVerifyInOnePass(t *testing.T) {
	address := os.Getenv("SKUPPER_TEST_LOCAL_ROUTER")
	if address == "" {
		t.Skip("SKUPPER_TEST_LOCAL_ROUTER is not set")
	}
	router := &AMQPLocalRouter{Pool: qdr.NewAgentPool(address, nil)}
	router.Pool.SetConnectionTimeout(5 * time.Second)
	for _, routingKey := range []string{"orders", "updated-orders", ""} {
		desired := basicConfig()
		if routingKey != "" {
			name := ownedNamePrefix + "test-bridge"
			desired.Bridges.TcpListeners[name] = qdr.TcpEndpoint{Name: name, Host: "127.0.0.1", Port: "18080", Address: routingKey}
			// Bridge changes must not skip remaining router-level effects.
			name = ownedNamePrefix + "test-amqp"
			desired.Listeners[name] = qdr.Listener{Name: name, Host: "127.0.0.1", Port: 15673, Role: qdr.RoleNormal}
		}
		result := Reconcile(router, CompiledIntent{Config: *desired})
		if !result.Applied {
			t.Fatalf("bridge create/update/delete %q did not reach fresh verified readback in one pass: %v", routingKey, result.Err)
		}
		if routingKey != "" && result.Actual.Bridges.TcpListeners[ownedNamePrefix+"test-bridge"].Address != routingKey {
			t.Fatal("verified result did not contain the new routing key")
		}
	}
}
