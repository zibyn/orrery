//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestPodmanParity is the "podman-parity" acceptance scenario (#96): the M1
// scenarios gpu-sandbox-ready, exec-log-stream and agent-restart-survives run
// on a rootless Podman Node. It reuses the remote-host shape of the GPU tests:
//
//	STARS_E2E_GPU_SSH=user@podman-host make e2e -run PodmanParity
//
// The SSH login must reach root (directly or with passwordless sudo), because
// the Agent runs as root (runtime.md §9). STARS_E2E_PODMAN_USER names the
// non-root user whose rootless Podman socket the Agent uses; it defaults to
// the SSH user when that user is not root. Podman, uidmap and the rootless
// network helper are installed over SSH when absent.
func TestPodmanParity(t *testing.T) {
	admin := adminToken(t)
	host := startPodmanHost(t, "e2e-podman-parity")
	waitForNodeReady(t, admin, host.nodeName)
	node := findNode(t, admin, host.nodeName)

	if got := node.Capabilities["runtime_provider"]; got != "podman" {
		t.Fatalf("runtime_provider = %v, want podman (capabilities %v)", got, node.Capabilities)
	}
	// The self-check runs the same probe as Docker; every container guarantee
	// must come back from the readback (runtime.md §9).
	entries, _ := node.Capabilities["entries"].([]any)
	values := map[string]string{}
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		name, _ := entry["name"].(string)
		value, _ := entry["value"].(string)
		values[name] = value
	}
	for _, name := range []string{
		"process_mount_namespace", "cpu_memory_pid_limits", "workspace_rw_mount",
		"exec_supervisor", "resource_mount",
	} {
		if values[name] != "enforced" {
			t.Fatalf("%s = %q, want enforced (capabilities %v)", name, values[name], node.Capabilities)
		}
	}
	// No --buildkit endpoint is configured, and Podman has no dockerd /grpc
	// upgrade: snapshot builds are unsupported, independent of the Runtime.
	if values["snapshot_build"] != "unsupported" {
		t.Fatalf("snapshot_build = %q, want unsupported without a BuildKit endpoint", values["snapshot_build"])
	}

	t.Run("gpu-sandbox-ready", func(t *testing.T) {
		cards := requireGPUs(t, node)
		setGPUQuota(t, admin, int32(cards), int32(cards)+2)

		sb := createSandbox(t, admin, gpuSandboxBody(t, node, 1))
		t.Cleanup(func() { deleteSandbox(t, admin, sb.ID) })
		waitForSandboxReady(t, admin, sb.ID)

		assigned := psqlCount(t, "SELECT gpu_uuid FROM gpu_allocations WHERE sandbox_id = '"+sb.ID+"'")
		if assigned == "" {
			t.Fatalf("no GPU allocation recorded for Sandbox %s", sb.ID)
		}
		seen := remotePodmanGPUUUIDs(t, host, sb.ID)
		if len(seen) != 1 || seen[0] != assigned {
			t.Fatalf("the Podman Sandbox sees %v, want exactly the allocated card %s", seen, assigned)
		}
	})

	t.Run("exec-log-stream", func(t *testing.T) {
		raiseSandboxQuota(t, admin, 10)
		sandboxID := host.createSandbox(t, admin, "podman-exec")
		e := createExec(t, admin, sandboxID, map[string]any{
			"argv": []string{"sh", "-c", "printf 'hello '; printf 'oops' >&2; printf '\\377'; exit 7"},
		})
		stdout, stderr, _, result := followExec(t, admin, e.ID, 30*time.Second)
		if want := []byte{'h', 'e', 'l', 'l', 'o', ' ', 0xff}; !bytes.Equal(stdout, want) {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
		if string(stderr) != "oops" {
			t.Fatalf("stderr = %q, want %q", stderr, "oops")
		}
		if result.Cause != "exited" || result.ExitCode == nil || *result.ExitCode != 7 {
			t.Fatalf("result = %+v, want exited with code 7", result)
		}
	})

	t.Run("agent-restart-survives", func(t *testing.T) {
		raiseSandboxQuota(t, admin, 10)
		sandboxID := host.createSandbox(t, admin, "podman-restart")

		const count = 60
		e := createExec(t, admin, sandboxID, numberedLines(count))
		waitExecRunning(t, admin, e.ID, 30*time.Second)
		// Let the first frames reach the Control Plane so the restart resumes
		// from stored_seq instead of replaying from zero.
		time.Sleep(500 * time.Millisecond)

		host.restartAgent(t)

		assertExitedWith(t, waitExecFinished(t, admin, e.ID, 60*time.Second), "exited")
		if got, want := execStdout(t, admin, e.ID), expectedLines(count); got != want {
			t.Fatalf("output after an Agent restart has gaps or repeats:\ngot  %q\nwant %q", got, want)
		}
	})
}

// podmanHost is one Agent running as root on a remote host, connected to a
// non-root user's rootless Podman socket.
type podmanHost struct {
	target   string
	sshUser  string
	user     string // rootless Podman user
	uid      string
	gid      string
	socket   string
	dir      string
	nodeName string
	state    string
	tunnel   *exec.Cmd
	agent    *exec.Cmd
	agentLog *bytes.Buffer
	args     []string
	// startedService records that this test started podman system service and
	// should stop it; a pre-existing service is left alone.
	startedService bool
}

func startPodmanHost(t *testing.T, name string) *podmanHost {
	t.Helper()
	target := os.Getenv("STARS_E2E_GPU_SSH")
	if target == "" {
		t.Skip("STARS_E2E_GPU_SSH is not set; connect a Podman host first")
	}
	suffix := time.Now().UnixNano()
	h := &podmanHost{
		target:   target,
		dir:      fmt.Sprintf("/tmp/%s-%d", name, suffix),
		nodeName: fmt.Sprintf("%s-%d", name, suffix),
		agentLog: &bytes.Buffer{},
	}
	h.state = h.dir + "/identity.json"
	h.sshUser = strings.TrimSpace(h.sshRun(t, "id", "-un"))
	h.user = os.Getenv("STARS_E2E_PODMAN_USER")
	if h.user == "" {
		if h.sshUser == "root" {
			t.Fatalf("STARS_E2E_PODMAN_USER is required when STARS_E2E_GPU_SSH logs in as root")
		}
		h.user = h.sshUser
	}
	h.sshRun(t, "mkdir", "-p", h.dir)
	// The rootless Podman daemon runs as another user and must traverse the
	// Agent's directory.
	h.sshRun(t, "chmod", "755", h.dir)

	h.ensurePodman(t)
	h.uid = strings.TrimSpace(h.sshRunRoot(t, "id", "-u", h.user))
	h.gid = strings.TrimSpace(h.sshRunRoot(t, "id", "-g", h.user))
	// A root SSH login created the directory; the rootless user must own it to
	// write the service log and pid file.
	if h.sshUser == "root" {
		h.sshRunRoot(t, "chown", h.user+":"+h.gid, h.dir)
	}
	h.socket = rootlessPodmanSocket(h.uid)
	h.startPodmanService(t)

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
	h.sshRun(t, h.dir+"/agent", "enroll",
		"--server", "127.0.0.1:"+remotePort, "--ca", h.dir+"/ca.crt",
		"--packet", packet, "--name", h.nodeName, "--state", h.state)

	h.args = []string{h.dir + "/agent", "run",
		"--server", "127.0.0.1:" + remotePort, "--ca", h.dir + "/ca.crt",
		"--state", h.state, "--data-root", h.dir + "/data",
		"--docker", h.socket, "--runtime", "podman", "--reserve-cpu", "0"}
	h.startAgent(t)

	t.Cleanup(func() {
		h.stopAgent(t)
		h.sshRunRootQuiet(t, "pkill", "-f", pgrepSafe(h.state))
		if h.startedService {
			h.sshRunScriptQuiet(t, h.userPrefix(), fmt.Sprintf(
				"kill $(cat %s/podman.pid) 2>/dev/null || true", h.dir))
		}
		if t.Failed() {
			t.Logf("remote Agent log:\n%s", h.agentLog.String())
		}
	})
	return h
}

func (h *podmanHost) startAgent(t *testing.T) {
	t.Helper()
	args := append(h.rootPrefix(), h.args...)
	h.agent = exec.Command("ssh", append([]string{h.target}, args...)...)
	h.agent.Stdout, h.agent.Stderr = h.agentLog, h.agentLog
	if err := h.agent.Start(); err != nil {
		t.Fatalf("start the remote Agent: %v", err)
	}
}

func (h *podmanHost) stopAgent(t *testing.T) {
	t.Helper()
	if h.agent != nil {
		_ = h.agent.Process.Kill()
		_ = h.agent.Wait()
		h.agent = nil
	}
}

// restartAgent kills the Agent and starts it again on the same identity and
// data root (ADR-0014). Runtimes and Execs are not stopped with it.
func (h *podmanHost) restartAgent(t *testing.T) {
	t.Helper()
	h.stopAgent(t)
	h.sshRunRootQuiet(t, "pkill", "-f", pgrepSafe(h.state))
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := h.trySSH(append(h.rootPrefix(), "pgrep", "-f", pgrepSafe(h.state))...); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the remote Agent did not die")
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.startAgent(t)
}

// createSandbox creates and waits for a ready Sandbox on the Podman Node.
func (h *podmanHost) createSandbox(t *testing.T, admin, name string) string {
	t.Helper()
	sandboxID := createReadySandbox(t, admin, fmt.Sprintf("%s-%d", name, time.Now().UnixNano()), 128<<20)
	t.Cleanup(func() { deleteSandbox(t, admin, sandboxID) })
	return sandboxID
}

func (h *podmanHost) ensurePodman(t *testing.T) {
	t.Helper()
	if _, err := h.trySSH("command", "-v", "podman"); err == nil {
		return
	}
	// The host can be taken to root over SSH (issue #96).
	h.sshRunScript(t, h.rootPrefix(), `
if command -v apt-get >/dev/null 2>&1; then
  apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y podman uidmap slirp4netns
elif command -v dnf >/dev/null 2>&1; then
  dnf install -y podman
elif command -v yum >/dev/null 2>&1; then
  yum install -y podman
else
  echo "cannot install podman: no supported package manager" >&2
  exit 1
fi`)
	// Rootless Podman needs a subordinate id range for the user namespace.
	h.sshRunScript(t, h.rootPrefix(), fmt.Sprintf(`
grep -q '^%[1]s:' /etc/subuid || usermod --add-subuids 100000-165535 %[1]s
grep -q '^%[1]s:' /etc/subgid || usermod --add-subgids 100000-165535 %[1]s`, h.user))
}

func (h *podmanHost) startPodmanService(t *testing.T) {
	t.Helper()
	h.sshRunRoot(t, "loginctl", "enable-linger", h.user)
	h.sshRunRoot(t, "mkdir", "-p", "/run/user/"+h.uid)
	h.sshRunRoot(t, "chown", h.uid+":"+h.gid, "/run/user/"+h.uid)

	if _, err := h.trySSH("test", "-S", h.socket); err == nil {
		return // an existing service is not ours to stop
	}
	h.startedService = true
	h.sshRunScript(t, h.userPrefix(), fmt.Sprintf(
		"mkdir -p -m 0700 /run/user/%[1]s/podman && XDG_RUNTIME_DIR=/run/user/%[1]s setsid podman system service --time=0 unix://%[2]s >%[3]s/podman.log 2>&1 </dev/null & echo $! >%[3]s/podman.pid",
		h.uid, h.socket, h.dir))

	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := h.trySSH("test", "-S", h.socket); err == nil {
			return
		}
		if time.Now().After(deadline) {
			out, _ := h.trySSH("cat", h.dir+"/podman.log")
			t.Fatalf("rootless Podman socket %s never appeared:\n%s", h.socket, out)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (h *podmanHost) rootPrefix() []string {
	if h.sshUser == "root" {
		return nil
	}
	return []string{"sudo", "-n"}
}

func (h *podmanHost) userPrefix() []string {
	if h.sshUser == h.user {
		return nil
	}
	if h.sshUser == "root" {
		return []string{"sudo", "-u", h.user, "-H"}
	}
	return []string{"sudo", "-n", "-u", h.user, "-H"}
}

func (h *podmanHost) sshRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := h.trySSH(args...)
	if err != nil {
		t.Fatalf("ssh %s %s: %v\n%s", h.target, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (h *podmanHost) sshRunRoot(t *testing.T, args ...string) string {
	t.Helper()
	return h.sshRun(t, append(h.rootPrefix(), args...)...)
}

func (h *podmanHost) sshRunUser(t *testing.T, args ...string) string {
	t.Helper()
	prefix := h.userPrefix()
	if len(prefix) == 0 {
		return h.sshRun(t, args...)
	}
	// A root SSH login leaves the command in /root, which the rootless user
	// cannot enter; /tmp is readable by every user.
	script := "cd /tmp && exec " + strings.Join(args, " ")
	return h.sshRun(t, append(prefix, "sh", "-c", shellQuote(script))...)
}

func (h *podmanHost) sshRunRootQuiet(t *testing.T, args ...string) {
	t.Helper()
	_, _ = h.trySSH(append(h.rootPrefix(), args...)...)
}

// sshRunScript runs a shell script remotely. The script is quoted as one argv
// element because ssh joins arguments without quoting them.
func (h *podmanHost) sshRunScript(t *testing.T, prefix []string, script string) string {
	t.Helper()
	out, err := h.trySSHScript(prefix, script)
	if err != nil {
		t.Fatalf("ssh %s script: %v\n%s", h.target, err, out)
	}
	return string(out)
}

func (h *podmanHost) sshRunScriptQuiet(t *testing.T, prefix []string, script string) {
	t.Helper()
	_, _ = h.trySSHScript(prefix, script)
}

func (h *podmanHost) trySSHScript(prefix []string, script string) ([]byte, error) {
	remote := strings.Join(append(prefix, "sh", "-c", shellQuote("cd /tmp && "+script)), " ")
	return exec.Command("ssh", h.target, remote).CombinedOutput()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// pgrepSafe keeps the pattern from matching the shell wrapper that carries it
// (the wrapper's command line contains the literal text, not the match).
func pgrepSafe(s string) string {
	if s == "" {
		return s
	}
	return "[" + s[:1] + "]" + s[1:]
}

func (h *podmanHost) trySSH(args ...string) ([]byte, error) {
	return exec.Command("ssh", append([]string{h.target}, args...)...).CombinedOutput()
}

func (h *podmanHost) scp(t *testing.T, src, dst string) {
	t.Helper()
	out, err := exec.Command("scp", src, h.target+":"+dst).CombinedOutput()
	if err != nil {
		t.Fatalf("scp %s to %s:%s: %v\n%s", src, h.target, dst, err, out)
	}
}

// remotePodmanGPUUUIDs runs nvidia-smi inside the Sandbox through the rootless
// user's Podman (the same container the Agent created).
func remotePodmanGPUUUIDs(t *testing.T, h *podmanHost, sandboxID string) []string {
	t.Helper()
	out := h.sshRunUser(t, "podman", "exec", "stars-sandbox-"+sandboxID,
		"nvidia-smi", "--query-gpu=uuid", "--format=csv,noheader,nounits")
	var uuids []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			uuids = append(uuids, line)
		}
	}
	return uuids
}

func rootlessPodmanSocket(uid string) string {
	return fmt.Sprintf("/run/user/%s/podman/podman.sock", uid)
}
