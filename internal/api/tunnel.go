package api

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/websocket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/store"
	"github.com/zibyn/orrery/internal/tunnel"
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
	// A PTY attachment is data-plane activity (ADR-0015).
	s.activity.begin(e.SandboxID)
	defer s.activity.end(e.SandboxID)
	conn, ok := s.openTunnel(w, r, *row.SandboxNodeID, agentHTTPTarget())
	if !ok {
		return
	}
	defer conn.Close() //nolint:errcheck // closes whichever copy is still running
	proxyPTY(w, r, conn)
}

// handlePortTunnel is the Gateway end of
// `GET /sandboxes/{id}/ports/{port}/tunnel` (api.md §6). It carries raw TCP so
// the CLI can forward any protocol.
func (s *Server) handlePortTunnel(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "invalid_request", "id must be a UUID")
		return
	}
	port, ok := pathPort(w, r)
	if !ok {
		return
	}
	sb, ok := s.getOwnedSandbox(w, r.Context(), p, id)
	if !ok {
		return
	}
	if SandboxState(sb.State) != SandboxReady {
		writeProblem(w, "conflict", "the Sandbox is not ready")
		return
	}
	if sb.NodeID == nil {
		writeProblem(w, "unavailable", "the Sandbox's Node is not connected")
		return
	}
	s.activity.begin(sb.ID)
	defer s.activity.end(sb.ID)
	conn, ok := s.openTunnel(w, r, *sb.NodeID, sandboxPortTarget(id, port))
	if !ok {
		return
	}
	defer conn.Close() //nolint:errcheck // one tunnel per WebSocket
	server := websocket.Server{
		Handshake: func(config *websocket.Config, _ *http.Request) error {
			config.Protocol = []string{"sandbox.v1"}
			return nil
		},
		Handler: func(ws *websocket.Conn) { relayWebSocket(ws, conn) },
	}
	server.ServeHTTP(w, r)
}

func agentHTTPTarget() *nodev1.OpenTunnel {
	return &nodev1.OpenTunnel{Target: &nodev1.OpenTunnel_AgentHttp{AgentHttp: &nodev1.AgentHTTP{}}}
}

// openTunnel asks the Node to dial back and waits for the stream.
func (s *Server) openTunnel(w http.ResponseWriter, r *http.Request, nodeID uuid.UUID, target *nodev1.OpenTunnel) (net.Conn, bool) {
	id := uuid.NewString()
	conns := s.tunnels.register(nodeID, id)
	defer s.tunnels.drop(id)
	target.CommandId = "open-tunnel:" + id
	target.TunnelId = id
	if !s.reconcile.hub.direct(nodeID, &nodev1.ControlMessage{
		Message: &nodev1.ControlMessage_OpenTunnel{OpenTunnel: target},
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

// forwardedRequest removes the caller's platform credentials. keepCookies
// keeps application cookies but always drops the Gateway's own grants.
func forwardedRequest(r *http.Request, keepCookies bool) *http.Request {
	forward := r.Clone(r.Context())
	forward.Header = r.Header.Clone()
	forward.Header.Del("Authorization")
	forward.Header.Del("Proxy-Authorization")
	if !keepCookies {
		forward.Header.Del("Cookie")
	} else {
		kept := appCookies(r.Header.Values("Cookie"))
		if kept == "" {
			forward.Header.Del("Cookie")
		} else {
			forward.Header.Set("Cookie", kept)
		}
	}
	forward.RequestURI = ""
	return forward
}

// appCookies drops the Session and port-grant cookies, leaving the app's own.
func appCookies(values []string) string {
	var kept []string
	for _, value := range values {
		for part := range strings.SplitSeq(value, ";") {
			name, _, _ := strings.Cut(strings.TrimSpace(part), "=")
			if name != sessionCookieName && name != gatewayCookieName {
				kept = append(kept, strings.TrimSpace(part))
			}
		}
	}
	return strings.Join(kept, "; ")
}

// proxyPTY forwards the upgrade request and then copies raw bytes (ADR-0018).
func proxyPTY(w http.ResponseWriter, r *http.Request, tunnelConn net.Conn) {
	forward := forwardedRequest(r, false)
	forward.URL.Path = "/execs/" + r.PathValue("id") + "/pty"
	proxyUpgrade(w, tunnelConn, forward)
}

// proxyUpgrade hijacks the client, writes an already-prepared request to the
// tunnel and splices raw bytes in both directions.
func proxyUpgrade(w http.ResponseWriter, tunnelConn net.Conn, forward *http.Request) {
	client, buffered, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer client.Close() //nolint:errcheck // connection lifetime

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
	relayRaw(client, tunnelConn)
}

// relayRaw copies bytes both ways until either side closes.
func relayRaw(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
}

// relayWebSocket carries raw TCP between the client's WebSocket and the
// tunnel. Binary messages avoid the text frame's UTF-8 assumption.
func relayWebSocket(ws *websocket.Conn, conn net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if err := websocket.Message.Send(ws, append([]byte(nil), buf[:n]...)); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			var data []byte
			if err := websocket.Message.Receive(ws, &data); err != nil {
				return
			}
			if _, err := conn.Write(data); err != nil {
				return
			}
		}
	}()
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
			frame := map[string]any{"type": "exit", "cause": string(ExecExited)}
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
