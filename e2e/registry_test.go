//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRegistryPullWithDeploymentCredential proves HelloAck.registry_credential
// reaches the Agent: it pushes a small image to the platform registry with the
// deployment credential, removes the local copy, then creates a Sandbox from
// that digest ref. Without the credential the Agent's pull fails.
func TestRegistryPullWithDeploymentCredential(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)

	addr := os.Getenv("STARS_E2E_REGISTRY_ADDR")
	credential := os.Getenv("STARS_E2E_REGISTRY_CREDENTIAL")
	if addr == "" || credential == "" {
		t.Fatal("STARS_E2E_REGISTRY_ADDR and STARS_E2E_REGISTRY_CREDENTIAL are not set; run ./scripts/e2e.sh")
	}
	dockerConfig := t.TempDir()
	if err := os.WriteFile(filepath.Join(dockerConfig, "config.json"), []byte(credential), 0o600); err != nil {
		t.Fatal(err)
	}

	suffix := time.Now().UnixNano()
	buildDir := t.TempDir()
	dockerfile := fmt.Sprintf("FROM alpine:3.22\nRUN echo %d > /e2e-marker\n", suffix)
	if err := os.WriteFile(filepath.Join(buildDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	ref := fmt.Sprintf("%s/snapshots/e2e-%d:test", addr, suffix)
	dockerCLI(t, dockerConfig, "build", "-t", ref, buildDir)
	dockerCLI(t, dockerConfig, "push", ref)

	digests := dockerCLI(t, dockerConfig, "image", "inspect", ref,
		"--format", "{{range .RepoDigests}}{{println .}}{{end}}")
	var digestRef string
	for _, line := range strings.Split(digests, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), addr+"/") && strings.Contains(line, "@sha256:") {
			digestRef = strings.TrimSpace(line)
		}
	}
	if digestRef == "" {
		t.Fatalf("pushed image has no registry digest: %q", digests)
	}

	// Force the Agent to pull: without a local copy only the registry path can
	// produce the image. Removing just the tag leaves the repo digest, so
	// remove the image itself.
	imageID := strings.TrimSpace(dockerCLI(t, dockerConfig, "image", "inspect", digestRef, "--format", "{{.Id}}"))
	dockerCLI(t, dockerConfig, "image", "rm", "-f", imageID)
	if err := runDocker(dockerConfig, "image", "inspect", digestRef).Run(); err == nil {
		t.Fatalf("%s is still local after removing it; the pull cannot be proven", digestRef)
	}

	nodeName := fmt.Sprintf("e2e-registry-node-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)

	createBody := map[string]any{
		"name":      fmt.Sprintf("registry-%d", suffix),
		"cpu":       0.5,
		"memory":    128 << 20,
		"idle_stop": 0,
		"snapshot":  digestRef,
	}
	status, data := call(t, http.MethodPost, "/api/v1/sandboxes", admin, createBody)
	if status != http.StatusCreated {
		t.Fatalf("create Sandbox from %s: %d %s", digestRef, status, data)
	}
	var created sandboxView
	mustJSON(t, data, &created)

	status, data = call(t, http.MethodGet, "/api/v1/sandboxes/"+created.ID+"?wait_for=ready", admin, nil)
	if status != http.StatusOK {
		t.Fatalf("wait_for=ready: %d %s", status, data)
	}
	var ready sandboxView
	mustJSON(t, data, &ready)
	if ready.State != "ready" {
		t.Fatalf("Sandbox from the platform registry = %q (cause %v); the Agent could not pull with the deployment credential",
			ready.State, ready.Cause)
	}
	call(t, http.MethodDelete, "/api/v1/sandboxes/"+created.ID, admin, nil)
}

// dockerCLI runs a Docker CLI command with the deployment credential as its
// config.json, and returns its output.
func dockerCLI(t *testing.T, configDir string, args ...string) string {
	t.Helper()
	cmd := runDocker(configDir, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func runDocker(configDir string, args ...string) *exec.Cmd {
	cmd := exec.Command("docker", args...)
	cmd.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
	if socket := os.Getenv("STARS_E2E_DOCKER_SOCKET"); socket != "" {
		cmd.Env = append(cmd.Env, "DOCKER_HOST=unix://"+socket)
	}
	return cmd
}
