package agent

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/nodeproto"
)

// blockingProbe is a driver whose self-check runs until its context ends.
type blockingProbe struct {
	runtimeDriver
	probing chan struct{}
}

func (d *blockingProbe) probe(ctx context.Context) (nodeproto.CapabilityReport, error) {
	d.probing <- struct{}{}
	<-ctx.Done()
	return nodeproto.CapabilityReport{}, ctx.Err()
}
func (d *blockingProbe) healthy(context.Context) bool                     { return true }
func (d *blockingProbe) adopt(context.Context, func(string, int64))       {}
func (d *blockingProbe) scan(context.Context, func(string, int64))        {}
func (d *blockingProbe) watchEvents(context.Context, func(string, int64)) {}

// dropControl accepts Hello, then ends the stream on a send to drop.
type dropControl struct {
	nodev1.UnimplementedNodeServer
	drop         chan struct{}
	capabilities chan struct{}
}

func (s *dropControl) Control(stream nodev1.Node_ControlServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	if err := stream.Send(&nodev1.ControlMessage{Message: &nodev1.ControlMessage_HelloAck{
		HelloAck: &nodev1.HelloAck{Accepted: true},
	}}); err != nil {
		return err
	}
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				return
			}
			if msg.GetCapabilities() != nil {
				s.capabilities <- struct{}{}
			}
		}
	}()
	select {
	case <-s.drop:
	case <-stream.Context().Done():
	}
	return nil
}

// #233: a stream the Control Plane drops ends the Control stream at once, even
// while the self-check runs; the next stream runs the self-check again.
func TestDroppedStreamCutsTheSelfCheckShort(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	srv := &dropControl{drop: make(chan struct{}), capabilities: make(chan struct{}, 4)}
	gs := grpc.NewServer()
	nodev1.RegisterNodeServer(gs, srv)
	go gs.Serve(lis) //nolint:errcheck // test server
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() }) //nolint:errcheck // test

	driver := &blockingProbe{probing: make(chan struct{}, 1)}
	rt := newRuntime(ProviderContainerd, t.TempDir(), "node-1")
	rt.driver = driver
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connect := func() chan error {
		done := make(chan error, 1)
		go func() {
			_, err := runControlStream(ctx, conn, rt, Identity{NodeID: "node-1"}, RunOptions{DiskWatermark: DefaultDiskWatermark}, "test")
			done <- err
		}()
		select {
		case <-driver.probing:
		case <-time.After(5 * time.Second):
			t.Fatal("the self-check did not start")
		}
		return done
	}

	done := connect()
	srv.drop <- struct{}{}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the stream ended without the receive error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the stream waited for the self-check after the stream dropped")
	}
	select {
	case <-srv.capabilities:
		t.Fatal("a cut-short self-check sent Capabilities")
	default:
	}

	// The reconnect: a new stream runs the self-check from the start.
	done = connect()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the second stream did not end")
	}
}
