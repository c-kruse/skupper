package adaptor

import (
	"errors"
	"strings"
	"testing"

	"github.com/skupperproject/skupper/internal/qdr"
	"github.com/skupperproject/skupper/internal/routercontrol"
)

type fakeLocalRouter struct {
	current         *qdr.RouterConfig
	runningSettings *qdr.RouterConfig
	apply           func(*qdr.RouterConfig)
	applies         int
	reads           int
	readErr         error
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
	f.applies++
	if f.apply != nil {
		f.apply(config)
	} else {
		c := cloneRouterConfig(config)
		f.current = &c
	}
	return nil
}

func (f *fakeLocalRouter) ReadRunningSettings() (*qdr.RouterConfig, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	settings := f.runningSettings
	if settings == nil {
		settings = f.current
	}
	c := cloneRouterConfig(settings)
	return &c, nil
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
	connectorName := ownedNamePrefix + "connector"
	desired.Bridges.TcpListeners[name] = qdr.TcpEndpoint{Name: name, Port: "8080", Address: "orders"}
	desired.Connectors[connectorName] = qdr.Connector{Name: connectorName, Host: "peer.example", Port: "55671"}
	fake := &fakeLocalRouter{current: basicConfig()}
	// Capture the QDR-like readback after Apply without deriving expectations
	// from the verifier under test.
	fake.apply = func(config *qdr.RouterConfig) {
		applied := cloneRouterConfig(config)
		listener := applied.Bridges.TcpListeners[name]
		listener.Host = "0.0.0.0"
		listener.Observer = "auto"
		listener.MultiAddressStrategy = "none"
		listener.OperStatus = "up"
		listener.ConnectionMsg = "listening"
		applied.Bridges.TcpListeners[name] = listener
		connector := applied.Connectors[connectorName]
		connector.ConnectionStatus = "SUCCESS"
		connector.ConnectionMsg = "Connection Opened: dir=out"
		applied.Connectors[connectorName] = connector
		fake.current = &applied
	}
	result := Reconcile(fake, CompiledIntent{Config: *desired})
	if !result.Applied {
		t.Fatalf("QDR runtime/default fields caused false mismatch: %v", result.Err)
	}
}

func TestVerifyOwnedPreservesMultiAddressStrategyDifferences(t *testing.T) {
	name := ownedNamePrefix + "listener"
	tests := []struct {
		name     string
		desired  qdr.TcpEndpoint
		actual   qdr.TcpEndpoint
		wantFail bool
	}{
		{
			name:    "single address omitted and none are equivalent",
			desired: qdr.TcpEndpoint{Name: name, Address: "orders"},
			actual:  qdr.TcpEndpoint{Name: name, Address: "orders", MultiAddressStrategy: "none"},
		},
		{
			name:     "single address priority is not none",
			desired:  qdr.TcpEndpoint{Name: name, Address: "orders"},
			actual:   qdr.TcpEndpoint{Name: name, Address: "orders", MultiAddressStrategy: "priority"},
			wantFail: true,
		},
		{
			name:     "multi address priority and weighted differ",
			desired:  qdr.TcpEndpoint{Name: name, MultiAddressStrategy: "priority"},
			actual:   qdr.TcpEndpoint{Name: name, MultiAddressStrategy: "weighted"},
			wantFail: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			desired := basicConfig()
			desired.Bridges.TcpListeners[name] = test.desired
			actual := cloneRouterConfig(desired)
			actual.Bridges.TcpListeners[name] = test.actual
			err := verifyOwned(&actual, desired)
			if test.wantFail {
				if err == nil || !strings.Contains(err.Error(), `field "multiAddressStrategy"`) {
					t.Fatalf("strategy mismatch not detected: %v", err)
				}
			} else if err != nil {
				t.Fatalf("documented default did not normalize: %v", err)
			}
		})
	}
}

func TestReconcileAppliesAndReadsBackWeightOnlyUpdate(t *testing.T) {
	intent := testIntent()
	intent.ServiceListeners = []routercontrol.ServiceListener{{ID: "weighted", Host: "0.0.0.0", Port: 8080, Protocol: routercontrol.ProtocolTCP, RoutingKeys: []string{"foo", "xfoo"}, RoutingStrategy: routercontrol.RoutingStrategyWeighted, RoutingKeyWeights: map[string]uint{"foo": 1, "xfoo": 3}, TLS: routercontrol.TLSIntent{Mode: routercontrol.TLSModeDisabled}}}
	first, err := CompileIntent(intent, nil)
	if err != nil {
		t.Fatal(err)
	}
	router := &fakeLocalRouter{current: basicConfig()}
	if result := Reconcile(router, first); !result.Applied {
		t.Fatalf("initial weighted apply failed: %v", result.Err)
	}

	intent.ServiceListeners[0].RoutingKeyWeights = map[string]uint{"foo": 4, "xfoo": 3}
	second, err := CompileIntent(intent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.RealizationID == second.RealizationID {
		t.Fatal("weight-only update did not change realization identity")
	}
	if result := Reconcile(router, second); !result.Applied {
		t.Fatalf("weight-only update did not verify by fresh readback: %v", result.Err)
	}
	listener := ownedName("tcp-listener", "weighted")
	values := map[string]int{}
	for _, address := range router.current.Bridges.ListenerAddresses {
		if address.Listener == listener {
			values[address.Address] = address.Value
		}
	}
	if values["foo"] != 4 || values["xfoo"] != 3 {
		t.Fatalf("updated weights were not present in readback: %#v", values)
	}
}

func TestVerifyOwnedReportsFirstCategoryAndFieldWithoutValues(t *testing.T) {
	name := ownedNamePrefix + "listener"
	desired := basicConfig()
	desired.Bridges.TcpListeners[name] = qdr.TcpEndpoint{Name: name, Host: "desired-sensitive-host", Port: "8080", Address: "orders"}
	actual := cloneRouterConfig(desired)
	listener := actual.Bridges.TcpListeners[name]
	listener.Host = "actual-sensitive-host"
	actual.Bridges.TcpListeners[name] = listener
	err := verifyOwned(&actual, desired)
	if err == nil {
		t.Fatal("owned field mismatch was accepted")
	}
	message := err.Error()
	if !strings.Contains(message, `tcpListener "`+name+`" field "host"`) {
		t.Fatalf("mismatch is not actionable: %q", message)
	}
	if strings.Contains(message, "desired-sensitive-host") || strings.Contains(message, "actual-sensitive-host") {
		t.Fatalf("mismatch leaked values: %q", message)
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
