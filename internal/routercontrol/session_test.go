package routercontrol

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

type testSink struct {
	mu           sync.Mutex
	disconnected []SessionKey
}

func (*testSink) Application(context.Context, SessionKey, ApplicationReport) error   { return nil }
func (*testSink) Observation(context.Context, SessionKey, ObservationSnapshot) error { return nil }
func (s *testSink) Disconnected(key SessionKey, _ string) {
	s.mu.Lock()
	s.disconnected = append(s.disconnected, key)
	s.mu.Unlock()
}

func startTestServer(t *testing.T, publisher *Publisher, limits Limits) (*grpc.ClientConn, *Server, func()) {
	t.Helper()
	listener := bufconn.Listen(8 * 1024 * 1024)
	authorize := func(ctx context.Context, _ TargetIdentity) (SessionIdentity, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		pod := "pod-default"
		if values := md.Get("test-pod"); len(values) == 1 {
			pod = values[0]
		}
		return SessionIdentity{PodUID: pod, ServiceAccountUID: "sa-uid"}, nil
	}
	server, err := NewServer(publisher, authorize, &testSink{}, limits)
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	RegisterRouterControlServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(listener) }()
	connection, err := grpc.DialContext(context.Background(), "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	return connection, server, func() { connection.Close(); grpcServer.Stop(); listener.Close() }
}

func openTestSession(t *testing.T, ctx context.Context, connection *grpc.ClientConn, target TargetIdentity, pod string, limits Limits) *ClientSession {
	t.Helper()
	ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("test-pod", pod))
	session, err := OpenClientSession(ctx, connection, Hello{Target: target, AdaptorInstanceID: "adaptor-" + pod, RouterIncarnation: "router-1"}, limits)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func largeIntent() RouterIntent {
	intent := testIntent()
	endpoints := make([]Endpoint, 7000)
	for i := range endpoints {
		endpoints[i] = Endpoint{ID: strings.Repeat("x", 120) + "-" + string(rune(i+1)), Host: "host.example", Port: 8080}
	}
	intent.ServiceConnectors[0].Endpoints = endpoints
	return intent
}

func TestSessionChunksLargeIntentCoalescesAgainstAcceptedAndRefreshesExactPod(t *testing.T) {
	publisher := NewPublisher()
	initial := largeIntent()
	canonical, _, err := CanonicalIntent(initial)
	if err != nil {
		t.Fatal(err)
	}
	if len(canonical) <= 1024*1024 {
		t.Fatalf("fixture is only %d bytes", len(canonical))
	}
	if _, err := publisher.Publish(initial); err != nil {
		t.Fatal(err)
	}
	limits := Limits{ChunkBytes: 64 * 1024, MaxDocumentBytes: 4 * 1024 * 1024, MaxChunks: 128}
	connection, server, stop := startTestServer(t, publisher, limits)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	clientA := openTestSession(t, ctx, connection, initial.Target, "pod-a", limits)
	update, err := clientA.NextIntent()
	if err != nil {
		t.Fatal(err)
	}
	if update.Kind != TransferSnapshot {
		t.Fatalf("first transfer kind = %s", update.Kind)
	}
	if err := clientA.Accept(update); err != nil {
		t.Fatal(err)
	}

	// A distinct rollout Pod for the same target remains connected and receives
	// its own full-first stream instead of replacing pod-a.
	clientB := openTestSession(t, ctx, connection, initial.Target, "pod-b", limits)
	updateB, err := clientB.NextIntent()
	if err != nil {
		t.Fatal(err)
	}
	if updateB.Kind != TransferSnapshot {
		t.Fatalf("rollout Pod first transfer kind = %s", updateB.Kind)
	}
	if err := clientB.Accept(updateB); err != nil {
		t.Fatal(err)
	}

	next := initial
	next.ServiceListeners = append([]ServiceListener(nil), initial.ServiceListeners...)
	next.ServiceListeners[0].Port = 8081
	if _, err := publisher.Publish(next); err != nil {
		t.Fatal(err)
	}
	pending, err := clientA.NextIntent()
	if err != nil {
		t.Fatal(err)
	}
	newest := next
	newest.ServiceListeners = append([]ServiceListener(nil), next.ServiceListeners...)
	newest.ServiceListeners[0].Port = 8082
	if _, err := publisher.Publish(newest); err != nil {
		t.Fatal(err)
	}
	if err := clientA.Accept(pending); err != nil {
		t.Fatal(err)
	}
	coalesced, err := clientA.NextIntent()
	if err != nil {
		t.Fatal(err)
	}
	if coalesced.Kind != TransferDelta || coalesced.Intent.ServiceListeners[0].Port != 8082 {
		t.Fatalf("coalesced update = %#v", coalesced)
	}
	// No Applied report was sent: delta progression depends on Accepted only.

	keyA := SessionKey{Target: initial.Target, Identity: SessionIdentity{PodUID: "pod-a", ServiceAccountUID: "sa-uid"}}
	if err := server.RequestRefresh(keyA, "refresh-a", ObservationScopeAddresses); err != nil {
		t.Fatal(err)
	}
	if err := clientA.Accept(coalesced); err != nil {
		t.Fatal(err)
	}
	event, err := clientA.NextEvent()
	if err != nil {
		t.Fatal(err)
	}
	if event.Refresh == nil || event.Refresh.RequestID != "refresh-a" {
		t.Fatalf("refresh event = %#v", event)
	}
	if err := clientA.SendObservation(ObservationSnapshot{Scope: ObservationScopeAddresses, SampleSequence: 3, RefreshRequestID: "refresh-a", Knowledge: KnowledgeComplete, RouterIncarnation: "router-1"}); err != nil {
		t.Fatal(err)
	}

	if err := clientA.CloseSend(); err != nil {
		t.Fatal(err)
	}
	reconnected := openTestSession(t, ctx, connection, initial.Target, "pod-a", limits)
	full, err := reconnected.NextIntent()
	if err != nil {
		t.Fatal(err)
	}
	if full.Kind != TransferSnapshot || full.Intent.ServiceListeners[0].Port != 8082 {
		t.Fatalf("reconnect did not get latest full snapshot: %#v", full)
	}
}

func TestSessionEnforcesDocumentLimit(t *testing.T) {
	publisher := NewPublisher()
	intent := testIntent()
	intent.ServiceListeners[0].Observer = strings.Repeat("x", 4096)
	if _, err := publisher.Publish(intent); err != nil {
		t.Fatal(err)
	}
	limits := Limits{ChunkBytes: 256, MaxDocumentBytes: 1024, MaxChunks: 8}
	connection, _, stop := startTestServer(t, publisher, limits)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := openTestSession(t, ctx, connection, intent.Target, "pod-limit", limits)
	if _, err := client.NextIntent(); err == nil {
		t.Fatal("oversized intent was delivered")
	}
}
