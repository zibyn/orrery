package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/store"
	"github.com/zibyn/orrery/internal/supervisor"
)

// ExecOutput is one stream per Exec: frames in, acknowledgements out. The
// Agent resumes from stored_seq, so a lost batch is replayed, never duplicated.
func (n *nodeService) ExecOutput(stream nodev1.Node_ExecOutputServer) error {
	ctx := stream.Context()
	node, err := n.s.authenticateNode(ctx)
	if err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return status.Error(codes.InvalidArgument, "the first message must name the Exec")
	}
	execID, err := uuid.Parse(first.GetExecId())
	if err != nil {
		return status.Error(codes.InvalidArgument, "exec_id must be a UUID")
	}
	row, err := n.s.q.GetExecWithNode(ctx, execID)
	if errors.Is(err, pgx.ErrNoRows) {
		return status.Error(codes.NotFound, "unknown Exec")
	}
	if err != nil {
		n.s.log.Error("look up Exec for output", "exec_id", execID, "error", err)
		return status.Error(codes.Internal, "look up Exec")
	}
	if row.SandboxNodeID == nil || *row.SandboxNodeID != node.ID {
		return status.Error(codes.PermissionDenied, "the Exec does not belong to this Node")
	}
	if err := stream.Send(&nodev1.OutputAck{ExecId: execID.String(), StoredSeq: row.Exec.StoredSeq}); err != nil {
		return err
	}

	for {
		batch, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if batch.GetExecId() != execID.String() {
			return status.Error(codes.InvalidArgument, "exec_id changed mid-stream")
		}
		data := make([]*nodev1.OutputFrame, 0, len(batch.Frames))
		var exit *nodev1.OutputFrame
		for _, f := range batch.Frames {
			switch f.Stream {
			case "exit":
				exit = f
			case "stdout", "stderr", "truncated":
				data = append(data, f)
			default:
				return status.Errorf(codes.InvalidArgument, "unknown output stream %q", f.Stream)
			}
		}
		stored, err := n.storeExecBatch(ctx, execID, data)
		if err != nil {
			return err
		}
		if exit != nil {
			cause := ExecCause(exit.Cause)
			if cause == "" {
				cause = ExecExited
			}
			if err := finishExec(ctx, n.s.q, execID, cause, exit.ExitCode, exit.Signal); err != nil {
				n.s.log.Error("finish Exec", "exec_id", execID, "error", err)
				return status.Error(codes.Internal, "store result")
			}
			if err := n.s.q.AdvanceExecStoredSeq(ctx, store.AdvanceExecStoredSeqParams{
				ID: execID, StoredSeq: exit.Seq, // log_truncated was set with the frames
			}); err != nil {
				n.s.log.Error("advance Exec stored_seq", "exec_id", execID, "error", err)
				return status.Error(codes.Internal, "store result")
			}
			n.s.reconcile.notify()
			return stream.Send(&nodev1.OutputAck{ExecId: execID.String(), StoredSeq: exit.Seq})
		}
		if err := stream.Send(&nodev1.OutputAck{ExecId: execID.String(), StoredSeq: stored}); err != nil {
			return err
		}
	}
}

// storeExecBatch writes a batch's frames and advances stored_seq in one
// transaction, filtering against the stored_seq read under the Exec's row
// lock, so a resend or a concurrent stream never loses or duplicates a frame.
// It returns the committed stored_seq.
func (n *nodeService) storeExecBatch(ctx context.Context, execID uuid.UUID, batch []*nodev1.OutputFrame) (int64, error) {
	tx, err := n.s.pool.Begin(ctx)
	if err != nil {
		n.s.log.Error("begin Exec output", "exec_id", execID, "error", err)
		return 0, status.Error(codes.Internal, "store output")
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := n.s.q.WithTx(tx)
	stored, err := q.LockExecStoredSeq(ctx, execID)
	if err != nil {
		n.s.log.Error("lock Exec stored_seq", "exec_id", execID, "error", err)
		return 0, status.Error(codes.Internal, "store output")
	}
	frames := make([]logFrame, 0, len(batch))
	truncated := false
	for _, f := range batch {
		if f.Seq < stored {
			continue // already stored; the ack was lost
		}
		if f.Seq != stored {
			return 0, status.Errorf(codes.InvalidArgument, "out-of-order frame: got %d, want %d", f.Seq, stored)
		}
		truncated = truncated || f.Stream == "truncated"
		frames = append(frames, logFrameFromProto(f.Seq, f.Stream, f.Data))
		stored = f.Seq + supervisor.HeaderSize + int64(len(f.Data))
	}
	if len(frames) == 0 {
		return stored, nil
	}
	raw, err := json.Marshal(frames)
	if err != nil {
		return 0, status.Error(codes.Internal, "encode output")
	}
	if err := q.AppendExecChunk(ctx, store.AppendExecChunkParams{
		ExecID: execID, FirstSeq: frames[0].Seq, LastSeq: stored, Frames: raw,
	}); err != nil {
		n.s.log.Error("append Exec output", "exec_id", execID, "error", err)
		return 0, status.Error(codes.Internal, "store output")
	}
	if err := q.AdvanceExecStoredSeq(ctx, store.AdvanceExecStoredSeqParams{
		ID: execID, StoredSeq: stored, LogTruncated: truncated,
	}); err != nil {
		n.s.log.Error("advance Exec stored_seq", "exec_id", execID, "error", err)
		return 0, status.Error(codes.Internal, "store output")
	}
	if err := tx.Commit(ctx); err != nil {
		n.s.log.Error("commit Exec output", "exec_id", execID, "error", err)
		return 0, status.Error(codes.Internal, "store output")
	}
	return stored, nil
}
