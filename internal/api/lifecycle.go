package api

import (
	"context"
	"errors"

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
	state           SandboxState
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
	case facts.state == SandboxStopped || facts.state == SandboxDeleted || facts.state == SandboxFailed:
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
	case facts.state != SandboxReady:
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
// caller owns the transaction and the wake.
func finalizeGone(ctx context.Context, q *store.Queries, sandboxID uuid.UUID) error {
	sb, err := q.GetSandbox(ctx, sandboxID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // already deleted
	}
	if err != nil {
		return err
	}
	// A deleted or missing Workspace row means the Sandbox finishes alone. A
	// transient read error must not orphan an implicit Workspace.
	ws, err := q.GetSandboxWorkspace(ctx, sandboxID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var workspace *store.Workspace
	if err == nil {
		workspace = &ws
	}
	if err := releaseCompute(ctx, q, sandboxID); err != nil {
		return err
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
