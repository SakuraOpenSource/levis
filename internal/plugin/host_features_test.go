// Package plugin tests: backup download stream must carry the session token.
package plugin

import (
	"context"
	"crypto/subtle"
	"io"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

// tokenStreamPlugin is a real in-process plugin server that enforces the
// session token on a streaming RPC, mirroring the StreamInterceptor the real
// virtualis plugin installs. It proves the host-side stream creation carries
// the token metadata (PLG-F01) — a mock client would hide exactly this.
type tokenStreamPlugin struct {
	pb.UnimplementedPluginServer
	token       string
	gotToken    bool
	firstRecvOK bool
	chunks      []*pb.HostBackupChunk
}

func (f *tokenStreamPlugin) DownloadHostBackup(req *pb.HostBackupRequest, stream pb.Plugin_DownloadHostBackupServer) error {
	md, ok := metadata.FromIncomingContext(stream.Context())
	if !ok || len(md.Get(MetadataToken)) == 0 ||
		subtle.ConstantTimeCompare([]byte(md.Get(MetadataToken)[0]), []byte(f.token)) != 1 {
		return status.Error(codes.Unauthenticated, "missing token")
	}
	f.gotToken = true
	for _, c := range f.chunks {
		if err := stream.Send(c); err != nil {
			return err
		}
	}
	return nil
}

func (f *tokenStreamPlugin) Describe(context.Context, *pb.DescribeRequest) (*pb.Manifest, error) {
	return &pb.Manifest{
		Name: "streamcheck", Version: "0.0.1",
		Capabilities: []pb.Capability{pb.Capability_CAPABILITY_PROVISION_PRODUCT},
	}, nil
}

func (f *tokenStreamPlugin) Health(context.Context, *pb.HealthRequest) (*pb.HealthReply, error) {
	return &pb.HealthReply{Ok: true}, nil
}

// TestDownloadHostBackupStreamCarriesToken drives the host-side
// Manager.DownloadHostBackup against a live gRPC server that rejects streams
// without a valid token. Before the fix, the stream was created from a bare
// context and the first Recv came back Unauthenticated.
func TestDownloadHostBackupStreamCarriesToken(t *testing.T) {
	token := "stream-secret-token"
	fake := &tokenStreamPlugin{token: token, chunks: []*pb.HostBackupChunk{
		{Filename: "b.tar", Data: []byte("hello ")},
		{Data: []byte("backup")},
	}}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	pb.RegisterPluginServer(server, fake)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	// Build the host-side conn exactly like handshake.launch does, then run
	// the manager method under test.
	conn := &conn{
		grpc:  dialPlain(t, listener.Addr().String()),
		token: token,
	}
	conn.client = pb.NewPluginClient(conn.grpc)

	inst := &Instance{id: "streamcheck", state: StateRunning, conn: conn,
		manifest: &pb.Manifest{Capabilities: []pb.Capability{pb.Capability_CAPABILITY_PROVISION_PRODUCT}}}
	m := &Manager{instances: map[string]*Instance{"streamcheck": inst}}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := m.DownloadHostBackup(ctx, "streamcheck", &pb.HostBackupRequest{HostId: "h1", BackupId: 7})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	var got []byte
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("stream must not be rejected as unauthenticated, got: %v", err)
		}
		got = append(got, chunk.GetData()...)
	}
	if string(got) != "hello backup" {
		t.Fatalf("stream content = %q", string(got))
	}
	if !fake.gotToken {
		t.Fatal("plugin never saw a valid token")
	}
}

func dialPlain(t *testing.T, target string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
