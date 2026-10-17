// Package nodeproto holds the version and capability vocabulary shared by the
// Control Plane and the Node Agent (docs/spec/node-protocol.md).
package nodeproto

import "encoding/json"

// Protocol version of this implementation. The Control Plane upgrades first,
// so an older Agent minor is accepted and an Agent from a newer minor is
// refused (ADR-0014).
const (
	Major uint32 = 1
	Minor uint32 = 2
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
)

// Required lists the capabilities the Scheduler needs before it can place a
// Sandbox on a Node (ADR-0009).
var Required = []string{
	CapProcessMountNamespace,
	CapCPUMemoryPIDLimits,
	CapWorkspaceRWMount,
	CapExecSupervisor,
}

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
	Runtime string            `json:"runtime"`
	Entries []CapabilityEntry `json:"entries"`
}

type CapabilityEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// CoreEnforced reports whether every Required capability is enforced in the
// stored report.
func CoreEnforced(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var report CapabilityReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return false
	}
	values := make(map[string]string, len(report.Entries))
	for _, entry := range report.Entries {
		values[entry.Name] = entry.Value
	}
	for _, name := range Required {
		if values[name] != ValueEnforced {
			return false
		}
	}
	return true
}
