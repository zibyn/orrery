//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

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
		"STARS_SANDBOX_API_URL="+apiURL(),
		"STARS_SANDBOX_TOKEN="+admin,
	)
	if os.Getenv("STARS_E2E_GPU_SSH") != "" {
		host := startGPUHost(t, "e2e-sdk-parity-gpu")
		waitForNodeReady(t, admin, host.nodeName)
		cards := requireGPUs(t, findNode(t, admin, host.nodeName))
		setGPUQuota(t, admin, 1, int32(cards)+2)
		env = append(env,
			"STARS_PARITY_GPU=1",
			"STARS_E2E_GPU_SNAPSHOT="+envOr("STARS_E2E_GPU_SNAPSHOT", "cuda"),
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
