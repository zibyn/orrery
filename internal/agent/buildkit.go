package agent

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/moby/buildkit/client"
)

// buildkitClient connects to the Node's BuildKit endpoint (ADR-0021). With no
// explicit --buildkit address, the Docker daemon's built-in BuildKit is the
// endpoint: the Docker socket accepts an h2c upgrade on /grpc and the BuildKit
// gRPC client runs over that hijacked connection.
func (r *Runtime) buildkitClient(ctx context.Context) (*client.Client, error) {
	if r.buildkit != "" {
		return client.New(ctx, r.buildkit)
	}
	return client.New(ctx, "", client.WithContextDialer(dockerGRPCDialer(r.socket)))
}

// buildkitAvailable is the snapshot_build capability self-check: a real RPC
// against the configured BuildKit endpoint, independent of the Node's Runtime
// (runtime.md §12, node-protocol.md).
func (r *Runtime) buildkitAvailable(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, err := r.buildkitClient(ctx)
	if err != nil {
		return false
	}
	defer c.Close() //nolint:errcheck // probe only
	_, err = c.ListWorkers(ctx)
	return err == nil
}

func dockerGRPCDialer(socket string) func(context.Context, string) (net.Conn, error) {
	return func(ctx context.Context, _ string) (net.Conn, error) {
		return dialDockerGRPC(ctx, socket)
	}
}

// dialDockerGRPC performs the Docker API's h2c upgrade on /grpc and returns the
// raw connection. This is the same handshake buildx's docker driver uses; the
// Docker socket by itself is not a buildkitd gRPC socket.
func dialDockerGRPC(ctx context.Context, socket string) (net.Conn, error) {
	path := strings.TrimPrefix(socket, "unix://")
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker/grpc", nil)
	if err != nil {
		conn.Close() //nolint:errcheck // dial failed
		return nil, err
	}
	req.Host = "docker"
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "h2c")
	if err := req.Write(conn); err != nil {
		conn.Close() //nolint:errcheck // handshake failed
		return nil, err
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		conn.Close() //nolint:errcheck // handshake failed
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close() //nolint:errcheck // handshake failed
		return nil, fmt.Errorf("docker /grpc upgrade: %s", resp.Status)
	}
	return &grpcConn{Conn: conn, reader: reader}, nil
}

// grpcConn preserves bytes the HTTP reader buffered after the 101 response.
type grpcConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *grpcConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
