package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
	"github.com/zibyn/stars-sandbox/internal/dockerfile"
	"github.com/zibyn/stars-sandbox/internal/registry"
	"github.com/zibyn/stars-sandbox/internal/store"
)

// Snapshot builds (api.md §3 "build", node-protocol.md "构建", runtime.md §12,
// ADR-0021). A build is an asynchronous job on a Node: this file validates the
// request, creates the internal kind=build Sandbox row, derives the
// BuildSnapshot command, and applies the Agent's BuildObserved.

type snapshotBuildRequest struct {
	Dockerfile string            `json:"dockerfile"`
	Base       *string           `json:"base"`
	Contexts   map[string]string `json:"contexts"`
	Secrets    map[string]string `json:"secrets"`
}

// storedBuildRequest is snapshot_versions.build_request. The Reconciler sends
// the Dockerfile on every tick until the build ends, so it must be durable.
type storedBuildRequest struct {
	Dockerfile string `json:"dockerfile"`
}

// buildContext is one named build context resolved from a ResourceVersion.
type buildContext struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	version uuid.UUID
}

// buildProvenance is snapshot_versions.provenance for a finished build:
// BuildKit's resolved FROM digests plus the version address of every context.
type buildProvenance struct {
	FromDigests []string       `json:"from_digests"`
	Contexts    []buildContext `json:"contexts,omitempty"`
}

var (
	buildNamePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	dockerfileFromFlag = regexp.MustCompile(`--from=([^,\s]+)`)
	// RUN --mount=type=bind,from=<ref> can pull an external image too.
	dockerfileMountFrom = regexp.MustCompile(`(?:^|[,\s])from=([^,\s]+)`)
)

// validBuildName is the Dockerfile name grammar shared by context names and
// Secret ids. `base` is reserved for the injected Snapshot context.
func validBuildName(name string) bool {
	return name != "base" && buildNamePattern.MatchString(name)
}

// parseContextRef splits `resource[@version]` at a trailing UUID. A Resource
// name that itself contains '@' survives as long as the suffix is not a UUID.
func parseContextRef(ref string) (string, *string) {
	if i := strings.LastIndexByte(ref, '@'); i > 0 {
		suffix := ref[i+1:]
		if _, err := uuid.Parse(suffix); err == nil {
			return ref[:i], &suffix
		}
	}
	return ref, nil
}

// validateBuildDockerfile rejects every external image reference that is not
// digest-pinned, so a build can only pull through the platform registry or a
// named context (ADR-0021). Stage aliases and numeric stage references are
// resolved as BuildKit would.
func validateBuildDockerfile(text string, contexts map[string]bool, hasBase bool) error {
	stages := map[string]bool{}
	for _, line := range dockerfile.LogicalLines(text) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if strings.EqualFold(fields[0], "FROM") {
			image, alias, err := dockerfileFrom(fields[1:])
			if err != nil {
				return err
			}
			if err := validBuildBase(image, contexts, stages, hasBase); err != nil {
				return err
			}
			if alias != "" {
				stages[strings.ToLower(alias)] = true
			}
			continue
		}
		for _, ref := range dockerfileFromFlag.FindAllStringSubmatch(line, -1) {
			if err := validBuildBase(ref[1], contexts, stages, hasBase); err != nil {
				return err
			}
		}
		for _, ref := range dockerfileMountFrom.FindAllStringSubmatch(line, -1) {
			if err := validBuildBase(ref[1], contexts, stages, hasBase); err != nil {
				return err
			}
		}
	}
	return nil
}

func dockerfileFrom(tokens []string) (image, alias string, err error) {
	i := 0
	for i < len(tokens) && strings.HasPrefix(tokens[i], "--") {
		i++
	}
	if i >= len(tokens) {
		return "", "", errors.New("FROM needs an image or a named context")
	}
	image = tokens[i]
	if i+2 < len(tokens) && strings.EqualFold(tokens[i+1], "as") {
		alias = tokens[i+2]
	}
	return image, alias, nil
}

func validBuildBase(image string, contexts, stages map[string]bool, hasBase bool) error {
	if image == "" {
		return errors.New("FROM needs an image or a named context")
	}
	lower := strings.ToLower(image)
	if contexts[lower] || stages[lower] || isStageIndex(lower) {
		return nil
	}
	switch lower {
	case "base":
		if hasBase {
			return nil
		}
		return errors.New("FROM base requires the base field")
	case "scratch":
		return nil
	}
	if strings.Contains(image, "@sha256:") {
		if _, err := registry.ParseSourceRef(image); err != nil {
			return fmt.Errorf("FROM %q is not a valid digest reference: %w", image, err)
		}
		return nil
	}
	return fmt.Errorf("FROM %q must be a named context, an earlier stage, or a digest-pinned image (ref@sha256:…)", image)
}

func isStageIndex(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// createBuild validates a POST /snapshots/{id}/versions build request and
// creates the building version, the kind=build Sandbox row, its Exec and the
// admission request in one transaction.
func (s *Server) createBuild(w http.ResponseWriter, r *http.Request, p *principal, snap store.Snapshot, req snapshotBuildRequest, key *string) {
	ctx := r.Context()
	if strings.TrimSpace(req.Dockerfile) == "" {
		writeProblem(w, "invalid_request", "dockerfile is required")
		return
	}
	contextNames := make(map[string]bool, len(req.Contexts))
	for name := range req.Contexts {
		if !validBuildName(name) {
			writeProblem(w, "invalid_request", fmt.Sprintf("context names must match %s and may not be base", buildNamePattern))
			return
		}
		contextNames[strings.ToLower(name)] = true
	}
	for id := range req.Secrets {
		if !validBuildName(id) {
			writeProblem(w, "invalid_request", fmt.Sprintf("secret ids must match %s and may not be base", buildNamePattern))
			return
		}
	}
	hasBase := req.Base != nil && *req.Base != ""
	if err := validateBuildDockerfile(req.Dockerfile, contextNames, hasBase); err != nil {
		writeProblem(w, "invalid_request", err.Error())
		return
	}
	if s.cfg.RegistryAddr == "" {
		writeProblem(w, "unavailable", "the platform registry is not configured; set STARS_REGISTRY_ADDR")
		return
	}
	if len(req.Secrets) > 0 && s.cfg.EncryptionKeys == nil {
		writeProblem(w, "unavailable", "Secret storage is not configured: set STARS_ENCRYPTION_KEYS_FILE")
		return
	}
	if s.cfg.BuildCPU <= 0 || s.cfg.BuildMemory <= 0 || s.cfg.BuildTimeout <= 0 {
		writeProblem(w, "internal", "the build limits are not configured")
		return
	}

	versionID, err := uuid.NewV7()
	if err != nil {
		s.internal(w, "generate SnapshotVersion id", err)
		return
	}
	sandboxID, err := uuid.NewV7()
	if err != nil {
		s.internal(w, "generate build Sandbox id", err)
		return
	}
	execID, err := uuid.NewV7()
	if err != nil {
		s.internal(w, "generate build Exec id", err)
		return
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.internal(w, "begin build transaction", err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)

	var baseDigest string
	if hasBase {
		var code, detail string
		baseDigest, code, detail = resolveSnapshotPin(ctx, q, s.cfg, p.user.ID, req.Base)
		if code != "" {
			writeProblem(w, code, detail)
			return
		}
	}
	// Contexts resolve to concrete versions now; the rows pin them for the
	// build's life and are read back by the Reconciler (schema.md).
	names := make([]string, 0, len(req.Contexts))
	for name := range req.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)
	contexts := make([]buildContext, 0, len(names))
	for _, name := range names {
		resourceRef, versionRef := parseContextRef(req.Contexts[name])
		resource, ok := s.resolveSandboxResource(w, ctx, q, p.user.ID, resourceRef)
		if !ok {
			return
		}
		version, ok := s.resolveSandboxResourceVersion(w, ctx, q, resource, versionRef)
		if !ok {
			return
		}
		if version.Address == nil {
			writeProblem(w, "conflict", "the ResourceVersion has no manifest")
			return
		}
		contexts = append(contexts, buildContext{Name: name, Address: *version.Address, version: version.ID})
	}
	secretEnvCt, secretEnv, ok := s.writeSecretSnapshot(w, ctx, q, p.user.ID, secretEnvRefs(req.Secrets), "invalid_request")
	if !ok {
		return
	}
	if _, err := q.CreateBuildSandbox(ctx, store.CreateBuildSandboxParams{
		ID: sandboxID, OwnerID: p.user.ID, Name: "build-" + versionID.String(),
		SnapshotDigest: baseDigest, Cpu: s.cfg.BuildCPU, Memory: s.cfg.BuildMemory,
		SecretEnvCt: secretEnvCt,
	}); err != nil {
		s.internal(w, "create build Sandbox", err)
		return
	}
	argv, _ := json.Marshal([]string{"build"})
	timeout := int32(s.cfg.BuildTimeout / time.Second)
	if timeout < 1 {
		timeout = 1
	}
	if _, err := q.CreateExec(ctx, store.CreateExecParams{
		ID: execID, SandboxID: sandboxID, Argv: argv, Env: []byte("{}"),
		Cwd: "", TimeoutSeconds: &timeout, Generation: 1,
	}); err != nil {
		s.internal(w, "create build Exec", err)
		return
	}
	for _, buildCtx := range contexts {
		if err := q.CreateSandboxResource(ctx, store.CreateSandboxResourceParams{
			SandboxID: sandboxID, ResourceVersionID: buildCtx.version, Mount: buildCtx.Name,
		}); err != nil {
			s.internal(w, "pin build context", err)
			return
		}
	}
	data, err := json.Marshal(storedBuildRequest{Dockerfile: req.Dockerfile})
	if err != nil {
		s.internal(w, "encode build request", err)
		return
	}
	version, err := q.CreateSnapshotVersion(ctx, store.CreateSnapshotVersionParams{
		ID: versionID, SnapshotID: snap.ID, State: string(SnapshotVersionBuilding),
		Source: "build", BuildSandboxID: &sandboxID, BuildExecID: &execID,
		IdempotencyKey: key, BuildRequest: data,
	})
	if err != nil {
		lookup := func() (store.SnapshotVersion, error) {
			return s.q.GetSnapshotVersionByIdempotencyKey(ctx, store.GetSnapshotVersionByIdempotencyKeyParams{
				SnapshotID: snap.ID, IdempotencyKey: key,
			})
		}
		if existing, replayed := replayConflict(key, err, "snapshot_versions_idempotency", lookup); replayed {
			writeJSON(w, http.StatusOK, snapshotVersionToDTO(existing))
			return
		}
		s.internal(w, "create build SnapshotVersion", err)
		return
	}
	deadline := time.Now().Add(s.cfg.BuildTimeout)
	admissionID, err := uuid.NewV7()
	if err != nil {
		s.internal(w, "generate admission id", err)
		return
	}
	// Builds are system-priority work and hold an Allocation like any other
	// admitted Sandbox (ADR-0021).
	if _, err := q.CreateAdmissionRequest(ctx, store.CreateAdmissionRequestParams{
		ID: admissionID, SandboxID: sandboxID, Priority: "system", DeadlineAt: timestamptzPtr(&deadline),
	}); err != nil {
		s.internal(w, "enqueue build admission", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.internal(w, "commit build", err)
		return
	}
	detail := map[string]any{
		"version": version.ID.String(), "source": "build",
		"cpu": s.cfg.BuildCPU, "memory": s.cfg.BuildMemory,
	}
	if len(contexts) > 0 {
		names := make([]string, 0, len(contexts))
		for _, ctx := range contexts {
			names = append(names, ctx.Name)
		}
		detail["contexts"] = names
	}
	if baseDigest != "" {
		detail["base"] = baseDigest
	}
	if names := referencedSecretNames(secretEnv); len(names) > 0 {
		detail["secrets"] = names
	}
	s.audit(ctx, p, "snapshot.version.create", "snapshot", snap.ID.String(), detail)
	s.changed(ctx)
	writeJSON(w, http.StatusCreated, snapshotVersionToDTO(version))
}

// --- Reconciler ---------------------------------------------------------

// builds derives BuildSnapshot and build cleanup from snapshot_versions. It is
// the build counterpart of the execs sweep: the desired-state tables are the
// queue (ADR-0014).
func (r *reconciler) builds(ctx context.Context) {
	rows, err := r.s.q.ListBuildRuns(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.s.log.Warn("reconcile: list builds", "error", err)
		}
		return
	}
	now := time.Now()
	for _, row := range rows {
		r.build(ctx, row, now)
	}
}

func (r *reconciler) build(ctx context.Context, row store.ListBuildRunsRow, now time.Time) {
	version, sb := row.SnapshotVersion, row.Sandbox
	if version.State != string(SnapshotVersionBuilding) {
		r.cleanupBuild(ctx, row, execCauseForVersion(version))
		return
	}
	if sb.State == string(SandboxFailed) || sb.State == string(SandboxStopped) || sb.State == string(SandboxDeleted) {
		r.failBuild(ctx, row, versionCauseForSandbox(sb))
		return
	}
	if sb.NodeID == nil {
		return // waiting for admission
	}
	node, err := r.s.q.GetNode(ctx, *sb.NodeID)
	if err == nil && node.LostAt.Valid {
		r.failBuild(ctx, row, "node_lost")
		return
	}
	if row.ExecCancelRequestedAt.Valid {
		if _, err := r.s.q.CancelSnapshotVersion(ctx, version.ID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			r.s.log.Warn("reconcile: cancel build version", "version_id", version.ID, "error", err)
		}
		r.cleanupBuild(ctx, row, "canceled")
		return
	}
	if buildExpired(row, now) {
		r.failBuild(ctx, row, "timed_out")
		return
	}
	cmd, err := r.buildCommand(ctx, row)
	if err != nil {
		r.s.log.Warn("reconcile: derive build command", "version_id", version.ID, "error", err)
		r.failBuild(ctx, row, "build_failed")
		return
	}
	if !row.ExecStartedAt.Valid {
		// The build Exec never runs through the Supervisor; the sweep stamps
		// its start so the API and the timeout have a clock.
		if err := r.s.q.SetExecStarted(ctx, store.SetExecStartedParams{
			ID: row.ExecID, StartedAt: timestamptzPtr(&now),
		}); err != nil {
			r.s.log.Warn("reconcile: start build Exec", "exec_id", row.ExecID, "error", err)
			return
		}
	}
	// The build row's generation is the stable token: SnapshotVersions have no
	// generation of their own, and this row never changes it.
	id := commandID("build-snapshot", version.ID, row.ExecGeneration)
	r.send(ctx, sb.NodeID, id, &nodev1.ControlMessage{
		Message: &nodev1.ControlMessage_BuildSnapshot{BuildSnapshot: cmd},
	})
}

// failBuild moves a still-building version to failed with cause and releases
// the build. MarkSnapshotVersionFailed is conditional on building, so a cancel
// or observation that won the race is left alone.
func (r *reconciler) failBuild(ctx context.Context, row store.ListBuildRunsRow, cause string) {
	version := row.SnapshotVersion
	if _, err := r.s.q.MarkSnapshotVersionFailed(ctx, store.MarkSnapshotVersionFailedParams{
		ID: version.ID, Cause: &cause,
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		r.s.log.Warn("reconcile: fail build version", "version_id", version.ID, "error", err)
	}
	r.cleanupBuild(ctx, row, execCauseForVersionCause(cause))
}

// buildCommand assembles the wire command from the rows. Every field is
// re-derived on every tick, so a reconnect redelivers the same work.
func (r *reconciler) buildCommand(ctx context.Context, row store.ListBuildRunsRow) (*nodev1.BuildSnapshot, error) {
	version, sb := row.SnapshotVersion, row.Sandbox
	var request storedBuildRequest
	if err := json.Unmarshal(version.BuildRequest, &request); err != nil || request.Dockerfile == "" {
		return nil, fmt.Errorf("build request is missing or malformed: %w", err)
	}
	mounts, err := r.s.q.ListSandboxResourceMounts(ctx, sb.ID)
	if err != nil {
		return nil, err
	}
	contexts := make([]*nodev1.BuildContext, 0, len(mounts))
	for _, mount := range mounts {
		if mount.Address == nil {
			return nil, fmt.Errorf("context %q has no manifest address", mount.Mount)
		}
		contexts = append(contexts, &nodev1.BuildContext{Name: mount.Mount, Address: *mount.Address})
	}
	// Secrets are decrypted only when the command is sent, the same rule as
	// StartExec.secret_env (schema.md, node-protocol.md).
	secretEnv, err := decryptSecretEnv(r.s.cfg.EncryptionKeys, sb.OwnerID, sb.SecretEnvCt)
	if err != nil {
		return nil, fmt.Errorf("decrypt build Secrets: %w", err)
	}
	ids := make([]string, 0, len(secretEnv))
	for id := range secretEnv {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	secrets := make([]*nodev1.BuildSecret, 0, len(ids))
	for _, id := range ids {
		secrets = append(secrets, &nodev1.BuildSecret{Id: id, Value: secretEnv[id]})
	}
	snapshot, err := r.s.q.GetSnapshot(ctx, version.SnapshotID)
	if err != nil {
		return nil, err
	}
	if r.s.cfg.RegistryAddr == "" {
		return nil, errors.New("the platform registry is not configured")
	}
	timeout := int64(r.s.cfg.BuildTimeout / time.Second)
	if row.ExecTimeoutSeconds != nil && *row.ExecTimeoutSeconds > 0 {
		timeout = int64(*row.ExecTimeoutSeconds)
	}
	pushRepository := strings.TrimSuffix(r.s.cfg.RegistryAddr, "/") + "/" +
		registry.Repo(snapshot.ID, platformName(snapshot))
	return &nodev1.BuildSnapshot{
		CommandId:      commandID("build-snapshot", version.ID, row.ExecGeneration),
		VersionId:      version.ID.String(),
		SandboxId:      sb.ID.String(),
		ExecId:         row.ExecID.String(),
		Dockerfile:     request.Dockerfile,
		BaseDigest:     sb.SnapshotDigest,
		Contexts:       contexts,
		Secrets:        secrets,
		Limits:         &nodev1.Limits{Cpu: sb.Cpu, Memory: sb.Memory},
		TimeoutSeconds: timeout,
		PushRepository: pushRepository,
	}, nil
}

// cleanupBuild stops the build on the Node (best effort) and releases the
// build row once its version is terminal. Every step is idempotent, so the
// sweep retries until it succeeds.
func (r *reconciler) cleanupBuild(ctx context.Context, row store.ListBuildRunsRow, cause string) {
	if !row.ExecFinishedAt.Valid {
		id := commandID("cancel-exec", row.ExecID, row.ExecGeneration)
		r.send(ctx, row.Sandbox.NodeID, id, &nodev1.ControlMessage{
			Message: &nodev1.ControlMessage_CancelExec{CancelExec: &nodev1.CancelExec{
				CommandId: id, ExecId: row.ExecID.String(),
			}},
		})
	}
	tx, err := r.s.pool.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := r.s.q.WithTx(tx)
	if !row.ExecFinishedAt.Valid {
		if _, err := q.FinishExec(ctx, store.FinishExecParams{
			ID: row.ExecID, Cause: &cause,
		}); err != nil {
			r.s.log.Warn("reconcile: finish build Exec", "exec_id", row.ExecID, "error", err)
			return
		}
	}
	if err := releaseCompute(ctx, q, row.Sandbox.ID); err != nil {
		r.s.log.Warn("reconcile: release build compute", "sandbox_id", row.Sandbox.ID, "error", err)
		return
	}
	if _, err := q.MarkBuildSandboxDone(ctx, row.Sandbox.ID); err != nil {
		r.s.log.Warn("reconcile: finish build Sandbox", "sandbox_id", row.Sandbox.ID, "error", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		r.s.log.Warn("reconcile: commit build cleanup", "version_id", row.SnapshotVersion.ID, "error", err)
	}
}

// buildExpired applies the deployment timeout while the Exec is still running
// (after SetExecStarted) or while it waits to start.
func buildExpired(row store.ListBuildRunsRow, now time.Time) bool {
	if row.ExecDeadlineAt.Valid {
		return !now.Before(row.ExecDeadlineAt.Time)
	}
	if row.ExecTimeoutSeconds != nil && *row.ExecTimeoutSeconds > 0 {
		return !now.Before(row.ExecAcceptedAt.Time.Add(time.Duration(*row.ExecTimeoutSeconds) * time.Second))
	}
	return false
}

// versionCauseForSandbox maps a failed build Sandbox to its SnapshotVersion
// cause (api.md §1's closed set).
func versionCauseForSandbox(sb store.Sandbox) string {
	if sb.Cause != nil {
		switch *sb.Cause {
		case "admission_timeout":
			return "timed_out"
		case "node_lost":
			return "node_lost"
		}
	}
	return "build_failed"
}

func execCauseForVersion(version store.SnapshotVersion) string {
	if version.State == string(SnapshotVersionCancelled) {
		return "canceled"
	}
	if version.State == string(SnapshotVersionFailed) && version.Cause != nil {
		return execCauseForVersionCause(*version.Cause)
	}
	return "exited"
}

func execCauseForVersionCause(cause string) string {
	switch cause {
	case "timed_out":
		return "timed_out"
	case "build_interrupted":
		return "runtime_lost"
	case "node_lost":
		return "node_lost"
	default:
		return "exited"
	}
}

// --- BuildObserved ------------------------------------------------------

// recordBuildObserved applies the Agent's terminal build result. A stale
// observation (the version already failed, timed out or was cancelled) is
// dropped: only a building version can move.
func (n *nodeService) recordBuildObserved(ctx context.Context, nodeID uuid.UUID, obs *nodev1.BuildObserved) {
	versionID, err := uuid.Parse(obs.VersionId)
	if err != nil {
		return
	}
	version, err := n.s.q.GetSnapshotVersionByID(ctx, versionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	if err != nil {
		n.s.log.Warn("record BuildObserved", "version_id", versionID, "error", err)
		return
	}
	if version.State != string(SnapshotVersionBuilding) || version.BuildSandboxID == nil || version.BuildExecID == nil {
		return
	}
	sb, err := n.s.q.GetSandboxIncludingDeleted(ctx, *version.BuildSandboxID)
	if err != nil || sb.Kind != string(SandboxKindBuild) || sb.NodeID == nil || *sb.NodeID != nodeID {
		return
	}
	if obs.Cause != "" {
		cause := buildFailureCause(obs.Cause)
		n.finishBuildObserved(ctx, version, sb, nil, cause)
		return
	}
	if obs.Digest == "" {
		n.s.log.Warn("BuildObserved carries neither a digest nor a cause", "version_id", versionID)
		return
	}
	if !digestPattern.MatchString(obs.Digest) {
		n.s.log.Warn("BuildObserved digest is malformed", "version_id", versionID, "digest", obs.Digest)
		n.finishBuildObserved(ctx, version, sb, nil, "build_failed")
		return
	}
	snapshot, err := n.s.q.GetSnapshot(ctx, version.SnapshotID)
	if err != nil {
		n.s.log.Warn("record BuildObserved Snapshot", "version_id", versionID, "error", err)
		return
	}
	// The Agent pushed to the registry the Node reaches; verify the manifest
	// through the Control Plane's push address before moving the pointer.
	ref := registry.Ref(n.s.cfg.RegistryPushAddr,
		registry.Repo(snapshot.ID, platformName(snapshot)), obs.Digest)
	verifyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := registry.VerifyDigest(verifyCtx, ref, n.s.cfg.RegistryCredentialDoc); err != nil {
		n.s.log.Warn("build digest is not in the platform registry", "version_id", versionID, "error", err)
		n.finishBuildObserved(ctx, version, sb, nil, "build_failed")
		return
	}
	mounts, err := n.s.q.ListSandboxResourceMounts(ctx, sb.ID)
	if err != nil {
		n.s.log.Warn("record BuildObserved contexts", "version_id", versionID, "error", err)
		return
	}
	contexts := make([]buildContext, 0, len(mounts))
	for _, mount := range mounts {
		address := ""
		if mount.Address != nil {
			address = *mount.Address
		}
		contexts = append(contexts, buildContext{Name: mount.Mount, Address: address})
	}
	provenance, err := json.Marshal(buildProvenance{
		FromDigests: append([]string{}, obs.FromDigests...), Contexts: contexts,
	})
	if err != nil {
		n.s.log.Warn("encode build provenance", "version_id", versionID, "error", err)
		return
	}
	n.finishBuildObserved(ctx, version, sb, &store.MarkSnapshotVersionReadyParams{
		ID: version.ID, Digest: &obs.Digest, Provenance: provenance,
	}, "")
}

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func buildFailureCause(cause string) string {
	switch cause {
	case "timed_out", "build_interrupted", "node_lost", "build_failed":
		return cause
	default:
		return "build_failed"
	}
}

// finishBuildObserved commits the version transition, the current pointer,
// the Exec result and the Allocation release together. A conditional update
// losing the race to a cancel or timeout writes nothing and is dropped.
func (n *nodeService) finishBuildObserved(ctx context.Context, version store.SnapshotVersion,
	sb store.Sandbox, ready *store.MarkSnapshotVersionReadyParams, cause string) {
	tx, err := n.s.pool.Begin(ctx)
	if err != nil {
		n.s.log.Warn("begin build observation", "version_id", version.ID, "error", err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := n.s.q.WithTx(tx)
	if ready != nil {
		if _, err := q.MarkSnapshotVersionReady(ctx, *ready); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				n.s.log.Warn("mark build ready", "version_id", version.ID, "error", err)
			}
			return
		}
		if _, err := q.SetSnapshotCurrentVersion(ctx, store.SetSnapshotCurrentVersionParams{
			ID: version.SnapshotID, VersionID: &version.ID,
		}); err != nil {
			n.s.log.Warn("move Snapshot current pointer", "version_id", version.ID, "error", err)
			return
		}
	} else {
		if _, err := q.MarkSnapshotVersionFailed(ctx, store.MarkSnapshotVersionFailedParams{
			ID: version.ID, Cause: &cause,
		}); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				n.s.log.Warn("mark build failed", "version_id", version.ID, "error", err)
			}
			return
		}
	}
	execCause := execCauseForVersionCause(cause)
	if ready != nil {
		execCause = "exited"
	}
	if _, err := q.FinishExec(ctx, store.FinishExecParams{ID: *version.BuildExecID, Cause: &execCause}); err != nil {
		n.s.log.Warn("finish build Exec", "exec_id", *version.BuildExecID, "error", err)
		return
	}
	if err := releaseCompute(ctx, q, sb.ID); err != nil {
		n.s.log.Warn("release build compute", "sandbox_id", sb.ID, "error", err)
		return
	}
	if _, err := q.MarkBuildSandboxDone(ctx, sb.ID); err != nil {
		n.s.log.Warn("finish build Sandbox", "sandbox_id", sb.ID, "error", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		n.s.log.Warn("commit build observation", "version_id", version.ID, "error", err)
		return
	}
	action := "snapshot.version.build.failed"
	detail := map[string]any{"version": version.ID.String(), "cause": cause}
	if ready != nil {
		action = "snapshot.version.build"
		detail["digest"] = *ready.Digest
	}
	owner := snapshotOwner(ctx, n.s.q, version.SnapshotID)
	n.s.auditAs(ctx, owner, nil, action, "snapshot", version.SnapshotID.String(), detail)
	n.s.reconcile.notify()
}

// snapshotOwner resolves the User a Snapshot belongs to, for audit records
// written from an Agent observation (which carries no principal).
func snapshotOwner(ctx context.Context, q *store.Queries, snapshotID uuid.UUID) *uuid.UUID {
	snap, err := q.GetSnapshot(ctx, snapshotID)
	if err != nil || snap.OwnerID == nil {
		return nil
	}
	return snap.OwnerID
}
