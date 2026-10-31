package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/nodeproto"
	"github.com/zibyn/orrery/internal/supervisor"
)

const (
	// localCommandTimeout bounds one systemctl/systemd-run invocation.
	localCommandTimeout = 30 * time.Second
	localProbeUnitCPU   = "CPUQuota=50%"
	localProbeUnitMem   = "MemoryMax=67108864"
	localProbeUnitPids  = "TasksMax=256"
	// localProbeSlice groups the registration self-check units.
	localProbeSlice = "orrery-probe.slice"
)

// localUser is the single unprivileged host user every Local Runtime Exec runs
// as (runtime.md §10). Users on a Local Node are not isolated from each other.
type localUser struct {
	name string
	uid  int
	gid  int
}

// lookupLocalUser resolves --local-user, accepting a name or a numeric uid.
// Root is refused: the Agent runs as root, the commands must not.
func lookupLocalUser(raw string) (localUser, error) {
	if raw == "" {
		return localUser{}, errors.New("--local-user is required when --runtime local")
	}
	var (
		u   *user.User
		err error
	)
	if _, numErr := strconv.Atoi(raw); numErr == nil {
		u, err = user.LookupId(raw)
	} else {
		u, err = user.Lookup(raw)
	}
	if err != nil {
		return localUser{}, fmt.Errorf("local user %q: %w", raw, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return localUser{}, fmt.Errorf("local user %q has a non-numeric uid %q", raw, u.Uid)
	}
	if uid == 0 {
		return localUser{}, fmt.Errorf("local user %q is root; --local-user must be unprivileged", raw)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		gid = -1
	}
	return localUser{name: u.Username, uid: uid, gid: gid}, nil
}

// agentBinDir is the directory holding the Agent binary and the sibling
// orrery CLI. Local Runtime appends it to PATH and points the Git
// credential helper at it (node-protocol.md).
func agentBinDir() string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(self)
}

func localSliceName(id uuid.UUID) string { return "orrery-sb-" + id.String() + ".slice" }

// localPlaceholderUnit is the --unit= value; systemd appends .service. The
// query helpers name the .service explicitly so is-active cannot guess wrong.
func localPlaceholderUnit(id uuid.UUID) string    { return "orrery-sb-" + id.String() }
func localPlaceholderService(id uuid.UUID) string { return localPlaceholderUnit(id) + ".service" }
func localExecUnit(id uuid.UUID) string           { return "orrery-exec-" + id.String() }
func localExecService(id uuid.UUID) string        { return localExecUnit(id) + ".service" }

// localCtlSocket is the Supervisor's abstract socket. The host record path can
// exceed unix sockets' sun_path limit, and an abstract name is not a path.
func localCtlSocket(execID uuid.UUID) string { return "@" + localExecUnit(execID) }

// localQuota is the CPU quota in microseconds per 100 ms period, the same
// rounding the container Runtime uses.
func localQuota(cpu float64) int64 {
	nanoCPUs := int64(cpu * 1e9)
	if nanoCPUs < 1e6 {
		nanoCPUs = 1e6
	}
	return nanoCPUs / 10000
}

// localLimitProperties is the slice boundary: CPU, memory and PID count
// (ADR-0009).
func localLimitProperties(cpu float64, memory int64) []string {
	percent := float64(localQuota(cpu)) / 1000
	return []string{
		"CPUQuota=" + strconv.FormatFloat(percent, 'f', -1, 64) + "%",
		"MemoryMax=" + strconv.FormatInt(memory, 10),
		"TasksMax=" + strconv.FormatInt(sandboxPidsLimit, 10),
	}
}

func localCreateArgs(id uuid.UUID, supervisorPath string) []string {
	return []string{
		"--unit=" + localPlaceholderUnit(id),
		"--slice=" + localSliceName(id),
		supervisorPath, "init",
	}
}

func localExecArgs(sandboxID, execID uuid.UUID, as localUser, runRoot, supervisorPath string, extra []string) []string {
	args := []string{
		"--unit=" + localExecUnit(execID),
		"--slice=" + localSliceName(sandboxID),
		"--uid=" + as.name,
		"--setenv=ORRERY_RUN_ROOT=" + runRoot,
		"--setenv=ORRERY_CTL_SOCKET=" + localCtlSocket(execID),
	}
	args = append(args, extra...)
	return append(args, supervisorPath, "run", execID.String())
}

// localGPUProperties closes the device policy for a transient Exec service
// (runtime.md §10). DeviceAllow entries name only the allocated cards plus the
// NVIDIA control nodes; with no allocation the closed policy leaves no device
// access at all. CUDA_VISIBLE_DEVICES travels in the Exec environment instead.
func localGPUProperties(uuids []string, indices map[string]int) ([]string, error) {
	props := []string{"--property=DevicePolicy=closed"}
	seen := make(map[int]bool, len(uuids))
	for _, id := range uuids {
		index, ok := indices[id]
		if !ok {
			return nil, fmt.Errorf("GPU %s has no /dev/nvidia device index", id)
		}
		if seen[index] {
			continue
		}
		seen[index] = true
		props = append(props, fmt.Sprintf("--property=DeviceAllow=/dev/nvidia%d rw", index))
	}
	if len(uuids) == 0 {
		return props, nil
	}
	return append(props,
		"--property=DeviceAllow=/dev/nvidiactl rw",
		"--property=DeviceAllow=/dev/nvidia-uvm rw",
	), nil
}

// parseGPUIndexes reads nvidia-smi's "index, uuid" lines.
func parseGPUIndexes(out string) map[string]int {
	indices := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		rawIndex, id, found := strings.Cut(line, ",")
		index, err := strconv.Atoi(strings.TrimSpace(rawIndex))
		id = strings.TrimSpace(id)
		if !found || err != nil || !strings.HasPrefix(id, "GPU-") {
			continue
		}
		indices[id] = index
	}
	return indices
}

// gpuDeviceIndices maps a card UUID to its /dev/nvidiaN number.
func gpuDeviceIndices(ctx context.Context) (map[string]int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=index,uuid", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi device indices: %w", err)
	}
	return parseGPUIndexes(string(out)), nil
}

// localLimitsMatch compares the host cgroup readback with the request
// (runtime.md §10). A runtime that silently ignored a limit fails creation.
func localLimitsMatch(dir string, cpu float64, memory int64) bool {
	return readCgroupValue(dir, "cpu.max") == fmt.Sprintf("%d 100000", localQuota(cpu)) &&
		readCgroupValue(dir, "memory.max") == strconv.FormatInt(memory, 10) &&
		readCgroupValue(dir, "pids.max") == strconv.FormatInt(sandboxPidsLimit, 10)
}

func readCgroupValue(dir, name string) string {
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// findSliceDir locates a slice under the cgroup root. systemd nests slices at
// every '-', so the path is derived by name, not assumed.
func findSliceDir(root, name string) string {
	var found string
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() && entry.Name() == name {
			found = path
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// localCapabilities is the Local posture in ADR-0009. Only facts the
// self-check verified are `enforced`; GPU visibility degrades to best_effort
// when the DevicePolicy probe was rejected, and Local reports Resource mounts
// unsupported.
func localCapabilities(result supervisor.Result, limitsEnforced, hasGPUs, devicePolicy bool) nodeproto.CapabilityReport {
	value := func(ok bool) string {
		if ok {
			return nodeproto.ValueEnforced
		}
		return nodeproto.ValueUnsupported
	}
	gpu := nodeproto.ValueUnsupported
	if hasGPUs {
		gpu = nodeproto.ValueBestEffort
		if devicePolicy {
			gpu = nodeproto.ValueEnforced
		}
	}
	return nodeproto.CapabilityReport{
		RuntimeProvider: ProviderLocal,
		Entries: []nodeproto.CapabilityEntry{
			{Name: nodeproto.CapProcessMountNamespace, Value: nodeproto.ValueUnsupported},
			{Name: nodeproto.CapCPUMemoryPIDLimits, Value: value(limitsEnforced)},
			{Name: nodeproto.CapWorkspaceRWMount, Value: value(result.WorkspaceWritable)},
			{Name: nodeproto.CapExecSupervisor, Value: nodeproto.ValueEnforced},
			{Name: nodeproto.CapGPUDeviceVisibility, Value: gpu},
			{Name: nodeproto.CapGPUMemoryLimit, Value: nodeproto.ValueUnsupported},
			{Name: nodeproto.CapDiskCapacityQuota, Value: nodeproto.ValueUnsupported},
			{Name: nodeproto.CapArbitraryMounts, Value: nodeproto.ValueUnsupported},
			{Name: nodeproto.CapResourceMount, Value: nodeproto.ValueUnsupported},
			// No netns, so Egress Rules do not apply (ADR-0026).
			{Name: nodeproto.CapEgressPolicy, Value: nodeproto.ValueUnsupported},
			{Name: nodeproto.CapSandboxLinks, Value: nodeproto.ValueUnsupported},
		},
	}
}

// localExecEnv adds the Local-only variables to the common Exec environment:
// the host Workspace path and the cards CUDA may see (node-protocol.md). A
// GPU-less Exec still names the variable as empty.
func localExecEnv(caller map[string]string, basePath, binDir, workspace string, gpus []string) map[string]string {
	env := execEnv(caller, basePath, binDir)
	env["ORRERY_WORKSPACE"] = workspace
	env["CUDA_VISIBLE_DEVICES"] = strings.Join(gpus, ",")
	return env
}

// localCwd turns the Exec's Workspace-relative cwd into the host path the
// Supervisor needs (api.md §4). It never rewrites an escaping path: the API
// rejects those first, and os.Root keeps the Agent safe on its own, including
// symlinks that point out of the Workspace.
func localCwd(workspace, cwd string) (string, error) {
	if cwd == "" {
		return workspace, nil
	}
	if !filepath.IsLocal(cwd) {
		return "", errBadPath
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return "", err
	}
	defer root.Close() //nolint:errcheck // read-only handle
	target, err := root.OpenRoot(filepath.Clean(cwd))
	if err != nil {
		return "", err
	}
	_ = target.Close()
	return filepath.Join(workspace, filepath.Clean(cwd)), nil
}

// localDriver is the systemd-backed Provider (runtime.md §10). It shares every
// host-path operation with the container Provider.
type localDriver struct {
	r          *Runtime
	user       localUser
	binDir     string
	supervisor string
}

// newLocalRuntime builds the Local Provider. The shared host-path plumbing is
// the same Runtime; only the driver differs, and owner maps the Workspace and
// record directories to the unprivileged user.
func newLocalRuntime(dataRoot, nodeID string, as localUser, binDir string) *Runtime {
	rt := newRuntime(ProviderLocal, dataRoot, nodeID)
	rt.owner = &rootlessOwner{uid: as.uid, gid: as.gid}
	rt.driver = &localDriver{
		r: rt, user: as, binDir: binDir,
		// PrepareDataRoot stages the platform binary as bin/supervisor.
		supervisor: filepath.Join(dataRoot, "bin", "supervisor"),
	}
	return rt
}

// rootlessOwner is the host uid/gid a Local Runtime's commands run as. A
// negative gid leaves the group untouched.
type rootlessOwner struct {
	uid int
	gid int
}

// own hands path to the Local Runtime user so its commands can write it. The
// Agent runs as root, so the chown is allowed; a nil owner is a no-op.
func (o *rootlessOwner) own(path string) error {
	if o == nil {
		return nil
	}
	return os.Chown(path, o.uid, o.gid)
}

// localCommand runs one host command and reports its exit code. An error means
// the command could not run at all.
func localCommand(ctx context.Context, name string, args ...string) (int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, localCommandTimeout)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err == nil {
		return 0, out.String(), nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), out.String(), nil
	}
	return -1, out.String(), fmt.Errorf("%s: %w", name, err)
}

func runLocalCommand(ctx context.Context, name string, args ...string) (string, error) {
	code, out, err := localCommand(ctx, name, args...)
	if err != nil {
		return out, err
	}
	if code != 0 {
		return out, fmt.Errorf("%s %s: exited %d: %s", name, strings.Join(args, " "), code, strings.TrimSpace(out))
	}
	return out, nil
}

// localUnitActive reports whether a unit is active. systemd's stable exit codes
// distinguish "not active" (3/4) from a systemd that could not answer.
func localUnitState(ctx context.Context, unit string) (active, known bool, out string) {
	code, out, err := localCommand(ctx, "systemctl", "is-active", unit)
	if err != nil {
		return false, false, out
	}
	switch code {
	case 0:
		return strings.TrimSpace(out) == "active", true, out
	case 3, 4:
		return false, true, out
	default:
		return false, false, out
	}
}

func (d *localDriver) prepareDirs(workspace, runDir string) error {
	for _, dir := range []string{workspace, runDir} {
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return err
		}
		// MkdirAll is subject to umask; the Local user must be able to write.
		if err := os.Chmod(dir, 0o777); err != nil {
			return err
		}
		if err := os.Chown(dir, d.user.uid, d.user.gid); err != nil {
			return err
		}
	}
	return nil
}

func writeSandboxGPUs(dir string, uuids []string) error {
	path := filepath.Join(dir, "gpus")
	if len(uuids) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	raw, err := json.Marshal(uuids)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

func readSandboxGPUs(dir string) []string {
	raw, err := os.ReadFile(filepath.Join(dir, "gpus"))
	if err != nil {
		return nil
	}
	var uuids []string
	if json.Unmarshal(raw, &uuids) != nil {
		return nil
	}
	return uuids
}

func (d *localDriver) createSandbox(ctx context.Context, cmd *createSandboxSpec) (string, error) {
	id, err := uuid.Parse(cmd.sandboxID)
	if err != nil {
		return "runtime_create_failed", err
	}
	workspaceID, err := uuid.Parse(cmd.workspaceID)
	if err != nil {
		return "runtime_create_failed", err
	}
	workspace := workspacePath(d.r.dataRoot, workspaceID)
	runDir := sandboxRunPath(d.r.dataRoot, id)
	if err := d.prepareDirs(workspace, runDir); err != nil {
		return "runtime_create_failed", err
	}
	mode := cmd.workspaceMode
	if mode != "rw" && mode != "ro" {
		mode = "rw"
	}
	// The file API reads this record; there is no mount table on Local.
	if err := d.r.writeWorkspaceRefFile(id, workspaceRef{ID: workspaceID.String(), Mode: mode}); err != nil {
		return "runtime_create_failed", err
	}
	// StartExec has no GPU list, so the allocation is remembered per Sandbox.
	if err := writeSandboxGPUs(sandboxPath(d.r.dataRoot, id), cmd.gpuUUIDs); err != nil {
		return "runtime_create_failed", err
	}

	// Adopt a placeholder a lost Agent reply left running.
	if active, _, _ := localUnitState(ctx, localPlaceholderService(id)); active {
		if !d.verifyLimits(ctx, id, cmd.cpu, cmd.memory) {
			_ = d.stopSandbox(context.WithoutCancel(ctx), id)
			return "runtime_create_failed", errors.New("the existing slice does not match the request")
		}
		d.r.track(id, cmd.generation)
		return "", nil
	}
	if _, err := runLocalCommand(ctx, "systemctl",
		append([]string{"set-property", "--runtime", localSliceName(id)}, localLimitProperties(cmd.cpu, cmd.memory)...)...); err != nil {
		return "runtime_create_failed", err
	}
	if _, err := runLocalCommand(ctx, "systemd-run", localCreateArgs(id, d.supervisor)...); err != nil {
		_ = d.stopSandbox(context.WithoutCancel(ctx), id)
		return "runtime_create_failed", err
	}
	if !d.verifyLimits(ctx, id, cmd.cpu, cmd.memory) {
		_ = d.stopSandbox(context.WithoutCancel(ctx), id)
		return "runtime_create_failed", errors.New("slice limits do not match the request")
	}
	d.r.track(id, cmd.generation)
	return "", nil
}

// verifyLimits waits for the slice's cgroup files and compares them with the
// request. systemd creates the slice asynchronously with the placeholder.
func (d *localDriver) verifyLimits(ctx context.Context, id uuid.UUID, cpu float64, memory int64) bool {
	deadline := time.Now().Add(5 * time.Second)
	for {
		if dir := findSliceDir("/sys/fs/cgroup", localSliceName(id)); dir != "" && localLimitsMatch(dir, cpu, memory) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (d *localDriver) stopSandbox(ctx context.Context, sandboxID uuid.UUID) error {
	code, out, err := localCommand(ctx, "systemctl", "stop", localSliceName(sandboxID))
	if err != nil {
		return err
	}
	// A missing slice is already stopped; systemd returns 0 for that, but be
	// explicit for versions that report "not loaded".
	if code != 0 && !strings.Contains(out, "not loaded") {
		return fmt.Errorf("stop slice: systemctl exited %d: %s", code, strings.TrimSpace(out))
	}
	d.r.forget(sandboxID.String())
	return nil
}

func (d *localDriver) startExec(ctx context.Context, cmd *nodev1.StartExec) error {
	sandboxID, err := uuid.Parse(cmd.SandboxId)
	if err != nil {
		return errors.New("invalid sandbox id")
	}
	execID, err := uuid.Parse(cmd.ExecId)
	if err != nil {
		return errors.New("invalid exec id")
	}
	if d.user.uid == 0 {
		return errors.New("refusing to run a Local Exec as root")
	}
	ref, ok := readWorkspaceRef(sandboxPath(d.r.dataRoot, sandboxID))
	if !ok {
		return errors.New("the Sandbox has no Workspace record")
	}
	workspaceID, err := uuid.Parse(ref.ID)
	if err != nil {
		return errors.New("the Sandbox Workspace record is invalid")
	}
	workspace := workspacePath(d.r.dataRoot, workspaceID)
	runRoot := sandboxRunPath(d.r.dataRoot, sandboxID)
	dir := filepath.Join(runRoot, execID.String())

	slog.Info("start local exec", "exec_id", execID, "sandbox_id", sandboxID)
	// Reconnect replay: the record holds everything still owed.
	if _, err := os.Stat(filepath.Join(dir, "exit")); err == nil {
		go d.r.streamExec(ctx, sandboxID, execID, dir)
		return nil
	}
	if err := d.prepareDirs(workspace, runRoot); err != nil {
		return err
	}
	// Execs run inside the Sandbox's slice. If the placeholder is gone the
	// slice is gone too, and systemd-run would silently create an unbounded
	// one; let the Control Plane re-create the Runtime instead.
	if active, _, _ := localUnitState(ctx, localPlaceholderService(sandboxID)); !active {
		return errors.New("the Sandbox Runtime is not running")
	}

	gpus := readSandboxGPUs(sandboxPath(d.r.dataRoot, sandboxID))
	var indices map[string]int
	if len(gpus) > 0 {
		indices, err = gpuDeviceIndices(ctx)
		if err != nil {
			return err
		}
	}
	props, err := localGPUProperties(gpus, indices)
	if err != nil {
		return err
	}
	if active, _, _ := localUnitState(ctx, localExecService(execID)); !active {
		if _, err := runLocalCommand(ctx, "systemd-run",
			localExecArgs(sandboxID, execID, d.user, runRoot, d.supervisor, props)...); err != nil {
			return err
		}
	}
	cwd, err := localCwd(workspace, cmd.Cwd)
	if err != nil {
		return err
	}
	return d.r.deliverExecSpec(ctx, sandboxID, execID, dir, cmd,
		localExecEnv(cmd.Env, os.Getenv("PATH"), d.binDir, workspace, gpus),
		cwd)
}

func (d *localDriver) probe(ctx context.Context) (nodeproto.CapabilityReport, error) {
	if os.Geteuid() != 0 {
		return nodeproto.CapabilityReport{}, errors.New("Local Runtime requires the Agent to run as root (system manager)")
	}
	result, limitsEnforced, hasGPUs, devicePolicy, err := d.selfCheck(ctx)
	if err != nil {
		return nodeproto.CapabilityReport{}, err
	}
	return localCapabilities(result, limitsEnforced, hasGPUs, devicePolicy), nil
}

// selfCheck runs a bounded, unprivileged probe service and reads its own
// cgroup and Workspace back (runtime.md §10). A host with cards also probes
// DevicePolicy=closed; a rejected policy degrades only GPU visibility.
func (d *localDriver) selfCheck(ctx context.Context) (supervisor.Result, bool, bool, bool, error) {
	dir, err := os.MkdirTemp("", "orrery-local-probe-*")
	if err != nil {
		return supervisor.Result{}, false, false, false, err
	}
	defer os.RemoveAll(dir) //nolint:errcheck // best effort
	// MkdirTemp is 0700 root; the Local user must traverse it to reach the
	// probe Workspace.
	if err := os.Chmod(dir, 0o755); err != nil {
		return supervisor.Result{}, false, false, false, err
	}
	workspace := filepath.Join(dir, "workspace")
	if err := os.Mkdir(workspace, 0o777); err != nil {
		return supervisor.Result{}, false, false, false, err
	}
	if err := os.Chown(workspace, d.user.uid, d.user.gid); err != nil {
		return supervisor.Result{}, false, false, false, err
	}

	gpus := enumerateGPUs(ctx)
	hasGPUs := len(gpus) > 0
	var deviceProps []string
	if hasGPUs {
		if indices, err := gpuDeviceIndices(ctx); err == nil {
			deviceProps, _ = localGPUProperties(gpuUUIDs(gpus), indices)
		}
	}
	attempts := []bool{false}
	if len(deviceProps) > 0 {
		attempts = []bool{true, false}
	}
	var (
		result       supervisor.Result
		devicePolicy bool
	)
	for _, withDevices := range attempts {
		var extra []string
		if withDevices {
			extra = deviceProps
		}
		result, err = d.runProbe(ctx, workspace, extra)
		if err == nil {
			devicePolicy = withDevices
			break
		}
	}
	_, _ = runLocalCommand(context.WithoutCancel(ctx), "systemctl", "stop", localProbeSlice)
	if err != nil {
		return supervisor.Result{}, false, false, false, err
	}
	limitsEnforced := result.CPUMax == "50000 100000" &&
		result.MemoryMax == "67108864" &&
		result.PidsMax == "256"
	return result, limitsEnforced, hasGPUs, devicePolicy, nil
}

func (d *localDriver) runProbe(ctx context.Context, workspace string, extra []string) (supervisor.Result, error) {
	args := []string{
		"--quiet", "--pipe", "--wait", "--collect",
		"--unit=orrery-probe-" + uuid.NewString(),
		"--slice=" + localProbeSlice,
		"--uid=" + d.user.name,
		"--property=" + localProbeUnitCPU,
		"--property=" + localProbeUnitMem,
		"--property=" + localProbeUnitPids,
	}
	args = append(args, extra...)
	args = append(args, d.supervisor, "probe", workspace)

	ctx, cancel := context.WithTimeout(ctx, localCommandTimeout)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "systemd-run", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return supervisor.Result{}, fmt.Errorf("Local probe: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var result supervisor.Result
	if err := json.Unmarshal(bytes.TrimSpace(out), &result); err != nil {
		return supervisor.Result{}, fmt.Errorf("decode Local probe: %w", err)
	}
	return result, nil
}

func (d *localDriver) healthy(ctx context.Context) bool {
	code, _, err := localCommand(ctx, "systemctl", "is-system-running")
	// 0 is running, 1 is degraded: both mean systemd answers.
	return err == nil && (code == 0 || code == 1)
}

// adopt picks up placeholders a previous Agent process left running, so the
// periodic scan (not systemd events) is the liveness path for them.
func (d *localDriver) adopt(ctx context.Context, report func(string, int64)) {
	entries, err := os.ReadDir(filepath.Join(d.r.dataRoot, "sandboxes"))
	if err != nil {
		return
	}
	for _, entry := range entries {
		sandboxID, err := uuid.Parse(entry.Name())
		if err != nil {
			continue
		}
		// A build Sandbox has no placeholder (runtime.md §12).
		if _, err := os.Stat(filepath.Join(sandboxPath(d.r.dataRoot, sandboxID), "build")); err == nil {
			continue
		}
		active, known, _ := localUnitState(ctx, localPlaceholderService(sandboxID))
		if !known || !active {
			continue
		}
		generation, ok := readGeneration(sandboxPath(d.r.dataRoot, sandboxID))
		if !ok {
			continue
		}
		d.r.track(sandboxID, generation)
		report(sandboxID.String(), generation)
	}
}

func (d *localDriver) scan(ctx context.Context, report func(string, int64)) {
	for id, generation := range d.r.managedSnapshot() {
		sandboxID, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		active, knownState, _ := localUnitState(ctx, localPlaceholderService(sandboxID))
		if !knownState {
			continue
		}
		if !active {
			d.r.forget(id)
			report(id, generation)
		}
	}
}

// watchEvents is a no-op: systemd has no event stream here, so the periodic
// scan is the liveness path.
func (d *localDriver) watchEvents(context.Context, func(string, int64)) {}

func (d *localDriver) startForward(context.Context, uuid.UUID, ...string) error {
	return errors.New("runtime local has no port forwarder")
}

// setLinks is a no-op: Local has no netns and no Links (runtime.md §15).
func (d *localDriver) setLinks(context.Context, []*nodev1.SandboxLink) error { return nil }

func (d *localDriver) close() {}

func (d *localDriver) runtimeAlive(ctx context.Context, sandboxID uuid.UUID) (bool, bool) {
	active, known, _ := localUnitState(ctx, localPlaceholderService(sandboxID))
	return active, known
}
