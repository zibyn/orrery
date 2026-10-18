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
	"strings"
	"sync"
	"time"

	"github.com/docker/cli/cli/config/types"
	"github.com/google/uuid"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/tonistiigi/fsutil"

	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
	"github.com/zibyn/stars-sandbox/internal/dockerfile"
	"github.com/zibyn/stars-sandbox/internal/supervisor"
)

// Snapshot builds run through BuildKit's Go client against a BuildKit endpoint
// (ADR-0021, runtime.md §12). With no --buildkit address the endpoint is
// dockerd's built-in BuildKit, reached through the Docker socket's /grpc h2c
// upgrade. Named contexts are session LocalMounts, base is a named
// docker-image context, Secrets come from the session's secrets provider, and
// push credentials from its auth provider. Output is written to the build
// Exec's record and delivered by the normal ExecOutput machinery.

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
	// fromDigestPattern captures the BuildKit-resolved FROM digest from the
	// progress vertex name (e.g. "[1/2] FROM repo@sha256:…").
	fromDigestPattern = regexp.MustCompile(`(?i)\bFROM\s+\S*@(sha256:[0-9a-f]{64})`)
	// pinnedDigestPattern finds digest-pinned image references in FROM and
	// --from= positions, for the provenance fallback.
	pinnedDigestPattern = regexp.MustCompile(`@(sha256:[0-9a-f]{64})`)
)

// buildMarker is the on-disk record of an in-flight build, so a restarted
// Agent reports build_interrupted instead of retrying (runtime.md §12).
type buildMarker struct {
	VersionID string `json:"version_id"`
	SandboxID string `json:"sandbox_id"`
	ExecID    string `json:"exec_id"`
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
	sandboxDir := sandboxPath(r.dataRoot, sandboxID)
	if err := os.MkdirAll(sandboxDir, 0o755); err != nil {
		return fmt.Errorf("create build sandbox directory: %w", err)
	}
	// A build Sandbox has no container; mark it so the startup record
	// reconciliation does not mistake it for a lost Runtime.
	if err := os.WriteFile(filepath.Join(sandboxDir, "build"), []byte("build\n"), 0o644); err != nil {
		return fmt.Errorf("mark build sandbox: %w", err)
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

	// Pin the build's inputs for cache eviction (runtime.md §11). The pin is
	// released when the build ends, and by resumeBuilds after an interruption.
	pins := sandboxPins{ImageDigest: cmd.GetBaseDigest()}
	for _, buildCtx := range cmd.GetContexts() {
		pins.Resources = append(pins.Resources, buildCtx.GetAddress())
	}
	if err := writeSandboxPins(sandboxDir, pins); err != nil {
		return fmt.Errorf("pin build inputs: %w", err)
	}

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
			_ = os.Remove(pinsPath(sandboxDir))
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

	result, err := r.solveBuild(runCtx, cmd, work, buildStreamWriter{record: record, stream: supervisor.FrameStdout})
	writeExit := func(exit supervisor.Exit) {
		if _, statErr := os.Stat(filepath.Join(dir, "exit")); statErr != nil {
			if writeErr := supervisor.WriteExit(dir, exit); writeErr != nil {
				slog.Warn("write build exit record", "dir", dir, "error", writeErr)
			}
		}
	}
	switch {
	case err == nil:
		completed = true
		writeExit(supervisor.Exit{Cause: "exited", ExitCode: i32(0)})
		r.send(&nodev1.AgentMessage{Message: &nodev1.AgentMessage_BuildObserved{
			BuildObserved: &nodev1.BuildObserved{
				VersionId: versionID.String(), Digest: result.Digest, FromDigests: result.FromDigests,
			},
		}})
		r.markBuildCompleted(versionID.String())
		return nil
	case errors.Is(context.Cause(runCtx), errBuildCanceled):
		// The Control Plane already moved the version to cancelled.
		completed = true
		writeExit(supervisor.Exit{Cause: "canceled"})
		return nil
	case errors.Is(context.Cause(runCtx), context.DeadlineExceeded):
		completed = true
		writeExit(supervisor.Exit{Cause: "timed_out"})
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
	completed = true
	writeExit(supervisor.Exit{Cause: "exited", ExitCode: i32(1)})
	r.send(&nodev1.AgentMessage{Message: &nodev1.AgentMessage_BuildObserved{
		BuildObserved: &nodev1.BuildObserved{VersionId: versionID.String(), Cause: "build_failed"},
	}})
	slog.Warn("snapshot build failed", "version_id", versionID, "error", err)
	return nil
}

func i32(v int32) *int32 { return &v }

// solveResult is what a successful BuildKit Solve reports.
type solveResult struct {
	Digest      string
	FromDigests []string
}

// solveBuild prepares the session inputs and runs the dockerfile frontend.
func (r *Runtime) solveBuild(ctx context.Context, cmd *nodev1.BuildSnapshot, work string, output io.Writer) (solveResult, error) {
	dockerfileDir := filepath.Join(work, "dockerfile")
	emptyContext := filepath.Join(work, "context")
	for _, dir := range []string{dockerfileDir, emptyContext} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return solveResult{}, err
		}
	}
	if err := os.WriteFile(filepath.Join(dockerfileDir, "Dockerfile"), []byte(cmd.GetDockerfile()), 0o600); err != nil {
		return solveResult{}, err
	}
	mounts := map[string]fsutil.FS{}
	for name, dir := range map[string]string{"dockerfile": dockerfileDir, "context": emptyContext} {
		mount, err := fsutil.NewFS(dir)
		if err != nil {
			return solveResult{}, err
		}
		mounts[name] = mount
	}
	attrs := map[string]string{
		"filename":      "Dockerfile",
		"frontend.caps": "moby.buildkit.frontend.contexts+forward",
	}
	for _, buildCtx := range cmd.GetContexts() {
		if !buildTokenPattern.MatchString(buildCtx.GetName()) {
			return solveResult{}, fmt.Errorf("invalid context name %q", buildCtx.GetName())
		}
		tree, err := r.ensureResourceVersion(ctx, buildCtx.GetAddress())
		if err != nil {
			return solveResult{}, fmt.Errorf("fetch context %s: %w", buildCtx.GetName(), err)
		}
		mount, err := fsutil.NewFS(tree)
		if err != nil {
			return solveResult{}, err
		}
		// `context` and `dockerfile` name the frontend's own mounts; a user
		// context with either name needs a distinct local name (as buildx does).
		localName := buildCtx.GetName()
		if localName == "context" || localName == "dockerfile" {
			localName = "_" + localName
		}
		mounts[localName] = mount
		attrs["context:"+buildCtx.GetName()] = "local:" + localName
	}
	if cmd.GetBaseDigest() != "" {
		attrs["context:base"] = "docker-image://" + cmd.GetBaseDigest()
	}
	secretMap := map[string][]byte{}
	for _, secret := range cmd.GetSecrets() {
		if !buildTokenPattern.MatchString(secret.GetId()) {
			return solveResult{}, fmt.Errorf("invalid secret id %q", secret.GetId())
		}
		secretMap[secret.GetId()] = []byte(secret.GetValue())
	}
	if len(secretMap) > 0 {
		// Secrets are not part of the cache key and buildkitd's cache is
		// shared by all its clients (runtime.md §12).
		attrs["no-cache"] = ""
	}
	if parent, stop := r.buildCgroup(ctx, cmd.GetVersionId(), cmd.GetLimits().GetCpu(), cmd.GetLimits().GetMemory()); parent != "" {
		attrs["cgroup-parent"] = parent
		defer stop()
	}
	pushRepository := strings.TrimSuffix(cmd.GetPushRepository(), "/")
	if pushRepository == "" {
		return solveResult{}, errors.New("push_repository is required")
	}

	c, err := r.buildkitClient(ctx)
	if err != nil {
		return solveResult{}, err
	}
	defer c.Close() //nolint:errcheck // the solve is done
	attachments := []session.Attachable{r.authProvider()}
	if len(secretMap) > 0 {
		attachments = append(attachments, secretsprovider.FromMap(secretMap))
	}

	statusCh := make(chan *client.SolveStatus)
	displayCh := make(chan *client.SolveStatus)
	digests := map[string]struct{}{}
	var mu sync.Mutex
	go func() {
		for status := range statusCh {
			mu.Lock()
			collectFromDigests(status, digests)
			mu.Unlock()
			select {
			case displayCh <- status:
			case <-ctx.Done():
			}
		}
		close(displayCh)
	}()
	display, err := progressui.NewDisplay(output, progressui.PlainMode)
	if err != nil {
		return solveResult{}, err
	}
	displayDone := make(chan struct{})
	go func() {
		defer close(displayDone)
		_, _ = display.UpdateFrom(ctx, displayCh)
	}()

	response, err := c.Solve(ctx, nil, client.SolveOpt{
		Frontend:      "dockerfile.v0",
		FrontendAttrs: attrs,
		LocalMounts:   mounts,
		Session:       attachments,
		Exports: []client.ExportEntry{{
			Type: client.ExporterImage,
			Attrs: map[string]string{
				"name":   pushRepository + ":build-" + cmd.GetVersionId(),
				"push":   "true",
				"unpack": "false",
			},
		}},
	}, statusCh)
	<-displayDone
	if err != nil {
		return solveResult{}, err
	}
	digest := response.ExporterResponse[exptypes.ExporterImageDigestKey]
	mu.Lock()
	fromDigests := mergeDigests(digests, digestSet(pinnedFromDigests(cmd.GetDockerfile(), cmd.GetBaseDigest())))
	mu.Unlock()
	return solveResult{Digest: digest, FromDigests: fromDigests}, nil
}

// authProvider supplies the platform registry credential to BuildKit for the
// base pull and the push (node-protocol.md "构建").
func (r *Runtime) authProvider() session.Attachable {
	return authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{
		AuthConfigProvider: func(_ context.Context, host string, _ []string, _ authprovider.ExpireCachedAuthCheck) (types.AuthConfig, error) {
			r.registryMu.Lock()
			defer r.registryMu.Unlock()
			host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
			cfg, ok := r.registry.Get(host)
			if !ok {
				return types.AuthConfig{}, nil
			}
			return types.AuthConfig{Username: cfg.Username, Password: cfg.Password, ServerAddress: host}, nil
		},
	})
}

// collectFromDigests records the FROM digests BuildKit actually resolved from
// the progress stream (the plain line names the resolved ref).
func collectFromDigests(status *client.SolveStatus, set map[string]struct{}) {
	scan := func(text string) {
		for _, match := range fromDigestPattern.FindAllStringSubmatch(text, -1) {
			set[match[1]] = struct{}{}
		}
	}
	for _, vertex := range status.Vertexes {
		scan(vertex.Name)
	}
	for _, log := range status.Logs {
		scan(string(log.Data))
	}
}

// pinnedFromDigests is the provenance fallback: every external FROM and
// --from= reference was validated as digest-pinned at create time, and the
// base Snapshot is pinned too.
func pinnedFromDigests(text, baseDigest string) []string {
	set := map[string]struct{}{}
	add := func(ref string) {
		if match := pinnedDigestPattern.FindStringSubmatch(ref); match != nil {
			set[match[1]] = struct{}{}
		}
	}
	add(baseDigest)
	// Every external image reference in a Dockerfile is a FROM or --from=
	// source and was validated as digest-pinned at create time. Scanning the
	// logical lines captures both forms (including RUN --mount=...,from=).
	for _, line := range dockerfile.LogicalLines(text) {
		for _, match := range pinnedDigestPattern.FindAllStringSubmatch(line, -1) {
			set[match[1]] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for digest := range set {
		out = append(out, digest)
	}
	sort.Strings(out)
	return out
}

func digestSet(digests []string) map[string]struct{} {
	set := make(map[string]struct{}, len(digests))
	for _, digest := range digests {
		set[digest] = struct{}{}
	}
	return set
}

// mergeDigests unions the Solve-resolved digests with the pinned fallback.
func mergeDigests(sets ...map[string]struct{}) []string {
	merged := map[string]struct{}{}
	for _, set := range sets {
		for digest := range set {
			merged[digest] = struct{}{}
		}
	}
	out := make([]string, 0, len(merged))
	for digest := range merged {
		out = append(out, digest)
	}
	sort.Strings(out)
	return out
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

// cancelBuild aborts a Solve when the Exec names a build.
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
		if sandboxID, err := uuid.Parse(marker.SandboxID); err == nil {
			sandboxDir := sandboxPath(r.dataRoot, sandboxID)
			_ = os.Remove(pinsPath(sandboxDir))
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
// for the frontend's cgroup-parent attribute. A root Agent uses the system
// manager and the slice name; an unprivileged Agent uses its user manager and
// the cgroupfs path, because the Docker daemon (root) cannot see the user
// manager's unit. An empty parent means the Node cannot bound a single build;
// the deployment must bound builds Node-wide (runtime.md §12).
func (r *Runtime) buildCgroup(ctx context.Context, versionID string, cpu float64, memory int64) (string, func()) {
	if cpu <= 0 || memory <= 0 {
		return "", func() {}
	}
	slice := "stars-build-" + versionID + ".slice"
	quota := "CPUQuota=" + fmt.Sprint(int64(cpu*100)) + "%"
	memoryMax := "MemoryMax=" + fmt.Sprint(memory)
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

// userSliceParent mirrors systemd's slice hierarchy. systemd treats every '-'
// in a slice name as a hierarchy separator, so stars-build-<uuid>.slice lives
// under stars-build-<prefix>.slice for each prefix of its name.
func userSliceParent(uid int, slice string) string {
	base := strings.TrimSuffix(slice, ".slice")
	parts := strings.Split(base, "-")
	dirs := make([]string, 0, len(parts))
	prefix := ""
	for _, part := range parts {
		if prefix == "" {
			prefix = part
		} else {
			prefix += "-" + part
		}
		dirs = append(dirs, prefix+".slice")
	}
	return fmt.Sprintf("user.slice/user-%d.slice/user@%d.service/%s", uid, uid, strings.Join(dirs, "/"))
}

func runSystemctl(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR=/run/user/"+fmt.Sprint(os.Getuid()))
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
