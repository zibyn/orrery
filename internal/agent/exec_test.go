package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
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

func (f *fakeExecOutput) Recv() (*nodev1.OutputAck, error) { return &nodev1.OutputAck{}, nil }
func (f *fakeExecOutput) Header() (metadata.MD, error)     { return nil, nil }
func (f *fakeExecOutput) Trailer() metadata.MD             { return nil }
func (f *fakeExecOutput) CloseSend() error                 { return nil }
func (f *fakeExecOutput) Context() context.Context         { return f.ctx }
func (f *fakeExecOutput) SendMsg(any) error                { return nil }
func (f *fakeExecOutput) RecvMsg(any) error                { return nil }

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
