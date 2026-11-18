package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/generation"
	"github.com/zibyn/orrery/internal/manifest"
)

// commitRetryMin and commitRetryMax bound the backoff between commit
// attempts (defaults.md "Generation 提交重试").
var commitRetryMin, commitRetryMax = time.Second, 5 * time.Minute

// commitStalledAfter is the failure from which a stop shows commit_stalled.
const commitStalledAfter = 3

// commitTarget is the filesystem a stopped Sandbox commits (runtime.md §16):
// a container upperdir (KindDiff) or a Local working directory (KindTree),
// without the runtime mount points in exclude.
type commitTarget struct {
	root    string
	kind    string
	exclude []string
}

// errCommitFailed is a commit no retry can finish: the filesystem cannot be
// read or the Control Plane rejects what it holds. The Sandbox ends
// commit_failed and its disk stays.
var errCommitFailed = errors.New("commit_failed")

// errHolderRefused is a holder the Control Plane no longer commits for: the
// Sandbox is deleted, elsewhere, or at a newer generation.
var errHolderRefused = errors.New("the Control Plane refused the holder")

func isCommitFailed(err error) bool { return errors.Is(err, errCommitFailed) }

// statTable is the Agent-local record of a Sandbox's last commit: the head
// it produced and the stat of every file in it.
type statTable struct {
	Generation string                `json:"generation"`
	Files      map[string]statRecord `json:"files"`
}

func statTablePath(dataRoot string, id uuid.UUID) string {
	return filepath.Join(sandboxPath(dataRoot, id), "generation-stat.json")
}

// pendingTablePath holds the table of a commit sent but not acknowledged, so
// a commit whose reply was lost is not rebuilt on a stale base.
func pendingTablePath(dataRoot string, id uuid.UUID) string {
	return statTablePath(dataRoot, id) + ".pending"
}

func readStatTable(path string) (statTable, error) {
	var table statTable
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return table, nil
	}
	if err != nil {
		return table, err
	}
	if err := json.Unmarshal(data, &table); err != nil {
		slog.Warn("discard an unreadable stat table", "path", path, "error", err)
		return statTable{}, nil
	}
	return table, nil
}

func writeStatTable(path string, table statTable) error {
	data, err := json.Marshal(table)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// commitWithRetry commits the target until it succeeds, backing off and
// calling retrying from the third failed attempt. It returns nil once
// committed or once the holder is refused (nothing is left to commit to), an
// errCommitFailed, or ctx's error.
func (r *Runtime) commitWithRetry(ctx context.Context, id uuid.UUID, gen int64, target commitTarget, retrying func()) error {
	log := slog.With("sandbox_id", id)
	return retryCommit(ctx, "the Sandbox filesystem", log, func() error { return r.commitOnce(ctx, id, gen, target) }, retrying)
}

// retryCommit runs one commit attempt until it succeeds, as commitWithRetry
// describes; a Volume's commit shares it. what names what is committed in
// the logs, log carries its ids.
func retryCommit(ctx context.Context, what string, log *slog.Logger, once func() error, retrying func()) error {
	delay := commitRetryMin
	for failures := 0; ; {
		err := once()
		switch {
		case err == nil:
			return nil
		case errors.Is(err, errHolderRefused):
			log.Info("commit of "+what+" refused; nothing to commit to", "error", err)
			return nil
		case isCommitFailed(err) || ctx.Err() != nil:
			return err
		}
		failures++
		log.Warn("commit "+what, "attempt", failures, "error", err, "retry_in", delay)
		if failures >= commitStalledAfter {
			retrying()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, commitRetryMax)
	}
}

// commitOnce is one pass of node-protocol.md "Generation 提交".
func (r *Runtime) commitOnce(ctx context.Context, id uuid.UUID, gen int64, target commitTarget) error {
	if target.root == "" {
		return fmt.Errorf("%w: the containerd snapshot has no upperdir", errCommitFailed)
	}
	if r.client == nil {
		return errors.New("the Node is not connected")
	}
	tablePath, pendingPath := statTablePath(r.dataRoot, id), pendingTablePath(r.dataRoot, id)
	if err := r.settlePending(ctx, tablePath, pendingPath); err != nil {
		return err
	}
	table, err := readStatTable(tablePath)
	if err != nil {
		return err
	}
	scan, err := scanFilesystem(target.root, target.kind, target.exclude, table.Files)
	if err != nil {
		return fmt.Errorf("%w: scan %s: %w", errCommitFailed, target.root, err)
	}
	encoded, err := generation.Encode(scan.manifest)
	if err != nil {
		return fmt.Errorf("%w: %w", errCommitFailed, err)
	}
	ack, err := r.submitGeneration(ctx, id, gen, table.Generation, scan.changed)
	if err := submitOutcome(err); err != nil {
		return err
	}
	root, err := os.OpenRoot(target.root)
	if err != nil {
		return fmt.Errorf("%w: %w", errCommitFailed, err)
	}
	defer root.Close() //nolint:errcheck // read-only handle
	if err := r.uploadGeneration(ctx, root, ack, scan.objects, encoded); err != nil {
		return err
	}
	pending := statTable{Generation: ack.GetUploadId(), Files: scan.files}
	if err := writeStatTable(pendingPath, pending); err != nil {
		return err
	}
	return r.finishCommit(ctx, tablePath, pendingPath, pending.Generation)
}

// submitOutcome classifies a SubmitGeneration error: a refused holder, a
// manifest no retry can fix (errCommitFailed), or a retryable error.
func submitOutcome(err error) error {
	switch status.Code(err) {
	case codes.OK:
		return nil
	case codes.FailedPrecondition:
		return fmt.Errorf("%w: %w", errHolderRefused, err)
	case codes.InvalidArgument:
		return fmt.Errorf("%w: %w", errCommitFailed, err)
	}
	return err
}

// uploadGeneration uploads the objects the store lacks from root, then the
// manifest of the open upload.
func (r *Runtime) uploadGeneration(ctx context.Context, root *os.Root, ack *nodev1.GenerationAck, objects map[string]manifest.Source, encoded []byte) error {
	if err := r.uploadPublishObjects(ctx, root, ack.GetMissingSha256(), objects); err != nil {
		return err
	}
	return r.putGenerationManifest(ctx, ack.GetUploadId(), encoded)
}

// settlePending finishes or drops a commit an earlier attempt sent: a retried
// CommitGeneration of the head still succeeds.
func (r *Runtime) settlePending(ctx context.Context, tablePath, pendingPath string) error {
	pending, err := readStatTable(pendingPath)
	if err != nil || pending.Generation == "" {
		return err
	}
	err = r.finishCommit(ctx, tablePath, pendingPath, pending.Generation)
	if status.Code(err) == codes.FailedPrecondition || status.Code(err) == codes.InvalidArgument {
		return os.Remove(pendingPath) // never committed; this attempt starts over
	}
	return err
}

func (r *Runtime) finishCommit(ctx context.Context, tablePath, pendingPath, uploadID string) error {
	if _, err := r.client.CommitGeneration(ctx, &nodev1.CommitGenerationRequest{UploadId: uploadID}); err != nil {
		return err
	}
	return os.Rename(pendingPath, tablePath)
}

// submitGeneration streams the changed file entries and opens the upload.
func (r *Runtime) submitGeneration(ctx context.Context, id uuid.UUID, gen int64, base string, changed []generation.Entry) (*nodev1.GenerationAck, error) {
	return r.streamGeneration(ctx, &nodev1.GenerationChunk{SandboxId: id.String(), Generation: gen, BaseGenerationId: base}, changed)
}

// streamGeneration sends first, holding the holder fields, then the changed
// file entries.
func (r *Runtime) streamGeneration(ctx context.Context, first *nodev1.GenerationChunk, changed []generation.Entry) (*nodev1.GenerationAck, error) {
	stream, err := r.client.SubmitGeneration(ctx)
	if err != nil {
		return nil, err
	}
	chunk := first
	for i, e := range changed {
		if i > 0 && i%publishChunkEntries == 0 {
			if err := stream.Send(chunk); err != nil {
				return nil, err
			}
			chunk = &nodev1.GenerationChunk{}
		}
		size := *e.Size
		chunk.Entries = append(chunk.Entries, &nodev1.ManifestEntry{
			Path: e.Path, Type: e.Type, Size: &size, Sha256: e.SHA256, Blocks: e.Blocks,
		})
	}
	if err := stream.Send(chunk); err != nil {
		return nil, err
	}
	return stream.CloseAndRecv()
}

// putGenerationManifest uploads the full manifest to the upload's key. The
// URL is signed without a checksum (node-protocol.md "Presign").
func (r *Runtime) putGenerationManifest(ctx context.Context, uploadID string, data []byte) error {
	resp, err := r.presign(ctx, &nodev1.PresignRequest{Generations: []string{uploadID}, Method: "PUT"})
	if err != nil {
		return err
	}
	if len(resp.GetUrls()) != 1 {
		return fmt.Errorf("Presign returned %d URLs for one Generation", len(resp.GetUrls()))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, resp.GetUrls()[0], bytes.NewReader(data))
	if err != nil {
		return err
	}
	res, err := publishHTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close() //nolint:errcheck // drained below
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("upload Generation manifest: HTTP %d: %s", res.StatusCode, bytes.TrimSpace(body))
	}
	return nil
}
