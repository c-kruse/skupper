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
	lifecycle    []string
}

func (s *testSink) Connected(_ SessionKey, sessionID string, _ Hello) {
	s.mu.Lock()
	s.lifecycle = append(s.lifecycle, "connected:"+sessionID)
	s.mu.Unlock()
}
func (s *testSink) Accepted(_ SessionKey, accepted Accepted) {
	s.mu.Lock()
	s.lifecycle = append(s.lifecycle, "accepted:"+accepted.SessionID)
	s.mu.Unlock()
}
func (s *testSink) Application(_ context.Context, _ SessionKey, report ApplicationReport) error {
	s.mu.Lock()
	s.lifecycle = append(s.lifecycle, "application:"+report.SessionID)
	s.mu.Unlock()
	return nil
}
func (s *testSink) Observation(_ context.Context, _ SessionKey, observation ObservationSnapshot) error {
	s.mu.Lock()
	s.lifecycle = append(s.lifecycle, "observation:"+observation.SessionID)
	s.mu.Unlock()
	return nil
}
func (s *testSink) Disconnected(key SessionKey, _ string) {
	s.mu.Lock()
	s.disconnected = append(s.disconnected, key)
	s.mu.Unlock()
}

func startTestServer(t *testing.T, publisher *Publisher, limits Limits) (*grpc.ClientConn, *Server, *testSink, func()) {
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
	sink := &testSink{}
	server, err := NewServer(publisher, authorize, sink, limits)
	if err != nil {
		t.Fatal(err)
	}
	options, err := GRPCServerOptions(limits)
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer(options...)
	RegisterRouterControlServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(listener) }()
	connection, err := grpc.DialContext(context.Background(), "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	return connection, server, sink, func() { connection.Close(); grpcServer.Stop(); listener.Close() }
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
	limits := Limits{ChunkBytes: 64 * 1024, MaxDocumentBytes: 4 * 1024 * 1024, MaxChunks: 128, MaxReportBytes: 1024 * 1024}
	connection, server, sink, stop := startTestServer(t, publisher, limits)
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
	if err := clientA.SendApplication(ApplicationReport{Sequence: update.Sequence, IntentDigest: update.Digest, RouterIncarnation: "router-1", RealizationID: "realization-1", State: ApplicationApplied}); err != nil {
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
	if err := server.RequestRefresh(keyA, clientA.SessionID(), "refresh-a", ObservationScopeAddresses); err != nil {
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
	deadline := time.Now().Add(2 * time.Second)
	var lifecycle []string
	for {
		sink.mu.Lock()
		lifecycle = append([]string(nil), sink.lifecycle...)
		sink.mu.Unlock()
		found := false
		for _, event := range lifecycle {
			if event == "observation:"+clientA.SessionID() {
				found = true
			}
		}
		if found || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	wantOrder := []string{"connected:", "accepted:", "application:", "accepted:", "observation:"}
	nextEvent := 0
	for _, event := range lifecycle {
		if event == wantOrder[nextEvent]+clientA.SessionID() {
			nextEvent++
			if nextEvent == len(wantOrder) {
				break
			}
		}
	}
	if nextEvent != len(wantOrder) {
		t.Fatalf("session lifecycle order = %v", lifecycle)
	}

	if err := clientA.CloseSend(); err != nil {
		t.Fatal(err)
	}
	reconnected := openTestSession(t, ctx, connection, initial.Target, "pod-a", limits)
	if err := server.RequestRefresh(keyA, clientA.SessionID(), "stale", ObservationScopeAddresses); err == nil {
		t.Fatal("refresh accepted stale replaced session ID")
	}
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
	limits := Limits{ChunkBytes: 256, MaxDocumentBytes: 1024, MaxChunks: 8, MaxReportBytes: 1024}
	connection, _, _, stop := startTestServer(t, publisher, limits)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := openTestSession(t, ctx, connection, intent.Target, "pod-limit", limits)
	if _, err := client.NextIntent(); err == nil {
		t.Fatal("oversized intent was delivered")
	}
}

func TestSessionStateRejectsUnacceptedAndNonMonotonicReports(t *testing.T) {
	state := newSessionState(Hello{RouterIncarnation: "router-1"})
	digest := Digest(strings.Repeat("a", 64))
	report := ApplicationReport{Sequence: 1, IntentDigest: digest, RouterIncarnation: "router-1", RealizationID: "r", State: ApplicationApplied}
	if err := state.validateApplication(report); err == nil {
		t.Fatal("accepted application before Accepted")
	}
	state.setAccepted(Accepted{Sequence: 2, Digest: digest})
	if err := state.validateApplication(report); err == nil {
		t.Fatal("accepted application for wrong desired sequence")
	}
	report.Sequence = 2
	if err := state.validateApplication(report); err != nil {
		t.Fatalf("current application rejected: %v", err)
	}
	report.RouterIncarnation = "old-router"
	if err := state.validateApplication(report); err == nil {
		t.Fatal("accepted stale router incarnation")
	}

	observation := ObservationSnapshot{Scope: ObservationScopeResources, SampleSequence: 3, RouterIncarnation: "router-1", Knowledge: KnowledgeComplete}
	if err := state.validateObservation(observation); err != nil {
		t.Fatal(err)
	}
	state.recordObservation(observation)
	if err := state.validateObservation(observation); err == nil {
		t.Fatal("accepted repeated observation sequence")
	}
}

func TestClientEnforcesReportLimit(t *testing.T) {
	publisher := NewPublisher()
	intent := testIntent()
	if _, err := publisher.Publish(intent); err != nil {
		t.Fatal(err)
	}
	limits := DefaultLimits()
	limits.MaxReportBytes = 512
	connection, _, _, stop := startTestServer(t, publisher, limits)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := openTestSession(t, ctx, connection, intent.Target, "pod-report-limit", limits)
	if err := client.SendObservation(ObservationSnapshot{Scope: ObservationScopeAddresses, SampleSequence: 1, Knowledge: KnowledgeUnknown, RouterIncarnation: "router-1", Reason: strings.Repeat("x", 1024)}); err == nil {
		t.Fatal("oversized observation report was sent")
	}
}

func TestReplacementCancelsPreviousPodSession(t *testing.T) {
	publisher := NewPublisher()
	intent := testIntent()
	if _, err := publisher.Publish(intent); err != nil {
		t.Fatal(err)
	}
	limits := DefaultLimits()
	connection, _, sink, stop := startTestServer(t, publisher, limits)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	oldSession := openTestSession(t, ctx, connection, intent.Target, "same-pod", limits)
	newSession := openTestSession(t, ctx, connection, intent.Target, "same-pod", limits)
	if _, err := newSession.NextIntent(); err != nil {
		t.Fatalf("replacement session did not receive full intent: %v", err)
	}
	// A transfer already buffered by gRPC may still be readable, but the old
	// session must not produce an Accepted lifecycle event or further facts.
	if oldUpdate, err := oldSession.NextIntent(); err == nil {
		_ = oldSession.Accept(oldUpdate)
		_, _ = oldSession.NextIntent()
	}
	time.Sleep(10 * time.Millisecond)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, event := range sink.lifecycle {
		if event == "accepted:"+oldSession.SessionID() {
			t.Fatal("replaced session delivered Accepted to sink")
		}
	}
}

type incompleteServer struct {
	target TargetIdentity
	resync chan *ResyncRequested
}

func (s *incompleteServer) Sync(stream grpc.BidiStreamingServer[ClientMessage, ServerMessage]) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	const sessionID = "incomplete-session"
	if err := stream.Send(&ServerMessage{Welcome: &Welcome{ProtocolVersion: ProtocolVersion, SchemaVersion: SchemaVersion, SessionID: sessionID, Target: s.target}}); err != nil {
		return err
	}
	digest := Digest(strings.Repeat("0", 64))
	if err := stream.Send(&ServerMessage{Begin: &TransferBegin{SessionID: sessionID, TransactionID: "tx", Sequence: 1, Kind: TransferSnapshot, ResultDigest: digest, EncodedSize: 2, ChunkCount: 2}}); err != nil {
		return err
	}
	if err := stream.Send(&ServerMessage{Chunk: &TransferChunk{SessionID: sessionID, TransactionID: "tx", Index: 0, Data: []byte{'{'}}}); err != nil {
		return err
	}
	if err := stream.Send(&ServerMessage{End: &TransferEnd{SessionID: sessionID, TransactionID: "tx", Complete: true}}); err != nil {
		return err
	}
	message, err := stream.Recv()
	if err == nil && message.ResyncRequested != nil {
		s.resync <- message.ResyncRequested
	}
	return err
}

func TestClientRejectsIncompleteTransferAndRequestsResync(t *testing.T) {
	target := testIntent().Target
	implementation := &incompleteServer{target: target, resync: make(chan *ResyncRequested, 1)}
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	RegisterRouterControlServer(server, implementation)
	go func() { _ = server.Serve(listener) }()
	defer func() { server.Stop(); listener.Close() }()
	connection, err := grpc.DialContext(context.Background(), "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := OpenClientSession(ctx, connection, Hello{Target: target, AdaptorInstanceID: "adaptor", RouterIncarnation: "router"}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.NextIntent(); err == nil {
		t.Fatal("incomplete transfer was accepted")
	}
	select {
	case request := <-implementation.resync:
		if request.SessionID != client.SessionID() {
			t.Fatalf("resync session = %q", request.SessionID)
		}
	case <-ctx.Done():
		t.Fatal("client did not request resync")
	}
}
