package supervisor

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPTYReplaysInputResizeAndExit(t *testing.T) {
	root := t.TempDir()
	done := make(chan error, 1)
	go func() { done <- RunIn(root, "pty-1") }()

	ctl := dialControl(t, filepath.Join(root, "pty-1", "ctl.sock"))
	spec, _ := json.Marshal(Spec{
		Argv: []string{"sh", "-c", "stty size; read a; stty size; read b; stty size; exit 3"},
		Cwd:  root,
		Pty:  &PTY{Rows: 24, Cols: 80},
	})
	if _, err := ctl.Write(append(spec, '\n')); err != nil {
		t.Fatal(err)
	}
	ctl.Close()

	att := dialControl(t, filepath.Join(root, "pty-1", "ctl.sock"))
	defer att.Close()
	if _, err := att.Write([]byte("attach\n")); err != nil {
		t.Fatal(err)
	}
	output := &ptyReader{Conn: att}
	expect(t, output, "24 80")
	output.buf = nil

	// The script runs `stty size` before each read, so each input line names
	// the window size in effect at that moment.
	_, _ = att.Write([]byte("input 4\none\n"))
	expect(t, output, "24 80") // still the initial size; the split before the resize
	output.buf = nil
	_, _ = att.Write([]byte("resize 40 100\n"))
	time.Sleep(50 * time.Millisecond) // let the relay apply the resize
	_, _ = att.Write([]byte("input 4\ntwo\n"))
	expect(t, output, "40 100")
	if !strings.Contains(string(output.buf), "two") {
		t.Fatalf("input was not echoed: %q", output.buf)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunIn: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Supervisor did not finish")
	}
	var exit Exit
	raw, err := os.ReadFile(filepath.Join(root, "pty-1", "exit"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &exit); err != nil {
		t.Fatal(err)
	}
	if exit.Cause != "exited" || exit.ExitCode == nil || *exit.ExitCode != 3 {
		t.Fatalf("exit = %+v", exit)
	}
	if _, err := os.Stat(filepath.Join(root, "pty-1", "output")); !os.IsNotExist(err) {
		t.Fatalf("PTY Exec must not write an output record: %v", err)
	}
}

func TestPTYIdleReapsOnlyWithoutAttachments(t *testing.T) {
	root := t.TempDir()
	done := make(chan error, 1)
	go func() { done <- RunIn(root, "pty-2") }()

	ctl := dialControl(t, filepath.Join(root, "pty-2", "ctl.sock"))
	spec, _ := json.Marshal(Spec{
		Argv:           []string{"sh", "-c", "sleep 30"},
		Cwd:            root,
		Pty:            &PTY{Rows: 24, Cols: 80},
		PtyIdleSeconds: 1,
	})
	if _, err := ctl.Write(append(spec, '\n')); err != nil {
		t.Fatal(err)
	}
	ctl.Close()

	att := dialControl(t, filepath.Join(root, "pty-2", "ctl.sock"))
	if _, err := att.Write([]byte("attach\n")); err != nil {
		t.Fatal(err)
	}
	// An attachment keeps the session alive past the idle window.
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(root, "pty-2", "exit")); err == nil {
		t.Fatal("a live attachment was reaped")
	}
	att.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("idle session was not reaped")
	}
	var exit Exit
	raw, err := os.ReadFile(filepath.Join(root, "pty-2", "exit"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &exit); err != nil {
		t.Fatal(err)
	}
	if exit.Cause != "pty_idle" {
		t.Fatalf("exit = %+v, want pty_idle", exit)
	}
}

func TestRingBufferKeepsTheTail(t *testing.T) {
	ring := newRingBuffer(8)
	ring.write([]byte("abcdef"))
	ring.write([]byte("ghij"))
	if got := string(ring.snapshot()); got != "cdefghij" {
		t.Fatalf("snapshot = %q", got)
	}
	ring.write([]byte("0123456789"))
	if got := string(ring.snapshot()); got != "23456789" {
		t.Fatalf("snapshot = %q", got)
	}
}

type ptyReader struct {
	net.Conn
	buf []byte
}

// expect reads until want appears, replaying everything seen so far.
func expect(t *testing.T, conn *ptyReader, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(string(conn.buf), want) {
		if time.Now().After(deadline) {
			t.Fatalf("never saw %q; output = %q", want, conn.buf)
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		conn.buf = append(conn.buf, buf[:n]...)
		if err != nil && !os.IsTimeout(err) {
			t.Fatalf("read: %v; output = %q", err, conn.buf)
		}
	}
}
