package api

// SandboxState is the lifecycle condition of a Sandbox independently of its
// Runtime instance (GLOSSARY.md). The database stores it as text and enforces
// the same set with a CHECK constraint (schema.md).
type SandboxState string

const (
	SandboxPending      SandboxState = "pending"
	SandboxProvisioning SandboxState = "provisioning"
	SandboxReady        SandboxState = "ready"
	SandboxStopping     SandboxState = "stopping"
	SandboxStopped      SandboxState = "stopped"
	SandboxDeleting     SandboxState = "deleting"
	SandboxDeleted      SandboxState = "deleted"
	SandboxFailed       SandboxState = "failed"
)

// SandboxDesiredState is the lifecycle the caller asked a Sandbox to reach.
type SandboxDesiredState string

const (
	SandboxDesiredRunning SandboxDesiredState = "running"
	SandboxDesiredStopped SandboxDesiredState = "stopped"
	SandboxDesiredDeleted SandboxDesiredState = "deleted"
)

// WorkspaceState is the lifecycle condition of a Workspace (GLOSSARY.md).
type WorkspaceState string

const (
	WorkspaceProvisioning WorkspaceState = "provisioning"
	WorkspaceReady        WorkspaceState = "ready"
	WorkspaceDeleting     WorkspaceState = "deleting"
	WorkspaceDeleted      WorkspaceState = "deleted"
	WorkspaceFailed       WorkspaceState = "failed"
	WorkspaceLost         WorkspaceState = "lost"
)

// The wire names in lifecycle order, used by the OpenAPI document.
var (
	sandboxStateNames = []string{
		string(SandboxPending), string(SandboxProvisioning), string(SandboxReady),
		string(SandboxStopping), string(SandboxStopped), string(SandboxDeleting),
		string(SandboxDeleted), string(SandboxFailed),
	}
	workspaceStateNames = []string{
		string(WorkspaceProvisioning), string(WorkspaceReady), string(WorkspaceDeleting),
		string(WorkspaceDeleted), string(WorkspaceFailed), string(WorkspaceLost),
	}
)

// ResourceVersionState is the lifecycle condition of a ResourceVersion
// (api.md §3, schema.md). unavailable means an object it references is gone
// from the store; a later successful publish check repairs it.
type ResourceVersionState string

const (
	ResourceVersionPending     ResourceVersionState = "pending"
	ResourceVersionReady       ResourceVersionState = "ready"
	ResourceVersionFailed      ResourceVersionState = "failed"
	ResourceVersionUnavailable ResourceVersionState = "unavailable"
)

var resourceVersionStateNames = []string{
	string(ResourceVersionPending), string(ResourceVersionReady),
	string(ResourceVersionFailed), string(ResourceVersionUnavailable),
}
