package api

import (
	"testing"

	"github.com/zibyn/orrery/internal/store"
)

// sandboxStep must route exactly as the Reconciler's switch did: desired state
// picks the sweep, and a running Sandbox only acts while it has not reached a
// Runtime yet.
func TestSandboxStep(t *testing.T) {
	tests := []struct {
		state   SandboxState
		desired SandboxDesiredState
		want    sandboxStepAction
	}{
		{SandboxPending, SandboxDesiredRunning, sandboxStepPending},
		{SandboxProvisioning, SandboxDesiredRunning, sandboxStepCreate},
		{SandboxReady, SandboxDesiredRunning, sandboxStepNone},
		{SandboxStopping, SandboxDesiredRunning, sandboxStepNone},
		{SandboxStopped, SandboxDesiredRunning, sandboxStepNone},
		{SandboxDeleting, SandboxDesiredRunning, sandboxStepNone},
		{SandboxDeleted, SandboxDesiredRunning, sandboxStepNone},
		{SandboxFailed, SandboxDesiredRunning, sandboxStepNone},
		{SandboxPending, SandboxDesiredStopped, sandboxStepStop},
		{SandboxProvisioning, SandboxDesiredStopped, sandboxStepStop},
		{SandboxReady, SandboxDesiredStopped, sandboxStepStop},
		{SandboxStopping, SandboxDesiredStopped, sandboxStepStop},
		{SandboxStopped, SandboxDesiredStopped, sandboxStepStop},
		{SandboxDeleting, SandboxDesiredStopped, sandboxStepStop},
		{SandboxDeleted, SandboxDesiredStopped, sandboxStepStop},
		{SandboxFailed, SandboxDesiredStopped, sandboxStepStop},
		{SandboxPending, SandboxDesiredDeleted, sandboxStepDelete},
		{SandboxProvisioning, SandboxDesiredDeleted, sandboxStepDelete},
		{SandboxReady, SandboxDesiredDeleted, sandboxStepDelete},
		{SandboxStopping, SandboxDesiredDeleted, sandboxStepDelete},
		{SandboxStopped, SandboxDesiredDeleted, sandboxStepDelete},
		{SandboxDeleting, SandboxDesiredDeleted, sandboxStepDelete},
		{SandboxDeleted, SandboxDesiredDeleted, sandboxStepDelete},
		{SandboxFailed, SandboxDesiredDeleted, sandboxStepDelete},
	}
	for _, tt := range tests {
		t.Run(string(tt.desired)+"/"+string(tt.state), func(t *testing.T) {
			if got := sandboxStep(tt.state, tt.desired); got != tt.want {
				t.Fatalf("sandboxStep(%s, %s) = %v, want %v", tt.state, tt.desired, got, tt.want)
			}
		})
	}
}

// execStep is the ordering rule of the Exec sweep. A dead Sandbox wins over
// cancellation, cancellation waits while the Agent is away, and an Exec whose
// start never reached the Supervisor finishes canceled without a command.
func TestExecStep(t *testing.T) {
	ready := func(state SandboxState, desired SandboxDesiredState) execFacts {
		return execFacts{sandboxState: state, desired: desired}
	}
	canceling := func(state SandboxState) execFacts {
		return execFacts{
			sandboxState: state, desired: SandboxDesiredRunning, cancelRequested: true,
		}
	}

	tests := []struct {
		name      string
		facts     execFacts
		connected bool
		delivered bool
		want      execStepAction
	}{
		{"stopped Sandbox loses the Exec", ready(SandboxStopped, SandboxDesiredStopped), true, true, execRuntimeLost},
		{"deleted Sandbox loses the Exec", ready(SandboxDeleted, SandboxDesiredDeleted), true, true, execRuntimeLost},
		{"failed Sandbox loses the Exec", ready(SandboxFailed, SandboxDesiredRunning), true, true, execRuntimeLost},
		{"dead Sandbox wins over cancellation", canceling(SandboxStopped), true, true, execRuntimeLost},
		{"cancel waits for a disconnected Agent, started", canceling(SandboxReady), false, false, execWait},
		{"cancel waits for a disconnected Agent, not started", canceling(SandboxReady), false, true, execWait},
		{"cancel before start and delivery finishes", canceling(SandboxReady), true, false, execFinishCanceled},
		{"cancel of a started Exec is sent", func() execFacts {
			f := canceling(SandboxReady)
			f.started = true
			return f
		}(), true, false, execSendCancel},
		{"a delivered start turns cancel into a command", canceling(SandboxReady), true, true, execSendCancel},
		{"stop asks the sweep to cancel", ready(SandboxReady, SandboxDesiredStopped), true, false, execCancelSandbox},
		{"delete asks the sweep to cancel", ready(SandboxReady, SandboxDesiredDeleted), true, false, execCancelSandbox},
		{"a provisioning Sandbox waits", ready(SandboxProvisioning, SandboxDesiredRunning), true, false, execWait},
		{"a ready Sandbox starts", ready(SandboxReady, SandboxDesiredRunning), true, false, execStart},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := execStep(tt.facts, tt.connected, tt.delivered); got != tt.want {
				t.Fatalf("execStep(%+v, connected=%v, delivered=%v) = %v, want %v",
					tt.facts, tt.connected, tt.delivered, got, tt.want)
			}
		})
	}
}

// finalizeAction decides what follows releasing a gone Runtime's resources.
func TestFinalizeAction(t *testing.T) {
	ephemeral := &store.Workspace{Kind: "ephemeral"}
	persistent := &store.Workspace{Kind: "persistent"}

	tests := []struct {
		name      string
		state     SandboxState
		desired   SandboxDesiredState
		workspace *store.Workspace
		want      goneAction
	}{
		{"failed keeps the row, running desired", SandboxFailed, SandboxDesiredRunning, ephemeral, goneReleaseOnly},
		{"failed keeps the row", SandboxFailed, SandboxDesiredStopped, ephemeral, goneReleaseOnly},
		{"stopped releases only", SandboxStopped, SandboxDesiredStopped, ephemeral, goneReleaseOnly},
		{"ready still wanted releases only", SandboxReady, SandboxDesiredRunning, ephemeral, goneReleaseOnly},
		{"deleting an ephemeral Workspace", SandboxDeleting, SandboxDesiredDeleted, ephemeral, goneWorkspaceDeleting},
		{"deleting a persistent Workspace", SandboxDeleting, SandboxDesiredDeleted, persistent, goneSandboxDeleted},
		{"deleting without a Workspace", SandboxDeleting, SandboxDesiredDeleted, nil, goneSandboxDeleted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := finalizeAction(tt.state, tt.desired, tt.workspace); got != tt.want {
				t.Fatalf("finalizeAction(%s, %s, %v) = %v, want %v",
					tt.state, tt.desired, tt.workspace, got, tt.want)
			}
		})
	}
}
