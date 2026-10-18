//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestLocalRuntime is the "local-runtime" acceptance scenario (#97): the Agent
// runs as root with --runtime local on a remote host and creates a
// runtime: local Sandbox whose Execs run as the configured unprivileged user
// in the Workspace's host directory.
//
//	STARS_E2E_GPU_SSH=user@host STARS_E2E_LOCAL_USER=builder make e2e -run LocalRuntime
//
// The SSH login must reach root (directly or with sudo -n). When the login is
// root, STARS_E2E_LOCAL_USER defaults to nobody; when it is a user, that user
// is the default. The scenario does not need a GPU, but it reuses the remote
// host variable the GPU and Podman scenarios already use.
func TestLocalRuntime(t *testing.T) {
	admin := adminToken(t)
	host := startLocalHost(t, "e2e-local-runtime")
	waitForNodeReady(t, admin, host.nodeName)
	node := findNode(t, admin, host.nodeName)

	if got := node.Capabilities["runtime_provider"]; got != "local" {
		t.Fatalf("runtime_provider = %v, want local (capabilities %v)", got, node.Capabilities)
	}
	values := capabilityValues(node)
	// ADR-0009's Local posture. Only the self-checked boundaries are enforced.
	if values["process_mount_namespace"] != "unsupported" {
		t.Errorf("process_mount_namespace = %q, want unsupported", values["process_mount_namespace"])
	}
	if values["resource_mount"] != "unsupported" {
		t.Errorf("resource_mount = %q, want unsupported", values["resource_mount"])
	}
	for _, name := range []string{"cpu_memory_pid_limits", "workspace_rw_mount", "exec_supervisor"} {
		if values[name] != "enforced" {
			t.Errorf("%s = %q, want enforced (capabilities %v)", name, values[name], node.Capabilities)
		}
	}
	// No --buildkit endpoint is configured, so the BuildKit self-check fails;
	// snapshot builds are independent of the Runtime (runtime.md §12).
	if values["snapshot_build"] != "unsupported" {
		t.Errorf("snapshot_build = %q, want unsupported without --buildkit", values["snapshot_build"])
	}

	// Local has no Snapshot: naming one is 422 unsupported, before placement.
	status, data := call(t, http.MethodPost, "/api/v1/sandboxes", admin, map[string]any{
		"cpu": 0.5, "memory": 64 << 20, "runtime": "local", "snapshot": "default",
	})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("local with a Snapshot: status = %d, want 422 (%s)", status, data)
	}
	requireProblemCode(t, data, "unsupported")

	raiseSandboxQuota(t, admin, 10)
	sb := createSandbox(t, admin, map[string]any{
		"cpu": 0.5, "memory": 64 << 20, "runtime": "local", "idle_stop": 0,
	})
	if sb.Runtime != "local" {
		t.Fatalf("created Sandbox runtime = %q, want local", sb.Runtime)
	}
	if sb.Workspace == nil {
		t.Fatal("a local Sandbox has no Workspace")
	}
	t.Cleanup(func() { deleteSandbox(t, admin, sb.ID) })
	waitForSandboxReady(t, admin, sb.ID)

	// The Workspace host path is <remote data-root>/workspaces/<id>; the e2e
	// harness always passes <dir>/data as --data-root.
	workspace := host.dir + "/data/workspaces/" + sb.Workspace.ID

	pwd := createExec(t, admin, sb.ID, map[string]any{"argv": []string{"pwd"}})
	stdout, _, _, result := followExec(t, admin, pwd.ID, 30*time.Second)
	if result.Cause != "exited" || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("pwd result = %+v, stdout %q", result, stdout)
	}
	if got := strings.TrimSpace(string(stdout)); got != workspace {
		t.Fatalf("pwd = %q, want the Workspace host path %q", got, workspace)
	}

	envExec := createExec(t, admin, sb.ID, map[string]any{
		"argv": []string{"sh", "-c", `printf '%s' "$STARS_WORKSPACE"`},
	})
	stdout, _, _, result = followExec(t, admin, envExec.ID, 30*time.Second)
	if result.Cause != "exited" || *result.ExitCode != 0 {
		t.Fatalf("STARS_WORKSPACE result = %+v", result)
	}
	if got := strings.TrimSpace(string(stdout)); got != workspace {
		t.Fatalf("STARS_WORKSPACE = %q, want %q", got, workspace)
	}

	// Every User on a Local Node shares the one configured uid (ADR-0009).
	whoami := createExec(t, admin, sb.ID, map[string]any{"argv": []string{"id", "-un"}})
	stdout, _, _, result = followExec(t, admin, whoami.ID, 30*time.Second)
	if result.Cause != "exited" || *result.ExitCode != 0 {
		t.Fatalf("id -un result = %+v", result)
	}
	if got := strings.TrimSpace(string(stdout)); got != host.user {
		t.Fatalf("Exec ran as %q, want the configured Local user %q", got, host.user)
	}

	// The cgroup slice is the resource boundary; the placeholder service and
	// the transient Exec service live in it.
	slice := "stars-sb-" + sb.ID + ".slice"
	if out := host.sshRoot(t, "systemctl", "is-active", "stars-sb-"+sb.ID+".service"); strings.TrimSpace(out) != "active" {
		t.Fatalf("placeholder unit is %q, want active", strings.TrimSpace(out))
	}
	state := strings.TrimSpace(host.sshRoot(t, "systemctl", "show", slice, "-p", "ActiveState"))
	if !strings.HasSuffix(state, "=active") {
		t.Fatalf("slice state = %q, want active", state)
	}
}

// capabilityValues flattens a Node's capability entries.
func capabilityValues(node nodeView) map[string]string {
	entries, _ := node.Capabilities["entries"].([]any)
	values := map[string]string{}
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		name, _ := entry["name"].(string)
		value, _ := entry["value"].(string)
		values[name] = value
	}
	return values
}

// localHost is one root Agent running with --runtime local on a remote host.
type localHost struct {
	target   string
	dir      string
	nodeName string
	state    string
	sshUser  string
	user     string
	tunnel   *exec.Cmd
	agent    *exec.Cmd
	agentLog *bytes.Buffer
}

func startLocalHost(t *testing.T, name string) *localHost {
	t.Helper()
	target := os.Getenv("STARS_E2E_GPU_SSH")
	if target == "" {
		t.Skip("STARS_E2E_GPU_SSH is not set; connect a remote host first")
	}
	suffix := time.Now().UnixNano()
	h := &localHost{
		target:   target,
		dir:      fmt.Sprintf("/tmp/%s-%d", name, suffix),
		nodeName: fmt.Sprintf("%s-%d", name, suffix),
		agentLog: &bytes.Buffer{},
	}
	h.state = h.dir + "/identity.json"
	h.sshUser = strings.TrimSpace(h.trySSH(t, "id", "-un"))
	h.user = os.Getenv("STARS_E2E_LOCAL_USER")
	if h.user == "" {
		if h.sshUser == "root" {
			h.user = "nobody"
		} else {
			h.user = h.sshUser
		}
	}
	h.trySSHRoot(t, "mkdir", "-p", h.dir)
	// The unprivileged user must traverse the Agent directory to reach the
	// staged supervisor and the Workspace.
	h.trySSHRoot(t, "chmod", "755", h.dir)

	h.scp(t, agentPath(t), h.dir+"/agent")
	h.scp(t, cliPath(t), h.dir+"/stars-sandbox")
	h.scp(t, nodeCA(), h.dir+"/ca.crt")

	remotePort := envOr("STARS_E2E_GPU_REMOTE_PORT", "18081")
	h.tunnel = exec.Command("ssh", "-N", "-R",
		remotePort+":127.0.0.1:"+fmt.Sprint(gpuAPIPort), target)
	if err := h.tunnel.Start(); err != nil {
		t.Fatalf("open the reverse tunnel: %v", err)
	}
	t.Cleanup(func() {
		_ = h.tunnel.Process.Kill()
		_ = h.tunnel.Wait()
	})

	packet := createEnrollmentPacket(t, adminToken(t))
	h.sshRoot(t, h.dir+"/agent", "enroll",
		"--server", "127.0.0.1:"+remotePort, "--ca", h.dir+"/ca.crt",
		"--packet", packet, "--name", h.nodeName, "--state", h.state)

	args := h.rootPrefix()
	args = append(args, h.dir+"/agent", "run",
		"--server", "127.0.0.1:"+remotePort, "--ca", h.dir+"/ca.crt",
		"--state", h.state, "--data-root", h.dir+"/data",
		"--runtime", "local", "--local-user", h.user,
		"--reserve-cpu", "0")
	h.agent = exec.Command("ssh", append([]string{target}, args...)...)
	h.agent.Stdout, h.agent.Stderr = h.agentLog, h.agentLog
	if err := h.agent.Start(); err != nil {
		t.Fatalf("start the remote Agent: %v", err)
	}
	t.Cleanup(func() {
		if h.agent != nil {
			_ = h.agent.Process.Kill()
			_ = h.agent.Wait()
		}
		tryRemote(t, "pkill", "-f", h.state)
		if t.Failed() {
			t.Logf("remote Agent log:\n%s", h.agentLog.String())
		}
	})
	return h
}

func (h *localHost) rootPrefix() []string {
	if h.sshUser == "root" {
		return nil
	}
	return []string{"sudo", "-n"}
}

func (h *localHost) trySSH(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("ssh", append([]string{h.target}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh %s %s: %v\n%s", h.target, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (h *localHost) sshRoot(t *testing.T, args ...string) string {
	t.Helper()
	return h.trySSH(t, append(h.rootPrefix(), args...)...)
}

func (h *localHost) trySSHRoot(t *testing.T, args ...string) {
	t.Helper()
	if _, err := exec.Command("ssh", append(append([]string{h.target}, h.rootPrefix()...), args...)...).CombinedOutput(); err != nil {
		t.Fatalf("ssh %s %s: %v", h.target, strings.Join(args, " "), err)
	}
}

func (h *localHost) scp(t *testing.T, src, dst string) {
	t.Helper()
	out, err := exec.Command("scp", src, h.target+":"+dst).CombinedOutput()
	if err != nil {
		t.Fatalf("scp %s to %s:%s: %v\n%s", src, h.target, dst, err, out)
	}
}

func tryRemote(t *testing.T, args ...string) {
	t.Helper()
	target := os.Getenv("STARS_E2E_GPU_SSH")
	prefix := []string{}
	if out, err := exec.Command("ssh", target, "id", "-un").CombinedOutput(); err == nil &&
		strings.TrimSpace(string(out)) != "root" {
		prefix = []string{"sudo", "-n"}
	}
	_ = exec.Command("ssh", append(append([]string{target}, prefix...), args...)...).Run()
}
