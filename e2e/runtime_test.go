//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"strings"
)

// inspectSandboxRuntime is the Runtime's own record of a Sandbox's container:
// ctr's container info (labels and OCI spec).
func inspectSandboxRuntime(id string) ([]byte, error) {
	return sandboxCommand("containers", "info", "orrery-"+id).CombinedOutput()
}

// ctrCommand is ctr against the Agent's own containerd (ADR-0028).
// ORRERY_E2E_CTR replaces it whole with one executable, for example a wrapper
// that runs ctr on the Node host over SSH.
func ctrCommand() []string {
	if c := os.Getenv("ORRERY_E2E_CTR"); c != "" {
		return []string{c}
	}
	return []string{"ctr", "--address", "/run/orrery/containerd/containerd.sock"}
}

// sandboxCommand runs ctr in the Agent's containerd namespace.
func sandboxCommand(args ...string) *exec.Cmd {
	argv := append(ctrCommand(), "-n", "orrery")
	return exec.Command(argv[0], append(argv[1:], args...)...)
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
