//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/zibyn/orrery/client"
)

// sdkClient is the public Go SDK pointed at the running e2e deployment; the
// exec-log-stream and files-api parity halves run inside TestExecLogStream and
// TestFilesAPI to share their Sandboxes.
func sdkClient(t *testing.T) *client.Client {
	t.Helper()
	return client.New(apiURL(), adminToken(t))
}

// TestSDKParityGPUSandboxReady runs M1 "gpu-sandbox-ready" through the client
// package on a real GPU host and compares it with the REST view.
func TestSDKParityGPUSandboxReady(t *testing.T) {
	admin := adminToken(t)
	host := startGPUHost(t, "e2e-sdk-gpu")
	waitForNodeReady(t, admin, host.nodeName)
	node := findNode(t, admin, host.nodeName)
	cards := requireGPUs(t, node)
	setGPUQuota(t, admin, int32(cards), int32(cards)+2)
	ctx := context.Background()
	c := sdkClient(t)

	body := gpuSandboxBody(t, node, 1)
	cpu := body["cpu"].(float64)
	memory := int64(body["memory"].(int))
	idleStop := int32(body["idle_stop"].(int))
	snapshot := body["snapshot"].(string)
	req := client.CreateSandboxRequest{
		Name:     strPtr(fmt.Sprintf("sdk-gpu-%d", time.Now().UnixNano())),
		CPU:      &cpu,
		Memory:   &memory,
		IdleStop: &idleStop,
		Snapshot: &snapshot,
		GPU:      &client.SandboxGPU{Count: 1},
	}
	created, err := c.CreateSandbox(ctx, req, "")
	if err != nil {
		t.Fatalf("sdk CreateSandbox: %v", err)
	}
	t.Cleanup(func() { _, _ = c.DeleteSandbox(context.Background(), created.ID) })
	ready, err := c.GetSandbox(ctx, created.ID, "ready")
	if err != nil {
		t.Fatalf("sdk wait_for=ready: %v", err)
	}
	if ready.State != "ready" || ready.GPU == nil || ready.GPU.Count != 1 {
		t.Fatalf("sdk Sandbox = %+v", ready)
	}

	// The REST view of the same Sandbox agrees.
	direct := getSandbox(t, admin, created.ID)
	if direct.State != ready.State || direct.CPU != ready.CPU || direct.Memory != ready.Memory ||
		direct.GPU == nil || direct.GPU.Count != ready.GPU.Count {
		t.Fatalf("sdk Sandbox = %+v, REST = %+v", ready, direct)
	}

	// The container sees exactly the allocated card.
	assigned := psqlCount(t, "SELECT gpu_uuid FROM gpu_allocations WHERE sandbox_id = '"+created.ID+"'")
	if assigned == "" {
		t.Fatalf("no GPU allocation for Sandbox %s", created.ID)
	}
	seen := remoteGPUUUIDs(t, host, created.ID)
	if len(seen) != 1 || seen[0] != assigned {
		t.Fatalf("the Sandbox sees %v, want the allocated card %s", seen, assigned)
	}

	// The SDK capacity view matches the REST one.
	capacity, err := c.Capacity(ctx)
	if err != nil {
		t.Fatalf("sdk Capacity: %v", err)
	}
	var allocated, total int
	for _, item := range capacity.Items {
		if item.NodeID != node.ID {
			continue
		}
		for _, gpu := range item.GPUs {
			allocated += gpu.Allocated
			total += gpu.Total
		}
	}
	restAllocated, restTotal := gpuCapacity(t, admin, node.ID)
	if allocated != restAllocated || total != restTotal || allocated != 1 || total != cards {
		t.Fatalf("sdk capacity = %d/%d, REST = %d/%d, want 1/%d",
			allocated, total, restAllocated, restTotal, cards)
	}

	// Deleting through the SDK releases the card.
	if _, err := c.DeleteSandbox(ctx, created.ID); err != nil {
		t.Fatalf("sdk DeleteSandbox: %v", err)
	}
	waitForGPUCapacity(t, admin, node.ID, 0)
	if used := myQuota(t, admin).GPUs.Used; used != 0 {
		t.Fatalf("GPU quota used after delete = %d, want 0", used)
	}
}

func strPtr(s string) *string { return &s }

// TestSDKParityPython is the Python half of the "sdk-parity" acceptance
// scenario (#110): sdk/python/tests/parity.py drives exec-log-stream and
// files-api through the Python SDK against this deployment, plus
// gpu-sandbox-ready when a GPU host is connected. The Go half is #108.
func TestSDKParityPython(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)

	suffix := time.Now().UnixNano()
	nodeName := fmt.Sprintf("e2e-sdk-parity-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)

	env := append(os.Environ(),
		"ORRERY_SANDBOX_API_URL="+apiURL(),
		"ORRERY_SANDBOX_TOKEN="+admin,
	)
	if os.Getenv("ORRERY_E2E_GPU_SSH") != "" {
		host := startGPUHost(t, "e2e-sdk-parity-gpu")
		waitForNodeReady(t, admin, host.nodeName)
		cards := requireGPUs(t, findNode(t, admin, host.nodeName))
		setGPUQuota(t, admin, 1, int32(cards)+2)
		env = append(env,
			"ORRERY_PARITY_GPU=1",
			"ORRERY_E2E_GPU_SNAPSHOT="+envOr("ORRERY_E2E_GPU_SNAPSHOT", "cuda"),
		)
	}

	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is required for the Python sdk-parity half: %v", err)
	}
	cmd := exec.Command(python, filepath.Join("..", "sdk", "python", "tests", "parity.py"))
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sdk-parity (python): %v\n%s", err, out)
	}
	t.Logf("sdk-parity (python):\n%s", out)
}
