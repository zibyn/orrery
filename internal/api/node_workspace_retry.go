package api

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/zibyn/stars-sandbox/internal/store"
)

// recordWorkspaceCommandFailure advances a Workspace's provisioning retry
// state. Backoff and the stalled condition follow defaults.md; once the
// retries are used up the Workspace ends failed (ADR-0015).
// ponytail: only a reported CommandResult counts as a failed attempt. An Agent
// that accepts the command and never reports leaves the Workspace
// provisioning; add a per-attempt timeout if a silently hung Agent matters.
func (n *nodeService) recordWorkspaceCommandFailure(ctx context.Context, commandID string) {
	const prefix = "provision-workspace:"
	if !strings.HasPrefix(commandID, prefix) {
		return
	}
	idPart, _, ok := strings.Cut(strings.TrimPrefix(commandID, prefix), ":")
	if !ok {
		return
	}
	id, err := uuid.Parse(idPart)
	if err != nil {
		return
	}
	ws, err := n.s.q.GetWorkspace(ctx, id)
	if err != nil || WorkspaceState(ws.State) != WorkspaceProvisioning {
		return
	}
	conditions := parseWorkspaceConditions(ws.Conditions)
	conditions.Attempts++
	if conditions.Attempts > len(workspaceProvisionBackoff) {
		cause := "provisioning_failed"
		if _, err := n.s.q.MarkWorkspaceFailed(ctx, store.MarkWorkspaceFailedParams{
			ID: id, Generation: ws.Generation, Cause: &cause,
		}); err != nil {
			n.s.log.Warn("record Workspace failure", "workspace_id", id, "error", err)
		}
		return
	}
	next := time.Now().Add(workspaceProvisionBackoff[conditions.Attempts-1])
	conditions.NextAttemptAt = &next
	if conditions.Attempts >= workspaceStalledAfter {
		conditions.ProvisioningStalled = true
	}
	if err := n.s.q.SetWorkspaceConditions(ctx, store.SetWorkspaceConditionsParams{
		ID: id, Conditions: encodeWorkspaceConditions(conditions),
	}); err != nil {
		n.s.log.Warn("record Workspace retry", "workspace_id", id, "error", err)
	}
}
