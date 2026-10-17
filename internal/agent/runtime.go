package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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
	supervisorBin    = "/.stars/bin/supervisor"
	workspaceMount   = "/workspace"
	runMount         = "/.stars/run"
)

// Runtime is the Docker adapter for this Node (runtime.md §2, §9). Its data
// root holds the Workspace directories and per-Sandbox Supervisor records.
type Runtime struct {
	d        *dockerClient
	dataRoot string

	// Execs are launched from the Control stream; the output stream is opened
	// per Exec against the same connection.
	execStream func(context.Context) (nodev1.Node_ExecOutputClient, error)
	send       func(*nodev1.AgentMessage)
	execMu     sync.Mutex
	execs      map[string]struct{} // exec id -> output delivery in flight

	mu      sync.Mutex
	managed map[string]int64 // sandbox id -> generation known to run
}

func NewRuntime(socket, dataRoot string) *Runtime {
	return &Runtime{
		d:        &dockerClient{http: unixHTTPClient(socket)},
		dataRoot: dataRoot,
		execs:    make(map[string]struct{}),
		managed:  make(map[string]int64),
	}
}

// PrepareDataRoot creates the directories the mount table needs and stages the
// Supervisor binary at <dataRoot>/bin/supervisor (runtime.md §2).
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
	bin := filepath.Join(root, "bin", "supervisor")
	tmp := bin + ".tmp"
	if err := copyFile(self, tmp); err != nil {
		return fmt.Errorf("stage Supervisor binary: %w", err)
	}
	return os.Rename(tmp, bin)
}

func workspacePath(root string, id uuid.UUID) string {
	return filepath.Join(root, "workspaces", id.String())
}

func sandboxRunPath(root string, id uuid.UUID) string {
	return filepath.Join(root, "sandboxes", id.String(), "run")
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
	if err := r.d.ensureImageRef(ctx, cmd.imageDigest); err != nil {
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

	mode := cmd.workspaceMode
	if mode != "rw" && mode != "ro" {
		mode = "rw"
	}
	nanoCPUs := int64(cmd.cpu * 1e9)
	if nanoCPUs < 1e6 {
		nanoCPUs = 1e6
	}
	hostConfig := map[string]any{
		"CgroupnsMode": "private",
		"Init":         false,
		"Binds": []string{
			workspace + ":" + workspaceMount + ":" + mode,
			filepath.Join(r.dataRoot, "bin") + ":/.stars/bin:ro",
			runDir + ":" + runMount + ":rw",
		},
		"NanoCpus":  nanoCPUs,
		"Memory":    cmd.memory,
		"PidsLimit": sandboxPidsLimit,
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
	if !limitsMatch(result, nanoCPUs, cmd.memory) {
		r.d.removeContainer(context.WithoutCancel(ctx), created.ID) //nolint:errcheck // best effort
		return "runtime_create_failed", fmt.Errorf("probe readback does not match the request: %+v", result)
	}
	r.track(id, cmd.generation)
	return "", nil
}

func limitsMatch(r supervisor.Result, nanoCPUs, memory int64) bool {
	// Docker expresses CPU as quota/period in microseconds with a fixed
	// 100000 us period.
	return r.CPUMax == fmt.Sprintf("%d 100000", nanoCPUs/10_000) &&
		r.MemoryMax == strconv.FormatInt(memory, 10) &&
		r.PidsMax == strconv.FormatInt(sandboxPidsLimit, 10) &&
		r.WorkspaceWritable
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
			if gen, tracked := r.take(id); tracked {
				report(id, gen)
			}
		}
		resp.Body.Close()
	}
}

// take removes a tracked Sandbox and returns the generation it was running at.
func (r *Runtime) take(id string) (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	gen, ok := r.managed[id]
	if ok {
		delete(r.managed, id)
	}
	return gen, ok
}

func (r *Runtime) track(id uuid.UUID, generation int64) {
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

// ensureImageRef pulls by the pinned reference when it is not local.
func (d *dockerClient) ensureImageRef(ctx context.Context, ref string) error {
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
	query := url.Values{"fromImage": {ref}}
	resp, err = d.request(ctx, http.MethodPost, d.api("/images/create")+"?"+query.Encode(), nil)
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
}
