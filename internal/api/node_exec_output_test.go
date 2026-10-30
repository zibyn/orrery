package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/store"
	"github.com/zibyn/orrery/internal/supervisor"
)

// testExecDB connects to ORRERY_TEST_DATABASE_URL, migrates it, and seeds one
// Exec. The test is skipped when no database is configured.
func testExecDB(t *testing.T) (*nodeService, uuid.UUID) {
	t.Helper()
	dsn := os.Getenv("ORRERY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORRERY_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := Migrate(ctx, pool, dsn); err != nil {
		t.Fatal(err)
	}
	userID, sandboxID, execID := uuid.New(), uuid.New(), uuid.New()
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, name, login_type) VALUES ($1, $2, 'none')`, []any{userID, "u-" + userID.String()}},
		{`INSERT INTO sandboxes (id, owner_id, name, snapshot_digest, cpu, memory) VALUES ($1, $2, 'sb', 'sha256:x', 1, 1)`, []any{sandboxID, userID}},
		{`INSERT INTO execs (id, sandbox_id, argv, generation) VALUES ($1, $2, '["true"]', 1)`, []any{execID, sandboxID}},
	} {
		if _, err := pool.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	s := &Server{pool: pool, q: store.New(pool), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return &nodeService{s: s}, execID
}

// frames builds n consecutive stdout frames starting at seq from, with data
// "line-<label>" for label = start, start+1, ...
func frames(from int64, start, n int) []*nodev1.OutputFrame {
	out := make([]*nodev1.OutputFrame, 0, n)
	seq := from
	for i := start; i < start+n; i++ {
		data := []byte(fmt.Sprintf("line-%d", i))
		out = append(out, &nodev1.OutputFrame{Seq: seq, Stream: "stdout", Data: data})
		seq += supervisor.HeaderSize + int64(len(data))
	}
	return out
}

// readAll pages the stored output from seq 0, as the output endpoint does.
func readAll(t *testing.T, n *nodeService, execID uuid.UUID) []logFrame {
	t.Helper()
	var all []logFrame
	after := int64(0)
	for {
		page, err := n.s.execFrames(context.Background(), execID, after)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			return all
		}
		all = append(all, page...)
		after = page[len(page)-1].end()
	}
}

func wantLines(t *testing.T, got []logFrame, written []*nodev1.OutputFrame) {
	t.Helper()
	if len(got) != len(written) {
		t.Fatalf("stored %d frames, want %d: %+v", len(got), len(written), got)
	}
	for i, f := range written {
		if got[i].Seq != f.Seq || got[i].Data != string(f.Data) {
			t.Fatalf("frame %d = {%d %q}, want {%d %q}", i, got[i].Seq, got[i].Data, f.Seq, f.Data)
		}
	}
}

// #157: a chunk stored at stored_seq whose advance never committed must not
// swallow the longer batch the restarted Agent resends from the same seq.
func TestExecOutputResendOverUnadvancedChunk(t *testing.T) {
	n, execID := testExecDB(t)
	ctx := context.Background()
	all := frames(0, 0, 12)
	orphan := all[:8]
	var lf []logFrame
	for _, f := range orphan {
		lf = append(lf, logFrameFromProto(f.Seq, f.Stream, f.Data))
	}
	raw, err := json.Marshal(lf)
	if err != nil {
		t.Fatal(err)
	}
	last := all[8].Seq
	if err := n.s.q.AppendExecChunk(ctx, store.AppendExecChunkParams{
		ExecID: execID, FirstSeq: 0, LastSeq: last, Frames: raw,
	}); err != nil {
		t.Fatal(err)
	}

	stored, err := n.storeExecBatch(ctx, execID, all)
	if err != nil {
		t.Fatal(err)
	}
	end := all[11].Seq + supervisor.HeaderSize + int64(len(all[11].Data))
	if stored != end {
		t.Fatalf("stored_seq = %d, want %d", stored, end)
	}
	wantLines(t, readAll(t, n, execID), all)
}

// #157: when stored_seq cannot advance, the batch's chunk is not left behind.
func TestExecOutputFailedAdvanceLeavesNoChunk(t *testing.T) {
	n, execID := testExecDB(t)
	ctx := context.Background()
	fn := "fail_advance_" + strings.ReplaceAll(execID.String(), "-", "")
	for _, stmt := range []string{
		fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF NEW.id = '%s' AND NEW.stored_seq <> OLD.stored_seq THEN
					RAISE EXCEPTION 'advance refused';
				END IF;
				RETURN NEW;
			END $$`, fn, execID),
		fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE ON execs FOR EACH ROW EXECUTE FUNCTION %s()`, fn, fn),
	} {
		if _, err := n.s.pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = n.s.pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER %s ON execs; DROP FUNCTION %s()`, fn, fn))
	})

	if _, err := n.storeExecBatch(ctx, execID, frames(0, 0, 4)); err == nil {
		t.Fatal("storeExecBatch succeeded, want the advance failure")
	}
	if got := readAll(t, n, execID); len(got) != 0 {
		t.Fatalf("chunk left behind without stored_seq: %+v", got)
	}
}

// Consecutive batches, with a resend overlapping what is already stored, read
// back byte for byte.
func TestExecOutputBatchesReadBackExactly(t *testing.T) {
	n, execID := testExecDB(t)
	ctx := context.Background()
	all := frames(0, 0, 10)
	for _, batch := range [][]*nodev1.OutputFrame{all[:3], all[1:6], all[6:10]} {
		if _, err := n.storeExecBatch(ctx, execID, batch); err != nil {
			t.Fatal(err)
		}
	}
	wantLines(t, readAll(t, n, execID), all)

	if _, err := n.storeExecBatch(ctx, execID, frames(all[9].Seq+100, 20, 1)); err == nil {
		t.Fatal("a gap was accepted, want out-of-order rejection")
	}
}
