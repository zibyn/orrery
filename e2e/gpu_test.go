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

// GPU scenarios need a real GPU host. They are isolated behind an environment
// variable and only run once ticket #58 connects one:
//
//	ORRERY_E2E_GPU_SSH=user@gpu-host make e2e -run GPU
//
// The test deploys the Agent over SSH, opens a reverse tunnel so the host
// reaches this machine's `node` listener on 127.0.0.1 (which the throwaway TLS
// certificate covers), and drives the scenarios through the public REST API.
// ORRERY_E2E_GPU_SNAPSHOT names a platform Snapshot for GPU Sandboxes (M4
// imports images by digest and Sandboxes reference a Snapshot by name). The
// deployment must list it in ORRERY_PLATFORM_SNAPSHOTS; a raw image reference
// is no longer accepted. The default CUDA image is the smallest glibc base
// whose nvidia-smi can run.

const gpuAPIPort = 18081 // published `node` listener on this machine

// gpuHost is one enrolled Agent running on the remote GPU host.
type gpuHost struct {
	target   string
	dir      string
	nodeName string
	state    string
	tunnel   *exec.Cmd
	agent    *exec.Cmd
	agentLog *bytes.Buffer
}

func TestGPUSandboxReady(t *testing.T) {
	admin := adminToken(t)
	host := startGPUHost(t, "e2e-gpu-ready")
	waitForNodeReady(t, admin, host.nodeName)
	node := findNode(t, admin, host.nodeName)
	cards := requireGPUs(t, node)

	setGPUQuota(t, admin, int32(cards), int32(cards)+2)

	sb := createSandbox(t, admin, gpuSandboxBody(t, node, 1))
	t.Cleanup(func() { deleteSandbox(t, admin, sb.ID) })
	waitForSandboxReady(t, admin, sb.ID)
	if sb.GPU == nil || sb.GPU.Count != 1 {
		t.Fatalf("created Sandbox gpu = %+v, want count 1", sb.GPU)
	}

	// The container sees exactly the card the Scheduler attached, and nothing
	// else (issue #57 acceptance): compare against the Allocation the Control
	// Plane recorded, not merely against the Node's inventory.
	assigned := psqlCount(t, "SELECT gpu_uuid FROM gpu_allocations WHERE sandbox_id = '"+sb.ID+"'")
	if assigned == "" {
		t.Fatalf("no GPU allocation recorded for Sandbox %s", sb.ID)
	}
	seen := remoteGPUUUIDs(t, host, sb.ID)
	if len(seen) != 1 || seen[0] != assigned {
		t.Fatalf("the Sandbox sees %v, want exactly the allocated card %s", seen, assigned)
	}

	// The capacity view and the User Quota mirror the allocation.
	if allocated, total := gpuCapacity(t, admin, node.ID); allocated != 1 || total != cards {
		t.Fatalf("capacity allocated/total = %d/%d, want 1/%d", allocated, total, cards)
	}
	if used := myQuota(t, admin).GPUs.Used; used != 1 {
		t.Fatalf("GPU quota used = %d, want 1", used)
	}

	// Deleting the Sandbox releases the card.
	deleteSandbox(t, admin, sb.ID)
	waitForGPUCapacity(t, admin, node.ID, 0)
	if used := myQuota(t, admin).GPUs.Used; used != 0 {
		t.Fatalf("GPU quota used after delete = %d, want 0", used)
	}
}

// TestGPUQueueAndDeadline is the GPU form of "queue-and-deadline" (#57): when
// every card is held, the next request waits and its deadline releases the
// quota hold.
func TestGPUQueueAndDeadline(t *testing.T) {
	admin := adminToken(t)
	host := startGPUHost(t, "e2e-gpu-queue")
	waitForNodeReady(t, admin, host.nodeName)
	node := findNode(t, admin, host.nodeName)
	cards := requireGPUs(t, node)

	// The queue is bounded by capacity, not by quota: the waiting request must
	// hold a quota slot while it waits (ADR-0015).
	setGPUQuota(t, admin, int32(cards)+1, int32(cards)+2)

	holder := createSandbox(t, admin, gpuSandboxBody(t, node, cards))
	t.Cleanup(func() { deleteSandbox(t, admin, holder.ID) })
	waitForSandboxReady(t, admin, holder.ID)

	waiting := createSandbox(t, admin, gpuSandboxBody(t, node, 1))
	t.Cleanup(func() { deleteSandbox(t, admin, waiting.ID) })
	if waiting.State != "pending" {
		t.Fatalf("a Sandbox over the card count is %q, want pending", waiting.State)
	}
	blocked := getSandbox(t, admin, waiting.ID)
	if blocked.QueuePosition == nil || *blocked.QueuePosition != 1 {
		t.Fatalf("queue_position = %v, want 1", blocked.QueuePosition)
	}

	// The deployment's short admission deadline ends the wait and releases the
	// quota hold; the holder keeps its cards.
	waitForFailed(t, admin, waiting.ID, "admission_timeout")
	if used := myQuota(t, admin).GPUs.Used; used != int32(cards) {
		t.Fatalf("GPU quota used = %d, want %d", used, cards)
	}
	if allocated, _ := gpuCapacity(t, admin, node.ID); allocated != cards {
		t.Fatalf("capacity allocated = %d, want %d", allocated, cards)
	}
}

// startGPUHost enrolls and runs an Agent on the remote host over SSH, with a
// reverse tunnel back to this machine's node listener.
func startGPUHost(t *testing.T, name string) *gpuHost {
	t.Helper()
	target := os.Getenv("ORRERY_E2E_GPU_SSH")
	if target == "" {
		t.Skip("ORRERY_E2E_GPU_SSH is not set; connect a GPU host first (#58)")
	}
	remotePort := envOr("ORRERY_E2E_GPU_REMOTE_PORT", "18081")

	suffix := time.Now().UnixNano()
	h := &gpuHost{
		target:   target,
		dir:      fmt.Sprintf("/tmp/%s-%d", name, suffix),
		nodeName: fmt.Sprintf("%s-%d", name, suffix),
	}
	h.state = h.dir + "/identity.json"
	h.agentLog = &bytes.Buffer{}

	sshRun(t, h, "mkdir -p "+h.dir)
	scpRun(t, h, agentPath(t), h.dir+"/agent")
	// The Agent stages the external CLI into every Sandbox from its own
	// directory (runtime.md §2).
	scpRun(t, h, cliPath(t), h.dir+"/orrery")
	scpRun(t, h, nodeCA(), h.dir+"/ca.crt")

	// The reverse tunnel gives the host a 127.0.0.1 that reaches this machine,
	// which the e2e TLS certificate already covers.
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
	sshRun(t, h, h.dir+"/agent", "enroll",
		"--server", "127.0.0.1:"+remotePort, "--ca", h.dir+"/ca.crt",
		"--packet", packet, "--name", h.nodeName, "--state", h.state)

	h.agent = exec.Command("ssh", target, h.dir+"/agent", "run",
		"--server", "127.0.0.1:"+remotePort, "--ca", h.dir+"/ca.crt",
		"--state", h.state, "--data-root", h.dir+"/data",
		"--reserve-cpu", "0")
	h.agent.Stdout, h.agent.Stderr = h.agentLog, h.agentLog
	if err := h.agent.Start(); err != nil {
		t.Fatalf("start the remote Agent: %v", err)
	}
	t.Cleanup(func() {
		_ = h.agent.Process.Kill()
		_ = h.agent.Wait()
		sshRunQuiet(t, "pkill", "-f", h.state)
		if t.Failed() {
			t.Logf("remote Agent log:\n%s", h.agentLog.String())
		}
	})
	return h
}

func sshRun(t *testing.T, h *gpuHost, args ...string) string {
	t.Helper()
	out, err := exec.Command("ssh", append([]string{h.target}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh %s %s: %v\n%s", h.target, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func sshRunQuiet(t *testing.T, args ...string) {
	t.Helper()
	_ = exec.Command("ssh", append([]string{os.Getenv("ORRERY_E2E_GPU_SSH")}, args...)...).Run()
}

func scpRun(t *testing.T, h *gpuHost, src, dst string) {
	t.Helper()
	out, err := exec.Command("scp", src, h.target+":"+dst).CombinedOutput()
	if err != nil {
		t.Fatalf("scp %s to %s:%s: %v\n%s", src, h.target, dst, err, out)
	}
}

// requireGPUs checks that the enrolled Node reported cards and that CDI is
// usable, returning the card count.
func requireGPUs(t *testing.T, node nodeView) int {
	t.Helper()
	if len(node.GPUs) == 0 {
		t.Fatalf("Node %s reports no GPUs; is nvidia-smi installed on the host?", node.Name)
	}
	entries, _ := node.Capabilities["entries"].([]any)
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		if entry["name"] == "gpu_device_visibility" && entry["value"] != "enforced" {
			t.Fatalf("Node %s does not enforce gpu_device_visibility (%v)", node.Name, entry["value"])
		}
	}
	return len(node.GPUs)
}

// remoteGPUUUIDs runs nvidia-smi inside the Sandbox on the GPU host.
func remoteGPUUUIDs(t *testing.T, h *gpuHost, sandboxID string) []string {
	t.Helper()
	out := sshRun(t, h, "docker", "exec", "orrery-"+sandboxID,
		"nvidia-smi", "--query-gpu=uuid", "--format=csv,noheader,nounits")
	var uuids []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			uuids = append(uuids, line)
		}
	}
	return uuids
}

// gpuSandboxBody asks for count cards within the Node's per-GPU share.
func gpuSandboxBody(t *testing.T, node nodeView, count int) map[string]any {
	t.Helper()
	cores, _ := node.Hardware["cpu_cores"].(float64)
	share := cores * 0.75 / float64(len(node.GPUs))
	cpu := 0.5
	if share > 0 && share < cpu {
		cpu = share
	}
	return map[string]any{
		"cpu": cpu, "memory": 64 << 20, "idle_stop": 0,
		// The default e2e Snapshot is Alpine, where the glibc-linked nvidia-smi
		// the toolkit mounts cannot run. A CUDA base image is the smallest image
		// that satisfies the acceptance check.
		"snapshot": gpuSnapshotName(t),
		"gpu":      map[string]any{"count": count},
	}
}

// gpuSnapshotName is a platform Snapshot name, not an image reference:
// Sandboxes resolve a Snapshot name to its current ready version (api.md §3).
func gpuSnapshotName(t *testing.T) string {
	t.Helper()
	name := envOr("ORRERY_E2E_GPU_SNAPSHOT", "cuda")
	if strings.ContainsAny(name, "/:") {
		t.Fatalf("ORRERY_E2E_GPU_SNAPSHOT must be a Snapshot name listed in platform_snapshots, got image reference %q", name)
	}
	return name
}

func createEnrollmentPacket(t *testing.T, admin string) string {
	t.Helper()
	status, data := call(t, http.MethodPost, "/api/v1/enrollment-packets", admin, nil)
	if status != http.StatusCreated {
		t.Fatalf("create Enrollment Packet: %d %s", status, data)
	}
	var packet struct {
		Packet string `json:"packet"`
	}
	mustJSON(t, data, &packet)
	return packet.Packet
}

func setGPUQuota(t *testing.T, admin string, gpus, sandboxes int32) {
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
		"gpus": gpus, "sandboxes": sandboxes,
	})
	if status != http.StatusOK {
		t.Fatalf("set quota: %d %s", status, data)
	}
}

func gpuCapacity(t *testing.T, admin, nodeID string) (allocated, total int) {
	t.Helper()
	status, data := call(t, http.MethodGet, "/api/v1/capacity", admin, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /capacity: %d %s", status, data)
	}
	var capacity struct {
		Items []struct {
			NodeID string `json:"node_id"`
			GPUs   []struct {
				Total     int `json:"total"`
				Allocated int `json:"allocated"`
			} `json:"gpus"`
		} `json:"items"`
	}
	mustJSON(t, data, &capacity)
	for _, item := range capacity.Items {
		if item.NodeID != nodeID {
			continue
		}
		for _, gpu := range item.GPUs {
			allocated += gpu.Allocated
			total += gpu.Total
		}
	}
	return allocated, total
}

func waitForGPUCapacity(t *testing.T, admin, nodeID string, want int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		allocated, _ := gpuCapacity(t, admin, nodeID)
		if allocated == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("capacity allocated stayed at %d, want %d", allocated, want)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func waitForSandboxReady(t *testing.T, admin, id string) sandboxView {
	t.Helper()
	deadline := time.Now().Add(300 * time.Second)
	for {
		sb := getSandbox(t, admin, id)
		if sb.State == "ready" {
			return sb
		}
		if sb.State == "failed" {
			t.Fatalf("Sandbox failed (cause %v)", sb.Cause)
		}
		if time.Now().After(deadline) {
			t.Fatalf("Sandbox stayed %q instead of becoming ready", sb.State)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
