package adaptor

import (
	"errors"
	"testing"

	"github.com/skupperproject/skupper/internal/qdr"
)

type fakeLocalRouter struct {
	current *qdr.RouterConfig
	apply   func(*qdr.RouterConfig)
	reads   int
	readErr error
}

func (f *fakeLocalRouter) Read() (*qdr.RouterConfig, error) {
	f.reads++
	if f.readErr != nil {
		return nil, f.readErr
	}
	c := cloneRouterConfig(f.current)
	return &c, nil
}
func (f *fakeLocalRouter) Apply(config *qdr.RouterConfig) error {
	if f.apply != nil {
		f.apply(config)
	} else {
		c := cloneRouterConfig(config)
		f.current = &c
	}
	return nil
}

func basicConfig() *qdr.RouterConfig {
	c := qdr.InitialConfig("r", "s", "v", false, 3)
	c.Bridges = qdr.NewBridgeConfig()
	return &c
}

func TestReconcileDoesNotClaimPartialApply(t *testing.T) {
	desired := basicConfig()
	desired.Bridges.TcpListeners[ownedNamePrefix+"wanted"] = qdr.TcpEndpoint{Name: ownedNamePrefix + "wanted", Port: "8080", Address: "orders"}
	fake := &fakeLocalRouter{current: basicConfig(), apply: func(config *qdr.RouterConfig) { /* management returned success without applying */ }}
	result := Reconcile(fake, CompiledIntent{Config: *desired})
	if result.Applied || result.Err == nil {
		t.Fatalf("partial apply reported success: %#v", result)
	}
}

func TestReconcileDeletesOnlyOwnedAndPreservesProtected(t *testing.T) {
	actual := basicConfig()
	actual.Listeners["amqp"] = qdr.Listener{Name: "amqp", Host: "localhost", Port: 5672}
	actual.Bridges.TcpListeners["foreign"] = qdr.TcpEndpoint{Name: "foreign", Port: "1"}
	actual.Bridges.TcpListeners[ownedNamePrefix+"old"] = qdr.TcpEndpoint{Name: ownedNamePrefix + "old", Port: "2"}
	fake := &fakeLocalRouter{current: actual}
	result := Reconcile(fake, CompiledIntent{Config: *basicConfig()})
	if !result.Applied {
		t.Fatal(result.Err)
	}
	if _, ok := fake.current.Listeners["amqp"]; !ok {
		t.Fatal("protected management listener deleted")
	}
	if _, ok := fake.current.Bridges.TcpListeners["foreign"]; !ok {
		t.Fatal("foreign entity deleted")
	}
	if _, ok := fake.current.Bridges.TcpListeners[ownedNamePrefix+"old"]; ok {
		t.Fatal("stale owned entity retained")
	}
}

func TestFailedReadIsUnknownNotEmpty(t *testing.T) {
	fake := &fakeLocalRouter{current: basicConfig(), readErr: errors.New("disconnected")}
	result := Reconcile(fake, CompiledIntent{Config: *basicConfig()})
	if result.Applied || result.Actual != nil || result.Err == nil {
		t.Fatalf("failed query treated as empty: %#v", result)
	}
}

func TestReconcileIgnoresOperationalReadbackAndRouterDefaults(t *testing.T) {
	desired := basicConfig()
	name := ownedNamePrefix + "listener"
	desired.Bridges.TcpListeners[name] = qdr.TcpEndpoint{Name: name, Port: "8080", Address: "orders"}
	fake := &fakeLocalRouter{current: basicConfig()}
	// Capture the QDR-like readback after Apply without deriving expectations
	// from the verifier under test.
	fake.apply = func(config *qdr.RouterConfig) {
		applied := cloneRouterConfig(config)
		listener := applied.Bridges.TcpListeners[name]
		listener.Host = "0.0.0.0"
		listener.Observer = "auto"
		listener.OperStatus = "up"
		listener.ConnectionMsg = "listening"
		applied.Bridges.TcpListeners[name] = listener
		fake.current = &applied
	}
	result := Reconcile(fake, CompiledIntent{Config: *desired})
	if !result.Applied {
		t.Fatalf("QDR runtime/default fields caused false mismatch: %v", result.Err)
	}
}

type stagedLocalRouter struct {
	*fakeLocalRouter
	prunes int
}

func (s *stagedLocalRouter) ApplyDependents(config *qdr.RouterConfig) error {
	// Simulate profile creation succeeding while the dependent listener did
	// not change. Pruning the old profile at this point would break traffic.
	s.current.SslProfiles = cloneMap(config.SslProfiles)
	return nil
}

func (s *stagedLocalRouter) PruneDependencies(*qdr.RouterConfig) error {
	s.prunes++
	return nil
}

func TestReconcileDoesNotPruneCredentialsBeforeDependentReadback(t *testing.T) {
	actual := basicConfig()
	oldProfile := ownedNamePrefix + "tls.old"
	newProfile := ownedNamePrefix + "tls.new"
	listener := ownedNamePrefix + "listener"
	actual.SslProfiles[oldProfile] = qdr.SslProfile{Name: oldProfile, CaCertFile: "old-ca"}
	actual.Bridges.TcpListeners[listener] = qdr.TcpEndpoint{Name: listener, Port: "8080", Address: "orders", SslProfile: oldProfile}
	desired := basicConfig()
	desired.SslProfiles[newProfile] = qdr.SslProfile{Name: newProfile, CaCertFile: "new-ca"}
	desired.Bridges.TcpListeners[listener] = qdr.TcpEndpoint{Name: listener, Port: "8080", Address: "orders", SslProfile: newProfile}
	staged := &stagedLocalRouter{fakeLocalRouter: &fakeLocalRouter{current: actual}}
	result := Reconcile(staged, CompiledIntent{Config: *desired})
	if result.Applied || result.Err == nil {
		t.Fatalf("partial dependent apply reported success: %#v", result)
	}
	if staged.prunes != 0 {
		t.Fatal("old credential profile pruned before dependent read-back")
	}
}
