package routercontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"google.golang.org/grpc"
)

// IntentUpdate is a fully reconstructed candidate. The accepted base changes
// only when Accept succeeds; receiving an update does not acknowledge it.
type IntentUpdate struct {
	Intent        RouterIntent
	Digest        Digest
	Sequence      uint64
	TransactionID string
	Kind          TransferKind
	Unavailable   bool
}

type ServerEvent struct {
	Intent  *IntentUpdate
	Refresh *RefreshRequest
}

// ClientSession is an authenticated stream over a caller-supplied gRPC
// connection. The caller is responsible for constructing that connection with
// the auth worker's mTLS credentials; this API has no insecure dial path.
type ClientSession struct {
	stream grpc.BidiStreamingClient[ClientMessage, ServerMessage]
	limits Limits
	target TargetIdentity
	id     string
	sendMu sync.Mutex

	accepted       *RouterIntent
	acceptedDigest Digest
	pending        *IntentUpdate
	lastSequence   uint64
}

func OpenClientSession(ctx context.Context, connection grpc.ClientConnInterface, hello Hello, limits Limits) (*ClientSession, error) {
	if connection == nil {
		return nil, errors.New("gRPC connection is required")
	}
	if err := limits.validate(); err != nil {
		return nil, err
	}
	if len(hello.ProtocolVersions) == 0 {
		hello.ProtocolVersions = []string{ProtocolVersion}
	}
	if len(hello.SchemaVersions) == 0 {
		hello.SchemaVersions = []string{SchemaVersion}
	}
	if err := validateTarget(hello.Target); err != nil {
		return nil, err
	}
	if hello.AdaptorInstanceID == "" || hello.RouterIncarnation == "" {
		return nil, errors.New("hello requires adaptor and router incarnation IDs")
	}
	stream, err := NewRouterControlClient(connection).Sync(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(&ClientMessage{Hello: &hello}); err != nil {
		return nil, err
	}
	message, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	if err := validateServerUnion(message); err != nil || message.Welcome == nil {
		return nil, errors.New("first server message must contain only welcome")
	}
	welcome := message.Welcome
	if welcome.ProtocolVersion != ProtocolVersion || welcome.SchemaVersion != SchemaVersion || welcome.Target != hello.Target || welcome.SessionID == "" {
		return nil, errors.New("welcome does not match negotiated target or versions")
	}
	return &ClientSession{stream: stream, limits: limits, target: hello.Target, id: welcome.SessionID}, nil
}

func (s *ClientSession) SessionID() string { return s.id }

// NextIntent returns explicit unavailability or one validated full logical
// document. A protocol/content error sends ResyncRequested and returns the error;
// the caller may call NextIntent again to receive the replacement snapshot.
func (s *ClientSession) NextIntent() (IntentUpdate, error) {
	event, err := s.NextEvent()
	if err != nil {
		return IntentUpdate{}, err
	}
	if event.Refresh != nil {
		return IntentUpdate{}, errors.New("received refresh request; use NextEvent to handle server events")
	}
	return *event.Intent, nil
}

// NextEvent receives either a validated intent candidate/unavailability event
// or a controller-timed refresh request. A single receive loop should dispatch
// these events while report methods send concurrently.
func (s *ClientSession) NextEvent() (ServerEvent, error) {
	if s.pending != nil {
		return ServerEvent{}, errors.New("previous intent update is not accepted or rejected")
	}
	message, err := s.stream.Recv()
	if err != nil {
		return ServerEvent{}, err
	}
	if err := validateServerUnion(message); err != nil {
		return ServerEvent{}, err
	}
	if message.IntentUnavailable {
		update := &IntentUpdate{Unavailable: true}
		return ServerEvent{Intent: update}, nil
	}
	if message.RefreshRequest != nil {
		if message.RefreshRequest.SessionID != s.id || message.RefreshRequest.RequestID == "" {
			return ServerEvent{}, errors.New("invalid refresh request")
		}
		request := *message.RefreshRequest
		return ServerEvent{Refresh: &request}, nil
	}
	if message.Begin == nil {
		return ServerEvent{}, errors.New("expected transfer begin")
	}
	update, err := s.receiveTransfer(*message.Begin)
	if err != nil {
		_ = s.send(&ClientMessage{ResyncRequested: &ResyncRequested{SessionID: s.id, Reason: err.Error()}})
		return ServerEvent{}, err
	}
	pending := update
	pending.Intent = deepCopyIntent(update.Intent)
	s.pending = &pending
	update.Intent = deepCopyIntent(update.Intent)
	return ServerEvent{Intent: &update}, nil
}

func (s *ClientSession) receiveTransfer(begin TransferBegin) (IntentUpdate, error) {
	if begin.SessionID != s.id || begin.TransactionID == "" {
		return IntentUpdate{}, errors.New("transfer begin has wrong session or empty transaction")
	}
	if begin.Sequence <= s.lastSequence {
		return IntentUpdate{}, errors.New("transfer sequence is not increasing")
	}
	if begin.EncodedSize > uint64(s.limits.MaxDocumentBytes) || begin.ChunkCount == 0 || begin.ChunkCount > uint32(s.limits.MaxChunks) {
		return IntentUpdate{}, errors.New("transfer exceeds receive limits")
	}
	if begin.Kind != TransferSnapshot && begin.Kind != TransferDelta {
		return IntentUpdate{}, fmt.Errorf("unknown transfer kind %q", begin.Kind)
	}
	if begin.Kind == TransferSnapshot && begin.BaseDigest != "" {
		return IntentUpdate{}, errors.New("snapshot unexpectedly has a base digest")
	}
	if begin.Kind == TransferDelta && (s.accepted == nil || begin.BaseDigest != s.acceptedDigest) {
		return IntentUpdate{}, errors.New("delta base does not match accepted intent")
	}
	content := bytes.NewBuffer(make([]byte, 0, int(begin.EncodedSize)))
	for index := uint32(0); index < begin.ChunkCount; index++ {
		message, err := s.stream.Recv()
		if err != nil {
			return IntentUpdate{}, fmt.Errorf("incomplete transfer: %w", err)
		}
		if err := validateServerUnion(message); err != nil {
			return IntentUpdate{}, err
		}
		if message.Chunk == nil {
			return IntentUpdate{}, errors.New("incomplete transfer: expected chunk")
		}
		chunk := message.Chunk
		if chunk.SessionID != s.id || chunk.TransactionID != begin.TransactionID || chunk.Index != index {
			return IntentUpdate{}, errors.New("chunk session, transaction, or order mismatch")
		}
		if len(chunk.Data) > s.limits.ChunkBytes || content.Len()+len(chunk.Data) > s.limits.MaxDocumentBytes {
			return IntentUpdate{}, errors.New("chunk exceeds receive limits")
		}
		content.Write(chunk.Data)
	}
	message, err := s.stream.Recv()
	if err != nil {
		return IntentUpdate{}, fmt.Errorf("incomplete transfer: %w", err)
	}
	if err := validateServerUnion(message); err != nil {
		return IntentUpdate{}, err
	}
	if message.End == nil || message.End.SessionID != s.id || message.End.TransactionID != begin.TransactionID || !message.End.Complete {
		return IntentUpdate{}, errors.New("transfer has invalid or incomplete end")
	}
	if uint64(content.Len()) != begin.EncodedSize {
		return IntentUpdate{}, errors.New("transfer encoded size mismatch")
	}
	var intent RouterIntent
	if begin.Kind == TransferSnapshot {
		intent, err = DecodeIntent(content.Bytes(), begin.ResultDigest)
	} else {
		var delta IntentDelta
		delta, err = DecodeDelta(content.Bytes())
		if err == nil && (delta.BaseDigest != begin.BaseDigest || delta.ResultDigest != begin.ResultDigest) {
			err = errors.New("delta envelope digests do not match transfer")
		}
		if err == nil {
			intent, err = ApplyIntentDelta(*s.accepted, delta)
		}
	}
	if err != nil {
		return IntentUpdate{}, err
	}
	if intent.Target != s.target {
		return IntentUpdate{}, errors.New("intent target substitution")
	}
	s.lastSequence = begin.Sequence
	return IntentUpdate{Intent: intent, Digest: begin.ResultDigest, Sequence: begin.Sequence, TransactionID: begin.TransactionID, Kind: begin.Kind}, nil
}

func (s *ClientSession) Accept(update IntentUpdate) error {
	if update.Unavailable || s.pending == nil || update.TransactionID != s.pending.TransactionID || update.Sequence != s.pending.Sequence || update.Digest != s.pending.Digest {
		return errors.New("intent update is not the pending transfer")
	}
	accepted := &Accepted{SessionID: s.id, TransactionID: update.TransactionID, Sequence: update.Sequence, Digest: update.Digest}
	if err := s.send(&ClientMessage{Accepted: accepted}); err != nil {
		return err
	}
	intent := deepCopyIntent(s.pending.Intent)
	s.accepted, s.acceptedDigest, s.pending = &intent, update.Digest, nil
	return nil
}

func (s *ClientSession) Reject(update IntentUpdate, reason string) error {
	if s.pending == nil || update.TransactionID != s.pending.TransactionID || update.Sequence != s.pending.Sequence {
		return errors.New("intent update is not the pending transfer")
	}
	if reason == "" {
		return errors.New("rejection reason is required")
	}
	err := s.send(&ClientMessage{Rejected: &Rejected{SessionID: s.id, TransactionID: update.TransactionID, Sequence: update.Sequence, Reason: reason}})
	if err == nil {
		s.pending = nil
	}
	return err
}

func (s *ClientSession) SendApplication(report ApplicationReport) error {
	report.SessionID = s.id
	if err := validateApplicationReport(report); err != nil {
		return err
	}
	return s.sendReport(ReportApplication, report)
}

func (s *ClientSession) SendObservation(observation ObservationSnapshot) error {
	observation.SessionID = s.id
	if err := ValidateObservation(observation); err != nil {
		return err
	}
	return s.sendReport(ReportObservation, observation)
}

func (s *ClientSession) SendHeartbeat() error {
	return s.send(&ClientMessage{Heartbeat: &Heartbeat{SessionID: s.id}})
}

func (s *ClientSession) CloseSend() error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.stream.CloseSend()
}

func (s *ClientSession) send(message *ClientMessage) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.stream.Send(message)
}

func (s *ClientSession) sendReport(kind ReportKind, report any) error {
	encoded, err := json.Marshal(report)
	if err != nil {
		return err
	}
	if len(encoded) > s.limits.MaxReportBytes {
		return fmt.Errorf("client report is %d bytes, limit is %d", len(encoded), s.limits.MaxReportBytes)
	}
	chunkCount := (len(encoded) + s.limits.ChunkBytes - 1) / s.limits.ChunkBytes
	if chunkCount == 0 {
		chunkCount = 1
	}
	if chunkCount > s.limits.MaxChunks {
		return errors.New("client report exceeds chunk limit")
	}
	transactionID := uuid.NewString()
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if err := s.stream.Send(&ClientMessage{ReportBegin: &ReportBegin{SessionID: s.id, TransactionID: transactionID, Kind: kind, EncodedSize: uint64(len(encoded)), ChunkCount: uint32(chunkCount)}}); err != nil {
		return err
	}
	for i := 0; i < chunkCount; i++ {
		start := i * s.limits.ChunkBytes
		end := min(start+s.limits.ChunkBytes, len(encoded))
		chunk := append([]byte(nil), encoded[start:end]...)
		if err := s.stream.Send(&ClientMessage{ReportChunk: &ReportChunk{SessionID: s.id, TransactionID: transactionID, Index: uint32(i), Data: chunk}}); err != nil {
			return err
		}
	}
	return s.stream.Send(&ClientMessage{ReportEnd: &ReportEnd{SessionID: s.id, TransactionID: transactionID, Complete: true}})
}

func validateServerUnion(message *ServerMessage) error {
	count := 0
	for _, set := range []bool{message.Welcome != nil, message.IntentUnavailable, message.Begin != nil, message.Chunk != nil, message.End != nil, message.RefreshRequest != nil} {
		if set {
			count++
		}
	}
	if count != 1 {
		return errors.New("server message must contain exactly one message")
	}
	return nil
}
