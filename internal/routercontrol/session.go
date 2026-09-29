package routercontrol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Limits struct {
	ChunkBytes       int
	MaxDocumentBytes int
	MaxChunks        int
	MaxReportBytes   int
}

func DefaultLimits() Limits {
	return Limits{ChunkBytes: 256 * 1024, MaxDocumentBytes: 16 * 1024 * 1024, MaxChunks: 256, MaxReportBytes: 16 * 1024 * 1024}
}

func (l Limits) validate() error {
	if l.ChunkBytes <= 0 || l.MaxDocumentBytes <= 0 || l.MaxChunks <= 0 || l.MaxReportBytes <= 0 {
		return errors.New("router control limits must be positive")
	}
	if l.ChunkBytes > l.MaxDocumentBytes {
		return errors.New("chunk limit exceeds document limit")
	}
	return nil
}

// Server implements RouterControlServer. The containing application owns the
// grpc.Server and its mandatory TLS credentials; this package does not provide
// an insecure listener or authentication fallback.
type Server struct {
	publisher *Publisher
	authorize AuthorizeFunc
	sink      ObservationSink
	limits    Limits

	mu          sync.Mutex
	sessions    map[SessionKey]activeSession
	lifecycleMu sync.Mutex
}

type activeSession struct {
	id      string
	cancel  context.CancelFunc
	refresh chan RefreshRequest
}

type sessionState struct {
	mu                sync.Mutex
	routerIncarnation string
	acceptedDigest    Digest
	acceptedSequence  uint64
	accepted          map[uint64]Digest
	acceptedOrder     []uint64
	sampleSequences   map[string]uint64
}

func newSessionState(hello Hello) *sessionState {
	return &sessionState{routerIncarnation: hello.RouterIncarnation, accepted: map[uint64]Digest{}, sampleSequences: map[string]uint64{}}
}

func NewServer(publisher *Publisher, authorize AuthorizeFunc, sink ObservationSink, limits Limits) (*Server, error) {
	if publisher == nil || authorize == nil || sink == nil {
		return nil, errors.New("publisher, authorizer, and observation sink are required")
	}
	if err := limits.validate(); err != nil {
		return nil, err
	}
	return &Server{publisher: publisher, authorize: authorize, sink: sink, limits: limits, sessions: map[SessionKey]activeSession{}}, nil
}

type sessionAck struct {
	accepted *Accepted
	resync   bool
	done     chan error
}

type pendingTransfer struct {
	transactionID string
	sequence      uint64
	digest        Digest
	intent        RouterIntent
}

func (s *Server) Sync(stream grpc.BidiStreamingServer[ClientMessage, ServerMessage]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if err := validateClientUnion(first); err != nil || first.Hello == nil {
		return status.Error(codes.InvalidArgument, "first message must contain only hello")
	}
	hello := *first.Hello
	if !contains(hello.ProtocolVersions, ProtocolVersion) || !contains(hello.SchemaVersions, SchemaVersion) {
		return status.Error(codes.FailedPrecondition, "no compatible protocol or schema version")
	}
	if hello.AdaptorInstanceID == "" || hello.RouterIncarnation == "" {
		return status.Error(codes.InvalidArgument, "hello requires adaptor and router incarnation IDs")
	}
	if err := validateTarget(hello.Target); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	identity, err := s.authorize(stream.Context(), hello.Target)
	if err != nil {
		return status.Error(codes.PermissionDenied, "router target is not authorized")
	}
	if identity.PodUID == "" || identity.ServiceAccountUID == "" {
		return status.Error(codes.PermissionDenied, "authorizer returned incomplete session identity")
	}

	ctx, cancel := context.WithCancel(stream.Context())
	sessionID := uuid.NewString()
	key := SessionKey{Target: hello.Target, Identity: identity}
	refreshes := make(chan RefreshRequest, 1)
	s.lifecycleMu.Lock()
	s.mu.Lock()
	if previous, found := s.sessions[key]; found {
		previous.cancel()
	}
	s.sessions[key] = activeSession{id: sessionID, cancel: cancel, refresh: refreshes}
	s.mu.Unlock()
	s.sink.Connected(key, sessionID, hello)
	s.lifecycleMu.Unlock()
	var sinkMu sync.Mutex
	defer func() {
		cancel()
		s.lifecycleMu.Lock()
		defer s.lifecycleMu.Unlock()
		sinkMu.Lock()
		s.sink.Disconnected(key, sessionID)
		sinkMu.Unlock()
		s.mu.Lock()
		if current, found := s.sessions[key]; found && current.id == sessionID {
			delete(s.sessions, key)
		}
		s.mu.Unlock()
	}()

	if err := stream.Send(&ServerMessage{Welcome: &Welcome{ProtocolVersion: ProtocolVersion, SchemaVersion: SchemaVersion, SessionID: sessionID, Target: hello.Target}}); err != nil {
		return err
	}
	updates, unsubscribe := s.publisher.subscribe(hello.Target)
	defer unsubscribe()
	acks := make(chan sessionAck, 1)
	receiveErrors := make(chan error, 1)
	state := newSessionState(hello)
	go func() { receiveErrors <- s.receive(ctx, stream, key, sessionID, state, &sinkMu, acks) }()
	sendErrors := make(chan error, 1)
	go func() {
		sendErrors <- s.send(ctx, stream, key, sessionID, state, &sinkMu, updates, acks, refreshes, receiveErrors)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-sendErrors:
		return err
	}
}

func (s *Server) receive(ctx context.Context, stream grpc.BidiStreamingServer[ClientMessage, ServerMessage], key SessionKey, sessionID string, state *sessionState, sinkMu *sync.Mutex, acks chan<- sessionAck) error {
	for {
		message, err := stream.Recv()
		if err != nil {
			return err
		}
		if err := validateClientUnion(message); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		if ctx.Err() != nil || !s.isCurrent(key, sessionID) {
			return context.Canceled
		}
		switch {
		case message.Accepted != nil:
			if message.Accepted.SessionID != sessionID {
				return status.Error(codes.InvalidArgument, "accepted has wrong session")
			}
			ack := sessionAck{accepted: message.Accepted, done: make(chan error, 1)}
			select {
			case acks <- ack:
			case <-ctx.Done():
				return ctx.Err()
			}
			select {
			case err := <-ack.done:
				if err != nil {
					return err
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		case message.Rejected != nil:
			if message.Rejected.SessionID != sessionID {
				return status.Error(codes.InvalidArgument, "rejected has wrong session")
			}
			select {
			case acks <- sessionAck{resync: true}:
			case <-ctx.Done():
				return ctx.Err()
			}
		case message.ResyncRequested != nil:
			if message.ResyncRequested.SessionID != sessionID {
				return status.Error(codes.InvalidArgument, "resync has wrong session")
			}
			select {
			case acks <- sessionAck{resync: true}:
			case <-ctx.Done():
				return ctx.Err()
			}
		case message.ApplicationReport != nil:
			report := *message.ApplicationReport
			if report.SessionID != sessionID {
				return status.Error(codes.InvalidArgument, "application report has wrong session")
			}
			if err := validateApplicationReport(report); err != nil {
				return status.Error(codes.InvalidArgument, err.Error())
			}
			superseded, err := state.validateApplication(report)
			if err != nil {
				return status.Error(codes.FailedPrecondition, err.Error())
			}
			if superseded {
				continue
			}
			sinkMu.Lock()
			if ctx.Err() != nil || !s.isCurrent(key, sessionID) {
				sinkMu.Unlock()
				return context.Canceled
			}
			err = s.sink.Application(ctx, key, report)
			sinkMu.Unlock()
			if err != nil {
				return err
			}
		case message.Observation != nil:
			observation := *message.Observation
			if observation.SessionID != sessionID {
				return status.Error(codes.InvalidArgument, "observation has wrong session")
			}
			if err := ValidateObservation(observation); err != nil {
				return status.Error(codes.InvalidArgument, err.Error())
			}
			if err := state.validateObservation(observation); err != nil {
				return status.Error(codes.FailedPrecondition, err.Error())
			}
			sinkMu.Lock()
			if ctx.Err() != nil || !s.isCurrent(key, sessionID) {
				sinkMu.Unlock()
				return context.Canceled
			}
			err := s.sink.Observation(ctx, key, observation)
			sinkMu.Unlock()
			if err != nil {
				return err
			}
			state.recordObservation(observation)
		case message.ReportBegin != nil:
			if err := s.receiveReport(ctx, stream, key, sessionID, state, sinkMu, *message.ReportBegin); err != nil {
				return err
			}
		case message.ReportChunk != nil, message.ReportEnd != nil:
			return status.Error(codes.InvalidArgument, "report chunk or end without begin")
		case message.Heartbeat != nil:
			if message.Heartbeat.SessionID != sessionID {
				return status.Error(codes.InvalidArgument, "heartbeat has wrong session")
			}
		case message.Hello != nil:
			return status.Error(codes.InvalidArgument, "hello may only be sent once")
		}
	}
}

func (s *Server) receiveReport(ctx context.Context, stream grpc.BidiStreamingServer[ClientMessage, ServerMessage], key SessionKey, sessionID string, state *sessionState, sinkMu *sync.Mutex, begin ReportBegin) error {
	if begin.SessionID != sessionID || begin.TransactionID == "" {
		return status.Error(codes.InvalidArgument, "report begin has wrong session or empty transaction")
	}
	if begin.Kind != ReportApplication && begin.Kind != ReportObservation {
		return status.Error(codes.InvalidArgument, "unknown report kind")
	}
	if begin.EncodedSize > uint64(s.limits.MaxReportBytes) || begin.ChunkCount == 0 || begin.ChunkCount > uint32(s.limits.MaxChunks) {
		return status.Error(codes.ResourceExhausted, "report exceeds receive limits")
	}
	expectedChunks := (begin.EncodedSize + uint64(s.limits.ChunkBytes) - 1) / uint64(s.limits.ChunkBytes)
	if expectedChunks == 0 {
		expectedChunks = 1
	}
	if uint64(begin.ChunkCount) != expectedChunks {
		return status.Error(codes.InvalidArgument, "report chunk count does not match encoded size")
	}
	content := bytes.NewBuffer(make([]byte, 0, int(begin.EncodedSize)))
	for index := uint32(0); index < begin.ChunkCount; index++ {
		message, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("incomplete report transfer: %w", err)
		}
		if err := validateClientUnion(message); err != nil || message.ReportChunk == nil {
			return status.Error(codes.InvalidArgument, "incomplete report transfer: expected chunk")
		}
		if ctx.Err() != nil || !s.isCurrent(key, sessionID) {
			return context.Canceled
		}
		chunk := message.ReportChunk
		if chunk.SessionID != sessionID || chunk.TransactionID != begin.TransactionID || chunk.Index != index {
			return status.Error(codes.InvalidArgument, "report chunk session, transaction, or order mismatch")
		}
		expectedBytes := s.limits.ChunkBytes
		if index == begin.ChunkCount-1 {
			expectedBytes = int(begin.EncodedSize) - content.Len()
		}
		if len(chunk.Data) != expectedBytes {
			return status.Error(codes.InvalidArgument, "report chunk length does not match encoded size")
		}
		content.Write(chunk.Data)
	}
	message, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("incomplete report transfer: %w", err)
	}
	if err := validateClientUnion(message); err != nil || message.ReportEnd == nil {
		return status.Error(codes.InvalidArgument, "incomplete report transfer: expected end")
	}
	end := message.ReportEnd
	if end.SessionID != sessionID || end.TransactionID != begin.TransactionID || !end.Complete || uint64(content.Len()) != begin.EncodedSize {
		return status.Error(codes.InvalidArgument, "invalid or incomplete report end")
	}
	if ctx.Err() != nil || !s.isCurrent(key, sessionID) {
		return context.Canceled
	}
	switch begin.Kind {
	case ReportApplication:
		var report ApplicationReport
		if err := decodeStrict(content.Bytes(), &report); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		if report.SessionID != sessionID {
			return status.Error(codes.InvalidArgument, "application report has wrong session")
		}
		if err := validateApplicationReport(report); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		superseded, err := state.validateApplication(report)
		if err != nil {
			return status.Error(codes.FailedPrecondition, err.Error())
		}
		if superseded {
			return nil
		}
		sinkMu.Lock()
		defer sinkMu.Unlock()
		if ctx.Err() != nil || !s.isCurrent(key, sessionID) {
			return context.Canceled
		}
		return s.sink.Application(ctx, key, report)
	case ReportObservation:
		var observation ObservationSnapshot
		if err := decodeStrict(content.Bytes(), &observation); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		if observation.SessionID != sessionID {
			return status.Error(codes.InvalidArgument, "observation has wrong session")
		}
		if err := ValidateObservation(observation); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		if err := state.validateObservation(observation); err != nil {
			return status.Error(codes.FailedPrecondition, err.Error())
		}
		sinkMu.Lock()
		defer sinkMu.Unlock()
		if ctx.Err() != nil || !s.isCurrent(key, sessionID) {
			return context.Canceled
		}
		if err := s.sink.Observation(ctx, key, observation); err != nil {
			return err
		}
		state.recordObservation(observation)
		return nil
	}
	return nil
}

func (s *Server) send(ctx context.Context, stream grpc.BidiStreamingServer[ClientMessage, ServerMessage], key SessionKey, sessionID string, state *sessionState, sinkMu *sync.Mutex, updates <-chan struct{}, acks <-chan sessionAck, refreshes <-chan RefreshRequest, receiveErrors <-chan error) error {
	var accepted *publishedIntent
	var pending *pendingTransfer
	var sentRevision uint64
	var sent bool
	sequence := uint64(0)
	for {
		published := s.publisher.current(key.Target)
		if pending == nil && (!sent || published.revision != sentRevision) {
			if !published.available {
				if err := stream.Send(&ServerMessage{IntentUnavailable: true}); err != nil {
					return err
				}
				sentRevision = published.revision
				sent = true
			} else {
				sequence++
				kind := TransferSnapshot
				content := published.canonical
				baseDigest := Digest("")
				if accepted != nil {
					delta, err := NewIntentDelta(accepted.intent, published.intent)
					if err != nil {
						return err
					}
					deltaContent, err := CanonicalDelta(delta)
					if err != nil {
						return err
					}
					if len(deltaContent) < len(content) {
						kind, content, baseDigest = TransferDelta, deltaContent, accepted.digest
					}
				}
				transactionID := uuid.NewString()
				if err := sendTransfer(stream, s.limits, sessionID, transactionID, sequence, kind, baseDigest, published.digest, content); err != nil {
					return err
				}
				pending = &pendingTransfer{transactionID: transactionID, sequence: sequence, digest: published.digest, intent: published.intent}
				sentRevision = published.revision
				sent = true
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-receiveErrors:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case <-updates:
			// The loop reloads only the newest state. A pending transfer remains
			// the sole unacknowledged transfer and intermediate states coalesce.
		case refresh := <-refreshes:
			refresh.SessionID = sessionID
			if err := stream.Send(&ServerMessage{RefreshRequest: &refresh}); err != nil {
				return err
			}
		case ack := <-acks:
			if ack.resync {
				accepted, pending, sentRevision, sent = nil, nil, 0, false
				continue
			}
			if pending == nil && state.isAccepted(*ack.accepted) {
				if ack.done != nil {
					ack.done <- nil
				}
				continue
			}
			if pending == nil || ack.accepted.TransactionID != pending.transactionID || ack.accepted.Sequence != pending.sequence || ack.accepted.Digest != pending.digest {
				err := status.Error(codes.InvalidArgument, "acceptance does not match pending transfer")
				if ack.done != nil {
					ack.done <- err
				}
				return err
			}
			sinkMu.Lock()
			if ctx.Err() != nil || !s.isCurrent(key, sessionID) {
				sinkMu.Unlock()
				if ack.done != nil {
					ack.done <- context.Canceled
				}
				return context.Canceled
			}
			s.sink.Accepted(key, *ack.accepted)
			sinkMu.Unlock()
			state.setAccepted(*ack.accepted)
			accepted = &publishedIntent{intent: pending.intent, digest: pending.digest, available: true}
			pending = nil
			if ack.done != nil {
				ack.done <- nil
			}
		}
	}
}

func (s *Server) isCurrent(key SessionKey, sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, found := s.sessions[key]
	return found && current.id == sessionID
}

func (s *sessionState) setAccepted(accepted Accepted) {
	s.mu.Lock()
	s.acceptedDigest = accepted.Digest
	s.acceptedSequence = accepted.Sequence
	s.accepted[accepted.Sequence] = accepted.Digest
	s.acceptedOrder = append(s.acceptedOrder, accepted.Sequence)
	if len(s.acceptedOrder) > 64 {
		delete(s.accepted, s.acceptedOrder[0])
		s.acceptedOrder = s.acceptedOrder[1:]
	}
	s.mu.Unlock()
}

func (s *sessionState) isAccepted(accepted Accepted) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acceptedDigest == accepted.Digest && s.acceptedSequence == accepted.Sequence
}

// validateApplication returns superseded=true for evidence matching an older
// accepted intent. Such evidence is ignored rather than closing the stream.
func (s *sessionState) validateApplication(report ApplicationReport) (superseded bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if report.RouterIncarnation != s.routerIncarnation {
		return false, errors.New("application report router incarnation does not match hello")
	}
	if report.IntentDigest == s.acceptedDigest && report.Sequence == s.acceptedSequence {
		return false, nil
	}
	if acceptedDigest, found := s.accepted[report.Sequence]; found && acceptedDigest == report.IntentDigest && report.Sequence < s.acceptedSequence {
		return true, nil
	}
	return false, errors.New("application report does not match an accepted intent")
}

func (s *sessionState) validateObservation(observation ObservationSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if observation.RouterIncarnation != s.routerIncarnation {
		return errors.New("observation router incarnation does not match hello")
	}
	if observation.SampleSequence <= s.sampleSequences[observation.Scope] {
		return errors.New("observation sample sequence is not increasing")
	}
	return nil
}

func (s *sessionState) recordObservation(observation ObservationSnapshot) {
	s.mu.Lock()
	s.sampleSequences[observation.Scope] = observation.SampleSequence
	s.mu.Unlock()
}

// GRPCServerOptions returns a per-message receive limit large enough for one
// encoded report chunk. Total reconstructed report size is separately bounded.
func GRPCServerOptions(limits Limits) ([]grpc.ServerOption, error) {
	if err := limits.validate(); err != nil {
		return nil, err
	}
	return []grpc.ServerOption{grpc.MaxRecvMsgSize(max(1024*1024, limits.ChunkBytes*2))}, nil
}

// RequestRefresh delivers a controller-timed request to one exact active Pod
// session. Scheduling, deadlines, and observation freshness remain owned by the
// controller observation cache.
func (s *Server) RequestRefresh(key SessionKey, expectedSessionID, requestID, scope string) error {
	if expectedSessionID == "" {
		return errors.New("expected session ID is required")
	}
	if requestID == "" {
		return errors.New("refresh request ID is required")
	}
	if scope != ObservationScopeResources && scope != ObservationScopeAddresses {
		return fmt.Errorf("unknown observation scope %q", scope)
	}
	s.mu.Lock()
	session, found := s.sessions[key]
	if !found {
		s.mu.Unlock()
		return errors.New("router session is not connected")
	}
	if session.id != expectedSessionID {
		s.mu.Unlock()
		return errors.New("router session was replaced")
	}
	request := RefreshRequest{RequestID: requestID, Scope: scope}
	select {
	case session.refresh <- request:
		s.mu.Unlock()
		return nil
	default:
		s.mu.Unlock()
		return errors.New("router session already has a pending refresh request")
	}
}

func sendTransfer(stream grpc.BidiStreamingServer[ClientMessage, ServerMessage], limits Limits, sessionID, transactionID string, sequence uint64, kind TransferKind, baseDigest, resultDigest Digest, content []byte) error {
	if len(content) > limits.MaxDocumentBytes {
		return status.Error(codes.ResourceExhausted, "intent exceeds document limit")
	}
	chunkCount := (len(content) + limits.ChunkBytes - 1) / limits.ChunkBytes
	if chunkCount == 0 {
		chunkCount = 1
	}
	if chunkCount > limits.MaxChunks {
		return status.Error(codes.ResourceExhausted, "intent exceeds chunk limit")
	}
	begin := &TransferBegin{SessionID: sessionID, TransactionID: transactionID, Sequence: sequence, Kind: kind, BaseDigest: baseDigest, ResultDigest: resultDigest, EncodedSize: uint64(len(content)), ChunkCount: uint32(chunkCount)}
	if err := stream.Send(&ServerMessage{Begin: begin}); err != nil {
		return err
	}
	for i := 0; i < chunkCount; i++ {
		start := i * limits.ChunkBytes
		end := min(start+limits.ChunkBytes, len(content))
		chunk := append([]byte(nil), content[start:end]...)
		if err := stream.Send(&ServerMessage{Chunk: &TransferChunk{SessionID: sessionID, TransactionID: transactionID, Index: uint32(i), Data: chunk}}); err != nil {
			return err
		}
	}
	return stream.Send(&ServerMessage{End: &TransferEnd{SessionID: sessionID, TransactionID: transactionID, Complete: true}})
}

func validateClientUnion(message *ClientMessage) error {
	count := 0
	for _, set := range []bool{message.Hello != nil, message.Accepted != nil, message.Rejected != nil, message.ResyncRequested != nil, message.ApplicationReport != nil, message.Observation != nil, message.ReportBegin != nil, message.ReportChunk != nil, message.ReportEnd != nil, message.Heartbeat != nil} {
		if set {
			count++
		}
	}
	if count != 1 {
		return errors.New("client message must contain exactly one message")
	}
	return nil
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func validateApplicationReport(report ApplicationReport) error {
	if !validDigest(report.IntentDigest) || report.RouterIncarnation == "" || report.RealizationID == "" {
		return errors.New("application report requires digest, router incarnation, and realization ID")
	}
	switch report.State {
	case ApplicationPending, ApplicationApplied, ApplicationFailed:
	default:
		return fmt.Errorf("invalid application state %q", report.State)
	}
	seen := map[ResourceID]struct{}{}
	for _, credential := range report.Credentials {
		if credential.BindingID == "" || credential.Revision == "" {
			return errors.New("credential revision requires binding ID and revision")
		}
		if _, found := seen[credential.BindingID]; found {
			return fmt.Errorf("duplicate credential revision %q", credential.BindingID)
		}
		seen[credential.BindingID] = struct{}{}
	}
	resourceIDs := map[ResourceID]struct{}{}
	for _, resource := range report.Resources {
		if resource.ResourceID == "" {
			return errors.New("resource application requires resource ID")
		}
		if _, found := resourceIDs[resource.ResourceID]; found {
			return fmt.Errorf("duplicate resource application %q", resource.ResourceID)
		}
		resourceIDs[resource.ResourceID] = struct{}{}
		switch resource.State {
		case ApplicationPending, ApplicationApplied, ApplicationFailed:
		default:
			return fmt.Errorf("resource %q has invalid application state %q", resource.ResourceID, resource.State)
		}
	}
	return nil
}

func ValidateObservation(observation ObservationSnapshot) error {
	if observation.Scope != ObservationScopeResources && observation.Scope != ObservationScopeAddresses {
		return fmt.Errorf("unknown observation scope %q", observation.Scope)
	}
	if observation.RouterIncarnation == "" {
		return errors.New("observation requires router incarnation")
	}
	if observation.SampleSequence == 0 {
		return errors.New("observation requires a positive sample sequence")
	}
	switch observation.Knowledge {
	case KnowledgeUnknown:
		if len(observation.Resources) != 0 || len(observation.Addresses) != 0 {
			return errors.New("unknown observation cannot contain current facts")
		}
		if observation.Reason == "" {
			return errors.New("unknown observation requires a reason")
		}
	case KnowledgeComplete, KnowledgePartial:
	default:
		return fmt.Errorf("invalid observation knowledge %q", observation.Knowledge)
	}
	if observation.Scope == ObservationScopeResources && len(observation.Addresses) != 0 {
		return errors.New("resource scope cannot contain addresses")
	}
	if observation.Scope == ObservationScopeAddresses && len(observation.Resources) != 0 {
		return errors.New("address scope cannot contain resources")
	}
	resourceIDs := map[ResourceID]struct{}{}
	for _, resource := range observation.Resources {
		if resource.ResourceID == "" {
			return errors.New("resource observation requires resource ID")
		}
		if _, found := resourceIDs[resource.ResourceID]; found {
			return fmt.Errorf("duplicate resource observation %q", resource.ResourceID)
		}
		resourceIDs[resource.ResourceID] = struct{}{}
		switch resource.Operational {
		case OperationalUnknown, OperationalUp, OperationalDown:
		default:
			return fmt.Errorf("resource %q has invalid operational state %q", resource.ResourceID, resource.Operational)
		}
	}
	addressKeys := map[string]struct{}{}
	for _, address := range observation.Addresses {
		if address.RoutingKey == "" {
			return errors.New("address observation requires routing key")
		}
		if _, found := addressKeys[address.RoutingKey]; found {
			return fmt.Errorf("duplicate address observation %q", address.RoutingKey)
		}
		addressKeys[address.RoutingKey] = struct{}{}
		if address.Reachable != (address.SubscriberCount > 0 || address.InProcessCount > 0 || address.RemoteCount > 0) {
			return fmt.Errorf("address %q reachable flag disagrees with counts", address.RoutingKey)
		}
	}
	return nil
}
