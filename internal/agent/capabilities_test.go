package agent

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/zibyn/orrery/internal/nodeproto"
	"github.com/zibyn/orrery/internal/supervisor"
)

func TestCapabilitiesFollowTheProbe(t *testing.T) {
	caps := capabilities(supervisor.Result{
		CPUMax:            "50000 100000",
		MemoryMax:         "67108864",
		PidsMax:           "64",
		PIDNamespace:      "pid:[different]",
		MountNamespace:    "mnt:[different]",
		NvidiaDevices:     []string{"/dev/nvidia0", "/dev/nvidiactl"},
		WorkspaceWritable: true,
	}, true, true, true, true, true)
	values := map[string]string{}
	for _, entry := range caps.Entries {
		values[entry.Name] = entry.Value
	}
	for _, name := range []string{
		"process_mount_namespace", "cpu_memory_pid_limits", "workspace_rw_mount",
		"exec_supervisor", "gpu_device_visibility", "resource_mount",
		"kernel_isolation", "egress_policy", "sandbox_links",
	} {
		if values[name] != nodeproto.ValueEnforced {
			t.Errorf("%s = %q, want enforced", name, values[name])
		}
	}
	if values["gpu_memory_limit"] != nodeproto.ValueUnsupported {
		t.Errorf("gpu_memory_limit = %q, want unsupported", values["gpu_memory_limit"])
	}

	// A daemon that refused the limits must not claim them, and a probe that
	// could not see the requested cards must not claim GPU visibility.
	weak := capabilities(supervisor.Result{WorkspaceWritable: false}, false, false, false, false, false)
	for _, entry := range weak.Entries {
		if entry.Name == "exec_supervisor" {
			continue // the probe ran, so the Supervisor works
		}
		if entry.Name == nodeproto.CapResourceMount {
			continue // the container adapter's bind mounts do not depend on the probe
		}
		if entry.Value != nodeproto.ValueUnsupported {
			t.Errorf("%s = %q, want unsupported", entry.Name, entry.Value)
		}
	}
}

func TestSnapshotBuildCapabilityIsIndependent(t *testing.T) {
	report := capabilities(supervisor.Result{WorkspaceWritable: false}, false, false, false, false, false)
	enforced := withSnapshotBuild(report, true)
	values := map[string]string{}
	for _, entry := range enforced.Entries {
		values[entry.Name] = entry.Value
	}
	if values[nodeproto.CapSnapshotBuild] != nodeproto.ValueEnforced {
		t.Fatalf("snapshot_build = %q, want enforced even when the runtime probe is weak",
			values[nodeproto.CapSnapshotBuild])
	}
	unsupported := withSnapshotBuild(report, false)
	for _, entry := range unsupported.Entries {
		if entry.Name == nodeproto.CapSnapshotBuild && entry.Value != nodeproto.ValueUnsupported {
			t.Fatalf("snapshot_build = %q, want unsupported", entry.Value)
		}
	}
}

func TestNumberedDeviceCount(t *testing.T) {
	devices := []string{
		"/dev/nvidia0", "/dev/nvidia1", "/dev/nvidiactl",
		"/dev/nvidia-uvm", "/dev/nvidia-uvm-tools", "/dev/nvidia-caps-imex-channels",
	}
	if got := numberedDeviceCount(devices); got != 2 {
		t.Fatalf("numberedDeviceCount = %d, want 2", got)
	}
}

func TestOnlyRejectionClearsIdentity(t *testing.T) {
	for _, code := range []codes.Code{codes.NotFound, codes.Unauthenticated} {
		if !clearsIdentity(status.Error(code, "x")) {
			t.Errorf("%s should clear identity", code)
		}
	}
	for _, code := range []codes.Code{codes.Unavailable, codes.FailedPrecondition, codes.Internal} {
		if clearsIdentity(status.Error(code, "x")) {
			t.Errorf("%s must preserve identity", code)
		}
	}
}
