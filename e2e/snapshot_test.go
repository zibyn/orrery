//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type snapshotView struct {
	ID               string  `json:"id"`
	OwnerID          *string `json:"owner_id"`
	Name             string  `json:"name"`
	CurrentVersionID *string `json:"current_version_id"`
}

type snapshotVersionView struct {
	ID          string          `json:"id"`
	SnapshotID  string          `json:"snapshot_id"`
	Digest      *string         `json:"digest"`
	State       string          `json:"state"`
	Cause       *string         `json:"cause"`
	Source      string          `json:"source"`
	BuildExecID *string         `json:"build_exec_id"`
	Provenance  json.RawMessage `json:"provenance"`
}

// TestSnapshotImport is the "snapshot-import" acceptance scenario (#88): an
// alpine RepoDigest imported into a Snapshot becomes runnable, proving the
// Agent pulls from the platform registry with the HelloAck credential.
func TestSnapshotImport(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 100)
	alpineRef := os.Getenv("STARS_E2E_ALPINE_REF")
	if alpineRef == "" {
		t.Fatal("STARS_E2E_ALPINE_REF is not set; run ./scripts/e2e.sh")
	}
	suffix := time.Now().UnixNano()
	name := fmt.Sprintf("e2e-import-%d", suffix)
	snapshot := createTestSnapshot(t, admin, name)

	version := importIntoSnapshot(t, admin, snapshot.ID, alpineRef, "")
	version = waitForSnapshotVersion(t, admin, snapshot.ID, version.ID, "ready", 2*time.Minute)
	if version.Digest == nil || !strings.HasPrefix(*version.Digest, "sha256:") {
		t.Fatalf("ready version digest = %v", version.Digest)
	}
	if version.Source != "import" {
		t.Fatalf("version source = %q", version.Source)
	}

	nodeName := fmt.Sprintf("e2e-import-node-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)

	created := createSandbox(t, admin, map[string]any{
		"name":      fmt.Sprintf("import-%d", suffix),
		"cpu":       0.5,
		"memory":    128 << 20,
		"idle_stop": 0,
		"snapshot":  name,
	})
	ready := waitForSandboxState(t, admin, created.ID, "ready", 3*time.Minute)
	if !strings.Contains(ready.Snapshot, "@sha256:") {
		t.Fatalf("Sandbox snapshot = %q, want a pinned digest reference", ready.Snapshot)
	}
	deleteSandbox(t, admin, created.ID)

	// Platform Snapshots are read-only through the API (api.md §3).
	platformName := os.Getenv("STARS_E2E_PLATFORM_SNAPSHOT")
	if platformName != "" {
		platform := findSnapshot(t, admin, platformName)
		if platform.OwnerID != nil {
			t.Fatalf("platform Snapshot %q has owner %v", platformName, platform.OwnerID)
		}
		if status, data := call(t, http.MethodDelete, "/api/v1/snapshots/"+platform.ID, admin, nil); status != http.StatusForbidden {
			t.Fatalf("delete platform Snapshot = %d %s, want 403", status, data)
		}
		if status, data := call(t, http.MethodPost, "/api/v1/snapshots/"+platform.ID+"/versions", admin,
			map[string]any{"import": map[string]any{"ref": alpineRef}}); status != http.StatusForbidden {
			t.Fatalf("mutate platform Snapshot = %d %s, want 403", status, data)
		}
	}
}

// TestSnapshotImportPrivateSource imports from the platform registry itself
// with an auth_secret, then runs a Sandbox from the result: the whole path is
// authorize source, copy, pull with the deployment credential.
func TestSnapshotImportPrivateSource(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 100)
	addr := os.Getenv("STARS_E2E_REGISTRY_ADDR")
	credential := os.Getenv("STARS_E2E_REGISTRY_CREDENTIAL")
	if addr == "" || credential == "" {
		t.Fatal("STARS_E2E_REGISTRY_ADDR and STARS_E2E_REGISTRY_CREDENTIAL are not set; run ./scripts/e2e.sh")
	}
	suffix := time.Now().UnixNano()
	dockerConfig := t.TempDir()
	if err := os.WriteFile(filepath.Join(dockerConfig, "config.json"), []byte(credential), 0o600); err != nil {
		t.Fatal(err)
	}
	buildDir := t.TempDir()
	dockerfile := fmt.Sprintf("FROM alpine:3.22\nRUN echo %d > /e2e-private-marker\n", suffix)
	if err := os.WriteFile(filepath.Join(buildDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	sourceRef := fmt.Sprintf("%s/snapshots/e2e-private-%d:test", addr, suffix)
	dockerCLI(t, dockerConfig, "build", "-t", sourceRef, buildDir)
	dockerCLI(t, dockerConfig, "push", sourceRef)
	digests := dockerCLI(t, dockerConfig, "image", "inspect", sourceRef,
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

	secretName := fmt.Sprintf("e2e-import-auth-%d", suffix)
	if status, data := call(t, http.MethodPut, "/api/v1/secrets/"+secretName, admin,
		map[string]string{"value": credential}); status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("create auth Secret: %d %s", status, data)
	}
	snapshot := createTestSnapshot(t, admin, fmt.Sprintf("e2e-private-%d", suffix))
	version := importIntoSnapshot(t, admin, snapshot.ID, digestRef, secretName)
	waitForSnapshotVersion(t, admin, snapshot.ID, version.ID, "ready", 2*time.Minute)

	// Only the platform registry can serve this image now.
	imageID := strings.TrimSpace(dockerCLI(t, dockerConfig, "image", "inspect", digestRef, "--format", "{{.Id}}"))
	dockerCLI(t, dockerConfig, "image", "rm", "-f", imageID)

	nodeName := fmt.Sprintf("e2e-private-node-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)
	created := createSandbox(t, admin, map[string]any{
		"name":      fmt.Sprintf("private-%d", suffix),
		"cpu":       0.5,
		"memory":    128 << 20,
		"idle_stop": 0,
		"snapshot":  snapshot.Name,
	})
	waitForSandboxState(t, admin, created.ID, "ready", 3*time.Minute)
	deleteSandbox(t, admin, created.ID)
}

// TestSnapshotDeleteGuard is the "snapshot-delete-guard" scenario: current
// versions and versions a stopped Sandbox pins cannot be deleted; deleting the
// Snapshot writes a tombstone and hides it.
func TestSnapshotDeleteGuard(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 100)
	addr := os.Getenv("STARS_E2E_REGISTRY_ADDR")
	credential := os.Getenv("STARS_E2E_REGISTRY_CREDENTIAL")
	platformName := os.Getenv("STARS_E2E_PLATFORM_SNAPSHOT")
	if addr == "" || credential == "" || platformName == "" {
		t.Fatal("the registry and platform Snapshot environment is not set; run ./scripts/e2e.sh")
	}
	suffix := time.Now().UnixNano()
	snapshot := createTestSnapshot(t, admin, fmt.Sprintf("e2e-guard-%d", suffix))

	// Import from the platform registry rather than Docker Hub: the second
	// import must not depend on an upstream pull rate limit.
	secretName := fmt.Sprintf("e2e-guard-auth-%d", suffix)
	if status, data := call(t, http.MethodPut, "/api/v1/secrets/"+secretName, admin,
		map[string]string{"value": credential}); status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("create auth Secret: %d %s", status, data)
	}
	platform := findSnapshot(t, admin, platformName)
	if platform.CurrentVersionID == nil {
		t.Fatalf("platform Snapshot %q has no current version", platformName)
	}
	platformVersion := getSnapshotVersion(t, admin, platform.ID, *platform.CurrentVersionID)
	if platformVersion.Digest == nil {
		t.Fatalf("platform SnapshotVersion %s has no digest", platformVersion.ID)
	}
	sourceRef := fmt.Sprintf("%s/platform/%s@%s", addr, platformName, *platformVersion.Digest)

	first := importIntoSnapshot(t, admin, snapshot.ID, sourceRef, secretName)
	first = waitForSnapshotVersion(t, admin, snapshot.ID, first.ID, "ready", 2*time.Minute)
	if status, data := call(t, http.MethodDelete,
		"/api/v1/snapshots/"+snapshot.ID+"/versions/"+first.ID, admin, nil); status != http.StatusConflict {
		t.Fatalf("delete current version = %d %s, want 409", status, data)
	}

	nodeName := fmt.Sprintf("e2e-guard-node-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)
	created := createSandbox(t, admin, map[string]any{
		"name":      fmt.Sprintf("guard-%d", suffix),
		"cpu":       0.5,
		"memory":    128 << 20,
		"idle_stop": 0,
		"snapshot":  snapshot.Name,
	})
	waitForSandboxState(t, admin, created.ID, "ready", 3*time.Minute)
	if status, data := call(t, http.MethodPost, "/api/v1/sandboxes/"+created.ID+"/stop", admin, nil); status != http.StatusAccepted {
		t.Fatalf("stop Sandbox: %d %s", status, data)
	}
	waitForSandboxState(t, admin, created.ID, "stopped", 2*time.Minute)

	// A new import takes the current pointer, so the pinned version is no
	// longer current but is still referenced by the stopped Sandbox.
	second := importIntoSnapshot(t, admin, snapshot.ID, sourceRef, secretName)
	waitForSnapshotVersion(t, admin, snapshot.ID, second.ID, "ready", 2*time.Minute)
	if status, data := call(t, http.MethodDelete,
		"/api/v1/snapshots/"+snapshot.ID+"/versions/"+first.ID, admin, nil); status != http.StatusConflict {
		t.Fatalf("delete a version referenced by a stopped Sandbox = %d %s, want 409", status, data)
	}
	if status, data := call(t, http.MethodDelete,
		"/api/v1/snapshots/"+snapshot.ID+"/versions/"+second.ID, admin, nil); status != http.StatusConflict {
		t.Fatalf("delete the current version = %d %s, want 409", status, data)
	}

	if status, data := call(t, http.MethodDelete, "/api/v1/snapshots/"+snapshot.ID, admin, nil); status != http.StatusAccepted {
		t.Fatalf("delete Snapshot: %d %s", status, data)
	}
	if status, _ := call(t, http.MethodGet, "/api/v1/snapshots/"+snapshot.ID, admin, nil); status != http.StatusNotFound {
		t.Fatalf("tombstoned Snapshot is still visible (status %d)", status)
	}
	if status, _ := call(t, http.MethodGet, "/api/v1/snapshots/"+snapshot.ID+"/versions/"+first.ID, admin, nil); status != http.StatusNotFound {
		t.Fatalf("versions of a tombstoned Snapshot are still visible (status %d)", status)
	}
	deleteSandbox(t, admin, created.ID)
}

func createTestSnapshot(t *testing.T, token, name string) snapshotView {
	t.Helper()
	status, data := call(t, http.MethodPost, "/api/v1/snapshots", token, map[string]string{"name": name})
	if status != http.StatusCreated {
		t.Fatalf("create Snapshot: %d %s", status, data)
	}
	var snap snapshotView
	mustJSON(t, data, &snap)
	return snap
}

func importIntoSnapshot(t *testing.T, token, snapshotID, ref, authSecret string) snapshotVersionView {
	t.Helper()
	payload := map[string]any{"ref": ref}
	if authSecret != "" {
		payload["auth_secret"] = authSecret
	}
	status, data := call(t, http.MethodPost, "/api/v1/snapshots/"+snapshotID+"/versions", token,
		map[string]any{"import": payload})
	if status != http.StatusCreated {
		t.Fatalf("import %s: %d %s", ref, status, data)
	}
	var version snapshotVersionView
	mustJSON(t, data, &version)
	return version
}

func getSnapshotVersion(t *testing.T, token, snapshotID, versionID string) snapshotVersionView {
	t.Helper()
	status, data := call(t, http.MethodGet, "/api/v1/snapshots/"+snapshotID+"/versions/"+versionID, token, nil)
	if status != http.StatusOK {
		t.Fatalf("get SnapshotVersion: %d %s", status, data)
	}
	var version snapshotVersionView
	mustJSON(t, data, &version)
	return version
}

func waitForSnapshotVersion(t *testing.T, token, snapshotID, versionID, want string, timeout time.Duration) snapshotVersionView {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		version := getSnapshotVersion(t, token, snapshotID, versionID)
		if version.State == want {
			return version
		}
		if version.State == "failed" || version.State == "cancelled" {
			cause := ""
			if version.Cause != nil {
				cause = *version.Cause
			}
			t.Fatalf("SnapshotVersion ended %s (cause %s), want %s", version.State, cause, want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("SnapshotVersion %s never became %s (state %q)", versionID, want, version.State)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func findSnapshot(t *testing.T, token, name string) snapshotView {
	t.Helper()
	cursor := ""
	for {
		path := "/api/v1/snapshots?limit=200"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		status, data := call(t, http.MethodGet, path, token, nil)
		if status != http.StatusOK {
			t.Fatalf("list Snapshots: %d %s", status, data)
		}
		var page struct {
			Items      []snapshotView `json:"items"`
			NextCursor *string        `json:"next_cursor"`
		}
		mustJSON(t, data, &page)
		for _, snap := range page.Items {
			if snap.Name == name {
				return snap
			}
		}
		if page.NextCursor == nil {
			t.Fatalf("Snapshot %q not found", name)
		}
		cursor = *page.NextCursor
	}
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
