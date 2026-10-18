package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
	"github.com/zibyn/stars-sandbox/internal/supervisor"
)

// Snapshot builds run on the host build API (ADR-0021, runtime.md §12). The
// Agent fetches each named context through the Node cache, writes the
// Dockerfile and Secret files to a temporary directory, and runs
// `docker buildx build` with the platform registry credential. Output is
// written to the build Exec's record and delivered by the normal ExecOutput
// machinery.

const (
	// buildLogCap mirrors defaults.md's Execution Log cap. BuildSnapshot has
	// no log_cap field in the protocol, so the deployment default is fixed
	// here.
	buildLogCap = int64(16 << 20)
	// buildChunk bounds one output frame.
	buildChunk = 32 << 10
)

var (
	buildTokenPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	// errBuildCanceled distinguishes CancelExec from a lost Control stream.
	errBuildCanceled = errors.New("build canceled")
)

// buildMarker is the on-disk record of an in-flight build, so a restarted
// Agent reports build_interrupted instead of retrying (runtime.md §12).
type buildMarker struct {
	VersionID string `json:"version_id"`
	SandboxID string `json:"sandbox_id"`
	ExecID    string `json:"exec_id"`
	PID       int    `json:"pid"`
}

func (r *Runtime) buildMarkerPath(versionID string) string {
	return filepath.Join(r.dataRoot, "builds", versionID+".json")
}

// buildSnapshot executes one BuildSnapshot command. Transport-level failures
// return an error (the command is redelivered); a build that fails is reported
// through BuildObserved and returns nil.
func (r *Runtime) buildSnapshot(ctx context.Context, cmd *nodev1.BuildSnapshot) error {
	versionID, err := uuid.Parse(cmd.GetVersionId())
	if err != nil {
		return fmt.Errorf("invalid version id: %w", err)
	}
	sandboxID, err := uuid.Parse(cmd.GetSandboxId())
	if err != nil {
		return fmt.Errorf("invalid sandbox id: %w", err)
	}
	execID, err := uuid.Parse(cmd.GetExecId())
	if err != nil {
		return fmt.Errorf("invalid exec id: %w", err)
	}
	// A reconnect redelivers the BuildSnapshot command. If this process
	// already finished the version, do nothing; if the marker still exists,
	// the build is running (or a crashed run will be resumed on startup).
	if r.buildCompleted(versionID.String()) {
		return nil
	}
	if _, err := os.Stat(r.buildMarkerPath(versionID.String())); err == nil {
		return nil
	}

	dir := filepath.Join(sandboxRunPath(r.dataRoot, sandboxID), execID.String())
	if _, err := os.Stat(filepath.Join(dir, "exit")); err == nil {
		// A redelivery after the record was written: deliver it again.
		go r.streamExec(ctx, sandboxID, execID, dir)
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create build record: %w", err)
	}
	record, err := newBuildRecord(filepath.Join(dir, "output"))
	if err != nil {
		return fmt.Errorf("create build output: %w", err)
	}
	defer record.Close()

	work, err := os.MkdirTemp(filepath.Join(r.dataRoot, "builds"), "work-")
	if err != nil {
		return fmt.Errorf("create build work directory: %w", err)
	}
	defer os.RemoveAll(work) //nolint:errcheck // best effort

	marker := buildMarker{
		VersionID: versionID.String(), SandboxID: sandboxID.String(), ExecID: execID.String(),
	}
	if err := writeBuildMarker(r.buildMarkerPath(versionID.String()), marker); err != nil {
		return fmt.Errorf("write build marker: %w", err)
	}
	completed := false
	defer func() {
		if completed {
			_ = os.Remove(r.buildMarkerPath(versionID.String()))
		}
	}()

	// Deliver output while the build runs. The stream context is the Control
	// stream; the build gets its own timeout/cancel context below.
	go r.streamExec(ctx, sandboxID, execID, dir)

	timeout := time.Duration(cmd.GetTimeoutSeconds()) * time.Second
	if timeout <= 0 {
		timeout = time.Hour
	}
	timeoutCtx, cancelTimeout := context.WithTimeout(ctx, timeout)
	defer cancelTimeout()
	runCtx, cancel := context.WithCancelCause(timeoutCtx)
	r.registerBuildCancel(execID.String(), cancel)
	defer func() {
		r.unregisterBuildCancel(execID.String())
		cancel(context.Canceled)
	}()

	args, cleanup, err := r.prepareBuild(runCtx, cmd, work)
	defer cleanup()
	if err != nil {
		// Preparation failures cannot succeed by retrying the same command
		// forever, but they are also not a build result: report build_failed.
		completed = true
		r.finishBuildRecord(dir, supervisor.Exit{Cause: "exited", ExitCode: i32(1)})
		r.send(&nodev1.AgentMessage{Message: &nodev1.AgentMessage_BuildObserved{
			BuildObserved: &nodev1.BuildObserved{VersionId: versionID.String(), Cause: "build_failed"},
		}})
		slog.Warn("prepare snapshot build", "version_id", versionID, "error", err)
		return nil
	}
	metadataPath := filepath.Join(work, "metadata.json")
	args = append(args, "--metadata-file", metadataPath)

	process := exec.Command("docker", args...)
	process.Env = append(os.Environ(),
		"DOCKER_HOST="+dockerHost(r.socket),
		"DOCKER_CONFIG="+filepath.Join(work, "docker"),
	)
	process.Stdout = buildStreamWriter{record: record, stream: supervisor.FrameStdout}
	process.Stderr = buildStreamWriter{record: record, stream: supervisor.FrameStderr}
	// A canceled build must take the whole process group with it: buildx
	// spawns a BuildKit client and, with the docker driver, build jobs of its
	// own.
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	process.Cancel = func() error {
		if process.Process == nil {
			return nil
		}
		return syscall.Kill(-process.Process.Pid, syscall.SIGKILL)
	}
	process.WaitDelay = 5 * time.Second
	if err := process.Start(); err != nil {
		completed = true
		r.finishBuildRecord(dir, supervisor.Exit{Cause: "start_failed"})
		r.send(&nodev1.AgentMessage{Message: &nodev1.AgentMessage_BuildObserved{
			BuildObserved: &nodev1.BuildObserved{VersionId: versionID.String(), Cause: "build_failed"},
		}})
		return fmt.Errorf("start build: %w", err)
	}
	marker.PID = process.Process.Pid
	if err := writeBuildMarker(r.buildMarkerPath(versionID.String()), marker); err != nil {
		slog.Warn("record build pid", "version_id", versionID, "error", err)
	}

	runErr := process.Run()
	buildCause := context.Cause(runCtx)
	switch {
	case errors.Is(buildCause, errBuildCanceled):
		// The Control Plane already moved the version to cancelled.
		completed = true
		r.finishBuildRecord(dir, supervisor.Exit{Cause: "canceled"})
		return nil
	case errors.Is(buildCause, context.DeadlineExceeded):
		completed = true
		r.finishBuildRecord(dir, supervisor.Exit{Cause: "timed_out"})
		r.send(&nodev1.AgentMessage{Message: &nodev1.AgentMessage_BuildObserved{
			BuildObserved: &nodev1.BuildObserved{VersionId: versionID.String(), Cause: "timed_out"},
		}})
		return nil
	case ctx.Err() != nil:
		// The Control stream ended (or the Agent is shutting down). Leave the
		// marker so the next startup reports build_interrupted and never
		// retries on its own (runtime.md §12).
		slog.Info("snapshot build interrupted by a lost connection", "version_id", versionID)
		return nil
	}
	if runErr != nil {
		completed = true
		exitCode := int32(-1)
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = int32(exitErr.ExitCode())
		}
		r.finishBuildRecord(dir, supervisor.Exit{Cause: "exited", ExitCode: &exitCode})
		r.send(&nodev1.AgentMessage{Message: &nodev1.AgentMessage_BuildObserved{
			BuildObserved: &nodev1.BuildObserved{VersionId: versionID.String(), Cause: "build_failed"},
		}})
		slog.Warn("snapshot build failed", "version_id", versionID, "error", runErr)
		return nil
	}
	metadata, err := readBuildMetadata(metadataPath)
	if err != nil || metadata.Digest == "" {
		completed = true
		r.finishBuildRecord(dir, supervisor.Exit{Cause: "exited", ExitCode: i32(1)})
		r.send(&nodev1.AgentMessage{Message: &nodev1.AgentMessage_BuildObserved{
			BuildObserved: &nodev1.BuildObserved{VersionId: versionID.String(), Cause: "build_failed"},
		}})
		return nil
	}
	completed = true
	r.finishBuildRecord(dir, supervisor.Exit{Cause: "exited", ExitCode: i32(0)})
	r.send(&nodev1.AgentMessage{Message: &nodev1.AgentMessage_BuildObserved{
		BuildObserved: &nodev1.BuildObserved{
			VersionId: versionID.String(), Digest: metadata.Digest, FromDigests: metadata.FromDigests,
		},
	}})
	r.markBuildCompleted(versionID.String())
	return nil
}

func i32(v int32) *int32 { return &v }

// prepareBuild writes the Dockerfile, the empty build context, Secret files
// and the registry config, and returns the buildx arguments. cleanup removes
// everything temporary.
func (r *Runtime) prepareBuild(ctx context.Context, cmd *nodev1.BuildSnapshot, work string) ([]string, func(), error) {
	cleanup := func() {}
	if err := os.Chmod(work, 0o700); err != nil {
		return nil, cleanup, err
	}
	emptyContext := filepath.Join(work, "context")
	if err := os.Mkdir(emptyContext, 0o700); err != nil {
		return nil, cleanup, err
	}
	dockerfile := filepath.Join(work, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte(cmd.GetDockerfile()), 0o600); err != nil {
		return nil, cleanup, err
	}
	args := []string{
		"buildx", "build",
		"--file", dockerfile,
		"--progress", "plain",
		"--push",
	}
	pushRepository := strings.TrimSuffix(cmd.GetPushRepository(), "/")
	if pushRepository == "" {
		return nil, cleanup, errors.New("push_repository is required")
	}
	args = append(args, "-t", pushRepository+":build-"+cmd.GetVersionId())
	if cmd.GetBaseDigest() != "" {
		args = append(args, "--build-context", "base=docker-image://"+cmd.GetBaseDigest())
	}
	for _, buildCtx := range cmd.GetContexts() {
		if !buildTokenPattern.MatchString(buildCtx.GetName()) {
			return nil, cleanup, fmt.Errorf("invalid context name %q", buildCtx.GetName())
		}
		tree, err := r.ensureResourceVersion(ctx, buildCtx.GetAddress())
		if err != nil {
			return nil, cleanup, fmt.Errorf("fetch context %s: %w", buildCtx.GetName(), err)
		}
		args = append(args, "--build-context", buildCtx.GetName()+"="+tree)
	}
	if len(cmd.GetSecrets()) > 0 {
		// A secret mount never enters the cache key, so a cached layer could
		// leak another User's secret-derived output (runtime.md §12).
		args = append(args, "--no-cache")
	}
	for _, secret := range cmd.GetSecrets() {
		if !buildTokenPattern.MatchString(secret.GetId()) {
			return nil, cleanup, fmt.Errorf("invalid secret id %q", secret.GetId())
		}
		path := filepath.Join(work, "secret-"+secret.GetId())
		if err := os.WriteFile(path, []byte(secret.GetValue()), 0o600); err != nil {
			return nil, cleanup, err
		}
		args = append(args, "--secret", "id="+secret.GetId()+",src="+path)
	}
	// buildx reads the deployment credential from a standard Docker config.
	auth := r.registryDocument()
	if auth != "" {
		configDir := filepath.Join(work, "docker")
		if err := os.Mkdir(configDir, 0o700); err != nil {
			return nil, cleanup, err
		}
		if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(auth), 0o600); err != nil {
			return nil, cleanup, err
		}
	}
	if parent, stop := r.buildCgroup(ctx, cmd.GetVersionId(), cmd.GetLimits().GetCpu(), cmd.GetLimits().GetMemory()); parent != "" {
		args = append(args, "--cgroup-parent", parent)
		cleanup = stop
	}
	args = append(args, emptyContext)
	return args, cleanup, nil
}

// finishBuildRecord writes the exit record the ExecOutput stream delivers. It
// is a no-op once the record already has one.
func (r *Runtime) finishBuildRecord(dir string, exit supervisor.Exit) {
	if _, err := os.Stat(filepath.Join(dir, "exit")); err == nil {
		return
	}
	if err := supervisor.WriteExit(dir, exit); err != nil {
		slog.Warn("write build exit record", "dir", dir, "error", err)
	}
}

// registryDocument returns the HelloAck credential document for DOCKER_CONFIG.
func (r *Runtime) registryDocument() string {
	r.registryMu.Lock()
	defer r.registryMu.Unlock()
	if r.registry == nil {
		return ""
	}
	return r.registry.raw
}

// --- output record ------------------------------------------------------

// buildRecord appends frames in the Supervisor's `[stream][len][data]` format.
type buildRecord struct {
	mu       sync.Mutex
	f        *os.File
	seq      int64
	truncate bool
}

func newBuildRecord(path string) (*buildRecord, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &buildRecord{f: f}, nil
}

func (w *buildRecord) Close() error { return w.f.Close() }

func (w *buildRecord) write(stream byte, data []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.truncate {
		return
	}
	for len(data) > 0 {
		chunk := data
		if len(chunk) > buildChunk {
			chunk = chunk[:buildChunk]
		}
		data = data[len(chunk):]
		if buildLogCap > 0 && w.seq+supervisor.HeaderSize+int64(len(chunk)) > buildLogCap {
			header := []byte{supervisor.FrameTruncated, 0, 0, 0, 0}
			_, _ = w.f.Write(header)
			w.truncate = true
			return
		}
		header := make([]byte, supervisor.HeaderSize)
		header[0] = stream
		header[1] = byte(len(chunk) >> 24)
		header[2] = byte(len(chunk) >> 16)
		header[3] = byte(len(chunk) >> 8)
		header[4] = byte(len(chunk))
		if _, err := w.f.Write(header); err != nil {
			w.truncate = true
			return
		}
		if _, err := w.f.Write(chunk); err != nil {
			w.truncate = true
			return
		}
		w.seq += supervisor.HeaderSize + int64(len(chunk))
	}
}

type buildStreamWriter struct {
	record *buildRecord
	stream byte
}

func (w buildStreamWriter) Write(p []byte) (int, error) {
	w.record.write(w.stream, p)
	return len(p), nil
}

// --- build metadata -----------------------------------------------------

type buildMetadata struct {
	Digest      string
	FromDigests []string
}

// readBuildMetadata parses buildx's metadata file. BuildKit reports the FROM
// materials with the digests it actually resolved.
func readBuildMetadata(path string) (buildMetadata, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return buildMetadata{}, err
	}
	var doc struct {
		Digest string `json:"containerimage.digest"`
		Prov   struct {
			Materials []struct {
				Digest struct {
					SHA256 string `json:"sha256"`
				} `json:"digest"`
			} `json:"materials"`
		} `json:"buildx.build.provenance"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return buildMetadata{}, err
	}
	seen := map[string]bool{}
	var from []string
	for _, material := range doc.Prov.Materials {
		if material.Digest.SHA256 == "" {
			continue
		}
		digest := "sha256:" + material.Digest.SHA256
		if !seen[digest] {
			seen[digest] = true
			from = append(from, digest)
		}
	}
	sort.Strings(from)
	return buildMetadata{Digest: doc.Digest, FromDigests: from}, nil
}

// --- cancellation and restart ------------------------------------------

func (r *Runtime) registerBuildCancel(execID string, cancel context.CancelCauseFunc) {
	r.buildMu.Lock()
	defer r.buildMu.Unlock()
	r.buildCancels[execID] = cancel
}

func (r *Runtime) unregisterBuildCancel(execID string) {
	r.buildMu.Lock()
	defer r.buildMu.Unlock()
	delete(r.buildCancels, execID)
}

// cancelBuild aborts a buildx process when the Exec names a build.
func (r *Runtime) cancelBuild(execID string) bool {
	r.buildMu.Lock()
	cancel, ok := r.buildCancels[execID]
	r.buildMu.Unlock()
	if ok {
		cancel(errBuildCanceled)
	}
	return ok
}

func (r *Runtime) buildCompleted(versionID string) bool {
	r.buildMu.Lock()
	defer r.buildMu.Unlock()
	_, ok := r.buildDone[versionID]
	return ok
}

func (r *Runtime) markBuildCompleted(versionID string) {
	r.buildMu.Lock()
	defer r.buildMu.Unlock()
	r.buildDone[versionID] = struct{}{}
}

// resumeBuilds applies runtime.md §12 to markers a previous Agent process (or
// a lost Control stream) left behind: the build is interrupted, never retried.
func (r *Runtime) resumeBuilds(ctx context.Context) {
	entries, err := os.ReadDir(filepath.Join(r.dataRoot, "builds"))
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(r.dataRoot, "builds", entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var marker buildMarker
		if err := json.Unmarshal(raw, &marker); err != nil || marker.VersionID == "" {
			_ = os.Remove(path)
			continue
		}
		killBuildPID(marker.PID)
		if sandboxID, err := uuid.Parse(marker.SandboxID); err == nil {
			if execID, err := uuid.Parse(marker.ExecID); err == nil {
				_ = os.RemoveAll(filepath.Join(sandboxRunPath(r.dataRoot, sandboxID), execID.String()))
			}
		}
		stopBuildSlice(ctx, marker.VersionID)
		r.markBuildCompleted(marker.VersionID)
		r.send(&nodev1.AgentMessage{Message: &nodev1.AgentMessage_BuildObserved{
			BuildObserved: &nodev1.BuildObserved{VersionId: marker.VersionID, Cause: "build_interrupted"},
		}})
		_ = os.Remove(path)
		slog.Info("reported interrupted snapshot build", "version_id", marker.VersionID)
	}
}

// killBuildPID ends a buildx process a previous Agent left running. The
// command line is checked so a reused PID is never killed blindly.
func killBuildPID(pid int) {
	if pid <= 0 {
		return
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || !strings.Contains(string(raw), "buildx") {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

func writeBuildMarker(path string, marker buildMarker) error {
	raw, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// --- cgroup slice -------------------------------------------------------

// buildCgroup creates a CPU- and memory-limited slice and returns the value
// for `--cgroup-parent`. A root Agent uses the system manager and the slice
// name; an unprivileged Agent uses its user manager and the cgroupfs path,
// because the Docker daemon (root) cannot see the user manager's unit.
// An empty parent means the Agent could not create the slice; the build then
// runs without a per-build ceiling and the deployment must bound builds
// Node-wide (runtime.md §12's documented fallback).
func (r *Runtime) buildCgroup(ctx context.Context, versionID string, cpu float64, memory int64) (string, func()) {
	if cpu <= 0 || memory <= 0 {
		return "", func() {}
	}
	slice := "stars-build-" + versionID + ".slice"
	quota := "CPUQuota=" + strconv.FormatInt(int64(cpu*100), 10) + "%"
	memoryMax := "MemoryMax=" + strconv.FormatInt(memory, 10)
	if os.Geteuid() == 0 {
		if runSystemctl(ctx, "set-property", "--runtime", slice, quota, memoryMax) == nil {
			return slice, func() { _ = runSystemctl(context.Background(), "stop", slice) }
		}
		return "", func() {}
	}
	if runSystemctl(ctx, "--user", "set-property", "--runtime", slice, quota, memoryMax) != nil {
		return "", func() {}
	}
	parent := userSliceParent(os.Getuid(), slice)
	if _, err := os.Stat(filepath.Join("/sys/fs/cgroup", parent)); err != nil {
		return "", func() {}
	}
	return parent, func() { _ = runSystemctl(context.Background(), "--user", "stop", slice) }
}

// userSliceParent mirrors systemd's slice hierarchy: stars-build-<id>.slice
// lives under stars-build.slice, under stars.slice, in the user manager.
func userSliceParent(uid int, slice string) string {
	return fmt.Sprintf("user.slice/user-%d.slice/user@%d.service/stars.slice/stars-build.slice/%s", uid, uid, slice)
}

func runSystemctl(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR=/run/user/"+strconv.Itoa(os.Getuid()))
	return cmd.Run()
}

func stopBuildSlice(ctx context.Context, versionID string) {
	slice := "stars-build-" + versionID + ".slice"
	if os.Geteuid() == 0 {
		_ = runSystemctl(ctx, "stop", slice)
		return
	}
	_ = runSystemctl(ctx, "--user", "stop", slice)
}

var _ io.Writer = buildStreamWriter{}
