//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type sandboxView struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels"`
	State     string            `json:"state"`
	Cause     *string           `json:"cause"`
	NodeID    *string           `json:"node_id"`
	Snapshot  string            `json:"snapshot"`
	CPU       float64           `json:"cpu"`
	Memory    int64             `json:"memory"`
	BlockedBy *string           `json:"blocked_reason"`
}

// TestSandboxLifecycle is the "sandbox-lifecycle" acceptance scenario (#50):
// create → ready → stop → start → delete on a real Docker Node, with the
// container limits matching the request and the implicit Workspace reclaimed.
func TestSandboxLifecycle(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)

	suffix := time.Now().UnixNano()
	nodeName := fmt.Sprintf("e2e-sandbox-node-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)

	label := fmt.Sprintf("suite-%d", suffix)
	createBody := map[string]any{
		"name":      fmt.Sprintf("lifecycle-%d", suffix),
		"labels":    map[string]string{"suite": label},
		"cpu":       0.5,
		"memory":    128 << 20,
		"idle_stop": 0,
	}
	headers := map[string]string{
		"Authorization":   "Bearer " + admin,
		"Idempotency-Key": fmt.Sprintf("lifecycle-%d", suffix),
	}
	resp, data := request(t, http.MethodPost, "/api/v1/sandboxes", headers, nil, createBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create Sandbox: %d %s", resp.StatusCode, data)
	}
	var created sandboxView
	mustJSON(t, data, &created)
	if created.State != "pending" || created.CPU != 0.5 || created.Memory != 128<<20 {
		t.Fatalf("created Sandbox = %+v", created)
	}
	if created.Snapshot == "" {
		t.Fatal("created Sandbox has no Snapshot digest")
	}

	// The same Idempotency-Key returns the existing Sandbox.
	if resp, data := request(t, http.MethodPost, "/api/v1/sandboxes", headers, nil, createBody); resp.StatusCode != http.StatusOK {
		t.Fatalf("idempotent create: %d %s, want 200", resp.StatusCode, data)
	}

	// api.md §1: ?wait_for=<state> blocks up to 60 seconds.
	status, data := call(t, http.MethodGet, "/api/v1/sandboxes/"+created.ID+"?wait_for=ready", admin, nil)
	if status != http.StatusOK {
		t.Fatalf("wait_for=ready: %d %s", status, data)
	}
	var ready sandboxView
	mustJSON(t, data, &ready)
	if ready.State != "ready" {
		t.Fatalf("wait_for=ready returned state %q (cause %v)", ready.State, ready.Cause)
	}
	if ready.NodeID == nil {
		t.Fatalf("ready Sandbox has no node_id: %+v", ready)
	}

	// The filters find it while it is running.
	status, data = call(t, http.MethodGet,
		"/api/v1/sandboxes?state=ready&label=suite="+label+"&node="+*ready.NodeID, admin, nil)
	if status != http.StatusOK {
		t.Fatalf("list Sandboxes: %d %s", status, data)
	}
	var list struct {
		Items []sandboxView `json:"items"`
	}
	mustJSON(t, data, &list)
	if len(list.Items) != 1 || list.Items[0].ID != created.ID {
		t.Fatalf("filtered list = %+v", list.Items)
	}

	// Container limits are the request, read back from the cgroup (runtime.md
	// §9: cpu.max is quota/period in microseconds).
	assertLimit(t, created.ID, "/sys/fs/cgroup/cpu.max", "50000 100000")
	assertLimit(t, created.ID, "/sys/fs/cgroup/memory.max", fmt.Sprint(128<<20))
	assertLimit(t, created.ID, "/sys/fs/cgroup/pids.max", "2048")

	// stop → stopped
	status, data = call(t, http.MethodPost, "/api/v1/sandboxes/"+created.ID+"/stop", admin, nil)
	if status != http.StatusAccepted {
		t.Fatalf("stop: %d %s", status, data)
	}
	mustJSON(t, data, &created)
	status, data = call(t, http.MethodGet, "/api/v1/sandboxes/"+created.ID+"?wait_for=stopped", admin, nil)
	var stopped sandboxView
	if status != http.StatusOK {
		t.Fatalf("wait_for=stopped: %d %s", status, data)
	}
	mustJSON(t, data, &stopped)
	if stopped.State != "stopped" || stopped.Cause != nil {
		t.Fatalf("stopped Sandbox = %+v", stopped)
	}
	if running := containerRunning(created.ID); running {
		t.Fatal("the container still runs after stop")
	}

	// start → pending → ready again, without losing the Workspace.
	status, data = call(t, http.MethodPost, "/api/v1/sandboxes/"+created.ID+"/start", admin, nil)
	if status != http.StatusAccepted {
		t.Fatalf("start: %d %s", status, data)
	}
	var started sandboxView
	mustJSON(t, data, &started)
	if started.State != "pending" {
		t.Fatalf("start state = %q, want pending", started.State)
	}
	status, data = call(t, http.MethodGet, "/api/v1/sandboxes/"+created.ID+"?wait_for=ready", admin, nil)
	if status != http.StatusOK {
		t.Fatalf("wait_for=ready after start: %d %s", status, data)
	}
	var restarted sandboxView
	mustJSON(t, data, &restarted)
	if restarted.State != "ready" {
		t.Fatalf("Sandbox did not return to ready: %+v", restarted)
	}
	assertLimit(t, created.ID, "/sys/fs/cgroup/cpu.max", "50000 100000")

	// delete → deleted; the implicit Workspace and its directory go with it.
	status, data = call(t, http.MethodDelete, "/api/v1/sandboxes/"+created.ID, admin, nil)
	if status != http.StatusAccepted {
		t.Fatalf("delete: %d %s", status, data)
	}
	workspaceID := waitForSandboxDeleted(t, created.ID, 60*time.Second)
	if workspaceID != "" {
		if _, err := os.Stat(filepath.Join(dataRoot, "workspaces", workspaceID)); !os.IsNotExist(err) {
			t.Fatalf("Workspace directory still exists (stat err = %v)", err)
		}
	}
}

func raiseSandboxQuota(t *testing.T, admin string, sandboxes int) {
	t.Helper()
	status, data := call(t, http.MethodGet, "/api/v1/users/me", admin, nil)
	if status != http.StatusOK {
		t.Fatalf("get me: %d %s", status, data)
	}
	var me struct {
		ID string `json:"id"`
	}
	mustJSON(t, data, &me)
	status, data = call(t, http.MethodPut, "/api/v1/users/"+me.ID+"/quota", admin, map[string]any{
		"gpus": 0, "sandboxes": sandboxes,
	})
	if status != http.StatusOK {
		t.Fatalf("raise quota: %d %s", status, data)
	}
}

func enrollAgent(t *testing.T, admin, name string) (state, dataRoot string) {
	t.Helper()
	status, data := call(t, http.MethodPost, "/api/v1/enrollment-packets", admin, nil)
	if status != http.StatusCreated {
		t.Fatalf("create Enrollment Packet: %d %s", status, data)
	}
	var packet struct {
		Packet string `json:"packet"`
	}
	mustJSON(t, data, &packet)
	state = filepath.Join(t.TempDir(), "identity.json")
	out, err := agentCommand(t, "enroll",
		"--server", nodeURL(), "--ca", nodeCA(),
		"--packet", packet.Packet, "--name", name, "--state", state).CombinedOutput()
	if err != nil {
		t.Fatalf("agent enroll: %v\n%s", err, out)
	}
	return state, t.TempDir()
}

// agentProcess is an Agent the test can stop and bring back with the same
// identity and data root.
type agentProcess struct {
	cmd *exec.Cmd
	log *bytes.Buffer
}

func startAgent(t *testing.T, state, dataRoot string) *agentProcess {
	t.Helper()
	p := &agentProcess{
		cmd: runAgentCommand(t, "--ca", nodeCA(), "--state", state, "--data-root", dataRoot),
		log: &bytes.Buffer{},
	}
	p.cmd.Stdout, p.cmd.Stderr = p.log, p.log
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start agent: %v", err)
	}
	t.Cleanup(func() {
		p.stop()
		if t.Failed() {
			t.Logf("agent log:\n%s", p.log.String())
		}
	})
	return p
}

func (p *agentProcess) stop() {
	if p.cmd.ProcessState != nil {
		return
	}
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
}

func waitForNodeReady(t *testing.T, admin, name string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		status, data := call(t, http.MethodGet, "/api/v1/nodes?limit=100", admin, nil)
		if status != http.StatusOK {
			t.Fatalf("list Nodes: %d %s", status, data)
		}
		var list struct {
			Items []nodeView `json:"items"`
		}
		mustJSON(t, data, &list)
		for _, item := range list.Items {
			if item.Name == name && item.Status == "ready" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Node %q never became ready", name)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// waitForSandboxDeleted polls until the Sandbox and its Workspace are deleted,
// then returns the Workspace id for the on-disk check.
func waitForSandboxDeleted(t *testing.T, id string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		sandboxDeleted := psqlCount(t, fmt.Sprintf(
			"SELECT count(*) FROM sandboxes WHERE id = '%s' AND state = 'deleted'", id)) == "1"
		workspaceDeleted := psqlCount(t, fmt.Sprintf(
			"SELECT count(*) FROM workspaces WHERE sandbox_id = '%s' AND state = 'deleted'", id)) == "1"
		if sandboxDeleted && workspaceDeleted {
			if n := psqlCount(t, fmt.Sprintf(
				"SELECT count(*) FROM attachments WHERE sandbox_id = '%s'", id)); n != "0" {
				t.Fatalf("Attachment rows left after delete: %s", n)
			}
			if n := psqlCount(t, fmt.Sprintf(
				"SELECT count(*) FROM allocations WHERE sandbox_id = '%s'", id)); n != "0" {
				t.Fatalf("Allocation rows left after delete: %s", n)
			}
			return psqlCount(t, fmt.Sprintf(
				"SELECT id FROM workspaces WHERE sandbox_id = '%s'", id))
		}
		if time.Now().After(deadline) {
			t.Fatalf("Sandbox %s was not fully deleted (sandbox_deleted=%v workspace_deleted=%v)",
				id, sandboxDeleted, workspaceDeleted)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func psqlCount(t *testing.T, query string) string {
	t.Helper()
	args := append(composeArgs(), "exec", "-T", "postgres",
		"psql", "-U", "stars", "-d", "stars", "-tAc", query)
	if len(composeArgs()) == 0 {
		t.Fatal("STARS_E2E_COMPOSE_FILE/PROJECT are not set; run make e2e")
	}
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("psql %q: %v\n%s", query, err, out)
	}
	return strings.TrimSpace(string(out))
}

func containerRunning(id string) bool {
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", "stars-sandbox-"+id).CombinedOutput()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func assertLimit(t *testing.T, id, path, want string) {
	t.Helper()
	out, err := exec.Command("docker", "exec", "stars-sandbox-"+id, "cat", path).CombinedOutput()
	if err != nil {
		t.Fatalf("read %s: %v\n%s", path, err, out)
	}
	if got := strings.TrimSpace(string(out)); got != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}
