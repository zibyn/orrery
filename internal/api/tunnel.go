package api

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/websocket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
	"github.com/zibyn/stars-sandbox/internal/store"
	"github.com/zibyn/stars-sandbox/internal/tunnel"
)

// tunnelDialTimeout bounds the Agent's reverse dial after OpenTunnel.
const tunnelDialTimeout = 10 * time.Second

// tunnelRegistry matches an Agent's Tunnel dial to the request that is waiting
// for it. The pending entry is scoped to one Node (ADR-0018).
type tunnelRegistry struct {
	mu    sync.Mutex
	items map[string]*pendingTunnel
}

type pendingTunnel struct {
	nodeID uuid.UUID
	conn   chan *tunnel.Conn
}

func newTunnelRegistry() *tunnelRegistry {
	return &tunnelRegistry{items: make(map[string]*pendingTunnel)}
}

func (t *tunnelRegistry) register(nodeID uuid.UUID, id string) chan *tunnel.Conn {
	conn := make(chan *tunnel.Conn, 1)
	t.mu.Lock()
	t.items[id] = &pendingTunnel{nodeID: nodeID, conn: conn}
	t.mu.Unlock()
	return conn
}

func (t *tunnelRegistry) drop(id string) {
	t.mu.Lock()
	delete(t.items, id)
	t.mu.Unlock()
}

// claim hands the dialled stream to its waiter. A tunnel id belongs to one
// Node: an Agent cannot claim another Node's tunnel.
func (t *tunnelRegistry) claim(nodeID uuid.UUID, id string, conn *tunnel.Conn) bool {
	t.mu.Lock()
	pending := t.items[id]
	delete(t.items, id)
	t.mu.Unlock()
	if pending == nil || pending.nodeID != nodeID {
		return false
	}
	pending.conn <- conn
	return true
}

// Tunnel is the Agent's reverse dial for a pending OpenTunnel
// (docs/spec/node-protocol.md).
func (n *nodeService) Tunnel(stream nodev1.Node_TunnelServer) error {
	ctx := stream.Context()
	node, err := n.s.authenticateNode(ctx)
	if err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return status.Error(codes.InvalidArgument, "the first frame must name the tunnel")
	}
	id := first.GetTunnelId()
	if id == "" {
		return status.Error(codes.InvalidArgument, "the first frame must name the tunnel")
	}
	conn := tunnel.NewConn(id, stream)
	if !n.s.tunnels.claim(node.ID, id, conn) {
		return status.Error(codes.NotFound, "no tunnel with that id is pending")
	}
	select {
	case <-conn.Done():
	case <-ctx.Done():
	}
	return nil
}

// handleExecPTY is the Gateway end of `GET /execs/{id}/pty` (api.md §4). It
// reverse-proxies the WebSocket verbatim to the Agent over a tunnel.
func (s *Server) handleExecPTY(w http.ResponseWriter, r *http.Request, p *principal) {
	e, ok := s.getOwnedExec(w, r, p)
	if !ok {
		return
	}
	if !hasPTY(e.Pty) {
		writeProblem(w, "conflict", "the Exec has no PTY")
		return
	}
	if e.FinishedAt.Valid {
		serveExitOnly(w, r, e)
		return
	}
	row, err := s.q.GetExecWithNode(r.Context(), e.ID)
	if err != nil {
		s.internal(w, "look up Exec Node", err)
		return
	}
	if row.SandboxNodeID == nil {
		writeProblem(w, "unavailable", "the Sandbox's Node is not connected")
		return
	}
	conn, ok := s.openTunnel(w, r, *row.SandboxNodeID)
	if !ok {
		return
	}
	defer conn.Close() //nolint:errcheck // closes whichever copy is still running
	proxyPTY(w, r, conn)
}

// openTunnel asks the Node to dial back and waits for the stream.
func (s *Server) openTunnel(w http.ResponseWriter, r *http.Request, nodeID uuid.UUID) (net.Conn, bool) {
	id := uuid.NewString()
	conns := s.tunnels.register(nodeID, id)
	defer s.tunnels.drop(id)
	if !s.reconcile.hub.direct(nodeID, &nodev1.ControlMessage{
		Message: &nodev1.ControlMessage_OpenTunnel{OpenTunnel: &nodev1.OpenTunnel{
			CommandId: "open-tunnel:" + id,
			TunnelId:  id,
			Target:    &nodev1.OpenTunnel_AgentHttp{AgentHttp: &nodev1.AgentHTTP{}},
		}},
	}) {
		writeProblem(w, "unavailable", "the Sandbox's Node is not connected")
		return nil, false
	}
	select {
	case conn := <-conns:
		return conn, true
	case <-time.After(tunnelDialTimeout):
		// A dial that raced the timeout still needs its stream closed.
		select {
		case conn := <-conns:
			conn.Close() //nolint:errcheck // losing race
		default:
		}
		writeProblem(w, "unavailable", "the Node did not open the tunnel")
		return nil, false
	case <-r.Context().Done():
		select {
		case conn := <-conns:
			conn.Close() //nolint:errcheck // client gone
		default:
		}
		return nil, false
	}
}

// proxyPTY forwards the upgrade request and then copies raw bytes in both
// directions (ADR-0018). The Agent sees no ambient credentials: the tunnel is
// already authenticated by the Node Token.
func proxyPTY(w http.ResponseWriter, r *http.Request, tunnelConn net.Conn) {
	client, buffered, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer client.Close() //nolint:errcheck // connection lifetime

	forward := r.Clone(r.Context())
	forward.Header = r.Header.Clone()
	forward.Header.Del("Authorization")
	forward.Header.Del("Cookie")
	forward.Header.Del("Proxy-Authorization")
	forward.URL.Path = "/execs/" + r.PathValue("id") + "/pty"
	if err := forward.Write(tunnelConn); err != nil {
		return
	}
	if n := buffered.Reader.Buffered(); n > 0 {
		early, _ := buffered.Reader.Peek(n)
		if _, err := tunnelConn.Write(early); err != nil {
			return
		}
		_, _ = buffered.Reader.Discard(n)
	}

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(tunnelConn, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, tunnelConn); done <- struct{}{} }()
	<-done
}

// serveExitOnly answers a finished PTY Exec locally: there is nothing left to
// attach to, but the client still reads the result off the same socket.
func serveExitOnly(w http.ResponseWriter, r *http.Request, e store.Exec) {
	server := websocket.Server{
		Handshake: func(config *websocket.Config, _ *http.Request) error {
			config.Protocol = []string{"sandbox.v1"}
			return nil
		},
		Handler: func(conn *websocket.Conn) {
			frame := map[string]any{"type": "exit", "cause": "exited"}
			if e.Cause != nil {
				frame["cause"] = *e.Cause
			}
			if e.ExitCode != nil {
				frame["exit_code"] = *e.ExitCode
			}
			if e.Signal != nil {
				frame["signal"] = *e.Signal
			}
			raw, _ := json.Marshal(frame)
			_ = websocket.Message.Send(conn, string(raw))
		},
	}
	server.ServeHTTP(w, r)
}
