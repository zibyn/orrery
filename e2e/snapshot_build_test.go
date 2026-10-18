//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestSnapshotBuildRun is the "snapshot-build-run" acceptance scenario (#89):
// a Snapshot builds on the Node's host build API from a platform base, a
// Resource-backed named context and a Secret; the build log is readable
// through /execs/{id}/output, the build's RUN lands in a stars-build-*.slice,
// the result runs a Sandbox, and the build does not count toward User Quota.
// A failing Dockerfile ends the version failed.
func TestSnapshotBuildRun(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 100)
	platformName := os.Getenv("STARS_E2E_PLATFORM_SNAPSHOT")
	if platformName == "" {
		t.Fatal("STARS_E2E_PLATFORM_SNAPSHOT is not set; run ./scripts/e2e.sh")
	}
	suffix := time.Now().UnixNano()

	nodeName := fmt.Sprintf("e2e-build-node-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)
	t.Cleanup(func() { makeTreeRemovable(dataRoot) })

	// A Resource-backed named context.
	cli := newCLIRunner(t, admin)
	resourceName := fmt.Sprintf("build-ctx-%d", suffix)
	src := t.TempDir()
	contextValue := fmt.Sprintf("context-%d\n", suffix)
	writeTestFile(t, filepath.Join(src, "probe.txt"), contextValue, 0o644)
	var resourceVersion resourceVersionView
	mustJSON(t, cli.run(t, "", "resource", "push", src, resourceName), &resourceVersion)
	if resourceVersion.State != "ready" {
		t.Fatalf("context ResourceVersion = %+v, want ready", resourceVersion)
	}

	// A build Secret. It must appear in the build log nowhere.
	secretName := fmt.Sprintf("build-secret-%d", suffix)
	secretValue := fmt.Sprintf("build-secret-value-%d", suffix)
	if status, data := call(t, http.MethodPut, "/api/v1/secrets/"+secretName, admin,
		map[string]string{"value": secretValue}); status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("create build Secret: %d %s", status, data)
	}

	quotaBefore := myQuota(t, admin)
	snapshot := createTestSnapshot(t, admin, fmt.Sprintf("e2e-build-%d", suffix))
	dockerfile := "FROM base\n" +
		"COPY --from=ctx probe.txt /ctxfile\n" +
		"RUN --mount=type=secret,id=token cat /ctxfile > /probe && " +
		"cat /run/secrets/token >> /probe && echo >> /probe && " +
		"cat /proc/self/cgroup >> /probe && sleep 10\n"
	version := startBuild(t, admin, snapshot.ID, map[string]any{
		"dockerfile": dockerfile,
		"base":       platformName,
		"contexts":   map[string]string{"ctx": resourceName},
		"secrets":    map[string]string{"token": secretName},
	})
	if version.Source != "build" || version.BuildExecID == nil {
		t.Fatalf("build version = %+v, want source build with build_exec_id", version)
	}

	// runtime.md §12: the RUN must land in the build's limited slice. The
	// build's own /proc/self/cgroup is namespaced (cgroupns=private), so the
	// proof is the host cgroup tree while the RUN is alive.
	if path, cpuMax, memoryMax, ok := waitForBuildCgroup(t, version.ID, 2*time.Minute); !ok {
		t.Fatalf("no live process under stars-build-%s.slice", version.ID)
	} else {
		t.Logf("build RUN observed in %s", path)
		// The slice carries the deployment build limits (defaults.md: 4 CPU +
		// 8 GiB), so a RUN cannot exceed them.
		if cpuMax != "400000 100000" {
			t.Fatalf("build slice cpu.max = %q, want 400000 100000", cpuMax)
		}
		if memoryMax != "8589934592" {
			t.Fatalf("build slice memory.max = %q, want 8589934592", memoryMax)
		}
	}

	stdout, stderr, _, result := followExec(t, admin, *version.BuildExecID, 5*time.Minute)
	log := string(stdout) + string(stderr)
	if result.Type != "result" || result.Cause != "exited" {
		t.Fatalf("build Exec result = %+v, want exited", result)
	}
	if strings.Contains(log, secretValue) {
		t.Fatal("the build log leaked the Secret value")
	}
	if !strings.Contains(log, "context-") {
		// The context was copied by the build; the log itself need not show
		// it, so this is only a sanity hint when BuildKit echoed the step.
		t.Logf("build log did not echo the context; log starts: %.200s", log)
	}

	ready := waitForSnapshotVersion(t, admin, snapshot.ID, version.ID, "ready", 5*time.Minute)
	if ready.Digest == nil {
		t.Fatal("ready build has no digest")
	}
	var provenance struct {
		FromDigests []string `json:"from_digests"`
		Contexts    []struct {
			Name    string `json:"name"`
			Address string `json:"address"`
		} `json:"contexts"`
	}
	if err := json.Unmarshal(ready.Provenance, &provenance); err != nil {
		t.Fatalf("decode build provenance: %v", err)
	}
	if len(provenance.FromDigests) == 0 {
		t.Fatalf("build provenance has no FROM digests: %s", ready.Provenance)
	}
	if len(provenance.Contexts) != 1 || provenance.Contexts[0].Name != "ctx" ||
		provenance.Contexts[0].Address != *resourceVersion.Address {
		t.Fatalf("build provenance contexts = %+v", provenance.Contexts)
	}

	// The build holds an Allocation but never counts toward User Quota.
	quotaAfter := myQuota(t, admin)
	if quotaAfter.Sandboxes.Used != quotaBefore.Sandboxes.Used ||
		quotaAfter.GPUs.Used != quotaBefore.GPUs.Used {
		t.Fatalf("build changed User Quota: before %+v, after %+v", quotaBefore, quotaAfter)
	}

	// Run the built Snapshot: the probe carries the context, the secret once,
	// and the cgroup line.
	created := createSandbox(t, admin, map[string]any{
		"name": fmt.Sprintf("build-run-%d", suffix), "cpu": 0.5, "memory": 128 << 20,
		"idle_stop": 0, "snapshot": snapshot.Name,
	})
	defer deleteSandbox(t, admin, created.ID)
	sb := waitForSandboxState(t, admin, created.ID, "ready", 5*time.Minute)
	probe, _, code := execInSandbox(t, admin, sb.ID, "cat", "/probe")
	if code != 0 {
		t.Fatalf("cat /probe exited %d: %q", code, probe)
	}
	if !strings.Contains(probe, contextValue) {
		t.Fatalf("/probe = %q, want the context %q", probe, contextValue)
	}
	if got := strings.Count(probe, secretValue); got != 1 {
		t.Fatalf("/probe contains the Secret %d times, want exactly once: %q", got, probe)
	}
	if !regexp.MustCompile(`(?m)^[0-9]+::`).MatchString(probe) {
		t.Fatalf("/probe has no cgroup line: %q", probe)
	}

	// The image config and history must not carry the Secret. (The probe
	// layer deliberately contains it; that is what the RUN wrote.)
	imageID := dockerContainerImage(t, "stars-sandbox-"+sb.ID)
	config := dockerInspect(t, imageID, "{{json .Config}}")
	history := dockerInspect(t, imageID, "{{json .History}}")
	if strings.Contains(config, secretValue) || strings.Contains(history, secretValue) {
		t.Fatal("the built image config or history leaks the Secret value")
	}

	// A failing Dockerfile ends the version failed and leaves the current
	// pointer on the successful build.
	failing := startBuild(t, admin, snapshot.ID, map[string]any{
		"dockerfile": "FROM base\nRUN exit 1\n", "base": platformName,
	})
	failed := waitForSnapshotTerminal(t, admin, snapshot.ID, failing.ID, 5*time.Minute)
	if failed.State != "failed" || failed.Cause == nil || *failed.Cause != "build_failed" {
		t.Fatalf("failing build = state %q cause %v, want failed/build_failed", failed.State, failed.Cause)
	}
	current := findSnapshot(t, admin, snapshot.Name)
	if current.CurrentVersionID == nil || *current.CurrentVersionID != version.ID {
		t.Fatalf("current version = %v, want the successful build %s", current.CurrentVersionID, version.ID)
	}
}

// startBuild posts a build request and returns the created version.
func startBuild(t *testing.T, admin, snapshotID string, build map[string]any) snapshotVersionView {
	t.Helper()
	status, data := call(t, http.MethodPost, "/api/v1/snapshots/"+snapshotID+"/versions", admin,
		map[string]any{"build": build})
	if status != http.StatusCreated {
		t.Fatalf("start build: %d %s", status, data)
	}
	var version snapshotVersionView
	mustJSON(t, data, &version)
	return version
}

// waitForSnapshotTerminal waits for a terminal state without insisting on
// ready (the build-failure assertion).
func waitForSnapshotTerminal(t *testing.T, admin, snapshotID, versionID string, timeout time.Duration) snapshotVersionView {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		version := getSnapshotVersion(t, admin, snapshotID, versionID)
		if version.State != "building" {
			return version
		}
		if time.Now().After(deadline) {
			t.Fatalf("SnapshotVersion %s never reached a terminal state", versionID)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// waitForBuildCgroup reports a non-empty cgroup.procs under the build's slice.
// The Docker daemon may nest it under the system root or, for an unprivileged
// Agent, under the user manager's hierarchy.
func waitForBuildCgroup(t *testing.T, versionID string, timeout time.Duration) (path, cpuMax, memoryMax string, ok bool) {
	t.Helper()
	name := "stars-build-" + versionID + ".slice"
	deadline := time.Now().Add(timeout)
	for {
		if root := findBuildSliceRoot(name); root != "" {
			if path, live := findBuildCgroupProcess(root); live {
				return path, readCgroupFile(root, "cpu.max"), readCgroupFile(root, "memory.max"), true
			}
		}
		if time.Now().After(deadline) {
			return "", "", "", false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func findBuildCgroupProcess(root string) (string, bool) {
	var hit string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "cgroup.procs" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err == nil && strings.TrimSpace(string(raw)) != "" {
			hit = path
			return fs.SkipAll
		}
		return nil
	})
	return hit, hit != ""
}

// findBuildSliceRoot locates the unit's cgroup root. An unprivileged Agent
// uses the user manager, whose slice hierarchy nests at every '-'.
func findBuildSliceRoot(name string) string {
	var root string
	_ = filepath.WalkDir("/sys/fs/cgroup", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == name {
			root = path
			return fs.SkipAll
		}
		return nil
	})
	return root
}

func readCgroupFile(dir, name string) string {
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func dockerInspect(t *testing.T, ref, format string) string {
	t.Helper()
	var last []byte
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		out, err := exec.Command("docker", "image", "inspect", ref, "--format", format).CombinedOutput()
		if err == nil {
			return string(out)
		}
		last, lastErr = out, err
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("docker image inspect %s: %v\n%s", ref, lastErr, last)
	return ""
}

// dockerContainerImage resolves a running Sandbox container's image id, which
// is more robust than a registry digest reference for `docker image inspect`.
func dockerContainerImage(t *testing.T, container string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", container, "--format", "{{.Image}}").CombinedOutput()
	if err != nil {
		t.Fatalf("docker inspect %s: %v\n%s", container, err, out)
	}
	return strings.TrimSpace(string(out))
}
