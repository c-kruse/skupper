package adaptor

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skupperproject/skupper/internal/routercontrol"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type blockingEventReceiver struct {
	ctx   context.Context
	calls atomic.Int32
}

type recordingRuntimeMetrics struct {
	NoopRuntimeMetrics
	mu      sync.Mutex
	streams []bool
}

func (m *recordingRuntimeMetrics) SetControlStreamUp(up bool) {
	m.mu.Lock()
	m.streams = append(m.streams, up)
	m.mu.Unlock()
}

func (m *recordingRuntimeMetrics) streamStates() []bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]bool(nil), m.streams...)
}

func (r *blockingEventReceiver) NextEvent() (routercontrol.ServerEvent, error) {
	r.calls.Add(1)
	<-r.ctx.Done()
	return routercontrol.ServerEvent{}, r.ctx.Err()
}

func TestCertificateRenewalDelayUsesRemainingLifetime(t *testing.T) {
	now := time.Unix(1_000, 0)
	tests := []struct {
		name      string
		remaining time.Duration
		jitter    float64
		want      time.Duration
	}{
		{name: "default two hour certificate", remaining: 2 * time.Hour, want: time.Hour},
		{name: "short test certificate", remaining: 90 * time.Second, want: 45 * time.Second},
		{name: "long certificate", remaining: 24 * time.Hour, want: 12 * time.Hour},
		{name: "negative jitter", remaining: 2 * time.Hour, jitter: -1, want: 54 * time.Minute},
		{name: "positive jitter", remaining: 2 * time.Hour, jitter: 1, want: 66 * time.Minute},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := certificateRenewalDelay(now, now.Add(test.remaining), test.jitter); got != test.want {
				t.Fatalf("renewal delay %s, want %s", got, test.want)
			}
		})
	}
	if got := certificateRenewalDelay(now, now.Add(-time.Second), 0); got != 0 {
		t.Fatalf("expired certificate delay %s, want immediate", got)
	}
}

func TestCredentialRenewalKeepsUsableSessionAcrossTransientEnrollmentFailure(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Now()
	newControl := func(name string, expiry time.Time) *connectedControlSession {
		ctx, stop := context.WithCancel(parent)
		return &connectedControlSession{ctx: ctx, cancel: stop, expiry: expiry, incarnation: name}
	}
	initial := newControl("initial", now.Add(40*time.Millisecond))
	started := make(chan string, 3)
	firstFailure := make(chan struct{})
	var attempts atomic.Int32
	metrics := &recordingRuntimeMetrics{}
	manager := controlSessionManager{
		now: func() time.Time { return now }, jitter: func() float64 { return 0 }, retryBase: 5 * time.Millisecond,
		metrics: metrics,
		serve: func(control *connectedControlSession) error {
			started <- control.incarnation
			<-control.ctx.Done()
			return control.ctx.Err()
		},
		connect: func(context.Context) (*connectedControlSession, error) {
			if attempts.Add(1) == 1 {
				close(firstFailure)
				return nil, errors.New("temporary enrollment failure")
			}
			return newControl("replacement", now.Add(2*time.Hour)), nil
		},
	}
	done := make(chan error, 1)
	go func() { done <- manager.run(parent, initial) }()
	if name := <-started; name != "initial" {
		t.Fatalf("first served session = %q", name)
	}
	select {
	case <-firstFailure:
	case <-time.After(time.Second):
		t.Fatal("renewal attempt did not run")
	}
	select {
	case <-initial.ctx.Done():
		t.Fatal("transient enrollment failure dropped the still-usable session")
	default:
	}
	select {
	case name := <-started:
		if name != "replacement" {
			t.Fatalf("replacement served session = %q", name)
		}
	case <-time.After(time.Second):
		t.Fatal("successful renewal did not hand off to replacement")
	}
	select {
	case <-initial.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("successful replacement did not retire old session")
	}
	if states := metrics.streamStates(); len(states) != 1 || !states[0] {
		t.Fatalf("healthy renewal changed transport health during handoff: %v", states)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("manager shutdown error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("session manager did not stop")
	}
	if states := metrics.streamStates(); len(states) != 2 || !states[0] || states[1] {
		t.Fatalf("transport health transitions = %v, want [true false]", states)
	}
}

func TestReconnectBackoffDelayIsBoundedAndJittered(t *testing.T) {
	base := 10 * time.Second
	if got := reconnectBackoffDelay(base, -1); got != 8*time.Second {
		t.Fatalf("minimum reconnect delay = %s", got)
	}
	if got := reconnectBackoffDelay(base, 0); got != base {
		t.Fatalf("unjittered reconnect delay = %s", got)
	}
	if got := reconnectBackoffDelay(base, 1); got != 12*time.Second {
		t.Fatalf("maximum reconnect delay = %s", got)
	}
}

func TestReceiveInactivityTimerResetsOnlyOnReceive(t *testing.T) {
	timer := time.NewTimer(20 * time.Millisecond)
	defer timer.Stop()
	time.Sleep(10 * time.Millisecond)
	resetInactivityTimer(timer, 40*time.Millisecond)
	select {
	case <-timer.C:
		t.Fatal("receive deadline retained its original expiry")
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case <-timer.C:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("receive inactivity deadline did not expire")
	}
}

func TestBoundedCallCancelsAndJoinsStalledOperation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	started := time.Now()
	err := boundedCall(ctx, cancel, 10*time.Millisecond, func() error {
		defer close(done)
		<-ctx.Done()
		return ctx.Err()
	})
	if err == nil || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("stalled operation was not canceled: err=%v context=%v", err, ctx.Err())
	}
	select {
	case <-done:
	default:
		t.Fatal("bounded call returned before operation goroutine exited")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stalled operation was not bounded: %s", elapsed)
	}
}

func TestControlReceiverCancellationJoinsOnePendingReceive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	receiver := &blockingEventReceiver{ctx: ctx}
	_, done := startControlReceiver(ctx, receiver)
	deadline := time.After(time.Second)
	for receiver.calls.Load() < 1 {
		select {
		case <-deadline:
			t.Fatal("receiver did not start pending receive")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("receiver goroutine did not exit after session cancellation")
	}
	if receiver.calls.Load() != 1 {
		t.Fatalf("one-shot receiver called NextEvent %d times", receiver.calls.Load())
	}
}

func TestBoundedErrorReasonIsSingleLineAndBounded(t *testing.T) {
	reason := boundedErrorReason(errors.New(strings.Repeat("x", 600) + "\nbody"))
	if len(reason) != 515 || strings.ContainsAny(reason, "\r\n") || !strings.HasSuffix(reason, "...") {
		t.Fatalf("unsafe reconnect reason %q (length %d)", reason, len(reason))
	}
}

func TestApplicationPendingLoggingIsBoundedDeduplicatedAndOmitsCredentials(t *testing.T) {
	var output bytes.Buffer
	logger := applicationReportLogger{logger: slog.New(slog.NewTextHandler(&output, nil))}
	report := routercontrol.ApplicationReport{
		State:       routercontrol.ApplicationPending,
		Credentials: []routercontrol.CredentialRevision{{BindingID: "traffic", Revision: "credential-value-must-not-appear"}},
		Resources:   []routercontrol.ResourceApplication{{ResourceID: "listener", State: routercontrol.ApplicationPending, Reason: "local router read-back mismatch: tcpListener field host"}},
	}
	logger.Log(report)
	logger.Log(report)
	report.Resources[0].Reason = strings.Repeat("x", 600) + "\nbody"
	logger.Log(report)
	logged := output.String()
	if count := strings.Count(logged, "router application not applied"); count != 2 {
		t.Fatalf("logged %d diagnostics, want one per distinct report: %q", count, logged)
	}
	if !strings.Contains(logged, "tcpListener") || strings.Contains(logged, "credential-value-must-not-appear") || strings.Contains(logged, "\nbody") {
		t.Fatalf("application diagnostic is missing or unsafe: %q", logged)
	}
	if len(logged) > 1400 {
		t.Fatalf("application diagnostic was not bounded: %d bytes", len(logged))
	}
}

type runtimeTransportSink struct {
	accepted chan routercontrol.Accepted
}

func (*runtimeTransportSink) Connected(routercontrol.SessionKey, string, routercontrol.Hello) {}
func (s *runtimeTransportSink) Accepted(_ routercontrol.SessionKey, accepted routercontrol.Accepted) {
	s.accepted <- accepted
}
func (*runtimeTransportSink) Application(context.Context, routercontrol.SessionKey, routercontrol.ApplicationReport) error {
	return nil
}
func (*runtimeTransportSink) Observation(context.Context, routercontrol.SessionKey, routercontrol.ObservationSnapshot) error {
	return nil
}
func (*runtimeTransportSink) Disconnected(routercontrol.SessionKey, string) {}

func TestOneShotReceiverSequencesFullRefreshDeltaAndAck(t *testing.T) {
	initial := testIntent()
	initial.ServiceConnectors[0].Endpoints = make([]routercontrol.Endpoint, 100)
	for i := range initial.ServiceConnectors[0].Endpoints {
		initial.ServiceConnectors[0].Endpoints[i] = routercontrol.Endpoint{ID: "pod-" + strconv.Itoa(i), Host: "10.0.0.2", Port: 8080}
	}
	initial.ServiceListeners = []routercontrol.ServiceListener{{ID: "listener", Host: "0.0.0.0", Port: 8080, Protocol: routercontrol.ProtocolTCP, RoutingKeys: []string{"orders"}, TLS: routercontrol.TLSIntent{Mode: routercontrol.TLSModeDisabled}}}
	publisher := routercontrol.NewPublisher()
	if _, err := publisher.Publish(initial); err != nil {
		t.Fatal(err)
	}
	sink := &runtimeTransportSink{accepted: make(chan routercontrol.Accepted, 2)}
	identity := routercontrol.SessionIdentity{PodUID: "pod", ServiceAccountUID: "sa"}
	server, err := routercontrol.NewServer(publisher, func(context.Context, routercontrol.TargetIdentity) (routercontrol.SessionIdentity, error) {
		return identity, nil
	}, sink, routercontrol.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	options, err := routercontrol.GRPCServerOptions(routercontrol.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer(options...)
	routercontrol.RegisterRouterControlServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(listener) }()
	defer grpcServer.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, err := grpc.DialContext(ctx, "bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	client, err := routercontrol.OpenClientSession(ctx, connection, routercontrol.Hello{Target: initial.Target, AdaptorInstanceID: "adaptor", RouterIncarnation: "router"}, routercontrol.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	receive := func() routercontrol.ServerEvent {
		t.Helper()
		events, done := startControlReceiver(ctx, client)
		select {
		case item := <-events:
			<-done
			if item.err != nil {
				t.Fatal(item.err)
			}
			return item.event
		case <-ctx.Done():
			t.Fatal("timed out receiving router-control event")
			return routercontrol.ServerEvent{}
		}
	}
	waitAccepted := func() {
		t.Helper()
		select {
		case <-sink.accepted:
		case <-ctx.Done():
			t.Fatal("server did not process acceptance")
		}
	}
	full := receive()
	if full.Intent == nil || full.Intent.Kind != routercontrol.TransferSnapshot {
		t.Fatalf("first event is not a full snapshot: %#v", full)
	}
	if err := client.Accept(*full.Intent); err != nil {
		t.Fatal(err)
	}
	waitAccepted()
	key := routercontrol.SessionKey{Target: initial.Target, Identity: identity}
	if err := server.RequestRefresh(key, client.SessionID(), "refresh-1", routercontrol.ObservationScopeResources); err != nil {
		t.Fatal(err)
	}
	refresh := receive()
	if refresh.Refresh == nil || refresh.Refresh.RequestID != "refresh-1" {
		t.Fatalf("refresh event = %#v", refresh)
	}
	next := initial
	next.ServiceListeners = append([]routercontrol.ServiceListener(nil), initial.ServiceListeners...)
	next.ServiceListeners[0].Port = 8081
	if _, err := publisher.Publish(next); err != nil {
		t.Fatal(err)
	}
	delta := receive()
	if delta.Intent == nil || delta.Intent.Kind != routercontrol.TransferDelta {
		if delta.Intent == nil {
			t.Fatalf("second intent is not an intent: %#v", delta)
		}
		t.Fatalf("second intent kind = %q, want %q", delta.Intent.Kind, routercontrol.TransferDelta)
	}
	if err := client.Accept(*delta.Intent); err != nil {
		t.Fatal(err)
	}
	waitAccepted()
	if err := server.RequestRefresh(key, client.SessionID(), "refresh-2", routercontrol.ObservationScopeAddresses); err != nil {
		t.Fatal(err)
	}
	refresh = receive()
	if refresh.Refresh == nil || refresh.Refresh.RequestID != "refresh-2" {
		t.Fatalf("session did not remain stable after delta acceptance: %#v", refresh)
	}
}
