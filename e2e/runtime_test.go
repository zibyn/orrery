//go:build e2e

package e2e

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// e2eRuntime is the --runtime the shared scenarios' Agents run with: docker
// unless ORRERY_E2E_RUNTIME names another container Provider (#163).
func e2eRuntime() string { return envOr("ORRERY_E2E_RUNTIME", "docker") }

// sandboxCommand runs a container CLI against a Sandbox's Runtime: docker, or
// ctr in the Agent's containerd namespace. ORRERY_E2E_CTR may name a ctr
// wrapper, for example one that runs ctr on the Node host over SSH.
func sandboxCommand(docker []string, ctr ...string) *exec.Cmd {
	if e2eRuntime() != "containerd" {
		return exec.Command("docker", docker...)
	}
	return exec.Command(envOr("ORRERY_E2E_CTR", "ctr"), append([]string{"-n", "orrery"}, ctr...)...)
}

func killSandboxRuntime(id string) ([]byte, error) {
	return sandboxCommand([]string{"kill", "orrery-" + id},
		"task", "kill", "-s", "SIGKILL", "orrery-"+id).CombinedOutput()
}

func removeSandboxRuntime(id string) {
	if e2eRuntime() == "containerd" {
		return // the Agent's Stop or the next Agent's adopt removes it
	}
	_ = exec.Command("docker", "rm", "-f", "orrery-"+id).Run()
}

func containerRunning(id string) bool {
	if e2eRuntime() != "containerd" {
		out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", "orrery-"+id).CombinedOutput()
		return err == nil && strings.TrimSpace(string(out)) == "true"
	}
	out, err := sandboxCommand(nil, "task", "ls").CombinedOutput()
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

func catInSandbox(id, path string) ([]byte, error) {
	return sandboxCommand([]string{"exec", "orrery-" + id, "cat", path},
		"task", "exec", "--exec-id", fmt.Sprintf("e2e-cat-%d", time.Now().UnixNano()),
		"orrery-"+id, "cat", path).CombinedOutput()
}

// nodeShell runs a script on the Node host: locally with sh, or through
// ORRERY_E2E_NODE_SHELL, an `sh -c` stand-in such as one over SSH. Data-root
// paths, cgroups and the BuildKit endpoint live on that host.
func nodeShell(script string) ([]byte, error) {
	return exec.Command(envOr("ORRERY_E2E_NODE_SHELL", "sh"), "-c", script).CombinedOutput()
}
