//go:build e2e

package e2e

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// ptyFrame keeps the payload type: binary frames are terminal bytes, text
// frames are controls (api.md §4).
type ptyFrame struct {
	text bool
	data []byte
}

var ptyTestCodec = websocket.Codec{
	Marshal: func(v interface{}) ([]byte, byte, error) {
		switch data := v.(type) {
		case []byte:
			return data, websocket.BinaryFrame, nil
		case string:
			return []byte(data), websocket.TextFrame, nil
		}
		return nil, websocket.UnknownFrame, websocket.ErrNotSupported
	},
	Unmarshal: func(data []byte, payloadType byte, v interface{}) error {
		f := v.(*ptyFrame)
		f.text = payloadType == websocket.TextFrame
		f.data = data
		return nil
	},
}

// ptyIdle is the deployment's PTY idle window. The scenarios need a short one;
// the default is 300s.
func ptyIdle(t *testing.T) time.Duration {
	t.Helper()
	raw := os.Getenv("ORRERY_E2E_PTY_IDLE_SECONDS")
	if raw == "" {
		t.Skip("ORRERY_E2E_PTY_IDLE_SECONDS is not set; the deployment default is 300s")
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		t.Fatalf("ORRERY_E2E_PTY_IDLE_SECONDS = %q", raw)
	}
	return time.Duration(seconds) * time.Second
}

type ptyClient struct {
	t   *testing.T
	ws  *websocket.Conn
	buf []byte
}

func tryDialPTY(admin, execID string) (*ptyClient, error) {
	cfg, err := websocket.NewConfig(wsURL()+"/api/v1/execs/"+execID+"/pty", apiOrigin())
	if err != nil {
		return nil, err
	}
	cfg.Protocol = []string{"sandbox.v1"}
	cfg.Dialer = &net.Dialer{Timeout: 5 * time.Second}
	cfg.Header.Set("Authorization", "Bearer "+admin)
	conn, err := websocket.DialConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &ptyClient{ws: conn}, nil
}

func dialPTY(t *testing.T, admin, execID string) *ptyClient {
	t.Helper()
	client, err := tryDialPTY(admin, execID)
	if err != nil {
		t.Fatalf("dial PTY WebSocket: %v", err)
	}
	client.t = t
	return client
}

func (p *ptyClient) close() {
	_ = p.ws.Close()
}

func (p *ptyClient) send(data string) {
	if err := ptyTestCodec.Send(p.ws, []byte(data)); err != nil {
		p.t.Fatalf("send terminal input: %v", err)
	}
}

func (p *ptyClient) resize(rows, cols int) {
	raw, _ := json.Marshal(map[string]any{"type": "resize", "rows": rows, "cols": cols})
	if err := ptyTestCodec.Send(p.ws, string(raw)); err != nil {
		p.t.Fatalf("send resize: %v", err)
	}
}

// waitContains reads frames until want appears in the terminal output. Text
// control frames are skipped.
func (p *ptyClient) waitContains(want string, timeout time.Duration) {
	p.t.Helper()
	if err := p.ws.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		p.t.Fatalf("set read deadline: %v", err)
	}
	for !strings.Contains(string(p.buf), want) {
		var frame ptyFrame
		err := ptyTestCodec.Receive(p.ws, &frame)
		if err != nil {
			var netErr net.Error
			if errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
				p.t.Fatalf("never saw %q; terminal output = %q", want, p.buf)
			}
			p.t.Fatalf("receive terminal frame: %v; output = %q", err, p.buf)
		}
		if !frame.text {
			p.buf = append(p.buf, frame.data...)
		}
	}
}

// TestPTYMultiIdle is the "pty-multi-idle" acceptance scenario (#39 M1 item
// 12): several attachments write at once, a late attachment replays the ring,
// and closing every attachment reaps the session with cause pty_idle.
func TestPTYMultiIdle(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	sandboxID := readyExecSandbox(t, admin, 128<<20)
	idle := ptyIdle(t)

	e := createExec(t, admin, sandboxID, map[string]any{
		"argv": []string{"sh"},
		"pty":  map[string]any{"rows": 24, "cols": 80},
	})
	waitExecRunning(t, admin, e.ID, 30*time.Second)

	a := dialPTY(t, admin, e.ID)
	defer a.close()
	// The quotes keep the marker out of the echoed input line, so seeing it
	// means the shell actually ran the command.
	a.send("echo A\"\"-OUT\n")
	a.waitContains("A-OUT", 20*time.Second)

	// A second attachment replays the ring before it joins the live stream.
	b := dialPTY(t, admin, e.ID)
	defer b.close()
	b.waitContains("A-OUT", 20*time.Second)
	b.resize(40, 100)
	b.send("echo B\"\"-OUT\n")
	a.waitContains("B-OUT", 20*time.Second) // every attachment sees every writer
	b.waitContains("B-OUT", 20*time.Second)

	// An attached session outlives the idle window.
	time.Sleep(idle + time.Second)
	if getExec(t, admin, e.ID).State == "finished" {
		t.Fatal("PTY was reaped while attachments were open")
	}

	a.close()
	b.close()
	assertExitedWith(t, waitExecFinished(t, admin, e.ID, idle+30*time.Second), "pty_idle")
}

// TestAgentRestartKeepsPTY is the PTY part of "agent-restart-survives": after
// the Agent restarts the attachment reattaches to the same Supervisor, so the
// shell process and its state are still there.
func TestAgentRestartKeepsPTY(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	ptyIdle(t) // this scenario depends on a short deployment idle window
	sandboxID, agent, state, dataRoot := readySandboxWithAgent(t, admin)

	e := createExec(t, admin, sandboxID, map[string]any{
		"argv": []string{"sh"},
		"pty":  map[string]any{"rows": 24, "cols": 80},
	})
	waitExecRunning(t, admin, e.ID, 30*time.Second)

	a := dialPTY(t, admin, e.ID)
	a.send("x=42; echo READ\"\"Y-OK\n")
	a.waitContains("READY-OK", 20*time.Second)
	a.close()

	agent.stop()
	startAgent(t, state, dataRoot)

	// The old tunnel died with the Agent; reattach through the new one.
	var b *ptyClient
	deadline := time.Now().Add(30 * time.Second)
	for {
		var err error
		if b, err = tryDialPTY(admin, e.ID); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reattach after Agent restart: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	b.t = t
	defer b.close()
	b.waitContains("READY-OK", 20*time.Second) // the ring survived the restart
	b.send("echo value=$x\n")
	b.waitContains("value=42", 20*time.Second) // same shell process, same variable

	b.send("exit\n")
	assertExitedWith(t, waitExecFinished(t, admin, e.ID, 30*time.Second), "exited")
}
