package supervisor

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ptyRingBytes is the PTY ring buffer (defaults.md, 64 KiB).
const ptyRingBytes = 64 << 10

// defaultPTYIdle is the deployment default (defaults.md, 300 s).
const defaultPTYIdle = 300 * time.Second

// runPTY runs the command on a pty. Its output lives only in the ring buffer
// and is broadcast to attachments; the Exec record holds no `output` file
// (ADR-0017).
func runPTY(dir string, spec Spec, ctl *controlServer) error {
	master, slave, err := openPTY(spec.Pty.Rows, spec.Pty.Cols)
	if err != nil {
		return failStart(dir)
	}
	defer master.Close() //nolint:errcheck // reader already stopped

	oomBefore := oomKills()
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = workspaceDir(spec.Cwd)
	cmd.Env = commandEnv(spec)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		slave.Close() //nolint:errcheck // start failed
		return failStart(dir)
	}
	slave.Close() //nolint:errcheck // the child owns it now
	_ = writeStarted(dir, time.Now().UTC())

	ctlProc := newController(cmd.Process.Pid, spec.GraceSeconds)
	idle := time.Duration(spec.PtyIdleSeconds) * time.Second
	if idle <= 0 {
		idle = defaultPTYIdle
	}
	session := newPTYSession(master, idle, func() { ctlProc.terminate("pty_idle") })
	ctl.setAttach(session.attach)

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctl.cancels:
			ctlProc.terminate("canceled")
		case <-done:
		}
	}()
	if spec.TimeoutSeconds != nil && *spec.TimeoutSeconds > 0 {
		time.AfterFunc(time.Duration(*spec.TimeoutSeconds)*time.Second, func() {
			ctlProc.terminate("timed_out")
		})
	}
	go session.relay(ctl, done)
	go session.pump()

	_ = cmd.Wait()
	ctlProc.exited()
	// Let the pty deliver what the main process wrote, then force the reader to
	// end: a background child may hold the slave open forever.
	session.finish(100 * time.Millisecond)

	exit := resultOf(cmd.ProcessState, ctlProc.cause(), oomBefore)
	if err := WriteExit(dir, exit); err != nil {
		return err
	}
	// The exit record exists before an attachment's read ends, so the Agent can
	// report the result on the same WebSocket (api.md §4).
	session.closeAttachments()
	return nil
}

// openPTY allocates a pty master and its slave. Unlock first: a locked slave
// cannot be opened.
func openPTY(rows, cols uint16) (*os.File, *os.File, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		master.Close() //nolint:errcheck // allocation failed
		return nil, nil, err
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		master.Close() //nolint:errcheck // allocation failed
		return nil, nil, err
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close() //nolint:errcheck // allocation failed
		return nil, nil, err
	}
	if rows > 0 && cols > 0 {
		_ = unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
	}
	return master, slave, nil
}

// ptySession owns the master, the ring buffer and the attachments.
type ptySession struct {
	master *os.File
	ring   *ringBuffer
	idle   time.Duration
	reap   func()

	writeMu sync.Mutex
	pumpEnd chan struct{}

	mu        sync.Mutex
	atts      map[chan []byte]net.Conn
	idleTimer *time.Timer
	idleGen   int
	closed    bool
}

func newPTYSession(master *os.File, idle time.Duration, reap func()) *ptySession {
	s := &ptySession{
		master:  master,
		ring:    newRingBuffer(ptyRingBytes),
		idle:    idle,
		reap:    reap,
		pumpEnd: make(chan struct{}),
		atts:    make(map[chan []byte]net.Conn),
	}
	s.mu.Lock()
	s.startIdleLocked() // no attachment yet; the Agent attaches immediately
	s.mu.Unlock()
	return s
}

// pump fans terminal output into the ring and every attachment.
func (s *ptySession) pump() {
	defer close(s.pumpEnd)
	buf := make([]byte, 32*1024)
	for {
		n, err := s.master.Read(buf)
		if n > 0 {
			// The pump reuses buf; attachments need their own copy.
			s.broadcast(append([]byte(nil), buf[:n]...))
		}
		if err != nil {
			return
		}
	}
}

func (s *ptySession) broadcast(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ring.write(data)
	for ch, conn := range s.atts {
		select {
		case ch <- data:
		default:
			// An attachment too slow to keep up is dropped; reattaching replays
			// the ring (api.md §4).
			delete(s.atts, ch)
			close(ch)
			_ = conn.Close()
			if len(s.atts) == 0 {
				s.startIdleLocked()
			}
		}
	}
}

// attach replays the ring and subscribes the connection to live output. The
// returned function detaches it; the caller closes the connection.
func (s *ptySession) attach(conn net.Conn) func() {
	ch := make(chan []byte, 256)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return func() {}
	}
	replay := s.ring.snapshot()
	s.atts[ch] = conn
	s.stopIdleLocked()
	s.mu.Unlock()

	_, _ = conn.Write(replay)
	go func() {
		for data := range ch {
			if _, err := conn.Write(data); err != nil {
				break
			}
		}
		_ = conn.Close()
	}()

	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.atts[ch]; ok {
			delete(s.atts, ch)
			close(ch)
		}
		if len(s.atts) == 0 {
			s.startIdleLocked()
		}
	}
}

// relay applies input and window resizes for the whole session.
func (s *ptySession) relay(ctl *controlServer, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case data := <-ctl.inputs:
			s.writeMaster(data)
		case ws := <-ctl.resizes:
			s.writeMu.Lock()
			_ = unix.IoctlSetWinsize(int(s.master.Fd()), unix.TIOCSWINSZ,
				&unix.Winsize{Row: ws.rows, Col: ws.cols})
			s.writeMu.Unlock()
		}
	}
}

func (s *ptySession) writeMaster(data []byte) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, _ = s.master.Write(data)
}

// finish waits for output to drain, then forces the reader to end.
func (s *ptySession) finish(grace time.Duration) {
	select {
	case <-s.pumpEnd:
	case <-time.After(grace):
		s.writeMu.Lock()
		_ = s.master.Close()
		s.writeMu.Unlock()
		<-s.pumpEnd
	}
}

// closeAttachments ends every live connection after the exit record exists.
func (s *ptySession) closeAttachments() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.stopIdleLocked()
	for ch, conn := range s.atts {
		delete(s.atts, ch)
		close(ch)
		_ = conn.Close()
	}
}

func (s *ptySession) startIdleLocked() {
	s.stopIdleLocked()
	if s.closed || s.idle <= 0 {
		return
	}
	s.idleGen++
	gen := s.idleGen
	s.idleTimer = time.AfterFunc(s.idle, func() { s.fireIdle(gen) })
}

func (s *ptySession) stopIdleLocked() {
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	s.idleGen++
}

func (s *ptySession) fireIdle(gen int) {
	s.mu.Lock()
	stale := gen != s.idleGen || s.closed || len(s.atts) > 0
	s.mu.Unlock()
	if !stale && s.reap != nil {
		s.reap()
	}
}

// ringBuffer keeps the last n bytes written.
type ringBuffer struct {
	buf []byte
}

func newRingBuffer(n int) *ringBuffer {
	return &ringBuffer{buf: make([]byte, 0, n)}
}

func (r *ringBuffer) write(p []byte) {
	limit := cap(r.buf)
	if len(p) > limit {
		p = p[len(p)-limit:]
	}
	if drop := len(r.buf) + len(p) - limit; drop > 0 {
		copy(r.buf, r.buf[drop:])
		r.buf = r.buf[:len(r.buf)-drop]
	}
	r.buf = append(r.buf, p...)
}

func (r *ringBuffer) snapshot() []byte {
	return append([]byte(nil), r.buf...)
}
