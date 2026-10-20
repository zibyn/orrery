package agent

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/zibyn/orrery/internal/nodeproto"
	"github.com/zibyn/orrery/internal/supervisor"
)

// runtime.md §10: Local Runtime needs one non-root host user. The flag names a
// user (or uid); root must be refused because every Exec runs as that user.
func TestLookupLocalUser(t *testing.T) {
	for _, raw := range []string{"", "root", "0"} {
		if _, err := lookupLocalUser(raw); err == nil {
			t.Errorf("lookupLocalUser(%q) accepted a root or empty user", raw)
		}
	}
	me, err := user.Current()
	if err != nil {
		t.Skipf("no current user record: %v", err)
	}
	if me.Uid == "0" {
		t.Skip("running as root; the non-root case is covered where possible")
	}
	got, err := lookupLocalUser(me.Uid)
	if err != nil {
		t.Fatalf("lookupLocalUser(%q): %v", me.Uid, err)
	}
	wantUID, _ := strconv.Atoi(me.Uid)
	if got.uid != wantUID || got.uid == 0 || got.name == "" {
		t.Fatalf("lookupLocalUser(%q) = %+v", me.Uid, got)
	}
}

// The slice is the resource boundary (runtime.md §10); the property strings
// must be exactly what systemd understands.
func TestLocalLimitProperties(t *testing.T) {
	for _, tt := range []struct {
		cpu    float64
		memory int64
		want   []string
	}{
		{0.5, 128 << 20, []string{"CPUQuota=50%", "MemoryMax=134217728", "TasksMax=2048"}},
		{2.5, 1 << 30, []string{"CPUQuota=250%", "MemoryMax=1073741824", "TasksMax=2048"}},
		{1, 64 << 20, []string{"CPUQuota=100%", "MemoryMax=67108864", "TasksMax=2048"}},
	} {
		got := localLimitProperties(tt.cpu, tt.memory)
		if len(got) != len(tt.want) {
			t.Fatalf("localLimitProperties(%v) = %v, want %v", tt.cpu, got, tt.want)
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Fatalf("localLimitProperties(%v)[%d] = %q, want %q", tt.cpu, i, got[i], tt.want[i])
			}
		}
	}
}

// The placeholder unit is the Runtime instance: `supervisor init` in a slice
// named after the Sandbox (runtime.md §10).
func TestLocalCreateArgs(t *testing.T) {
	id := uuid.MustParse("00000000-0000-7000-8000-000000000001")
	got := localCreateArgs(id, "/data/bin/supervisor")
	want := []string{
		"--unit=orrery-sb-" + id.String(),
		"--slice=orrery-sb-" + id.String() + ".slice",
		"/data/bin/supervisor", "init",
	}
	if len(got) != len(want) {
		t.Fatalf("localCreateArgs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("localCreateArgs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// Exec is a transient service in the Sandbox's slice, as the Local user. The
// record root tells the Supervisor where this Provider keeps it.
func TestLocalExecArgs(t *testing.T) {
	sandboxID := uuid.MustParse("00000000-0000-7000-8000-000000000001")
	execID := uuid.MustParse("00000000-0000-7000-8000-000000000002")
	runRoot := "/data/sandboxes/" + sandboxID.String() + "/run"
	user := localUser{name: "builder", uid: 1001}
	got := localExecArgs(sandboxID, execID, user, runRoot, "/data/bin/supervisor", nil)
	want := []string{
		"--unit=orrery-exec-" + execID.String(),
		"--slice=orrery-sb-" + sandboxID.String() + ".slice",
		"--uid=builder",
		"--setenv=ORRERY_RUN_ROOT=" + runRoot,
		"--setenv=ORRERY_CTL_SOCKET=@orrery-exec-" + execID.String(),
		"/data/bin/supervisor", "run", execID.String(),
	}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("localExecArgs = %v, want %v", got, want)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("localExecArgs = %v, want %v", got, want)
	}
}

// ADR-0009: GPU visibility is enforced by closing the device policy to exactly
// the allocated cards plus the NVIDIA control nodes (runtime.md §10).
func TestLocalGPUProperties(t *testing.T) {
	props, err := localGPUProperties([]string{"GPU-aaaa"}, map[string]int{"GPU-aaaa": 0})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--property=DevicePolicy=closed",
		"--property=DeviceAllow=/dev/nvidia0 rw",
		"--property=DeviceAllow=/dev/nvidiactl rw",
		"--property=DeviceAllow=/dev/nvidia-uvm rw",
	}
	if len(props) != len(want) {
		t.Fatalf("localGPUProperties = %v, want %v", props, want)
	}
	for i := range want {
		if props[i] != want[i] {
			t.Fatalf("localGPUProperties[%d] = %q, want %q", i, props[i], want[i])
		}
	}
	if _, err := localGPUProperties([]string{"GPU-missing"}, map[string]int{}); err == nil {
		t.Fatal("an allocated card with no device index was accepted")
	}

	// A GPU-less Exec still closes the device policy: with no DeviceAllow
	// entries it has no device access at all (runtime.md §10).
	props, err = localGPUProperties(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 1 || props[0] != "--property=DevicePolicy=closed" {
		t.Fatalf("localGPUProperties(nil) = %v, want only DevicePolicy=closed", props)
	}
}

// parseGPUIndexes reads nvidia-smi's "index, uuid" lines.
func TestParseGPUIndexes(t *testing.T) {
	got := parseGPUIndexes("0, GPU-aaaa\n1, GPU-bbbb\n")
	if got["GPU-aaaa"] != 0 || got["GPU-bbbb"] != 1 {
		t.Fatalf("parseGPUIndexes = %v", got)
	}
	if len(parseGPUIndexes("garbage\n")) != 0 {
		t.Fatal("a malformed line produced an index")
	}
}

// The create-time readback is the guarantee that systemd applied the slice
// limits (runtime.md §10).
func TestLocalLimitsMatch(t *testing.T) {
	dir := t.TempDir()
	write := func(name, value string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("cpu.max", "50000 100000")
	write("memory.max", "134217728")
	write("pids.max", "2048")
	if !localLimitsMatch(dir, 0.5, 134217728) {
		t.Fatal("a matching slice readback was rejected")
	}
	for name, tt := range map[string]struct {
		file, value string
	}{
		"cpu":    {"cpu.max", "max 100000"},
		"memory": {"memory.max", "max"},
		"pids":   {"pids.max", "max"},
	} {
		t.Run(name, func(t *testing.T) {
			write(tt.file, tt.value)
			defer write(tt.file, map[string]string{"cpu.max": "50000 100000", "memory.max": "134217728", "pids.max": "2048"}[tt.file])
			if localLimitsMatch(dir, 0.5, 134217728) {
				t.Fatalf("a mismatched %s was accepted", name)
			}
		})
	}
	if localLimitsMatch(t.TempDir(), 0.5, 134217728) {
		t.Fatal("a missing cgroup was accepted")
	}
}

// systemd nests slices at every '-', so the readback locates the slice rather
// than assuming a path.
func TestFindSliceDir(t *testing.T) {
	root := t.TempDir()
	want := filepath.Join(root, "orrery.slice", "orrery-sb.slice", "orrery-sb-x.slice")
	if err := os.MkdirAll(want, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := findSliceDir(root, "orrery-sb-x.slice"); got != want {
		t.Fatalf("findSliceDir = %q, want %q", got, want)
	}
	if got := findSliceDir(root, "missing.slice"); got != "" {
		t.Fatalf("findSliceDir(missing) = %q, want empty", got)
	}
}

// ADR-0009's Local posture: namespaces unsupported, CPU/memory/PID via the
// slice, host cwd writable, Resources unsupported, GPU enforced or
// best_effort. Only a verified self-check reports enforced.
func TestLocalCapabilities(t *testing.T) {
	values := func(caps nodeproto.CapabilityReport) map[string]string {
		out := map[string]string{}
		for _, entry := range caps.Entries {
			out[entry.Name] = entry.Value
		}
		return out
	}
	result := supervisor.Result{WorkspaceWritable: true}

	caps := localCapabilities(result, true, true, true)
	if caps.RuntimeProvider != ProviderLocal {
		t.Fatalf("runtime_provider = %q, want local", caps.RuntimeProvider)
	}
	v := values(caps)
	for _, name := range []string{nodeproto.CapCPUMemoryPIDLimits, nodeproto.CapWorkspaceRWMount, nodeproto.CapExecSupervisor, nodeproto.CapGPUDeviceVisibility} {
		if v[name] != nodeproto.ValueEnforced {
			t.Errorf("%s = %q, want enforced", name, v[name])
		}
	}
	if v[nodeproto.CapProcessMountNamespace] != nodeproto.ValueUnsupported {
		t.Errorf("process_mount_namespace = %q, want unsupported", v[nodeproto.CapProcessMountNamespace])
	}
	if v[nodeproto.CapResourceMount] != nodeproto.ValueUnsupported {
		t.Errorf("resource_mount = %q, want unsupported", v[nodeproto.CapResourceMount])
	}

	// A failed limits readback is not enforced; a failed DevicePolicy check
	// degrades GPU visibility to best_effort, not unsupported.
	v = values(localCapabilities(result, false, true, false))
	if v[nodeproto.CapCPUMemoryPIDLimits] != nodeproto.ValueUnsupported {
		t.Errorf("unverified limits = %q, want unsupported", v[nodeproto.CapCPUMemoryPIDLimits])
	}
	if v[nodeproto.CapGPUDeviceVisibility] != nodeproto.ValueBestEffort {
		t.Errorf("unverified device policy = %q, want best_effort", v[nodeproto.CapGPUDeviceVisibility])
	}
	// A host without cards has nothing to attach.
	v = values(localCapabilities(result, true, false, false))
	if v[nodeproto.CapGPUDeviceVisibility] != nodeproto.ValueUnsupported {
		t.Errorf("no-card visibility = %q, want unsupported", v[nodeproto.CapGPUDeviceVisibility])
	}
	// An unwritable Workspace is not enforced.
	v = values(localCapabilities(supervisor.Result{}, true, false, false))
	if v[nodeproto.CapWorkspaceRWMount] != nodeproto.ValueUnsupported {
		t.Errorf("unwritable Workspace = %q, want unsupported", v[nodeproto.CapWorkspaceRWMount])
	}
}

// Local PATH and Git helper use the Agent's own bin directory, not /.orrery,
// and ORRERY_WORKSPACE names the host Workspace (node-protocol.md).
func TestLocalExecEnv(t *testing.T) {
	env := localExecEnv(map[string]string{"PATH": "/caller/bin", "KEEP": "kept"},
		"/fallback/bin", "/agent/bin", "/data/workspaces/ws", []string{"GPU-a", "GPU-b"})
	if got := env["PATH"]; got != "/caller/bin:/agent/bin" {
		t.Fatalf("PATH = %q", got)
	}
	if env["ORRERY_WORKSPACE"] != "/data/workspaces/ws" {
		t.Fatalf("ORRERY_WORKSPACE = %q", env["ORRERY_WORKSPACE"])
	}
	if env["GIT_CONFIG_VALUE_1"] != "/agent/bin/orrery git-credential" {
		t.Fatalf("credential helper = %q", env["GIT_CONFIG_VALUE_1"])
	}
	if env["CUDA_VISIBLE_DEVICES"] != "GPU-a,GPU-b" {
		t.Fatalf("CUDA_VISIBLE_DEVICES = %q", env["CUDA_VISIBLE_DEVICES"])
	}
	if env["KEEP"] != "kept" {
		t.Fatal("caller env was dropped")
	}
	// Without a caller PATH the Agent's own PATH is the base.
	noGPU := localExecEnv(nil, "/host/bin", "/agent/bin", "/ws", nil)
	if got := noGPU["PATH"]; got != "/host/bin:/agent/bin" {
		t.Fatalf("fallback PATH = %q", got)
	}
	// A GPU-less Exec still names the variable as empty, so a value inherited
	// from elsewhere cannot leak in (runtime.md §10).
	if got, ok := noGPU["CUDA_VISIBLE_DEVICES"]; !ok || got != "" {
		t.Fatalf("GPU-less CUDA_VISIBLE_DEVICES = %q (set %v), want an empty value", got, ok)
	}
}

// api.md §4: cwd is relative to the Workspace, on Local its host path. An
// absolute path or one that escapes the root is refused, never rewritten.
func TestLocalCwd(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "sub", "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ cwd, want string }{
		{"", workspace},
		{".", workspace},
		{"sub/dir", filepath.Join(workspace, "sub", "dir")},
		{"sub/../sub/dir", filepath.Join(workspace, "sub", "dir")},
	} {
		got, err := localCwd(workspace, tt.cwd)
		if err != nil {
			t.Errorf("localCwd(%q): %v", tt.cwd, err)
			continue
		}
		if got != tt.want {
			t.Errorf("localCwd(%q) = %q, want %q", tt.cwd, got, tt.want)
		}
	}
	for _, cwd := range []string{"/etc", "../outside", "sub/../../outside"} {
		if _, err := localCwd(workspace, cwd); err == nil {
			t.Errorf("localCwd(%q) escaped the Workspace", cwd)
		}
	}

	// A symlink inside the Workspace must not redirect the cwd outside it.
	if err := os.Symlink("/etc", filepath.Join(workspace, "escape")); err != nil {
		t.Skipf("cannot create a symlink: %v", err)
	}
	if _, err := localCwd(workspace, "escape"); err == nil {
		t.Error("localCwd followed a symlink out of the Workspace")
	}
}
