package agent

import (
	"encoding/binary"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/zibyn/stars-sandbox/internal/nodeproto"
	"github.com/zibyn/stars-sandbox/internal/supervisor"
)

func TestDemux(t *testing.T) {
	frame := func(stream byte, payload string) []byte {
		header := make([]byte, 8)
		header[0] = stream
		binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
		return append(header, payload...)
	}
	raw := append(frame(1, `{"cpu_max":"50000 100000"}`), frame(2, "warning")...)
	if got := string(demux(raw)); got != `{"cpu_max":"50000 100000"}warning` {
		t.Fatalf("demux = %q", got)
	}
}

func TestCapabilitiesFollowTheProbe(t *testing.T) {
	caps := capabilities(supervisor.Result{
		CPUMax:            "50000 100000",
		MemoryMax:         "67108864",
		PidsMax:           "64",
		PIDNamespace:      "pid:[different]",
		MountNamespace:    "mnt:[different]",
		NvidiaDevices:     []string{"/dev/nvidia0", "/dev/nvidiactl"},
		WorkspaceWritable: true,
	}, true, true)
	values := map[string]string{}
	for _, entry := range caps.Entries {
		values[entry.Name] = entry.Value
	}
	for _, name := range []string{
		"process_mount_namespace", "cpu_memory_pid_limits", "workspace_rw_mount",
		"exec_supervisor", "gpu_device_visibility",
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
	weak := capabilities(supervisor.Result{WorkspaceWritable: false}, false, false)
	for _, entry := range weak.Entries {
		if entry.Name == "exec_supervisor" {
			continue // the probe ran, so the Supervisor works
		}
		if entry.Value != nodeproto.ValueUnsupported {
			t.Errorf("%s = %q, want unsupported", entry.Name, entry.Value)
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
