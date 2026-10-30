//go:build e2e

package e2e

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// inspectSandboxRuntime is the Runtime's own record of a Sandbox's container:
// ctr's container info (labels and OCI spec).
func inspectSandboxRuntime(id string) ([]byte, error) {
	return sandboxCommand("containers", "info", "orrery-"+id).CombinedOutput()
}

// sandboxCommand runs ctr in the Agent's containerd namespace. ORRERY_E2E_CTR
// may name a ctr wrapper, for example one that runs ctr on the Node host over
// SSH.
func sandboxCommand(args ...string) *exec.Cmd {
	return exec.Command(envOr("ORRERY_E2E_CTR", "ctr"), append([]string{"-n", "orrery"}, args...)...)
}

func killSandboxRuntime(id string) ([]byte, error) {
	return sandboxCommand("task", "kill", "-s", "SIGKILL", "orrery-"+id).CombinedOutput()
}

func containerRunning(id string) bool {
	out, err := sandboxCommand("task", "ls").CombinedOutput()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if fields := strings.Fields(line); len(fields) == 3 && fields[0] == "orrery-"+id {
			return fields[2] == "RUNNING"
		}
	}
	return false
}

// hostGPUCount is the number of whole cards nvidia-smi lists on the Node host;
// a host without nvidia-smi has none.
func hostGPUCount() int {
	out, err := nodeShell("nvidia-smi --query-gpu=uuid --format=csv,noheader")
	if err != nil {
		return 0
	}
	return strings.Count(string(out), "GPU-")
}

func catInSandbox(id, path string) ([]byte, error) {
	return sandboxCommand("task", "exec", "--exec-id", fmt.Sprintf("e2e-cat-%d", time.Now().UnixNano()),
		"orrery-"+id, "cat", path).CombinedOutput()
}

// nodeShell runs a script on the Node host: locally with sh, or through
// ORRERY_E2E_NODE_SHELL, an `sh -c` stand-in such as one over SSH. Data-root
// paths, cgroups and the BuildKit endpoint live on that host.
func nodeShell(script string) ([]byte, error) {
	return exec.Command(envOr("ORRERY_E2E_NODE_SHELL", "sh"), "-c", script).CombinedOutput()
}

// shellQuote single-quotes s for a nodeShell or ssh script.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
