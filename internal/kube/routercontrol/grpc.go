package routercontrol

import (
	"context"
	"fmt"

	protocol "github.com/skupperproject/skupper/internal/routercontrol"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type sessionContextKey struct{}

// StreamServerInterceptor authenticates the mTLS peer, establishes bounded live
// Kubernetes authorization, and installs the resulting session in stream context.
func (a *Authenticator) StreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(server any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		peerInfo, ok := peer.FromContext(stream.Context())
		if !ok {
			return status.Error(codes.Unauthenticated, "router-control TLS peer is missing")
		}
		tlsInfo, ok := peerInfo.AuthInfo.(credentials.TLSInfo)
		if !ok {
			return status.Error(codes.Unauthenticated, "router-control mTLS is required")
		}
		session, err := a.Session(stream.Context(), tlsInfo.State)
		if err != nil {
			return status.Error(codes.Unauthenticated, "router-control authentication failed")
		}
		defer session.Close()
		ctx := context.WithValue(session.Context(), sessionContextKey{}, session)
		result := make(chan error, 1)
		go func() {
			result <- handler(server, &sessionServerStream{ServerStream: stream, ctx: ctx, session: session})
		}()
		select {
		case <-session.Context().Done():
			// Returning cancels the underlying gRPC stream as well as its wrapped
			// context, releasing handlers blocked in transport Send or Recv.
			return status.Error(codes.PermissionDenied, "router-control authorization ended")
		case err := <-result:
			return err
		}
	}
}

// AuthorizeSession matches internal/routercontrol.AuthorizeFunc. It cannot be
// used without a session installed by StreamServerInterceptor.
func AuthorizeSession(ctx context.Context, target protocol.TargetIdentity) (protocol.SessionIdentity, error) {
	session, ok := ctx.Value(sessionContextKey{}).(*Session)
	if !ok {
		return protocol.SessionIdentity{}, fmt.Errorf("router-control authenticated session is missing")
	}
	if err := session.CheckTarget(target); err != nil {
		return protocol.SessionIdentity{}, err
	}
	return protocol.SessionIdentity{PodUID: string(session.Identity.PodUID), ServiceAccountUID: string(session.Identity.ServiceAccountUID)}, nil
}

type sessionServerStream struct {
	grpc.ServerStream
	ctx     context.Context
	session *Session
}

func (s *sessionServerStream) Context() context.Context { return s.ctx }

func (s *sessionServerStream) SendMsg(message any) error {
	if err := s.session.Check(); err != nil {
		return status.Error(codes.PermissionDenied, "router-control authorization ended")
	}
	return s.ServerStream.SendMsg(message)
}

func (s *sessionServerStream) RecvMsg(message any) error {
	if err := s.ServerStream.RecvMsg(message); err != nil {
		return err
	}
	if err := s.session.Check(); err != nil {
		return status.Error(codes.PermissionDenied, "router-control authorization ended")
	}
	return nil
}
