package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
	"github.com/zibyn/stars-sandbox/internal/supervisor"
)

const (
	// execGrace is the SIGTERM-to-SIGKILL grace (defaults.md).
	execGrace = int64(10)
	execDial  = 30 * time.Second
	// execBatchBytes bounds one OutputBatch.
	execBatchBytes = 64 << 10
)

// startExec launches `supervisor run` detached and starts delivering its
// record. The Supervisor's atomic mkdir makes a repeated command harmless.
func (r *Runtime) startExec(ctx context.Context, cmd *nodev1.StartExec) error {
	sandboxID, err := uuid.Parse(cmd.SandboxId)
	if err != nil {
		return errors.New("invalid sandbox id")
	}
	execID, err := uuid.Parse(cmd.ExecId)
	if err != nil {
		return errors.New("invalid exec id")
	}
	slog.Info("start exec", "exec_id", execID, "sandbox_id", sandboxID)
	dir := filepath.Join(sandboxRunPath(r.dataRoot, sandboxID), execID.String())
	// Reconnect replay: the Supervisor may have finished and exited while the
	// control stream was down. The record still holds every frame still owed.
	if _, err := os.Stat(filepath.Join(dir, "exit")); err == nil {
		go r.streamExec(ctx, sandboxID, execID, dir)
		return nil
	}
	if err := r.d.negotiate(ctx); err != nil {
		return err
	}

	create := map[string]any{
		"AttachStdout": false,
		"AttachStderr": false,
		"Tty":          false,
		"Cmd":          []string{supervisorBin, "run", execID.String()},
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := r.d.do(ctx, "POST", r.d.api("/containers/"+containerName(sandboxID)+"/exec"), create, &created); err != nil {
		return fmt.Errorf("create exec: %w", err)
	}
	if err := r.d.do(ctx, "POST", r.d.api("/exec/"+created.ID+"/start"),
		map[string]any{"Detach": true, "Tty": false}, nil); err != nil {
		return fmt.Errorf("start exec: %w", err)
	}

	// The record outlives the control socket: the Supervisor writes `exit` and
	// removes ctl.sock before the Agent can reconnect. Delivery therefore never
	// waits for the spec dial; both goroutines poll the record instead.
	go r.observeStarted(ctx, execID.String(), dir)
	go r.streamExec(ctx, sandboxID, execID, dir)

	socket := filepath.Join(dir, "ctl.sock")
	conn, err := dialUnix(ctx, socket, execDial)
	if err != nil {
		// A Supervisor that finished while we dialed has already removed the
		// socket; streamExec is delivering the record, so this is not a failure.
		if _, statErr := os.Stat(filepath.Join(dir, "exit")); statErr == nil {
			return nil
		}
		return fmt.Errorf("connect to Supervisor: %w", err)
	}
	spec := supervisor.Spec{
		Argv:           cmd.Argv,
		Env:            r.commandEnv(ctx, sandboxID, cmd.Env),
		SecretEnv:      cmd.SecretEnv,
		Cwd:            cmd.Cwd,
		GraceSeconds:   execGrace,
		LogCap:         cmd.LogCap,
		Stdin:          cmd.Stdin,
		TimeoutSeconds: timeoutPtr(cmd.TimeoutSeconds),
	}
	if cmd.Pty != nil {
		// The Supervisor allocates the pty itself; stdin is ignored in PTY mode
		// (api.md §4).
		spec.Pty = &supervisor.PTY{Rows: uint16(cmd.Pty.Rows), Cols: uint16(cmd.Pty.Cols)}
		spec.PtyIdleSeconds = cmd.PtyIdleSeconds
		spec.Stdin = nil
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		conn.Close() //nolint:errcheck // nothing to flush
		return err
	}
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		conn.Close() //nolint:errcheck // write already failed
		return fmt.Errorf("send spec: %w", err)
	}
	conn.Close() //nolint:errcheck // the Supervisor keeps the listener

	return nil
}

func timeoutPtr(seconds int64) *int64 {
	if seconds <= 0 {
		return nil
	}
	return &seconds
}

// execEnv merges the platform's values last: the caller's variables first,
// then PATH with /.stars/bin appended, then the Git config that clears the
// image's helpers and installs ours (node-protocol.md).
func execEnv(caller map[string]string, imagePath string) map[string]string {
	env := make(map[string]string, len(caller)+7)
	for key, value := range caller {
		env[key] = value
	}
	path := env["PATH"]
	if path == "" {
		path = imagePath
	}
	if path == "" {
		env["PATH"] = binaryDir
	} else {
		env["PATH"] = path + ":" + binaryDir
	}
	env["GIT_CONFIG_COUNT"] = "2"
	env["GIT_CONFIG_KEY_0"] = "credential.helper"
	env["GIT_CONFIG_VALUE_0"] = ""
	env["GIT_CONFIG_KEY_1"] = "credential.helper"
	env["GIT_CONFIG_VALUE_1"] = binaryDir + "/stars-sandbox git-credential"
	return env
}

// commandEnv chooses the PATH base: the caller's env wins, then the image
// config, then the Agent's own PATH for a Runtime without an image.
func (r *Runtime) commandEnv(ctx context.Context, sandboxID uuid.UUID, caller map[string]string) map[string]string {
	imagePath := ""
	if caller["PATH"] == "" {
		imagePath = r.imagePath(ctx, sandboxID)
	}
	return execEnv(caller, imagePath)
}

// imagePath reads the container's PATH, which the Runtime filled from the
// image config. A failed read falls back to the Agent's own PATH.
func (r *Runtime) imagePath(ctx context.Context, sandboxID uuid.UUID) string {
	var info struct {
		Config struct {
			Env []string `json:"Env"`
		} `json:"Config"`
	}
	if err := r.d.do(ctx, http.MethodGet, r.d.api("/containers/"+containerName(sandboxID)+"/json"), nil, &info); err != nil {
		slog.Warn("inspect container PATH", "sandbox_id", sandboxID, "error", err)
		return os.Getenv("PATH")
	}
	for _, entry := range info.Config.Env {
		if path, ok := strings.CutPrefix(entry, "PATH="); ok {
			return path
		}
	}
	return os.Getenv("PATH")
}

// cancelExec relays the persistent cancel intent. It waits for the Supervisor's
// socket so a cancel that arrives before StartExec still lands.
func (r *Runtime) cancelExec(ctx context.Context, execID uuid.UUID) error {
	pattern := filepath.Join(r.dataRoot, "sandboxes", "*", "run", execID.String())
	// ponytail: one glob over every Sandbox on this Node; index run/ by Exec if
	// the Sandbox count makes the scan visible.
	deadline := time.Now().Add(execDial)
	for {
		matches, _ := filepath.Glob(pattern)
		if len(matches) > 0 {
			conn, err := dialSocket(filepath.Join(matches[0], "ctl.sock"))
			if err == nil {
				_, writeErr := conn.Write([]byte("cancel\n"))
				conn.Close() //nolint:errcheck // one line
				return writeErr
			}
		}
		if time.Now().After(deadline) {
			return errors.New("supervisor socket never appeared")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func dialUnix(ctx context.Context, path string, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := dialSocket(path)
		if err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// dialSocket connects to a unix socket. The kernel caps sun_path at 108 bytes,
// and the Agent's data root can be longer, so a long path is reached through a
// directory fd instead.
func dialSocket(path string) (net.Conn, error) {
	if len(path) < 108 { // including the trailing NUL the kernel reserves
		return net.Dial("unix", path)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer dir.Close() //nolint:errcheck // the connection outlives the dir fd
	return net.Dial("unix", fmt.Sprintf("/proc/self/fd/%d/%s", dir.Fd(), filepath.Base(path)))
}

// observeStarted reports execve success from the `started` record.
func (r *Runtime) observeStarted(ctx context.Context, execID, dir string) {
	path := filepath.Join(dir, "started")
	deadline := time.Now().Add(supervisor.SpecWait)
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			r.sendExecObserved(execID, string(raw))
			return
		}
		if _, exitErr := os.Stat(filepath.Join(dir, "exit")); exitErr == nil {
			return // start_failed; the exit frame carries it
		}
		if time.Now().After(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (r *Runtime) sendExecObserved(execID, startedAt string) {
	r.send(&nodev1.AgentMessage{Message: &nodev1.AgentMessage_ExecObserved{
		ExecObserved: &nodev1.ExecObserved{ExecId: execID, StartedAt: startedAt},
	}})
}

// streamExec pushes the record to the Control Plane and resumes from the
// stored_seq it acknowledges. Once the exit frame is acknowledged the record
// is deleted (runtime.md §7).
func (r *Runtime) streamExec(ctx context.Context, sandboxID, execID uuid.UUID, dir string) {
	if !r.beginExec(execID.String()) {
		return
	}
	defer r.endExec(execID.String())

	stream, err := r.execStream(ctx)
	if err != nil {
		return
	}
	defer stream.CloseSend() //nolint:errcheck // best effort
	if err := stream.Send(&nodev1.OutputBatch{ExecId: execID.String()}); err != nil {
		return
	}
	ack, err := stream.Recv()
	if err != nil {
		return
	}

	// The output file appears only after a successful execve; start_failed
	// writes just the exit record.
	f, err := waitOpen(ctx, dir)
	if err != nil {
		return
	}
	seq := ack.StoredSeq
	if f != nil {
		defer f.Close() //nolint:errcheck // read-only
		if _, err := f.Seek(seq, io.SeekStart); err != nil {
			return
		}
	}

	for ctx.Err() == nil {
		if f != nil {
			frames, next, err := readBatches(f, seq)
			if err != nil {
				return
			}
			// A partial trailing frame is consumed by the failed read; seek back
			// to its first byte so the next pass parses it whole.
			if _, err := f.Seek(next, io.SeekStart); err != nil {
				return
			}
			seq = next
			if len(frames) > 0 {
				if err := stream.Send(&nodev1.OutputBatch{ExecId: execID.String(), Frames: frames}); err != nil {
					return
				}
				ack, err := stream.Recv()
				if err != nil {
					return
				}
				seq = ack.StoredSeq
				if _, err := f.Seek(seq, io.SeekStart); err != nil {
					return
				}
				continue
			}
		}

		exit, ready := readExit(dir)
		if ready {
			frame := &nodev1.OutputFrame{Seq: seq, Stream: "exit", Cause: exit.Cause}
			if exit.ExitCode != nil {
				frame.ExitCode = exit.ExitCode
			}
			if exit.Signal != nil {
				frame.Signal = exit.Signal
			}
			if err := stream.Send(&nodev1.OutputBatch{ExecId: execID.String(), Frames: []*nodev1.OutputFrame{frame}}); err != nil {
				return
			}
			if _, err := stream.Recv(); err != nil {
				return
			}
			// A live attachment still reads `exit` to report the result on its
			// WebSocket (api.md §4).
			r.waitPTYDetached(ctx, execID.String())
			_ = os.RemoveAll(dir)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// waitOpen waits for the output record. A nil file with a nil error means the
// Supervisor wrote only the exit record (start_failed).
func waitOpen(ctx context.Context, dir string) (*os.File, error) {
	path := filepath.Join(dir, "output")
	for {
		f, err := os.Open(path)
		if err == nil {
			return f, nil
		}
		if _, exitErr := os.Stat(filepath.Join(dir, "exit")); exitErr == nil {
			return nil, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// readBatches reads frames until a batch is large enough or the file ends. The
// returned seq is the byte offset after the last frame read.
func readBatches(f *os.File, seq int64) ([]*nodev1.OutputFrame, int64, error) {
	var frames []*nodev1.OutputFrame
	size := 0
	for size < execBatchBytes {
		start := seq
		stream, data, err := supervisor.ReadFrame(f)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return frames, seq, nil
			}
			return frames, seq, err
		}
		seq += supervisor.HeaderSize + int64(len(data))
		frames = append(frames, &nodev1.OutputFrame{
			Seq: start, Stream: supervisor.FrameName(stream), Data: data,
		})
		size += len(data)
	}
	return frames, seq, nil
}

func readExit(dir string) (supervisor.Exit, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "exit"))
	if err != nil {
		return supervisor.Exit{}, false
	}
	var exit supervisor.Exit
	if err := json.Unmarshal(raw, &exit); err != nil {
		return supervisor.Exit{}, false
	}
	return exit, true
}

func (r *Runtime) beginExec(id string) bool {
	r.execMu.Lock()
	defer r.execMu.Unlock()
	if _, ok := r.execs[id]; ok {
		return false
	}
	r.execs[id] = struct{}{}
	return true
}

func (r *Runtime) endExec(id string) {
	r.execMu.Lock()
	defer r.execMu.Unlock()
	delete(r.execs, id)
}
