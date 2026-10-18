package supervisor

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// RunMount is where the Sandbox's run directory is mounted (runtime.md §2).
const RunMount = "/.stars/run"

const (
	// SpecWait is the Supervisor's wait for the first `spec` message
	// (defaults.md).
	SpecWait = 30 * time.Second
	// CancelGrace is the SIGTERM-to-SIGKILL grace (defaults.md).
	CancelGrace = 10 * time.Second

	// Frame streams (runtime.md §3). The first byte of every frame.
	FrameStdout    byte = 1
	FrameStderr    byte = 2
	FrameTruncated byte = 3

	// HeaderSize is the stream byte plus the four-byte big-endian length.
	HeaderSize = 5
)

// Spec is the `spec` control message: everything about the command that must
// not appear in the exec's argv or env (runtime.md §4).
type Spec struct {
	Argv           []string          `json:"argv"`
	Env            map[string]string `json:"env,omitempty"`
	SecretEnv      map[string]string `json:"secret_env,omitempty"`
	Cwd            string            `json:"cwd,omitempty"`
	TimeoutSeconds *int64            `json:"timeout,omitempty"`
	GraceSeconds   int64             `json:"grace,omitempty"`
	LogCap         int64             `json:"log_cap,omitempty"`
	Stdin          []byte            `json:"stdin,omitempty"`
	Pty            *PTY              `json:"pty,omitempty"`
	PtyIdleSeconds int64             `json:"pty_idle,omitempty"`
}

// PTY is the initial terminal size.
type PTY struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// Exit is the `exit` record (runtime.md §3).
type Exit struct {
	Cause      string    `json:"cause"`
	ExitCode   *int32    `json:"exit_code,omitempty"`
	Signal     *int32    `json:"signal,omitempty"`
	FinishedAt time.Time `json:"finished_at"`
}

// Run is `supervisor run <exec_id>`. A container mounts the record directory
// at RunMount; Local Runtime has no mounts, so its transient service names the
// host path in STARS_RUN_ROOT (runtime.md §10).
func Run(execID string) error {
	root := RunMount
	if dir := os.Getenv("STARS_RUN_ROOT"); dir != "" {
		root = dir
	}
	return RunIn(root, execID)
}

// RunIn runs one Exec under root/<exec_id>. The atomic mkdir is the dedupe:
// a repeated StartExec exits before the command can run twice.
func RunIn(root, execID string) error {
	dir := filepath.Join(root, execID)
	if err := os.Mkdir(dir, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return err
	}
	// The Agent owns the record after the exit is acknowledged. Inside a
	// container this process is root and the Agent may not be, so the directory
	// stays writable for whoever has to delete it (the bind mount's owner).
	_ = os.Chmod(dir, 0o777)
	return runIn(dir)
}

// ctlSocketAddress is where the Supervisor listens. Local Runtime sets an
// abstract name because the host record path can exceed sun_path's 108-byte
// limit (runtime.md §10); a container keeps the filesystem socket.
func ctlSocketAddress(dir string) (string, bool) {
	if socket := os.Getenv("STARS_CTL_SOCKET"); socket != "" {
		return socket, true
	}
	return filepath.Join(dir, "ctl.sock"), false
}

func runIn(dir string) error {
	socket, abstract := ctlSocketAddress(dir)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listen on ctl.sock: %w", err)
	}
	if !abstract {
		// Connecting to a unix socket needs write permission on it. The container
		// runs this process as root and the Agent may not be, so the umask's 0755
		// would make the socket unreachable for the Agent.
		_ = os.Chmod(socket, 0o777)
	}
	defer func() {
		_ = listener.Close()
		if !abstract {
			_ = os.Remove(socket)
		}
	}()

	ctl := newControlServer(listener)
	go ctl.serve()

	var spec Spec
	select {
	case spec = <-ctl.specs:
	case <-time.After(SpecWait):
		return failStart(dir)
	}
	if len(spec.Argv) == 0 {
		return failStart(dir)
	}
	if spec.Pty != nil {
		return runPTY(dir, spec, ctl)
	}
	// Announce that no attachment surface exists: an `attach` line then gets an
	// immediate answer instead of waiting for a PTY that never comes.
	ctl.setAttach(nil)
	return runPipe(dir, spec, ctl)
}

// runPipe is the non-PTY command: stdout and stderr are stored as frames
// (runtime.md §3).
func runPipe(dir string, spec Spec, ctl *controlServer) error {
	frames, err := newFrameWriter(filepath.Join(dir, "output"), spec.LogCap)
	if err != nil {
		return failStart(dir)
	}
	defer frames.Close() //nolint:errcheck // read-only by the Agent

	// Read the OOM counter before the child exists: a very fast kill would
	// otherwise be counted in the baseline and look like a plain SIGKILL.
	oomBefore := oomKills()
	cmd := buildCommand(spec)
	stdout, stderr, err := pipeOutputs(cmd)
	if err != nil {
		return failStart(dir)
	}
	// A failed Start is the execve verdict: Go's own CLOEXEC status pipe is
	// exactly the pipe runtime.md §4 describes.
	if err := cmd.Start(); err != nil {
		closePipes(stdout, stderr)
		return failStart(dir)
	}
	// The child holds the write ends now; the parent closes its copies so the
	// readers see EOF when the last writer exits.
	stdout.w.Close() //nolint:errcheck // child owns the write end
	stderr.w.Close() //nolint:errcheck // child owns the write end

	// The command is already running; a lost `started` record does not stop it.
	_ = writeStarted(dir, time.Now().UTC())

	ctlProc := newController(cmd.Process.Pid, spec.GraceSeconds)
	done := make(chan struct{})
	defer close(done)
	if spec.TimeoutSeconds != nil && *spec.TimeoutSeconds > 0 {
		time.AfterFunc(time.Duration(*spec.TimeoutSeconds)*time.Second, func() {
			ctlProc.terminate("timed_out")
		})
	}
	go func() {
		select {
		case <-ctl.cancels:
			ctlProc.terminate("canceled")
		case <-done:
		}
	}()
	select {
	case <-ctl.cancels: // cancel arrived while the spec was still being applied
		ctlProc.terminate("canceled")
	default:
	}

	var pumps sync.WaitGroup
	pumps.Add(2)
	go pump(stdout.r, FrameStdout, frames, &pumps)
	go pump(stderr.r, FrameStderr, frames, &pumps)
	pumped := make(chan struct{})
	go func() { pumps.Wait(); close(pumped) }()

	_ = cmd.Wait() // the exit status is read from ProcessState
	ctlProc.exited()

	// Read what the main process already wrote, then close the read ends: a
	// background child may hold the pipe open forever (runtime.md §4).
	select {
	case <-pumped:
	case <-time.After(100 * time.Millisecond):
		_ = stdout.r.Close()
		_ = stderr.r.Close()
		<-pumped
	}

	exit := resultOf(cmd.ProcessState, ctlProc.cause(), oomBefore)
	return WriteExit(dir, exit)
}

type pipe struct {
	r *os.File
	w *os.File
}

func pipeOutputs(cmd *exec.Cmd) (pipe, pipe, error) {
	stdout, err := newPipe()
	if err != nil {
		return pipe{}, pipe{}, err
	}
	stderr, err := newPipe()
	if err != nil {
		closePipes(stdout, stderr)
		return pipe{}, pipe{}, err
	}
	cmd.Stdout, cmd.Stderr = stdout.w, stderr.w
	return stdout, stderr, nil
}

func newPipe() (pipe, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return pipe{}, err
	}
	return pipe{r: r, w: w}, nil
}

func closePipes(pipes ...pipe) {
	for _, p := range pipes {
		if p.r != nil {
			_ = p.r.Close()
		}
		if p.w != nil {
			_ = p.w.Close()
		}
	}
}

func buildCommand(spec Spec) *exec.Cmd {
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = workspaceDir(spec.Cwd)
	cmd.Env = commandEnv(spec)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if len(spec.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(spec.Stdin)
	}
	return cmd
}

func workspaceDir(cwd string) string {
	if cwd == "" {
		return "/workspace"
	}
	if filepath.IsAbs(cwd) {
		return cwd
	}
	return filepath.Join("/workspace", cwd)
}

// commandEnv is the container environment plus the request's env, with Secret
// values applied last. Secrets reach only this process, never argv or Env of a
// Runtime-level exec (runtime.md §4).
func commandEnv(spec Spec) []string {
	env := os.Environ()
	for key, value := range spec.Env {
		env = append(env, key+"="+value)
	}
	for key, value := range spec.SecretEnv {
		env = append(env, key+"="+value)
	}
	return env
}

func pump(r *os.File, stream byte, frames *frameWriter, wg *sync.WaitGroup) {
	defer wg.Done()
	defer r.Close() //nolint:errcheck // reader owns the read end
	w := &utf8Writer{frames: frames, stream: stream}
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
		}
		if err != nil {
			// A command can end in the middle of a sequence; the held bytes are
			// then an invalid tail, delivered as its own frame (runtime.md §3).
			w.finish()
			return
		}
	}
}

// controller applies cancel and timeout: SIGTERM to the process group, then
// SIGKILL after the grace. The first trigger wins.
type controller struct {
	mu        sync.Mutex
	pid       int
	grace     time.Duration
	termCause string
	gone      bool
}

func newController(pid int, graceSeconds int64) *controller {
	grace := CancelGrace
	if graceSeconds > 0 {
		grace = time.Duration(graceSeconds) * time.Second
	}
	return &controller{pid: pid, grace: grace}
}

func (c *controller) terminate(cause string) {
	c.mu.Lock()
	if c.termCause != "" || c.gone {
		c.mu.Unlock()
		return
	}
	c.termCause = cause
	_ = syscall.Kill(-c.pid, syscall.SIGTERM)
	c.mu.Unlock()

	go func() {
		time.Sleep(c.grace)
		c.mu.Lock()
		defer c.mu.Unlock()
		if !c.gone {
			_ = syscall.Kill(-c.pid, syscall.SIGKILL)
		}
	}()
}

func (c *controller) exited() {
	c.mu.Lock()
	c.gone = true
	c.mu.Unlock()
}

func (c *controller) cause() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.termCause
}

func resultOf(state *os.ProcessState, terminated string, oomBefore int64) Exit {
	exit := Exit{FinishedAt: time.Now().UTC()}
	if terminated != "" {
		exit.Cause = terminated
	} else {
		exit.Cause = "exited"
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok {
		return exit
	}
	if status.Signaled() {
		signal := int32(status.Signal())
		exit.Signal = &signal
		if exit.Cause == "exited" && status.Signal() == syscall.SIGKILL && oomKills() > oomBefore {
			exit.Cause = "oom_killed"
		}
		return exit
	}
	code := int32(status.ExitStatus())
	exit.ExitCode = &code
	return exit
}

// oomKills reads the cgroup's cumulative OOM kill count (runtime.md §4).
func oomKills() int64 {
	for _, path := range []string{"/sys/fs/cgroup/memory.events", "/sys/fs/cgroup/memory/memory.oom_control"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == "oom_kill" {
				var n int64
				if _, err := fmt.Sscan(fields[1], &n); err == nil {
					return n
				}
			}
		}
	}
	return 0
}

func writeStarted(dir string, at time.Time) error {
	tmp := filepath.Join(dir, "started.tmp")
	if err := os.WriteFile(tmp, []byte(at.Format(time.RFC3339Nano)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "started"))
}

func failStart(dir string) error {
	return WriteExit(dir, Exit{Cause: "start_failed", FinishedAt: time.Now().UTC()})
}

// WriteExit atomically writes the `exit` record. The Agent uses it when it has
// to end an Exec the Supervisor no longer can (runtime.md §8).
func WriteExit(dir string, exit Exit) error {
	raw, err := json.Marshal(exit)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "exit.tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "exit"))
}

// controlServer accepts the line protocol on ctl.sock (runtime.md §5): one
// `spec`, then `cancel`, `attach`, `input <n>` plus n raw bytes, and
// `resize <rows> <cols>`.
type controlServer struct {
	listener net.Listener
	specs    chan Spec
	cancels  chan struct{}
	inputs   chan []byte
	resizes  chan winsize

	mu          sync.Mutex
	attach      func(net.Conn) func()
	attachReady chan struct{} // closed once the attach decision is made
}

type winsize struct {
	rows uint16
	cols uint16
}

func newControlServer(listener net.Listener) *controlServer {
	return &controlServer{
		listener:    listener,
		specs:       make(chan Spec, 1),
		cancels:     make(chan struct{}, 1),
		inputs:      make(chan []byte, 32),
		resizes:     make(chan winsize, 32),
		attachReady: make(chan struct{}),
	}
}

func (c *controlServer) setAttach(fn func(net.Conn) func()) {
	c.mu.Lock()
	c.attach = fn
	c.mu.Unlock()
	close(c.attachReady)
}

func (c *controlServer) attachFunc() func(net.Conn) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attach
}

func (c *controlServer) serve() {
	for {
		conn, err := c.listener.Accept()
		if err != nil {
			return
		}
		go c.handle(conn)
	}
}

func (c *controlServer) handle(conn net.Conn) {
	defer conn.Close() //nolint:errcheck // connection lifetime
	var detach func()
	defer func() {
		if detach != nil {
			detach()
		}
	}()
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case line == "cancel":
			select {
			case c.cancels <- struct{}{}:
			default:
			}
		case line == "attach":
			if detach != nil {
				continue
			}
			// The spec and the attach can arrive on two connections in either
			// order; wait for the PTY decision before answering.
			select {
			case <-c.attachReady:
			case <-time.After(5 * time.Second):
				return
			}
			if fn := c.attachFunc(); fn != nil {
				detach = fn(conn)
			}
		case strings.HasPrefix(line, "input "):
			n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "input ")))
			if err != nil || n < 0 || n > 1<<20 {
				return
			}
			data := make([]byte, n)
			if _, err := io.ReadFull(r, data); err != nil {
				return
			}
			select {
			case c.inputs <- data:
			case <-time.After(time.Second):
			}
		case strings.HasPrefix(line, "resize "):
			fields := strings.Fields(line)
			if len(fields) != 3 {
				continue
			}
			rows, errRows := strconv.ParseUint(fields[1], 10, 16)
			cols, errCols := strconv.ParseUint(fields[2], 10, 16)
			if errRows != nil || errCols != nil || rows == 0 || cols == 0 {
				continue
			}
			select {
			case c.resizes <- winsize{rows: uint16(rows), cols: uint16(cols)}:
			default:
			}
		default:
			var spec Spec
			if json.Unmarshal([]byte(line), &spec) == nil && len(spec.Argv) > 0 {
				select {
				case c.specs <- spec:
				default:
				}
			}
		}
	}
}

// frameWriter appends `[stream][len][data]` frames and enforces log_cap.
type frameWriter struct {
	mu        sync.Mutex
	f         *os.File
	seq       int64
	cap       int64
	truncated bool
}

func newFrameWriter(path string, cap int64) (*frameWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &frameWriter{f: f, cap: cap}, nil
}

func (w *frameWriter) Close() error { return w.f.Close() }

func (w *frameWriter) write(stream byte, data []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.truncated {
		return
	}
	if w.cap > 0 && w.seq+HeaderSize+int64(len(data)) > w.cap {
		w.append(FrameTruncated, nil)
		w.truncated = true
		return
	}
	w.append(stream, data)
}

func (w *frameWriter) append(stream byte, data []byte) {
	var header [HeaderSize]byte
	header[0] = stream
	binary.BigEndian.PutUint32(header[1:], uint32(len(data)))
	if _, err := w.f.Write(header[:]); err != nil {
		return
	}
	if _, err := w.f.Write(data); err != nil {
		return
	}
	w.seq += HeaderSize + int64(len(data))
}

// utf8Writer splits output on rune boundaries. A byte that is not valid UTF-8
// becomes its own frame so the Control Plane can mark it base64.
type utf8Writer struct {
	frames  *frameWriter
	stream  byte
	pending []byte
}

func (u *utf8Writer) Write(p []byte) (int, error) {
	buf := append(u.pending, p...)
	start, i := 0, 0
	for i < len(buf) {
		if !utf8.FullRune(buf[i:]) {
			break
		}
		r, size := utf8.DecodeRune(buf[i:])
		if r == utf8.RuneError && size == 1 {
			if i > start {
				u.frames.write(u.stream, buf[start:i])
			}
			u.frames.write(u.stream, buf[i:i+1])
			i++
			start = i
			continue
		}
		i += size
	}
	if i > start {
		u.frames.write(u.stream, buf[start:i])
	}
	u.pending = append([]byte(nil), buf[i:]...)
	return len(p), nil
}

func (u *utf8Writer) finish() {
	if len(u.pending) > 0 {
		u.frames.write(u.stream, u.pending)
		u.pending = nil
	}
}

// FrameName maps a frame's first byte to the API's stream name.
func FrameName(stream byte) string {
	switch stream {
	case FrameStdout:
		return "stdout"
	case FrameStderr:
		return "stderr"
	default:
		return "truncated"
	}
}

// ReadFrame reads one frame and returns its stream and data.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var header [HeaderSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	data := make([]byte, binary.BigEndian.Uint32(header[1:]))
	if _, err := io.ReadFull(r, data); err != nil {
		return 0, nil, err
	}
	return header[0], data, nil
}
