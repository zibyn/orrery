package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/zibyn/stars-sandbox/internal/store"
)

// This file is the one home of Sandbox and Exec lifecycle rules. The SQL
// transitions stay the executor and the concurrency guard (a guarded UPDATE
// with no read-modify-write in Go); what lives here is the Go-side decision of
// what to ask for next.

// sandboxStepAction names the sweep one Sandbox needs this tick.
type sandboxStepAction int

const (
	// sandboxStepNone means desired and current state already agree, or a
	// transition owned by another sweep is in progress.
	sandboxStepNone sandboxStepAction = iota
	sandboxStepPending
	sandboxStepCreate
	sandboxStepStop
	sandboxStepDelete
)

// sandboxStep is the desired×state routing of the Reconciler. A running
// Sandbox only acts before its Runtime is up; stopping and deleting are
// idempotent sweeps that decide by the row they read.
func sandboxStep(state SandboxState, desired SandboxDesiredState) sandboxStepAction {
	switch desired {
	case SandboxDesiredRunning:
		switch state {
		case SandboxPending:
			return sandboxStepPending
		case SandboxProvisioning:
			return sandboxStepCreate
		}
	case SandboxDesiredStopped:
		return sandboxStepStop
	case SandboxDesiredDeleted:
		return sandboxStepDelete
	}
	return sandboxStepNone
}

// execFacts are the durable row facts the Exec ordering rules read.
type execFacts struct {
	sandboxState    SandboxState
	desired         SandboxDesiredState
	cancelRequested bool
	started         bool
}

// execStepAction names what the Exec sweep does with one active Exec.
type execStepAction int

const (
	execWait execStepAction = iota
	execRuntimeLost
	execFinishCanceled
	execSendCancel
	execCancelSandbox
	execStart
)

// execStep is the ordering rule of the Exec sweep. A Sandbox whose Runtime is
// gone loses its Execs before anything else. Cancellation is a persistent
// intent, so it waits while the Agent is away (runtime.md §5); an Exec whose
// start never reached the Supervisor has nothing to signal and finishes
// canceled. A stop or delete turns active Execs into cancel intents before the
// Runtime goes.
func execStep(facts execFacts, connected, delivered bool) execStepAction {
	switch {
	case facts.sandboxState == SandboxStopped || facts.sandboxState == SandboxDeleted || facts.sandboxState == SandboxFailed:
		return execRuntimeLost
	case facts.cancelRequested:
		if !connected {
			return execWait
		}
		if !facts.started && !delivered {
			return execFinishCanceled
		}
		return execSendCancel
	case facts.desired != SandboxDesiredRunning:
		return execCancelSandbox
	case facts.sandboxState != SandboxReady:
		return execWait
	default:
		return execStart
	}
}

// goneAction is what follows releasing a gone Runtime's resources.
type goneAction int

const (
	goneReleaseOnly goneAction = iota
	goneWorkspaceDeleting
	goneSandboxDeleted
)

// finalizeAction is the pure part of tearing a Sandbox down once its Runtime
// is gone. A failed Sandbox, or one the caller has not asked to delete, only
// releases its Runtime. An ephemeral Workspace is deleted with its Sandbox; a
// persistent one outlives it. A nil workspace means the Workspace row is
// already gone, so the Sandbox finishes alone.
func finalizeAction(state SandboxState, desired SandboxDesiredState, workspace *store.Workspace) goneAction {
	if state == SandboxFailed || desired != SandboxDesiredDeleted {
		return goneReleaseOnly
	}
	if workspace != nil && workspace.Kind == "ephemeral" {
		return goneWorkspaceDeleting
	}
	return goneSandboxDeleted
}

// finalizeGone is the one teardown of a Sandbox whose Runtime is gone. It
// releases the Runtime and then finishes the deletion the desired state asks
// for. Every step is idempotent, so a repeated observation converges; the
// caller owns the transaction, the wake, and the proof that the Runtime is
// really gone.
func finalizeGone(ctx context.Context, q *store.Queries, sandboxID uuid.UUID) error {
	sb, err := q.GetSandbox(ctx, sandboxID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // already deleted
	}
	if err != nil {
		return err
	}
	// Release before the Workspace lookup: a transient read must not leave the
	// compute allocated.
	if err := releaseRuntime(ctx, q, sandboxID); err != nil {
		return err
	}
	// A deleted or missing Workspace row means the Sandbox finishes alone.
	ws, err := q.GetSandboxWorkspace(ctx, sandboxID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		// Do not orphan an implicit Workspace; the next sweep retries.
		return err
	}
	var workspace *store.Workspace
	if err == nil {
		workspace = &ws
	}
	switch finalizeAction(SandboxState(sb.State), SandboxDesiredState(sb.DesiredState), workspace) {
	case goneWorkspaceDeleting:
		_, err := q.SetWorkspaceDeleting(ctx, ws.ID)
		return err
	case goneSandboxDeleted:
		_, err := q.MarkSandboxDeleted(ctx, sandboxID)
		return err
	}
	return nil
}

// releaseRuntime drops the Allocation, the GPU claims and the Attachment,
// which exist only while a runtime does. Both are attempted so one failure
// does not hide the other.
func releaseRuntime(ctx context.Context, q *store.Queries, sandboxID uuid.UUID) error {
	var errs []error
	if err := q.ReleaseCompute(ctx, sandboxID); err != nil {
		errs = append(errs, fmt.Errorf("release allocation: %w", err))
	}
	if err := q.DeleteAttachment(ctx, sandboxID); err != nil {
		errs = append(errs, fmt.Errorf("release attachment: %w", err))
	}
	return errors.Join(errs...)
}

// ExecCause is the platform cause of an Exec's terminal result (api.md §4).
// The Agent reports its own causes on the wire; these are the closed set the
// Control Plane stores and serves.
type ExecCause string

const (
	ExecExited      ExecCause = "exited"
	ExecCanceled    ExecCause = "canceled"
	ExecTimedOut    ExecCause = "timed_out"
	ExecOOMKilled   ExecCause = "oom_killed"
	ExecRuntimeLost ExecCause = "runtime_lost"
	ExecNodeLost    ExecCause = "node_lost"
	ExecPTYIdle     ExecCause = "pty_idle"
	ExecStartFailed ExecCause = "start_failed"
)

// execCauseNames is the wire enum in lifecycle order, used by the OpenAPI
// document.
var execCauseNames = []string{
	string(ExecExited), string(ExecCanceled), string(ExecTimedOut),
	string(ExecOOMKilled), string(ExecRuntimeLost), string(ExecNodeLost),
	string(ExecPTYIdle), string(ExecStartFailed),
}

// finishExec records an Exec's terminal result. The Control Plane passes a
// cause alone; the Supervisor's exit code and signal come through Output.
// A second finish is a no-op at the database.
func finishExec(ctx context.Context, q *store.Queries, execID uuid.UUID, cause ExecCause, exitCode, signal *int32) error {
	c := string(cause)
	_, err := q.FinishExec(ctx, store.FinishExecParams{
		ID: execID, Cause: &c, ExitCode: exitCode, Signal: signal,
	})
	return err
}

// finishSandboxExecs ends every unfinished Exec of a Sandbox up to a
// generation; the Runtime that could still produce their output is gone.
func finishSandboxExecs(ctx context.Context, q *store.Queries, sandboxID uuid.UUID, cause ExecCause, generation int64) error {
	c := string(cause)
	_, err := q.FinishSandboxExecs(ctx, store.FinishSandboxExecsParams{
		SandboxID: sandboxID, Cause: &c, Generation: generation,
	})
	return err
}
