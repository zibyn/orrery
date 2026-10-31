package agent

import (
	"github.com/zibyn/orrery/internal/nodeproto"
	"github.com/zibyn/orrery/internal/supervisor"
)

const (
	probeImage      = "alpine:3.22"
	probeMemory     = int64(64 << 20)
	probeWorkspace  = "/workspace"
	probeSupervisor = "/.orrery/bin/supervisor"
)

// withSnapshotBuild adds the BuildKit endpoint self-check to a Runtime report.
// It is independent of the Runtime type (runtime.md §12): a Local Node with a
// reachable BuildKit endpoint builds too.
func withSnapshotBuild(report nodeproto.CapabilityReport, ok bool) nodeproto.CapabilityReport {
	value := nodeproto.ValueUnsupported
	if ok {
		value = nodeproto.ValueEnforced
	}
	for i := range report.Entries {
		if report.Entries[i].Name == nodeproto.CapSnapshotBuild {
			report.Entries[i].Value = value
			return report
		}
	}
	report.Entries = append(report.Entries, nodeproto.CapabilityEntry{
		Name: nodeproto.CapSnapshotBuild, Value: value,
	})
	return report
}

// capabilities is the containerd Node's Runtime report from its probe.
// kernel_isolation is enforced only under runsc (ADR-0026).
func capabilities(r supervisor.Result, limitsEnforced, gpuVisible, egressEnforced, linksEnforced, runsc bool) nodeproto.CapabilityReport {
	value := func(ok bool) string {
		if ok {
			return nodeproto.ValueEnforced
		}
		return nodeproto.ValueUnsupported
	}
	pidNS, mntNS := supervisor.NamespaceID("/proc/self/ns/pid"), supervisor.NamespaceID("/proc/self/ns/mnt")
	isolated := r.PIDNamespace != "" && r.PIDNamespace != pidNS &&
		r.MountNamespace != "" && r.MountNamespace != mntNS
	return nodeproto.CapabilityReport{
		RuntimeProvider: ProviderContainerd,
		Entries: []nodeproto.CapabilityEntry{
			{Name: nodeproto.CapProcessMountNamespace, Value: value(isolated)},
			{Name: nodeproto.CapCPUMemoryPIDLimits, Value: value(limitsEnforced)},
			{Name: nodeproto.CapWorkspaceRWMount, Value: value(r.WorkspaceWritable)},
			{Name: nodeproto.CapExecSupervisor, Value: nodeproto.ValueEnforced},
			// Whole-card visibility is enforced by CDI; GPU memory limits stay
			// unsupported (ADR-0009).
			{Name: nodeproto.CapGPUDeviceVisibility, Value: value(gpuVisible)},
			{Name: nodeproto.CapGPUMemoryLimit, Value: nodeproto.ValueUnsupported},
			// Disk quota and arbitrary mounts are separate tickets; report them
			// honestly instead of guessing.
			{Name: nodeproto.CapDiskCapacityQuota, Value: nodeproto.ValueUnsupported},
			{Name: nodeproto.CapArbitraryMounts, Value: nodeproto.ValueUnsupported},
			// The containerd adapter mounts Resource version trees as extra
			// read-only binds (runtime.md §2, §11). The Local Runtime reports
			// unsupported, which keeps the Scheduler from placing a Sandbox
			// with Resources there.
			{Name: nodeproto.CapResourceMount, Value: nodeproto.ValueEnforced},
			{Name: nodeproto.CapKernelIsolation, Value: value(runsc)},
			{Name: nodeproto.CapEgressPolicy, Value: value(egressEnforced)},
			{Name: nodeproto.CapSandboxLinks, Value: value(linksEnforced)},
			// snapshot_build is added by the Agent from the BuildKit endpoint
			// self-check, independent of the Runtime (runtime.md §12).
		},
	}
}
