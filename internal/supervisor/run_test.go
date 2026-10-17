package supervisor

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFrameWriterSplitsUTF8AndEnforcesCap(t *testing.T) {
	dir := t.TempDir()
	frames, err := newFrameWriter(filepath.Join(dir, "output"), 0)
	if err != nil {
		t.Fatal(err)
	}
	w := &utf8Writer{frames: frames, stream: FrameStdout}
	_, _ = w.Write([]byte{0xC3})       // the first half of "é"
	_, _ = w.Write([]byte{0xA9, 0xFF}) // second half, then an invalid byte
	w.finish()
	frames.Close()

	got := readFrames(t, filepath.Join(dir, "output"))
	want := []frame{{FrameStdout, "é"}, {FrameStdout, string([]byte{0xFF})}}
	if len(got) != len(want) {
		t.Fatalf("frames = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].stream != want[i].stream || got[i].data != want[i].data {
			t.Fatalf("frame %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	capped, err := newFrameWriter(filepath.Join(dir, "capped"), 5+4)
	if err != nil {
		t.Fatal(err)
	}
	c := &utf8Writer{frames: capped, stream: FrameStdout}
	_, _ = c.Write([]byte("abcd"))
	_, _ = c.Write([]byte("efgh"))
	capped.Close()
	got = readFrames(t, filepath.Join(dir, "capped"))
	if len(got) != 2 || got[0].data != "abcd" || got[1].stream != FrameTruncated {
		t.Fatalf("capped frames = %+v", got)
	}
}

func TestRunStreamsOutputAndExit(t *testing.T) {
	root := t.TempDir()
	done := make(chan error, 1)
	go func() { done <- RunIn(root, "exec-1") }()

	conn := dialControl(t, filepath.Join(root, "exec-1", "ctl.sock"))
	spec, err := json.Marshal(Spec{
		Argv: []string{"sh", "-c", "printf 'h\\303\\251llo'; printf 'oops' >&2; exit 7"},
		Cwd:  root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(append(spec, '\n')); err != nil {
		t.Fatal(err)
	}
	conn.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunIn: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Supervisor did not finish")
	}

	frames := readFrames(t, filepath.Join(root, "exec-1", "output"))
	var stdout, stderr string
	for _, f := range frames {
		switch f.stream {
		case FrameStdout:
			stdout += f.data
		case FrameStderr:
			stderr += f.data
		}
	}
	if stdout != "héllo" || stderr != "oops" {
		t.Fatalf("stdout = %q, stderr = %q", stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "exec-1", "started")); err != nil {
		t.Fatalf("started file: %v", err)
	}
	var exit Exit
	raw, err := os.ReadFile(filepath.Join(root, "exec-1", "exit"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &exit); err != nil {
		t.Fatal(err)
	}
	if exit.Cause != "exited" || exit.ExitCode == nil || *exit.ExitCode != 7 {
		t.Fatalf("exit = %+v", exit)
	}

	// A second run for the same exec id is a no-op (the atomic mkdir dedupe).
	if err := RunIn(root, "exec-1"); err != nil {
		t.Fatalf("duplicate run: %v", err)
	}
}

func TestRunCancel(t *testing.T) {
	root := t.TempDir()
	done := make(chan error, 1)
	go func() { done <- RunIn(root, "exec-2") }()

	conn := dialControl(t, filepath.Join(root, "exec-2", "ctl.sock"))
	spec, _ := json.Marshal(Spec{Argv: []string{"sleep", "30"}, Cwd: root})
	if _, err := conn.Write(append(spec, '\n')); err != nil {
		t.Fatal(err)
	}
	// Wait for execve, then cancel on the same connection.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "exec-2", "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("command never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := conn.Write([]byte("cancel\n")); err != nil {
		t.Fatal(err)
	}
	conn.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not end the command")
	}
	var exit Exit
	raw, err := os.ReadFile(filepath.Join(root, "exec-2", "exit"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &exit); err != nil {
		t.Fatal(err)
	}
	if exit.Cause != "canceled" {
		t.Fatalf("exit = %+v, want canceled", exit)
	}
}

type frame struct {
	stream byte
	data   string
}

func readFrames(t *testing.T, path string) []frame {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var frames []frame
	for {
		stream, data, err := ReadFrame(f)
		if err != nil {
			return frames
		}
		frames = append(frames, frame{stream, string(data)})
	}
}

func dialControl(t *testing.T, path string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.Dial("unix", path)
		if err == nil {
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial ctl.sock: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
