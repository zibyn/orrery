package agent

import (
	"context"
	"net"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/generation"
)

// tokenNode is the Control Plane's Node listener: it refuses any call
// without the Node Token, as node_auth.go does, and serves Presign from a
// fakeGenerationNode.
type tokenNode struct {
	nodev1.UnimplementedNodeServer
	f *fakeGenerationNode
}

func (s tokenNode) Presign(ctx context.Context, req *nodev1.PresignRequest) (*nodev1.PresignResponse, error) {
	return s.f.Presign(ctx, req)
}

func requireToken(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	if got := md.Get("authorization"); len(got) != 1 || got[0] != "Bearer node-token" {
		return status.Errorf(codes.Unauthenticated, "a Node Token is required, got %v", got)
	}
	return nil
}

// Every Agent call to the Control Plane carries the Node Token, also those
// made outside the Control stream's context: the block cache's presigns on
// restore, the periodic Volume commit (node-protocol.md).
func TestEveryNodeRPCCarriesTheNodeToken(t *testing.T) {
	f := newFakeGenerationNode(t)
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer(
		grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			if err := requireToken(ctx); err != nil {
				return nil, err
			}
			return h(ctx, req)
		}),
		grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			if err := requireToken(ss.Context()); err != nil {
				return err
			}
			return h(srv, ss)
		}),
	)
	nodev1.RegisterNodeServer(gs, tokenNode{f: f})
	go gs.Serve(lis) //nolint:errcheck // test server
	t.Cleanup(gs.Stop)
	options := append([]grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, nodeTokenOptions("node-token")...)
	conn, err := grpc.NewClient("passthrough:///bufnet", options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() }) //nolint:errcheck // test

	// A Generation committed through the fake, restored through the real
	// connection on a context that carries nothing.
	upper := t.TempDir()
	writeTestFile(t, filepath.Join(upper, "etc/apk/world"), "git\n", 0o644)
	id := uuid.New()
	nodeA := newRuntime(ProviderContainerd, t.TempDir(), "a")
	nodeA.client = f
	if err := nodeA.commitWithRetry(context.Background(), id, 1, commitTarget{root: upper, kind: generation.KindDiff}, func() {}); err != nil {
		t.Fatal(err)
	}
	nodeB := newRuntime(ProviderContainerd, t.TempDir(), "b")
	nodeB.client = nodev1.NewNodeClient(conn)
	if err := nodeB.restoreFilesystem(context.Background(), id, f.head, filepath.Join(t.TempDir(), "fs")); err != nil {
		t.Fatalf("restore through the authenticated connection: %v", err)
	}

	// Streams too: the handler is unimplemented, so anything but
	// Unauthenticated means the token got through.
	stream, err := nodeB.client.SubmitGeneration(context.Background())
	if err == nil {
		_, err = stream.CloseAndRecv()
	}
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("SubmitGeneration without a token in ctx = %v, want it past the token check", err)
	}
}
