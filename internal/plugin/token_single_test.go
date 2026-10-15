// Package plugin tests: manager RPCs must not carry duplicate token metadata.
package plugin

import (
	"context"
	"crypto/subtle"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

// strictTokenPlugin enforces the exact production contract of the real
// virtualis plugin: the token metadata key must appear exactly once and match.
// A context that was pre-injected with TokenContext and then passed through
// conn.call (which appends the token again) carries the key twice and MUST be
// rejected — that is precisely what the shop purchase page hit ("令牌不匹配").
type strictTokenPlugin struct {
	pb.UnimplementedPluginServer
	token string
	saw   int
}

func (f *strictTokenPlugin) ListAgents(ctx context.Context, _ *pb.ListAgentsRequest) (*pb.ListAgentsReply, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing metadata")
	}
	values := md.Get(MetadataToken)
	f.saw = len(values)
	if len(values) != 1 || subtle.ConstantTimeCompare([]byte(values[0]), []byte(f.token)) != 1 {
		return nil, status.Error(codes.Unauthenticated, "令牌不匹配")
	}
	return &pb.ListAgentsReply{Agents: []*pb.UpstreamAgent{{Id: "6", Name: "node-1"}}}, nil
}

func (f *strictTokenPlugin) Describe(context.Context, *pb.DescribeRequest) (*pb.Manifest, error) {
	return &pb.Manifest{
		Name: "strict-token", Version: "0.0.1",
		Capabilities: []pb.Capability{pb.Capability_CAPABILITY_PROVISION_PRODUCT},
	}, nil
}

func (f *strictTokenPlugin) Health(context.Context, *pb.HealthRequest) (*pb.HealthReply, error) {
	return &pb.HealthReply{Ok: true}, nil
}

// TestListAgentsToleratesPreinjectedTokenContext reproduces the shop bug:
// UpstreamService.ProductAgents passes inst.TokenContext(ctx) into
// Manager.ListAgents, whose call wrapper appends the token again. Against the
// production plugin (strict len==1 check) every purchase-page agent listing
// died with Unauthenticated and the frontend bounced the user to /login.
func TestListAgentsToleratesPreinjectedTokenContext(t *testing.T) {
	token := "strict-secret-token"
	fake := &strictTokenPlugin{token: token}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	pb.RegisterPluginServer(server, fake)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conn := &conn{
		grpc:  dialPlain(t, listener.Addr().String()),
		token: token,
	}
	conn.client = pb.NewPluginClient(conn.grpc)

	inst := &Instance{id: "strict", state: StateRunning, conn: conn,
		manifest: &pb.Manifest{Capabilities: []pb.Capability{pb.Capability_CAPABILITY_PROVISION_PRODUCT}}}
	m := &Manager{instances: map[string]*Instance{"strict": inst}}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Exactly how UpstreamService.ProductAgents calls it today.
	reply, err := m.ListAgents(inst.TokenContext(ctx), "strict", &pb.ListAgentsRequest{})
	if err != nil {
		t.Fatalf("ListAgents with a pre-injected TokenContext must succeed, got: %v (metadata count=%d)", err, fake.saw)
	}
	if len(reply.GetAgents()) != 1 {
		t.Fatalf("agents = %d, want 1", len(reply.GetAgents()))
	}
	if fake.saw != 1 {
		t.Fatalf("plugin saw %d token values, want exactly 1", fake.saw)
	}
}
