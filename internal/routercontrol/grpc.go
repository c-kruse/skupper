package routercontrol

import (
	"context"
	"encoding/json"

	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding"
)

const grpcServiceName = "skupper.routercontrol.v1.RouterControl"

type jsonCodec struct{}

func (jsonCodec) Name() string                           { return "skupper-json" }
func (jsonCodec) Marshal(value any) ([]byte, error)      { return json.Marshal(value) }
func (jsonCodec) Unmarshal(data []byte, value any) error { return decodeStrict(data, value) }

func init() { encoding.RegisterCodec(jsonCodec{}) }

// RouterControlClient is the low-level bidirectional transport. Most adaptor
// code should use OpenClientSession, which validates and reconstructs content.
type RouterControlClient interface {
	Sync(ctx context.Context, opts ...grpc.CallOption) (grpc.BidiStreamingClient[ClientMessage, ServerMessage], error)
}

type routerControlClient struct{ cc grpc.ClientConnInterface }

func NewRouterControlClient(cc grpc.ClientConnInterface) RouterControlClient {
	return &routerControlClient{cc: cc}
}

func (c *routerControlClient) Sync(ctx context.Context, opts ...grpc.CallOption) (grpc.BidiStreamingClient[ClientMessage, ServerMessage], error) {
	opts = append([]grpc.CallOption{grpc.CallContentSubtype(jsonCodec{}.Name())}, opts...)
	stream, err := c.cc.NewStream(ctx, &RouterControl_ServiceDesc.Streams[0], "/"+grpcServiceName+"/Sync", opts...)
	if err != nil {
		return nil, err
	}
	return &grpc.GenericClientStream[ClientMessage, ServerMessage]{ClientStream: stream}, nil
}

type RouterControlServer interface {
	Sync(grpc.BidiStreamingServer[ClientMessage, ServerMessage]) error
}

func RegisterRouterControlServer(registrar grpc.ServiceRegistrar, server RouterControlServer) {
	registrar.RegisterService(&RouterControl_ServiceDesc, server)
}

func syncHandler(server any, stream grpc.ServerStream) error {
	return server.(RouterControlServer).Sync(&grpc.GenericServerStream[ClientMessage, ServerMessage]{ServerStream: stream})
}

var RouterControl_ServiceDesc = grpc.ServiceDesc{
	ServiceName: grpcServiceName,
	HandlerType: (*RouterControlServer)(nil),
	Streams:     []grpc.StreamDesc{{StreamName: "Sync", Handler: syncHandler, ServerStreams: true, ClientStreams: true}},
}
