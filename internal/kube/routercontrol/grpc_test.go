package routercontrol

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"

	protocol "github.com/skupperproject/skupper/internal/routercontrol"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

func TestGRPCInterceptorInstallsTargetBoundSessionIdentity(t *testing.T) {
	fixture := newSessionFixture(t)
	authenticator := &Authenticator{Kube: fixture.client, Installation: fixture.install, Gate: fixture.gate, Revocations: fixture.revocations, Now: func() time.Time { return testNow }, Authorize: func(context.Context, Identity) error { return nil }}
	state := tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{fixture.leaf, fixture.install.ClientCA}}}
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: state}})
	stream := &testServerStream{ctx: ctx}
	target := protocol.TargetIdentity{NamespaceUID: string(fixture.identity.NamespaceUID), SiteUID: string(fixture.identity.SiteUID), RouterGroup: fixture.identity.Group}
	called := false
	err := authenticator.StreamServerInterceptor()(nil, stream, nil, func(_ any, stream grpc.ServerStream) error {
		called = true
		identity, err := AuthorizeSession(stream.Context(), target)
		if err != nil {
			return err
		}
		if identity.PodUID != string(fixture.identity.PodUID) || identity.ServiceAccountUID != string(fixture.identity.ServiceAccountUID) {
			t.Fatalf("protocol session identity = %#v", identity)
		}
		wrong := target
		wrong.RouterGroup = "another-router"
		if _, err := AuthorizeSession(stream.Context(), wrong); err == nil {
			t.Fatal("authenticated session authorized another target")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("gRPC stream handler was not called")
	}
}

func TestGRPCInterceptorRejectsMissingTLS(t *testing.T) {
	authenticator := &Authenticator{}
	err := authenticator.StreamServerInterceptor()(nil, &testServerStream{ctx: context.Background()}, nil, func(any, grpc.ServerStream) error {
		t.Fatal("handler called without mTLS")
		return nil
	})
	if err == nil {
		t.Fatal("stream without mTLS was accepted")
	}
}

func TestGRPCRevocationReturnsWhileHandlerIsBlocked(t *testing.T) {
	for _, test := range []struct {
		name    string
		expires time.Duration
		revoke  func(*sessionFixture)
	}{
		{name: "leadership loss", revoke: func(fixture *sessionFixture) { close(fixture.gate.done) }},
		{name: "authorization watch", revoke: func(fixture *sessionFixture) {
			fixture.revocations.InvalidateAuthorization(AuthorizationPod, "site-ns", "router-pod")
		}},
		{name: "certificate expiry", expires: 20 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSessionFixture(t)
			authenticator := &Authenticator{Kube: fixture.client, Installation: fixture.install, Gate: fixture.gate, Revocations: fixture.revocations, Now: func() time.Time { return testNow }, Authorize: func(context.Context, Identity) error { return nil }}
			leaf := *fixture.leaf
			if test.expires > 0 {
				leaf.NotAfter = testNow.Add(test.expires)
			}
			state := tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{&leaf, fixture.install.ClientCA}}}
			ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: state}})
			started := make(chan grpc.ServerStream, 1)
			release := make(chan struct{})
			defer close(release)
			finished := make(chan error, 1)
			go func() {
				finished <- authenticator.StreamServerInterceptor()(nil, &testServerStream{ctx: ctx}, nil, func(_ any, stream grpc.ServerStream) error {
					started <- stream
					<-release // A transport call need not obey the wrapped context.
					return nil
				})
			}()
			stream := <-started
			if test.revoke != nil {
				test.revoke(fixture)
			}
			select {
			case err := <-finished:
				if err == nil {
					t.Fatal("revoked stream succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("revocation waited for blocked handler instead of terminating gRPC stream")
			}
			if err := stream.SendMsg("late intent"); err == nil {
				t.Fatal("message was sent after authority was revoked")
			}
		})
	}
}

type testServerStream struct {
	ctx context.Context
}

func (s *testServerStream) SetHeader(metadata.MD) error  { return nil }
func (s *testServerStream) SendHeader(metadata.MD) error { return nil }
func (s *testServerStream) SetTrailer(metadata.MD)       {}
func (s *testServerStream) Context() context.Context     { return s.ctx }
func (s *testServerStream) SendMsg(any) error            { return nil }
func (s *testServerStream) RecvMsg(any) error            { return nil }
