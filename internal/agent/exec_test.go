package agent

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/metadata"

	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
	"github.com/zibyn/stars-sandbox/internal/supervisor"
)

// fakeExecOutput is an ExecOutput stream that acknowledges every batch.
type fakeExecOutput struct {
	ctx     context.Context
	batches []*nodev1.OutputBatch
	exited  chan struct{}
}

func (f *fakeExecOutput) Send(batch *nodev1.OutputBatch) error {
	f.batches = append(f.batches, batch)
	if len(batch.Frames) > 0 && batch.Frames[0].Stream == "exit" {
		close(f.exited)
	}
	return nil
}

func (f *fakeExecOutput) Recv() (*nodev1.OutputAck, error) {
	ack := &nodev1.OutputAck{}
	if len(f.batches) > 0 {
		frames := f.batches[len(f.batches)-1].Frames
		if len(frames) > 0 {
			last := frames[len(frames)-1]
			ack.StoredSeq = last.Seq + supervisor.HeaderSize + int64(len(last.Data))
		}
	}
	return ack, nil
}
func (f *fakeExecOutput) Header() (metadata.MD, error) { return nil, nil }
func (f *fakeExecOutput) Trailer() metadata.MD         { return nil }
func (f *fakeExecOutput) CloseSend() error             { return nil }
func (f *fakeExecOutput) Context() context.Context     { return f.ctx }
func (f *fakeExecOutput) SendMsg(any) error            { return nil }
func (f *fakeExecOutput) RecvMsg(any) error            { return nil }

// TestStartExecReplaysFinishedRecord is the reconnect case: the Supervisor
// finished while the control stream was down, so StartExec is redelivered
// against a record whose ctl.sock is already gone.
func TestStartExecReplaysFinishedRecord(t *testing.T) {
	root := t.TempDir()
	sandboxID, execID := uuid.New(), uuid.New()
	dir := filepath.Join(sandboxRunPath(root, sandboxID), execID.String())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	code := int32(7)
	raw, err := json.Marshal(supervisor.Exit{Cause: "exited", ExitCode: &code, FinishedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "exit"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	stream := &fakeExecOutput{ctx: context.Background(), exited: make(chan struct{})}
	r := &Runtime{
		dataRoot: root,
		execs:    map[string]struct{}{},
		execStream: func(context.Context) (nodev1.Node_ExecOutputClient, error) {
			return stream, nil
		},
	}
	if err := r.startExec(context.Background(), &nodev1.StartExec{
		SandboxId: sandboxID.String(), ExecId: execID.String(),
	}); err != nil {
		t.Fatalf("startExec replay: %v", err)
	}

	select {
	case <-stream.exited:
	case <-time.After(2 * time.Second):
		t.Fatal("the exit frame was not delivered")
	}
	frame := stream.batches[len(stream.batches)-1].Frames[0]
	if frame.Stream != "exit" || frame.Cause != "exited" || frame.GetExitCode() != code {
		t.Fatalf("exit frame = %+v", frame)
	}
	// The acknowledged record is deleted.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("record was not removed after the exit was acknowledged")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newTestRuntime(root string, stream *fakeExecOutput) *Runtime {
	return &Runtime{
		dataRoot: root,
		execs:    map[string]struct{}{},
		execStream: func(context.Context) (nodev1.Node_ExecOutputClient, error) {
			return stream, nil
		},
	}
}

func writeFrame(t *testing.T, dir string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, "output"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	header := make([]byte, supervisor.HeaderSize)
	header[0] = supervisor.FrameStdout
	binary.BigEndian.PutUint32(header[1:], uint32(len(data)))
	if _, err := f.Write(append(header, data...)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReconcileRecordSynthesizesRuntimeLost is runtime.md §8: the Supervisor
// died while the Runtime lives, so no exit was written and its socket is gone.
// The Agent writes runtime_lost and delivers the frames it still holds.
func TestReconcileRecordSynthesizesRuntimeLost(t *testing.T) {
	root := t.TempDir()
	sandboxID, execID := uuid.New(), uuid.New()
	dir := filepath.Join(sandboxRunPath(root, sandboxID), execID.String())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFrame(t, dir, []byte("partial"))

	stream := &fakeExecOutput{ctx: context.Background(), exited: make(chan struct{})}
	r := newTestRuntime(root, stream)
	r.reconcileRecord(context.Background(), sandboxID, execID, dir, true)

	select {
	case <-stream.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the synthesized exit was not delivered")
	}
	if frame := stream.batches[len(stream.batches)-1].Frames[0]; frame.Cause != "runtime_lost" {
		t.Fatalf("exit cause = %q, want runtime_lost", frame.Cause)
	}
	var delivered string
	for _, batch := range stream.batches {
		for _, frame := range batch.Frames {
			if frame.Stream == "stdout" {
				delivered += string(frame.Data)
			}
		}
	}
	if delivered != "partial" {
		t.Fatalf("delivered output = %q, want partial", delivered)
	}
}

// TestReconcileRecordLeavesLiveSupervisorAlone is the other §8 branch: a
// connectable ctl.sock means the Exec is still running.
func TestReconcileRecordLeavesLiveSupervisorAlone(t *testing.T) {
	// A unix socket path is capped at 108 bytes, and t.TempDir() names are long.
	root, err := os.MkdirTemp("", "rec-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	sandboxID, execID := uuid.New(), uuid.New()
	dir := filepath.Join(root, "run", "exec-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "ctl.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	stream := &fakeExecOutput{ctx: context.Background(), exited: make(chan struct{})}
	r := newTestRuntime(root, stream)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.reconcileRecord(ctx, sandboxID, execID, dir, true)
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	if _, err := os.Stat(filepath.Join(dir, "exit")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a live Supervisor's Exec was ended (stat err = %v)", err)
	}
}

// TestReconcileRecordDoesNotSettleOnCancel guards the shutdown case: a
// canceled context is not evidence the Supervisor died, so nothing may be
// written into a live Exec's record.
func TestReconcileRecordDoesNotSettleOnCancel(t *testing.T) {
	root := t.TempDir()
	sandboxID, execID := uuid.New(), uuid.New()
	dir := filepath.Join(sandboxRunPath(root, sandboxID), execID.String())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := newTestRuntime(root, nil)
	r.reconcileRecord(ctx, sandboxID, execID, dir, true)
	if _, err := os.Stat(filepath.Join(dir, "exit")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a canceled connection ended the Exec (stat err = %v)", err)
	}
}

// TestFinalizeLostEndsUnfinishedExecs is the Runtime-gone row of §8.
func TestFinalizeLostEndsUnfinishedExecs(t *testing.T) {
	root := t.TempDir()
	sandboxID := uuid.New()
	runDir := sandboxRunPath(root, sandboxID)
	finished, unfinished := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{finished, unfinished} {
		if err := os.MkdirAll(filepath.Join(runDir, id.String()), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := supervisor.WriteExit(filepath.Join(runDir, finished.String()), supervisor.Exit{Cause: "exited"}); err != nil {
		t.Fatal(err)
	}

	r := newTestRuntime(root, nil)
	r.finalizeLost(sandboxID.String())

	var exit supervisor.Exit
	raw, err := os.ReadFile(filepath.Join(runDir, unfinished.String(), "exit"))
	if err != nil {
		t.Fatalf("unfinished Exec has no exit: %v", err)
	}
	if err := json.Unmarshal(raw, &exit); err != nil {
		t.Fatal(err)
	}
	if exit.Cause != "runtime_lost" {
		t.Fatalf("exit cause = %q, want runtime_lost", exit.Cause)
	}
	raw, err = os.ReadFile(filepath.Join(runDir, finished.String(), "exit"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &exit); err != nil {
		t.Fatal(err)
	}
	if exit.Cause != "exited" {
		t.Fatalf("an existing exit was overwritten: %+v", exit)
	}
}
