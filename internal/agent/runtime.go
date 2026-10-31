package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/registry"
	"github.com/zibyn/orrery/internal/supervisor"
)

const (
	// sandboxPidsLimit is the fixed PID ceiling for every Sandbox (runtime.md
	// §9; defaults.md does not name a value).
	sandboxPidsLimit = int64(2048)
	binaryDir        = "/.orrery/bin"
	supervisorBin    = binaryDir + "/supervisor"
	workspaceMount   = "/workspace"
	runMount         = "/.orrery/run"
	// recordSettle is how long a startup reconciliation waits for a
	// just-created Supervisor to listen before calling it dead (runtime.md §8).
	recordSettle = time.Second
)

// Runtime is this Node's Runtime Provider (runtime.md §2, §9). Its data root
// holds the Workspace directories and per-Sandbox Supervisor records.
type Runtime struct {
	dataRoot string
	// nodeID is this Agent's Node identity. Every Runtime instance carries it
	// as orrery.node so two Agents sharing a containerd never touch each
	// other's containers (runtime.md §8).
	nodeID string
	// provider is the Node's single Runtime Provider (node-protocol.md).
	provider string
	// driver is the Provider-specific half of the Runtime contract (ADR-0009):
	// container lifecycle/Exec/self-check for containerd, systemd for Local.
	// Host-path work (Workspaces, records, files, publish, resources, tunnels)
	// stays on Runtime and is shared.
	driver runtimeDriver
	// owner maps host paths to the Local Runtime user; nil for containerd,
	// whose container root maps to host root.
	owner *rootlessOwner
	// buildkit is the Agent-configured BuildKit endpoint; empty means none.
	buildkit string
	// mesh is the Node Mesh interface; nil when the Node has none.
	mesh *mesh
	// hostCgroup is the host cgroupfs root on a runsc Node, where the Agent
	// attributes oom_killed (runtime.md §9); empty elsewhere.
	hostCgroup string

	// Execs are launched from the Control stream; the output stream is opened
	// per Exec against the same connection.
	execStream     func(context.Context) (nodev1.Node_ExecOutputClient, error)
	tunnelStream   func(context.Context) (nodev1.Node_TunnelClient, error)
	send           func(*nodev1.AgentMessage)
	execMu         sync.Mutex
	execs          map[string]struct{} // exec id -> output delivery in flight
	ptyAttachments map[string]int      // exec id -> live tunnel attachments

	// buildCancels aborts a running build by Exec id. CancelExec reaches
	// builds here instead of a Supervisor socket.
	buildMu      sync.Mutex
	buildCancels map[string]context.CancelCauseFunc
	buildDone    map[string]struct{}

	// client makes one-shot Node RPCs outside the Control stream (Presign).
	client nodev1.NodeClient

	registryMu sync.Mutex
	registry   *registry.Credential

	// Egress Rules from the last HelloAck, applied at each Create.
	egressMu    sync.Mutex
	egressAllow []string
	egressDeny  []string
	// egressEnforced is this Agent's last reported egress_policy.
	egressEnforced bool

	mu      sync.Mutex
	managed map[string]int64 // sandbox id -> generation known to run
}

func newRuntime(provider, dataRoot, nodeID string) *Runtime {
	return &Runtime{
		dataRoot:       dataRoot,
		nodeID:         nodeID,
		provider:       provider,
		execs:          make(map[string]struct{}),
		ptyAttachments: make(map[string]int),
		buildCancels:   make(map[string]context.CancelCauseFunc),
		buildDone:      make(map[string]struct{}),
		managed:        make(map[string]int64),
	}
}

// connect attaches the Runtime to the live Control stream. Commands arrive only
// after this call, so both callbacks are set together.
func (r *Runtime) connect(client nodev1.NodeClient, send func(*nodev1.AgentMessage)) {
	r.client = client
	r.send = send
	r.execStream = func(ctx context.Context) (nodev1.Node_ExecOutputClient, error) {
		return client.ExecOutput(ctx)
	}
	r.tunnelStream = func(ctx context.Context) (nodev1.Node_TunnelClient, error) {
		return client.Tunnel(ctx)
	}
}

// setRegistryCredential stores the deployment credential from HelloAck. A
// malformed document leaves the Runtime unauthenticated and is logged.
func (r *Runtime) setRegistryCredential(raw string) {
	var cred *registry.Credential
	if raw != "" {
		var err error
		cred, err = registry.ParseCredential(raw)
		if err != nil {
			slog.Warn("registry credential from HelloAck is invalid", "error", err)
			cred = nil
		}
	}
	r.registryMu.Lock()
	r.registry = cred
	r.registryMu.Unlock()
}

func (r *Runtime) registryAuthHeader(ref string) string {
	r.registryMu.Lock()
	defer r.registryMu.Unlock()
	return authHeader(r.registry, ref)
}

// presign asks the Control Plane for object URLs (node-protocol.md "Presign").
func (r *Runtime) presign(ctx context.Context, req *nodev1.PresignRequest) (*nodev1.PresignResponse, error) {
	if r.client == nil {
		return nil, errors.New("the Node is not connected")
	}
	return r.client.Presign(ctx, req)
}

// PrepareDataRoot creates the directories the mount table needs and stages the
// Supervisor binary and the external CLI at <dataRoot>/bin (runtime.md §2).
func PrepareDataRoot(root string) error {
	for _, dir := range []string{
		filepath.Join(root, "bin"),
		filepath.Join(root, "workspaces"),
		filepath.Join(root, "sandboxes"),
		filepath.Join(root, "builds"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create data root: %w", err)
		}
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := stageBinary(self, filepath.Join(root, "bin", "supervisor")); err != nil {
		return fmt.Errorf("stage Supervisor binary: %w", err)
	}
	// The Node deployment package places the external CLI next to the Agent.
	if err := stageBinary(filepath.Join(filepath.Dir(self), "orrery"),
		filepath.Join(root, "bin", "orrery")); err != nil {
		return fmt.Errorf("stage external CLI: %w", err)
	}
	return nil
}

// stageBinary installs src at dst atomically, so an interrupted copy never
// leaves a half-written binary in the read-only mount.
func stageBinary(src, dst string) error {
	tmp := dst + ".tmp"
	if err := copyFile(src, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // read-only
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close() //nolint:errcheck // report the copy error instead
		return err
	}
	return out.Close()
}

func workspacePath(root string, id uuid.UUID) string {
	return filepath.Join(root, "workspaces", id.String())
}

func sandboxPath(root string, id uuid.UUID) string {
	return filepath.Join(root, "sandboxes", id.String())
}

func sandboxRunPath(root string, id uuid.UUID) string {
	return filepath.Join(sandboxPath(root, id), "run")
}

func containerName(id uuid.UUID) string { return "orrery-" + id.String() }

// Runtime container label keys (runtime.md §8).
const (
	labelSandboxID  = "orrery.sandbox_id"
	labelGeneration = "orrery.generation"
	labelManaged    = "orrery.managed"
	labelNode       = "orrery.node"
)

// containerOwnedBy reports whether a container belongs to this Node. A
// container carrying orrery.managed without this Node's label was left by a
// previous identity (or another Node on the same daemon) and must not be
// adopted (runtime.md §8).
func containerOwnedBy(labels map[string]string, nodeID string) bool {
	return labels[labelManaged] == "true" && nodeID != "" && labels[labelNode] == nodeID
}

// createAdopts reports whether a Create may take over the running Runtime it
// finds under its name: this Node owns it, at the requested generation.
func createAdopts(labels map[string]string, nodeID string, generation int64) bool {
	return containerOwnedBy(labels, nodeID) && labels[labelGeneration] == strconv.FormatInt(generation, 10)
}

func (r *Runtime) provisionWorkspace(workspaceID uuid.UUID) error {
	path := workspacePath(r.dataRoot, workspaceID)
	if err := os.MkdirAll(path, 0o777); err != nil {
		return err
	}
	// MkdirAll is subject to umask; the Sandbox user must be able to write.
	if err := os.Chmod(path, 0o777); err != nil {
		return err
	}
	// A Local Runtime's commands run as its unprivileged user (runtime.md §10).
	return r.owner.own(path)
}

func (r *Runtime) deleteWorkspace(workspaceID uuid.UUID) error {
	// uuid.String() is the only thing that reaches the path, so a malformed
	// request cannot escape the data root.
	return os.RemoveAll(workspacePath(r.dataRoot, workspaceID))
}

// prepareSandboxHost lays out the host side the containerd Provider mounts:
// the Workspace, the record directory, the file API's Workspace record and
// the eviction pins (runtime.md §2, §11). It returns the effective mode.
func (r *Runtime) prepareSandboxHost(id, workspaceID uuid.UUID, cmd *createSandboxSpec) (workspace, runDir, mode string, err error) {
	workspace = workspacePath(r.dataRoot, workspaceID)
	if err := os.MkdirAll(workspace, 0o777); err != nil {
		return "", "", "", err
	}
	if err := os.Chmod(workspace, 0o777); err != nil {
		return "", "", "", err
	}
	// The image's default USER writes here, and the Agent cannot chown it
	// correctly without knowing the image. ponytail: world-writable, per-uid
	// chown when USERs become common.
	runDir = sandboxRunPath(r.dataRoot, id)
	if err := os.MkdirAll(runDir, 0o777); err != nil {
		return "", "", "", err
	}
	if err := os.Chmod(runDir, 0o777); err != nil {
		return "", "", "", err
	}
	mode = cmd.workspaceMode
	if mode != "rw" && mode != "ro" {
		mode = "rw"
	}
	// Recorded before adoption so the file API survives an Agent restart.
	if err := r.writeWorkspaceRefFile(id, workspaceRef{ID: workspaceID.String(), Mode: mode}); err != nil {
		return "", "", "", err
	}
	// The pin record is written before the trees are fetched: eviction must
	// never mistake a tree being assembled for this Sandbox for garbage. It
	// survives StopSandbox (a stopped Sandbox still references these) and is
	// only dropped when the Sandbox is known to be gone; see sandboxPins.
	pins := sandboxPins{ImageDigest: cmd.imageDigest}
	for _, resource := range cmd.resources {
		pins.Resources = append(pins.Resources, resource.address)
	}
	if err := writeSandboxPins(sandboxPath(r.dataRoot, id), pins); err != nil {
		return "", "", "", err
	}
	// Any create or adopt counts as a use of the image for LRU eviction
	// (runtime.md §11).
	touchImageUsage(r.dataRoot, cmd.imageDigest)
	return workspace, runDir, mode, nil
}

// reconcileRecords applies runtime.md §8 to the records a previous Agent
// process left behind. It runs after every (re)connect: finished Execs are
// pushed again, running ones resume from stored_seq, and an Exec whose
// Supervisor or Runtime died gets a runtime_lost exit. A Sandbox whose
// Runtime is gone is reported so the Control Plane stops it and releases its
// resources (ADR-0017).
func (r *Runtime) reconcileRecords(ctx context.Context, report func(sandboxID string, generation int64)) {
	entries, err := os.ReadDir(filepath.Join(r.dataRoot, "sandboxes"))
	if err != nil {
		return
	}
	for _, entry := range entries {
		sandboxID, err := uuid.Parse(entry.Name())
		if err != nil {
			continue
		}
		// A build Sandbox has no container. Its build marker is resumed by
		// resumeBuilds; treating the missing Runtime as lost would settle the
		// build Exec with the wrong cause.
		if _, err := os.Stat(filepath.Join(sandboxPath(r.dataRoot, sandboxID), "build")); err == nil {
			continue
		}
		runDir := sandboxRunPath(r.dataRoot, sandboxID)
		dirs, _ := filepath.Glob(filepath.Join(runDir, "*"))
		alive, known := r.runtimeAlive(ctx, sandboxID)
		if known && !alive {
			r.finalizeLost(sandboxID.String())
			// A missing generation file predates this Agent; without it the
			// Control Plane's fence rejects the observation, so say nothing.
			if generation, ok := readGeneration(sandboxPath(r.dataRoot, sandboxID)); ok {
				report(sandboxID.String(), generation)
			}
		}
		for _, dir := range dirs {
			execID, err := uuid.Parse(filepath.Base(dir))
			if err != nil {
				continue
			}
			// An unknown Runtime state is not proof the Exec stopped, so only a
			// daemon that answered may settle the record.
			go r.reconcileRecord(ctx, sandboxID, execID, dir, alive)
		}
	}
}

// reconcileRecord settles one Exec record and delivers it. With the Runtime
// alive and no exit, a dead Supervisor is the only explanation left once the
// socket has had a moment to appear (a Supervisor listens before it waits for
// its spec).
func (r *Runtime) reconcileRecord(ctx context.Context, sandboxID, execID uuid.UUID, dir string, runtimeAlive bool) {
	if raw, err := os.ReadFile(filepath.Join(dir, "started")); err == nil {
		// The observation is lost whenever the Control stream was down; the
		// record still knows the truth.
		r.sendExecObserved(execID.String(), string(raw))
	}
	if runtimeAlive {
		if _, err := os.Stat(filepath.Join(dir, "exit")); err != nil {
			if !r.supervisorSocketUp(dir, execID) && !r.waitSupervisorSocket(ctx, dir, execID) {
				// A canceled context means this connection is going away, not
				// that the Supervisor died: settle on the next one.
				if ctx.Err() != nil {
					return
				}
				if _, err := os.Stat(filepath.Join(dir, "exit")); err != nil {
					r.writeRuntimeLost(sandboxID, dir)
				}
			}
		}
	}
	r.streamExec(ctx, sandboxID, execID, dir)
}

// finalizeLost ends every unfinished Exec of a Sandbox whose Runtime is gone
// (runtime.md §8). Deliveries already in flight pick the exit up; the startup
// scan streams the rest.
func (r *Runtime) finalizeLost(sandboxID string) {
	dirs, _ := filepath.Glob(filepath.Join(r.dataRoot, "sandboxes", sandboxID, "run", "*"))
	id, _ := uuid.Parse(sandboxID) // a bad id names no cgroup, so nothing is attributed
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, "exit")); err == nil {
			continue
		}
		r.writeRuntimeLost(id, dir)
	}
}

func (r *Runtime) writeRuntimeLost(sandboxID uuid.UUID, dir string) {
	exit := supervisor.Exit{Cause: "runtime_lost", FinishedAt: time.Now().UTC()}
	exit.Cause = hostOOMCause(exit, r.hostOOMRose(sandboxID, dir))
	if err := supervisor.WriteExit(dir, exit); err != nil {
		slog.Warn("write runtime_lost record", "dir", dir, "error", err)
	}
}

// ctlSocket is where the Supervisor listens. A container mounts the record
// directory at /.orrery/run, so its socket is short; Local Runtime keeps the
// record on the host, where <data-root>/sandboxes/<id>/run/<exec>/ctl.sock can
// exceed unix sockets' 108-byte sun_path, so it uses an abstract name
// (runtime.md §10).
func (r *Runtime) ctlSocket(dir string, execID uuid.UUID) string {
	if r.provider == ProviderLocal {
		return localCtlSocket(execID)
	}
	return filepath.Join(dir, "ctl.sock")
}

func (r *Runtime) supervisorSocketUp(dir string, execID uuid.UUID) bool {
	conn, err := dialSocket(r.ctlSocket(dir, execID))
	if err != nil {
		return false
	}
	conn.Close() //nolint:errcheck // the liveness check is the dial
	return true
}

// waitSupervisorSocket gives a just-created Supervisor time to listen, and
// stops early when it exits instead.
func (r *Runtime) waitSupervisorSocket(ctx context.Context, dir string, execID uuid.UUID) bool {
	deadline := time.Now().Add(recordSettle)
	for {
		if r.supervisorSocketUp(dir, execID) {
			return true
		}
		if _, err := os.Stat(filepath.Join(dir, "exit")); err == nil {
			return false
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// writeGeneration records the Sandbox generation beside its run record.
func writeGeneration(dir string, generation int64) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "generation.tmp")
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(generation, 10)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "generation"))
}

func readGeneration(dir string) (int64, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "generation"))
	if err != nil {
		return 0, false
	}
	generation, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	return generation, err == nil
}

// takeIf removes a tracked Sandbox only when the event belongs to the
// generation it is running; a delayed die from a replaced container is
// ignored. It returns the generation it was running at.
func (r *Runtime) takeIf(id string, generation int64) (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	gen, ok := r.managed[id]
	if !ok || gen != generation {
		return 0, false
	}
	delete(r.managed, id)
	return gen, true
}

func (r *Runtime) track(id uuid.UUID, generation int64) {
	// Only a Sandbox that reached running may be reported gone, so the marker
	// is written here and not while the container is still being created: a
	// restart mid-create must not fence the new generation off as dead.
	if err := writeGeneration(sandboxPath(r.dataRoot, id), generation); err != nil {
		slog.Warn("record sandbox generation", "sandbox_id", id, "error", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.managed[id.String()] = generation
}

func (r *Runtime) forget(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.managed, id)
}

// managedSnapshot copies the tracked Sandboxes so a liveness scan can call out
// without holding the lock.
func (r *Runtime) managedSnapshot() map[string]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	known := make(map[string]int64, len(r.managed))
	for id, generation := range r.managed {
		known[id] = generation
	}
	return known
}

// createSandboxSpec is the subset of CreateSandbox the drivers use.
type createSandboxSpec struct {
	sandboxID     string
	generation    int64
	imageDigest   string
	cpu           float64
	memory        int64
	workspaceID   string
	workspaceMode string
	gpuUUIDs      []string
	resources     []resourceMount
	egress        egressMode
}

// resourceMount is one version to fetch and mount at /resources/<mount>.
type resourceMount struct {
	address string
	mount   string
}
