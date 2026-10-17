package cli

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zibyn/stars-sandbox/internal/supervisor"
)

type execFrame struct {
	Seq      int64  `json:"seq"`
	Stream   string `json:"stream"`
	Data     string `json:"data"`
	Encoding string `json:"encoding"`
}

type execState struct {
	ID           string          `json:"id"`
	State        string          `json:"state"`
	LogTruncated bool            `json:"log_truncated"`
	Result       json.RawMessage `json:"result"`
}

type execResult struct {
	Cause    string `json:"cause"`
	ExitCode *int32 `json:"exit_code"`
	Signal   *int32 `json:"signal"`
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func runExec(c *client, args []string, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: stars-sandbox exec run|cancel")
	}
	switch args[0] {
	case "run":
		return runExecRun(c, args[1:], stderr)
	case "cancel":
		fs := flag.NewFlagSet("exec cancel", flag.ContinueOnError)
		fs.SetOutput(stderr)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: stars-sandbox exec cancel <id>")
		}
		return c.sendAndPrint(http.MethodPost, "/api/v1/execs/"+url.PathEscape(fs.Arg(0))+"/cancel", nil, "")
	default:
		return fmt.Errorf("unknown exec command %q", args[0])
	}
}

func runExecRun(c *client, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("exec run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cwd := fs.String("cwd", "", "working directory inside /workspace")
	timeout := fs.Int64("timeout", 0, "timeout in seconds; 0 means none")
	useStdin := fs.Bool("stdin", false, "send this process's stdin")
	noFollow := fs.Bool("no-follow", false, "create the Exec and return without waiting")
	idem := fs.String("idempotency-key", "", "idempotency key")
	var env stringList
	fs.Var(&env, "env", "KEY=VALUE; may repeat")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: stars-sandbox exec run [flags] <sandbox-id> -- argv...")
	}
	// The Sandbox id comes before the program: flag.Parse leaves the first
	// non-flag in NArg, so take it and treat the rest as argv.
	sandboxID := fs.Arg(0)
	argv := fs.Args()[1:]
	body := map[string]any{"argv": argv}
	if *cwd != "" {
		body["cwd"] = *cwd
	}
	if *timeout > 0 {
		body["timeout"] = *timeout
	}
	if *useStdin {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		body["stdin"] = string(data)
	}
	if len(env) > 0 {
		vars := map[string]string{}
		for _, pair := range env {
			k, v, ok := strings.Cut(pair, "=")
			if !ok || k == "" {
				return fmt.Errorf("--env must be KEY=VALUE, got %q", pair)
			}
			vars[k] = v
		}
		body["env"] = vars
	}
	data, err := c.do(http.MethodPost, "/api/v1/sandboxes/"+url.PathEscape(sandboxID)+"/execs",
		nil, body, *idem)
	if err != nil {
		return err
	}
	var created execState
	if err := json.Unmarshal(data, &created); err != nil {
		return fmt.Errorf("decode Exec: %w", err)
	}
	if *noFollow {
		return c.print(data)
	}
	state, err := c.followExec(created.ID, 0, c.out, stderr)
	if err != nil {
		return err
	}
	var result *execResult
	if state.Result != nil {
		var r execResult
		if json.Unmarshal(state.Result, &r) == nil {
			result = &r
		}
	}
	printExecResult(stderr, created.ID, state.State, result)
	if result != nil && result.Cause == "exited" && result.ExitCode != nil && *result.ExitCode != 0 {
		return fmt.Errorf("command exited with status %d", *result.ExitCode)
	}
	return nil
}

func printExecResult(w io.Writer, id, state string, r *execResult) {
	if r == nil {
		fmt.Fprintf(w, "exec %s: %s\n", id, state)
		return
	}
	switch {
	case r.ExitCode != nil:
		fmt.Fprintf(w, "exec %s: %s (exit %d)\n", id, r.Cause, *r.ExitCode)
	case r.Signal != nil:
		fmt.Fprintf(w, "exec %s: %s (signal %d)\n", id, r.Cause, *r.Signal)
	default:
		fmt.Fprintf(w, "exec %s: %s\n", id, r.Cause)
	}
}

// followExec polls output pages until a page is empty and the Exec is
// finished. Frames are written as they arrive.
// ponytail: polls instead of the follow WebSocket; reuse the socket if 100 ms
// latency matters.
func (c *client) followExec(id string, after int64, stdout, stderr io.Writer) (execState, error) {
	for {
		frames, err := c.execPage(id, after)
		if err != nil {
			return execState{}, err
		}
		for _, f := range frames {
			after = writeExecFrame(stdout, stderr, f)
		}
		if len(frames) > 0 {
			continue
		}
		data, err := c.do(http.MethodGet, "/api/v1/execs/"+url.PathEscape(id), nil, nil, "")
		if err != nil {
			return execState{}, err
		}
		var e execState
		if err := json.Unmarshal(data, &e); err != nil {
			return execState{}, fmt.Errorf("decode Exec: %w", err)
		}
		if e.State == "finished" {
			return e, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (c *client) execPage(id string, after int64) ([]execFrame, error) {
	q := url.Values{}
	q.Set("after", strconv.FormatInt(after, 10))
	data, err := c.do(http.MethodGet, "/api/v1/execs/"+url.PathEscape(id)+"/output", q, nil, "")
	if err != nil {
		return nil, err
	}
	var frames []execFrame
	if err := json.Unmarshal(data, &frames); err != nil {
		return nil, fmt.Errorf("decode output: %w", err)
	}
	return frames, nil
}

// writeExecFrame writes one frame and returns the next byte offset.
func writeExecFrame(stdout, stderr io.Writer, f execFrame) int64 {
	data := []byte(f.Data)
	if f.Encoding == "base64" {
		if raw, err := base64.StdEncoding.DecodeString(f.Data); err == nil {
			data = raw
		}
	}
	switch f.Stream {
	case "stderr":
		_, _ = stderr.Write(data)
	case "truncated":
		fmt.Fprintln(stderr, "\n[output truncated]")
	default:
		_, _ = stdout.Write(data)
	}
	return f.Seq + supervisor.HeaderSize + int64(len(data))
}
