// Package nodeproto holds the version, capability, and observation-phase
// vocabulary shared by the Control Plane and the Node Agent
// (docs/spec/node-protocol.md).
package nodeproto

import "encoding/json"

// Protocol version of this implementation. The Control Plane upgrades first,
// so an older Agent minor is accepted and an Agent from a newer minor is
// refused (ADR-0014).
const (
	Major uint32 = 1
	Minor uint32 = 4
)

// Capability names are the Runtime Provider facts a Node self-checks
// (ADR-0009). Values are "enforced", "best_effort" or "unsupported".
const (
	ValueEnforced    = "enforced"
	ValueBestEffort  = "best_effort"
	ValueUnsupported = "unsupported"

	CapProcessMountNamespace = "process_mount_namespace"
	CapCPUMemoryPIDLimits    = "cpu_memory_pid_limits"
	CapWorkspaceRWMount      = "workspace_rw_mount"
	CapGPUDeviceVisibility   = "gpu_device_visibility"
	CapGPUMemoryLimit        = "gpu_memory_limit"
	CapDiskCapacityQuota     = "disk_capacity_quota"
	CapExecSupervisor        = "exec_supervisor"
	CapArbitraryMounts       = "arbitrary_mounts_privileged_mode"
	// CapResourceMount is the Runtime's ability to bind-mount Resource version
	// trees read-only. It is optional: a Sandbox without Resources does not
	// need it, and the Scheduler filters Nodes that do not enforce it when the
	// Sandbox has Resources (runtime.md §10-11).
	CapResourceMount = "resource_mount"
	// CapSnapshotBuild is the Runtime's ability to run a Snapshot build on the
	// host build API. It is optional: Local Runtime reports it unsupported and
	// the Scheduler filters build rows to Nodes that enforce it (ADR-0021).
	CapSnapshotBuild = "snapshot_build"
	// CapKernelIsolation is enforced only by a gvisor containerd Node
	// (ADR-0026).
	CapKernelIsolation = "kernel_isolation"
	// CapEgressPolicy is enforced only after the registration egress
	// self-check passes (runtime.md §13).
	CapEgressPolicy = "egress_policy"
)

// Required lists the capabilities a container Runtime must enforce before the
// Scheduler places work there (ADR-0009).
var Required = []string{
	CapProcessMountNamespace,
	CapCPUMemoryPIDLimits,
	CapWorkspaceRWMount,
	CapExecSupervisor,
}

// RequiredFor is the capability set a Node's Provider must enforce. Local
// Runtime reports process/mount namespace unsupported by design, and a caller
// accepts that by requesting runtime: local, so that one capability is not
// required of it (ADR-0009). Everything else still is.
func RequiredFor(provider string) []string {
	if provider == "local" {
		return []string{CapCPUMemoryPIDLimits, CapWorkspaceRWMount, CapExecSupervisor}
	}
	return Required
}

// SandboxPhase is what a Node observes about a Sandbox's Runtime. Phases are
// not SandboxState values: a running Runtime maps to a ready Sandbox.
type SandboxPhase string

const (
	SandboxPhaseRunning           SandboxPhase = "running"
	SandboxPhaseFetchingResources SandboxPhase = "fetching_resources"
	SandboxPhaseGone              SandboxPhase = "gone"
	SandboxPhaseCreateFailed      SandboxPhase = "create_failed"
)

// WorkspacePhase is what a Node observes about a Workspace's storage.
type WorkspacePhase string

const (
	WorkspacePhaseReady WorkspacePhase = "ready"
	WorkspacePhaseGone  WorkspacePhase = "gone"
)

// Compatible applies the protocol version rule: a different major or a newer
// minor is refused; an older minor is accepted and the Node is `outdated`.
func Compatible(major, minor uint32) (accepted, outdated bool) {
	return compatible(Major, Minor, major, minor)
}

func compatible(cpMajor, cpMinor, agentMajor, agentMinor uint32) (accepted, outdated bool) {
	if agentMajor != cpMajor || agentMinor > cpMinor {
		return false, false
	}
	return true, agentMinor < cpMinor
}

// CapabilityReport is the stored form of the Capabilities message. GPUs live
// in their own table, so they are not repeated here.
type CapabilityReport struct {
	RuntimeProvider string            `json:"runtime_provider"`
	Entries         []CapabilityEntry `json:"entries"`
}

type CapabilityEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// CoreEnforced reports whether every capability the Node's Provider requires
// is enforced in the stored report.
func CoreEnforced(raw json.RawMessage) bool {
	values := storedValues(raw)
	for _, name := range RequiredFor(RuntimeProvider(raw)) {
		if values[name] != ValueEnforced {
			return false
		}
	}
	return true
}

// CapabilityEnforced reports whether one capability is enforced in the stored
// report. An absent capability is not enforced.
func CapabilityEnforced(raw json.RawMessage, name string) bool {
	return storedValues(raw)[name] == ValueEnforced
}

// CapabilityValue returns a stored capability's value, or "" when the Node
// never reported it.
func CapabilityValue(raw json.RawMessage, name string) string {
	return storedValues(raw)[name]
}

// RuntimeProvider is the Node's one Runtime Provider (containerd or
// local), or "" when the Node has not reported Capabilities.
func RuntimeProvider(raw json.RawMessage) string {
	var report CapabilityReport
	if len(raw) == 0 || json.Unmarshal(raw, &report) != nil {
		return ""
	}
	return report.RuntimeProvider
}

func storedValues(raw json.RawMessage) map[string]string {
	var report CapabilityReport
	if len(raw) == 0 || json.Unmarshal(raw, &report) != nil {
		return nil
	}
	values := make(map[string]string, len(report.Entries))
	for _, entry := range report.Entries {
		values[entry.Name] = entry.Value
	}
	return values
}
