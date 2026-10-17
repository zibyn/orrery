package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/websocket"

	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
	"github.com/zibyn/stars-sandbox/internal/ptywire"
	"github.com/zibyn/stars-sandbox/internal/supervisor"
	"github.com/zibyn/stars-sandbox/internal/tunnel"
)

// openTunnel dials the Tunnel RPC and serves the Agent's internal HTTP over it
// (ADR-0018). The Control Plane forwards one request per tunnel: the Gateway
// hijacks the client connection and copies bytes.
func (r *Runtime) openTunnel(ctx context.Context, tunnelID string) error {
	conn, cancel, err := r.dialTunnel(ctx, tunnelID)
	if err != nil {
		return err
	}
	defer cancel()
	listener := &oneConnListener{conn: conn, closed: make(chan struct{})}
	srv := &http.Server{
		Handler:           r.tunnelMux(),
		ReadHeaderTimeout: 30 * time.Second,
	}
	go srv.Serve(listener) //nolint:errcheck // closed below

	select {
	case <-conn.Done(): // the WebSocket handler returned, or the HTTP conn closed
	case <-ctx.Done():
		conn.Close() //nolint:errcheck // shutdown
	}
	return srv.Close()
}

// forwardSandboxPort bridges one Tunnel to a port inside the Sandbox. The
// Supervisor stays in the Sandbox's network namespace, so the Agent never
// needs netns privileges (api.md §6).
func (r *Runtime) forwardSandboxPort(ctx context.Context, tunnelID string, target *nodev1.SandboxPort) error {
	sandboxID, err := uuid.Parse(target.GetSandboxId())
	if err != nil {
		return errors.New("invalid sandbox id")
	}
	port := int(target.GetPort())
	if port < 1 || port > 65535 {
		return errors.New("invalid port")
	}
	conn, cancel, err := r.dialTunnel(ctx, tunnelID)
	if err != nil {
		return err
	}
	defer cancel()
	defer conn.Close() //nolint:errcheck // one tunnel per connection
	local, err := r.dialSandboxPort(ctx, sandboxID, port)
	if err != nil {
		return err
	}
	defer local.Close() //nolint:errcheck // bridge ends
	supervisor.Bridge(local, conn)
	return nil
}

// dialTunnel opens the reverse tunnel and sends the frame that names it, so
// the Control Plane can match this dial to its OpenTunnel (node-protocol.md).
func (r *Runtime) dialTunnel(ctx context.Context, tunnelID string) (*tunnel.Conn, context.CancelFunc, error) {
	if r.tunnelStream == nil {
		return nil, nil, errors.New("agent is not connected")
	}
	// The Conn's pump blocks in Recv; cancelling the stream context is what
	// releases it once the attachment is over.
	ctx, cancel := context.WithCancel(ctx)
	stream, err := r.tunnelStream(ctx)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	if err := stream.Send(&nodev1.TunnelFrame{TunnelId: tunnelID}); err != nil {
		cancel()
		return nil, nil, err
	}
	return tunnel.NewConn(tunnelID, stream), cancel, nil
}

// dialSandboxPort starts a forwarder inside the Sandbox and connects to it.
// An already running forwarder for the same port is reused.
func (r *Runtime) dialSandboxPort(ctx context.Context, sandboxID uuid.UUID, port int) (net.Conn, error) {
	dir := filepath.Join(sandboxRunPath(r.dataRoot, sandboxID), "ports")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		return nil, err
	}
	socket := filepath.Join(dir, strconv.Itoa(port)+".sock")
	if conn, err := dialSocket(socket); err == nil {
		return conn, nil
	}
	if err := r.d.negotiate(ctx); err != nil {
		return nil, err
	}
	create := map[string]any{
		"AttachStdout": false,
		"AttachStderr": false,
		"Tty":          false,
		"Cmd": []string{supervisorBin, "forward", strconv.Itoa(port),
			runMount + "/ports/" + filepath.Base(socket)},
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := r.d.do(ctx, "POST", r.d.api("/containers/"+containerName(sandboxID)+"/exec"), create, &created); err != nil {
		return nil, fmt.Errorf("create forward exec: %w", err)
	}
	if err := r.d.do(ctx, "POST", r.d.api("/exec/"+created.ID+"/start"),
		map[string]any{"Detach": true, "Tty": false}, nil); err != nil {
		return nil, fmt.Errorf("start forward exec: %w", err)
	}
	return dialUnix(ctx, socket, 5*time.Second)
}

func (r *Runtime) tunnelMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /execs/{id}/pty", r.handlePTY)
	mux.HandleFunc("GET /sandboxes/{id}/files", r.handleFileList)
	mux.HandleFunc("DELETE /sandboxes/{id}/files", r.handleFileDelete)
	mux.HandleFunc("GET /sandboxes/{id}/files/stat", r.handleFileStat)
	mux.HandleFunc("GET /sandboxes/{id}/files/content", r.handleFileContent)
	mux.HandleFunc("PUT /sandboxes/{id}/files/content", r.handleFileContent)
	mux.HandleFunc("POST /sandboxes/{id}/files/mkdir", r.handleFileMkdir)
	mux.HandleFunc("POST /sandboxes/{id}/files/move", r.handleFileMove)
	mux.HandleFunc("GET /sandboxes/{id}/files/archive", r.handleArchiveGet)
	mux.HandleFunc("PUT /sandboxes/{id}/files/archive", r.handleArchivePut)
	return mux
}

// handlePTY is the Agent end of `GET /execs/{id}/pty` (api.md §4). Binary
// frames are terminal bytes; text frames are controls.
func (r *Runtime) handlePTY(w http.ResponseWriter, req *http.Request) {
	execID := req.PathValue("id")
	if _, err := uuid.Parse(execID); err != nil {
		http.NotFound(w, req)
		return
	}
	r.attachPTY(execID)
	defer r.detachPTY(execID)
	matches, _ := filepath.Glob(filepath.Join(r.dataRoot, "sandboxes", "*", "run", execID))
	if len(matches) == 0 {
		http.NotFound(w, req)
		return
	}
	dir := matches[0]
	ctl, dialErr := dialSocket(filepath.Join(dir, "ctl.sock"))
	if dialErr != nil {
		// A finished Exec still answers: the client learns the exit result.
		if _, err := os.Stat(filepath.Join(dir, "exit")); err != nil {
			http.Error(w, "exec is not attachable", http.StatusServiceUnavailable)
			return
		}
	}
	server := websocket.Server{
		Handshake: func(config *websocket.Config, _ *http.Request) error {
			config.Protocol = []string{ptySubprotocol}
			return nil
		},
		Handler: func(ws *websocket.Conn) {
			servePTY(ws, ctl, dir)
		},
	}
	server.ServeHTTP(w, req)
}

const ptySubprotocol = "sandbox.v1"

// attachPTY and detachPTY bracket one attachment. The record must outlive it:
// servePTY reads `exit` off disk after the Supervisor closes the socket, while
// streamExec may already have the exit frame acknowledged (runtime.md §7).
func (r *Runtime) attachPTY(id string) {
	r.execMu.Lock()
	r.ptyAttachments[id]++
	r.execMu.Unlock()
}

func (r *Runtime) detachPTY(id string) {
	r.execMu.Lock()
	defer r.execMu.Unlock()
	if n := r.ptyAttachments[id] - 1; n > 0 {
		r.ptyAttachments[id] = n
	} else {
		delete(r.ptyAttachments, id)
	}
}

// waitPTYDetached lets live attachments read the exit record before cleanup.
// The bound keeps a stalled client from pinning the record forever.
func (r *Runtime) waitPTYDetached(ctx context.Context, id string) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.execMu.Lock()
		n := r.ptyAttachments[id]
		r.execMu.Unlock()
		if n == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// servePTY relays one attachment. Output arrives on the same ctl.sock
// connection: the Supervisor writes the ring replay and then live bytes, so a
// reattach restores the screen (ADR-0017).
func servePTY(ws *websocket.Conn, ctl net.Conn, dir string) {
	defer ws.Close() //nolint:errcheck // connection lifetime
	if ctl == nil {
		sendExit(ws, dir)
		return
	}
	defer ctl.Close() //nolint:errcheck // connection lifetime
	if _, err := ctl.Write([]byte("attach\n")); err != nil {
		return
	}

	out := make(chan struct{})
	go func() {
		defer close(out)
		buf := make([]byte, 32*1024)
		for {
			n, err := ctl.Read(buf)
			if n > 0 {
				if err := ptywire.Codec.Send(ws, append([]byte(nil), buf[:n]...)); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	in := make(chan struct{})
	go func() {
		defer close(in)
		for {
			var frame ptywire.Frame
			if err := ptywire.Codec.Receive(ws, &frame); err != nil {
				return
			}
			if frame.Text {
				var control struct {
					Type string `json:"type"`
					Rows uint16 `json:"rows"`
					Cols uint16 `json:"cols"`
				}
				if json.Unmarshal(frame.Data, &control) != nil || control.Type != "resize" {
					continue
				}
				if _, err := fmt.Fprintf(ctl, "resize %d %d\n", control.Rows, control.Cols); err != nil {
					return
				}
				continue
			}
			if _, err := fmt.Fprintf(ctl, "input %d\n", len(frame.Data)); err != nil {
				return
			}
			if _, err := ctl.Write(frame.Data); err != nil {
				return
			}
		}
	}()

	select {
	case <-out:
		// The Supervisor writes `exit` before it closes attachments, so the
		// result is readable here. Reporting it lets the client tell a finished
		// Exec from a dropped tunnel.
		sendExit(ws, dir)
	case <-in:
	}
}

func sendExit(ws *websocket.Conn, dir string) {
	raw, err := os.ReadFile(filepath.Join(dir, "exit"))
	if err != nil {
		return
	}
	var frame map[string]any
	if json.Unmarshal(raw, &frame) != nil {
		return
	}
	frame["type"] = "exit"
	msg, _ := json.Marshal(frame)
	_ = ptywire.Codec.Send(ws, string(msg))
}

// oneConnListener serves one already-established connection and then blocks
// until it is closed; net/http owns the connection from Accept on.
type oneConnListener struct {
	conn     net.Conn
	taken    sync.Once
	closed   chan struct{}
	shutdown sync.Once
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	first := false
	l.taken.Do(func() { first = true })
	if first {
		return l.conn, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error {
	l.shutdown.Do(func() { close(l.closed) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }
