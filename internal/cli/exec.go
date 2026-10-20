package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	orreryclient "github.com/zibyn/orrery/client"
)

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func runExec(c *client, args []string, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: orrery exec run|cancel")
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
			return fmt.Errorf("usage: orrery exec cancel <id>")
		}
		e, err := c.CancelExec(ctx, fs.Arg(0))
		if err != nil {
			return err
		}
		return c.print(e)
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
		return fmt.Errorf("usage: orrery exec run [flags] <sandbox-id> -- argv...")
	}
	// The Sandbox id comes before the program: flag.Parse leaves the first
	// non-flag in NArg, so take it and treat the rest as argv.
	sandboxID := fs.Arg(0)
	req := orreryclient.ExecRequest{Argv: fs.Args()[1:]}
	if *cwd != "" {
		req.Cwd = *cwd
	}
	if *timeout > 0 {
		req.Timeout = timeout
	}
	if *useStdin {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		stdin := string(data)
		req.Stdin = &stdin
	}
	if len(env) > 0 {
		req.Env = map[string]string{}
		for _, pair := range env {
			k, v, ok := strings.Cut(pair, "=")
			if !ok || k == "" {
				return fmt.Errorf("--env must be KEY=VALUE, got %q", pair)
			}
			req.Env[k] = v
		}
	}
	created, err := c.CreateExec(ctx, sandboxID, req, *idem)
	if err != nil {
		return err
	}
	if *noFollow {
		return c.print(created)
	}
	final, err := c.FollowOutput(ctx, created.ID, c.out, stderr)
	if err != nil {
		return err
	}
	printExecResult(stderr, created.ID, final.State, final.Result)
	if final.Result != nil && final.Result.Cause == "exited" &&
		final.Result.ExitCode != nil && *final.Result.ExitCode != 0 {
		return fmt.Errorf("command exited with status %d", *final.Result.ExitCode)
	}
	return nil
}

func printExecResult(w io.Writer, id, state string, r *orreryclient.ExecResult) {
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
