package api

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// fileHandler reverse-proxies one Workspace file endpoint to the Sandbox's Node
// over an agent_http Tunnel (api.md §5).
func (s *Server) fileHandler(suffix string, write bool) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request, p *principal) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			writeProblem(w, "invalid_request", "id must be a UUID")
			return
		}
		sb, ok := s.getOwnedSandbox(w, r.Context(), p, id)
		if !ok {
			return
		}
		if sb.State != "ready" {
			writeProblem(w, "conflict", "the Sandbox is not ready")
			return
		}
		if write {
			// Only the read-write Attachment may write (ADR-0018).
			// ponytail: its generation is not forwarded as a fence; OpenTunnel
			// carries none. Add one if a writer-revoke race ever matters.
			att, err := s.q.GetAttachment(r.Context(), id)
			if errors.Is(err, pgx.ErrNoRows) {
				writeProblem(w, "conflict", "the Sandbox has no Workspace attachment")
				return
			}
			if err != nil {
				s.internal(w, "get Attachment", err)
				return
			}
			if att.Mode != "rw" {
				writeProblem(w, "conflict", "the Workspace is attached read-only")
				return
			}
		}
		if sb.NodeID == nil {
			writeProblem(w, "unavailable", "the Sandbox's Node is not connected")
			return
		}
		// A file request is data-plane activity: it must postpone Idle Stop.
		s.activity.begin(sb.ID)
		defer s.activity.end(sb.ID)
		conn, ok := s.openTunnel(w, r, *sb.NodeID, agentHTTPTarget())
		if !ok {
			return
		}
		defer conn.Close() //nolint:errcheck // one request per tunnel
		proxyFile(w, r, conn, suffix)
	}
}

var hopByHop = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Connection": true,
	"Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
}

// proxyFile writes the request onto the tunnel, reads the Agent's response and
// copies it back. The tunnel already carries the Node's authority, so ambient
// credentials are not forwarded.
func proxyFile(w http.ResponseWriter, r *http.Request, conn net.Conn, suffix string) {
	forward := r.Clone(r.Context())
	forward.Header = r.Header.Clone()
	forward.Header.Del("Authorization")
	forward.Header.Del("Cookie")
	forward.Header.Del("Proxy-Authorization")
	// The client already got its 100 Continue from this server; forwarding the
	// Expect header would make ReadResponse see the Agent's interim 100 as the
	// answer and drop the real response.
	forward.Header.Del("Expect")
	for k := range hopByHop {
		forward.Header.Del(k)
	}
	forward.RequestURI = ""
	forward.URL.Path = "/sandboxes/" + r.PathValue("id") + "/files" + suffix
	forward.URL.RawPath = ""

	type answer struct {
		resp *http.Response
		err  error
	}
	answers := make(chan answer, 1)
	go func() {
		resp, err := http.ReadResponse(bufio.NewReader(conn), forward)
		answers <- answer{resp, err}
	}()

	var got answer
	if err := forward.Write(conn); err != nil {
		// The Agent can reject early (400 for a traversal path, 413 for an
		// oversized file) and hang up before the upload is drained; still
		// deliver its answer instead of reporting the Node unreachable.
		select {
		case got = <-answers:
		case <-time.After(5 * time.Second):
			writeProblem(w, "unavailable", "the Tunnel closed before the request was sent")
			return
		}
	} else {
		got = <-answers
	}
	if got.err != nil {
		writeProblem(w, "unavailable", "the Agent did not answer")
		return
	}
	resp := got.resp
	defer resp.Body.Close() //nolint:errcheck // tunnel closes
	for k, vv := range resp.Header {
		if !hopByHop[k] {
			w.Header()[k] = vv
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
