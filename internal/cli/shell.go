package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/websocket"
	"golang.org/x/term"

	orreryclient "github.com/zibyn/orrery/client"
	"github.com/zibyn/orrery/internal/ptywire"
)

func runShell(c *client, args []string, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: orrery shell <sandbox-id> [-- argv...]")
	}
	sandboxID := args[0]
	argv := args[1:]
	if len(argv) > 0 && argv[0] == "--" {
		argv = argv[1:]
	}
	if len(argv) == 0 {
		argv = []string{"sh"}
	}
	rows, cols := terminalSize()
	created, err := c.CreateExec(ctx, sandboxID, orreryclient.ExecRequest{
		Argv: argv,
		PTY:  &orreryclient.PTY{Rows: uint16(rows), Cols: uint16(cols)},
	}, "")
	if err != nil {
		return err
	}
	if err := c.waitExecStarted(created.ID); err != nil {
		return err
	}
	conn, err := c.dialPTY(created.ID)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck // connection lifetime

	// Stdin and SIGWINCH both send; a WebSocket connection has one writer.
	var sendMu sync.Mutex
	send := func(v any) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return ptywire.Codec.Send(conn, v)
	}

	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		state, err := term.MakeRaw(fd)
		if err == nil {
			defer term.Restore(fd, state) //nolint:errcheck // best effort
		}
		wins := make(chan os.Signal, 1)
		signal.Notify(wins, syscall.SIGWINCH)
		defer signal.Stop(wins)
		go func() {
			for range wins {
				rows, cols := terminalSize()
				raw, _ := json.Marshal(map[string]any{"type": "resize", "rows": rows, "cols": cols})
				if send(string(raw)) != nil {
					return
				}
			}
		}()
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 && send(append([]byte(nil), buf[:n]...)) != nil {
				return
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		var frame ptywire.Frame
		if err := ptywire.Codec.Receive(conn, &frame); err != nil {
			return fmt.Errorf("terminal connection closed")
		}
		if !frame.Text {
			if _, err := c.out.Write(frame.Data); err != nil {
				return err
			}
			continue
		}
		var control struct {
			Type     string `json:"type"`
			Cause    string `json:"cause"`
			ExitCode *int32 `json:"exit_code"`
		}
		if json.Unmarshal(frame.Data, &control) != nil || control.Type != "exit" {
			continue
		}
		if control.ExitCode != nil && *control.ExitCode != 0 {
			return fmt.Errorf("terminal exited with status %d", *control.ExitCode)
		}
		if control.Cause != "" && control.Cause != "exited" {
			return fmt.Errorf("terminal ended: %s", control.Cause)
		}
		return nil
	}
}

// waitExecStarted returns once the Exec is no longer pending. The Agent's
// tunnel reaches a Supervisor record that does not exist yet only after
// start-up, so dialling earlier gets a 404 (api.md §4).
func (c *client) waitExecStarted(id string) error {
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		e, err := c.GetExec(ctx, id)
		if err != nil {
			return err
		}
		if e.State != "pending" {
			return nil
		}
	}
	return fmt.Errorf("exec %s did not start", id)
}

func (c *client) dialPTY(id string) (*websocket.Conn, error) {
	config, err := websocket.NewConfig(c.PTYURL(id), c.BaseURL())
	if err != nil {
		return nil, err
	}
	config.Protocol = []string{"sandbox.v1"}
	config.Header.Set("Authorization", "Bearer "+c.Token())
	return websocket.DialConfig(config)
}

func terminalSize() (rows, cols int) {
	width, height, err := term.GetSize(int(os.Stdin.Fd()))
	if err != nil || width <= 0 || height <= 0 {
		return 24, 80
	}
	return height, width
}
