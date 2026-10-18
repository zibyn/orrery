package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
	"github.com/zibyn/stars-sandbox/internal/supervisor"
)

const (
	// sandboxPidsLimit is the fixed PID ceiling for every Sandbox (runtime.md
	// §9; defaults.md does not name a value).
	sandboxPidsLimit = int64(2048)
	binaryDir        = "/.stars/bin"
	supervisorBin    = binaryDir + "/supervisor"
	workspaceMount   = "/workspace"
	runMount         = "/.stars/run"
	// recordSettle is how long a startup reconciliation waits for a
	// just-created Supervisor to listen before calling it dead (runtime.md §8).
	recordSettle = time.Second
)

// Runtime is the Docker adapter for this Node (runtime.md §2, §9). Its data
// root holds the Workspace directories and per-Sandbox Supervisor records.
type Runtime struct {
	d        *dockerClient
	dataRoot string

	// Execs are launched from the Control stream; the output stream is opened
	// per Exec against the same connection.
	execStream     func(context.Context) (nodev1.Node_ExecOutputClient, error)
	tunnelStream   func(context.Context) (nodev1.Node_TunnelClient, error)
	send           func(*nodev1.AgentMessage)
	execMu         sync.Mutex
	execs          map[string]struct{} // exec id -> output delivery in flight
	ptyAttachments map[string]int      // exec id -> live tunnel attachments

	// client makes one-shot Node RPCs outside the Control stream (Presign).
	client nodev1.NodeClient

	registryMu sync.Mutex
	registry   *registryCredential

	mu      sync.Mutex
	managed map[string]int64 // sandbox id -> generation known to run
}

func NewRuntime(socket, dataRoot string) *Runtime {
	return &Runtime{
		d:              &dockerClient{http: unixHTTPClient(socket)},
		dataRoot:       dataRoot,
		execs:          make(map[string]struct{}),
		ptyAttachments: make(map[string]int),
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
	cred, err := parseRegistryCredential(raw)
	if err != nil {
		slog.Warn("registry credential from HelloAck is invalid", "error", err)
	}
	r.registryMu.Lock()
	r.registry = cred
	r.registryMu.Unlock()
}

func (r *Runtime) registryAuthHeader(ref string) string {
	r.registryMu.Lock()
	defer r.registryMu.Unlock()
	return r.registry.authHeader(ref)
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
	if err := stageBinary(filepath.Join(filepath.Dir(self), "stars-sandbox"),
		filepath.Join(root, "bin", "stars-sandbox")); err != nil {
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

func workspacePath(root string, id uuid.UUID) string {
	return filepath.Join(root, "workspaces", id.String())
}

func sandboxPath(root string, id uuid.UUID) string {
	return filepath.Join(root, "sandboxes", id.String())
}

func sandboxRunPath(root string, id uuid.UUID) string {
	return filepath.Join(sandboxPath(root, id), "run")
}

func containerName(id uuid.UUID) string { return "stars-sandbox-" + id.String() }

func (r *Runtime) provisionWorkspace(workspaceID uuid.UUID) error {
	path := workspacePath(r.dataRoot, workspaceID)
	if err := os.MkdirAll(path, 0o777); err != nil {
		return err
	}
	// MkdirAll is subject to umask; the Sandbox user must be able to write.
	return os.Chmod(path, 0o777)
}

func (r *Runtime) deleteWorkspace(workspaceID uuid.UUID) error {
	// uuid.String() is the only thing that reaches the path, so a malformed
	// request cannot escape the data root.
	return os.RemoveAll(workspacePath(r.dataRoot, workspaceID))
}

// createSandbox is idempotent: an existing running container is adopted, a
// stopped one is replaced. It returns a Sandbox lifecycle cause on failure.
func (r *Runtime) createSandbox(ctx context.Context, cmd *createSandboxSpec) (string, error) {
	id, err := uuid.Parse(cmd.sandboxID)
	if err != nil {
		return "runtime_create_failed", err
	}
	workspaceID, err := uuid.Parse(cmd.workspaceID)
	if err != nil {
		return "runtime_create_failed", err
	}
	if err := r.d.negotiate(ctx); err != nil {
		return "runtime_create_failed", err
	}
	if err := r.d.ensureImageRef(ctx, cmd.imageDigest, r.registryAuthHeader(cmd.imageDigest)); err != nil {
		return "image_pull_failed", err
	}
	workspace := workspacePath(r.dataRoot, workspaceID)
	if err := os.MkdirAll(workspace, 0o777); err != nil {
		return "runtime_create_failed", err
	}
	if err := os.Chmod(workspace, 0o777); err != nil {
		return "runtime_create_failed", err
	}
	// The image's default USER writes here; rootful and rootless map that uid
	// differently, so the Agent cannot chown it correctly without knowing the
	// image. ponytail: world-writable, per-uid chown when USERs become common.
	runDir := sandboxRunPath(r.dataRoot, id)
	if err := os.MkdirAll(runDir, 0o777); err != nil {
		return "runtime_create_failed", err
	}
	if err := os.Chmod(runDir, 0o777); err != nil {
		return "runtime_create_failed", err
	}
	mode := cmd.workspaceMode
	if mode != "rw" && mode != "ro" {
		mode = "rw"
	}
	// Recorded before adoption so the file API survives an Agent restart.
	if err := r.writeWorkspaceRefFile(id, workspaceRef{ID: workspaceID.String(), Mode: mode}); err != nil {
		return "runtime_create_failed", err
	}
	name := containerName(id)

	if running, exists, err := r.d.inspectContainer(ctx, name); err == nil && exists {
		if running {
			r.track(id, cmd.generation)
			return "", nil // adopted after an Agent restart or a lost reply
		}
		if err := r.d.removeContainer(ctx, name); err != nil {
			return "runtime_create_failed", err
		}
	}
	// Resource trees are fetched only when a runtime is actually built. A
	// resend of the same command after a lost reply adopts the running
	// container instead of downloading again.
	resourcePaths, cause, err := r.resourceBinds(ctx, cmd.resources)
	if err != nil {
		return cause, err
	}
	nanoCPUs := int64(cmd.cpu * 1e9)
	if nanoCPUs < 1e6 {
		nanoCPUs = 1e6
	}
	binds := []string{
		workspace + ":" + workspaceMount + ":" + mode,
		filepath.Join(r.dataRoot, "bin") + ":/.stars/bin:ro",
		runDir + ":" + runMount + ":rw",
	}
	binds = append(binds, resourcePaths...)
	hostConfig := map[string]any{
		"CgroupnsMode": "private",
		"Init":         false,
		"Binds":        binds,
		"NanoCpus":     nanoCPUs,
		"Memory":       cmd.memory,
		"PidsLimit":    sandboxPidsLimit,
	}
	if requests := cdiDeviceRequest(cmd.gpuUUIDs); requests != nil {
		hostConfig["DeviceRequests"] = requests
	}
	create := map[string]any{
		"Image":      cmd.imageDigest,
		"Entrypoint": []string{supervisorBin},
		"Cmd":        []string{"init"},
		"WorkingDir": workspaceMount,
		"Labels": map[string]string{
			"stars.sandbox_id": id.String(),
			"stars.generation": strconv.FormatInt(cmd.generation, 10),
			"stars.managed":    "true",
		},
		"HostConfig": hostConfig,
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := r.d.do(ctx, http.MethodPost, r.d.api("/containers/create")+"?name="+name, create, &created); err != nil {
		return "runtime_create_failed", fmt.Errorf("create container: %w", err)
	}
	if err := r.d.do(ctx, http.MethodPost, r.d.api("/containers/"+created.ID+"/start"), nil, nil); err != nil {
		r.d.removeContainer(context.WithoutCancel(ctx), created.ID) //nolint:errcheck // best effort
		return "runtime_create_failed", fmt.Errorf("start container: %w", err)
	}
	result, err := r.d.execProbe(ctx, created.ID)
	if err != nil {
		r.d.removeContainer(context.WithoutCancel(ctx), created.ID) //nolint:errcheck // best effort
		return "runtime_create_failed", fmt.Errorf("probe: %w", err)
	}
	if !limitsMatch(result, nanoCPUs, cmd.memory, len(cmd.gpuUUIDs), mode) {
		r.d.removeContainer(context.WithoutCancel(ctx), created.ID) //nolint:errcheck // best effort
		return "runtime_create_failed", fmt.Errorf("probe readback does not match the request: %+v", result)
	}
	r.track(id, cmd.generation)
	return "", nil
}

func limitsMatch(r supervisor.Result, nanoCPUs, memory int64, gpus int, workspaceMode string) bool {
	// The projected mount is part of the request: a read-write Attachment must
	// be writable and a read-only one must not be.
	workspaceOK := r.WorkspaceWritable == (workspaceMode != "ro")
	// Docker expresses CPU as quota/period in microseconds with a fixed
	// 100000 us period.
	return r.CPUMax == fmt.Sprintf("%d 100000", nanoCPUs/10_000) &&
		r.MemoryMax == strconv.FormatInt(memory, 10) &&
		r.PidsMax == strconv.FormatInt(sandboxPidsLimit, 10) &&
		numberedDeviceCount(r.NvidiaDevices) == gpus &&
		workspaceOK
}

func (r *Runtime) stopSandbox(ctx context.Context, sandboxID uuid.UUID) error {
	if err := r.d.negotiate(ctx); err != nil {
		return err
	}
	name := containerName(sandboxID)
	resp, err := r.d.request(ctx, http.MethodDelete,
		r.d.api("/containers/"+name)+"?force=true&v=true", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck // drain for reuse
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode < 300 {
		r.forget(sandboxID.String())
		return nil
	}
	return fmt.Errorf("remove container: %s", resp.Status)
}

// scan reports a Sandbox whose runtime died while the Node stayed up.
func (r *Runtime) scan(ctx context.Context, report func(sandboxID string, generation int64)) {
	if err := r.d.negotiate(ctx); err != nil {
		return
	}
	r.mu.Lock()
	known := make(map[string]int64, len(r.managed))
	for id, gen := range r.managed {
		known[id] = gen
	}
	r.mu.Unlock()
	for id, gen := range known {
		sandboxID, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		running, exists, err := r.d.inspectContainer(ctx, containerName(sandboxID))
		if err != nil {
			continue
		}
		if !exists || !running {
			r.forget(id)
			report(id, gen)
		}
	}
}

// adopt picks up containers from a previous Agent process. The Control Plane
// also re-derives every command, so this only avoids false "gone" reports.
func (r *Runtime) adopt(ctx context.Context, report func(sandboxID string, generation int64)) {
	if err := r.d.negotiate(ctx); err != nil {
		return
	}
	var containers []struct {
		Labels map[string]string `json:"Labels"`
	}
	filters := url.QueryEscape(`{"label":["stars.managed=true"]}`)
	if err := r.d.do(ctx, http.MethodGet, r.d.api("/containers/json?all=true&filters="+filters), nil, &containers); err != nil {
		return
	}
	for _, c := range containers {
		id := c.Labels["stars.sandbox_id"]
		gen, err := strconv.ParseInt(c.Labels["stars.generation"], 10, 64)
		if err != nil {
			continue
		}
		sandboxID, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		running, exists, err := r.d.inspectContainer(ctx, containerName(sandboxID))
		if err != nil || !exists || !running {
			continue
		}
		r.track(sandboxID, gen)
		report(id, gen)
	}
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
			if !socketUp(dir) && !waitSocket(ctx, dir) {
				// A canceled context means this connection is going away, not
				// that the Supervisor died: settle on the next one.
				if ctx.Err() != nil {
					return
				}
				if _, err := os.Stat(filepath.Join(dir, "exit")); err != nil {
					writeRuntimeLost(dir)
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
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, "exit")); err == nil {
			continue
		}
		writeRuntimeLost(dir)
	}
}

func writeRuntimeLost(dir string) {
	if err := supervisor.WriteExit(dir, supervisor.Exit{
		Cause: "runtime_lost", FinishedAt: time.Now().UTC(),
	}); err != nil {
		slog.Warn("write runtime_lost record", "dir", dir, "error", err)
	}
}

// runtimeAlive reports the container's state. known is false when the daemon
// cannot be asked; that is not evidence the Runtime is gone.
func (r *Runtime) runtimeAlive(ctx context.Context, id uuid.UUID) (alive, known bool) {
	if err := r.d.negotiate(ctx); err != nil {
		return false, false
	}
	running, exists, err := r.d.inspectContainer(ctx, containerName(id))
	if err != nil {
		return false, false
	}
	return exists && running, true
}

func socketUp(dir string) bool {
	conn, err := dialSocket(filepath.Join(dir, "ctl.sock"))
	if err != nil {
		return false
	}
	conn.Close() //nolint:errcheck // the liveness check is the dial
	return true
}

// waitSocket gives a just-created Supervisor time to listen, and stops early
// when it exits instead.
func waitSocket(ctx context.Context, dir string) bool {
	deadline := time.Now().Add(recordSettle)
	for {
		if socketUp(dir) {
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

// watchEvents reports a Sandbox whose container died without a Stop command
// (runtime.md §9); the periodic scan is the fallback when the stream breaks.
func (r *Runtime) watchEvents(ctx context.Context, report func(sandboxID string, generation int64)) {
	filters := url.QueryEscape(`{"type":["container"],"event":["die"],"label":["stars.managed=true"]}`)
	for ctx.Err() == nil {
		if err := r.d.negotiate(ctx); err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		resp, err := r.d.request(ctx, http.MethodGet, r.d.api("/events")+"?filters="+filters, nil)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		dec := json.NewDecoder(resp.Body)
		for {
			var event struct {
				Actor struct {
					Attributes map[string]string `json:"Attributes"`
				} `json:"Actor"`
			}
			if err := dec.Decode(&event); err != nil {
				break
			}
			id := event.Actor.Attributes["stars.sandbox_id"]
			gen, err := strconv.ParseInt(event.Actor.Attributes["stars.generation"], 10, 64)
			if err != nil {
				continue
			}
			// A die event from a replaced container must not end the Execs of
			// the generation running now.
			if gen, tracked := r.takeIf(id, gen); tracked {
				report(id, gen)
			}
		}
		resp.Body.Close()
	}
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

func (d *dockerClient) inspectContainer(ctx context.Context, name string) (running, exists bool, err error) {
	resp, err := d.request(ctx, http.MethodGet, d.api("/containers/"+name+"/json"), nil)
	if err != nil {
		return false, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck // drain for reuse
		return false, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, false, fmt.Errorf("inspect container: %s", resp.Status)
	}
	var info struct {
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return false, false, err
	}
	return info.State.Running, true, nil
}

func (d *dockerClient) removeContainer(ctx context.Context, idOrName string) error {
	resp, err := d.request(ctx, http.MethodDelete,
		d.api("/containers/"+idOrName)+"?force=true&v=true", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck // drain for reuse
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("remove container: %s", resp.Status)
}

// execProbe runs `supervisor probe` inside the Sandbox and decodes its JSON
// readback (runtime.md §9). A plain attached exec returns the multiplexed
// stdout when it is not detached.
func (d *dockerClient) execProbe(ctx context.Context, containerID string) (supervisor.Result, error) {
	create := map[string]any{
		"AttachStdout": true,
		"AttachStderr": true,
		"Cmd":          []string{supervisorBin, "probe", workspaceMount},
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := d.do(ctx, http.MethodPost, d.api("/containers/"+containerID+"/exec"), create, &created); err != nil {
		return supervisor.Result{}, fmt.Errorf("create exec: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := d.request(ctx, http.MethodPost, d.api("/exec/"+created.ID+"/start"),
		map[string]any{"Detach": false, "Tty": false})
	if err != nil {
		return supervisor.Result{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if err != nil {
		return supervisor.Result{}, err
	}
	var state struct {
		Running  bool  `json:"Running"`
		ExitCode int64 `json:"ExitCode"`
	}
	if err := d.do(context.WithoutCancel(ctx), http.MethodGet, d.api("/exec/"+created.ID+"/json"), nil, &state); err != nil {
		return supervisor.Result{}, err
	}
	output := demux(raw)
	if state.Running {
		return supervisor.Result{}, errors.New("probe did not finish")
	}
	if state.ExitCode != 0 {
		return supervisor.Result{}, fmt.Errorf("probe exited %d: %s", state.ExitCode, output)
	}
	var result supervisor.Result
	if err := json.Unmarshal(output, &result); err != nil {
		return supervisor.Result{}, fmt.Errorf("decode probe: %w", err)
	}
	return result, nil
}

// ensureImageRef pulls by the pinned reference when it is not local. auth is
// the X-Registry-Auth value for the image's registry, or "" for Docker Hub.
func (d *dockerClient) ensureImageRef(ctx context.Context, ref, auth string) error {
	resp, err := d.request(ctx, http.MethodGet, d.api("/images/"+url.PathEscape(ref)+"/json"), nil)
	if err == nil {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck // drain for reuse
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return nil
		}
		if resp.StatusCode != http.StatusNotFound {
			return fmt.Errorf("inspect image: %s", resp.Status)
		}
	}
	headers := map[string]string{}
	if auth != "" {
		headers["X-Registry-Auth"] = auth
	}
	query := url.Values{"fromImage": {ref}}
	resp, err = d.requestHeaders(ctx, http.MethodPost, d.api("/images/create")+"?"+query.Encode(), nil, headers)
	if err != nil {
		return fmt.Errorf("pull image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("pull image: %s: %s", resp.Status, raw)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

// createSandboxSpec is the subset of CreateSandbox the Docker adapter uses.
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
}

// resourceMount is one version to fetch and mount at /resources/<mount>.
type resourceMount struct {
	address string
	mount   string
}
