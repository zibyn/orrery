package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

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
	lookup := func() (store.SnapshotVersion, error) {
		return s.q.GetSnapshotVersionByIdempotencyKey(ctx, store.GetSnapshotVersionByIdempotencyKeyParams{
			SnapshotID: snap.ID, IdempotencyKey: key,
		})
	}
	version, replayed, err := createIdempotent(key, lookup,
		func() (store.SnapshotVersion, error) {
			return q.CreateSnapshotVersion(ctx, store.CreateSnapshotVersionParams{
				ID: versionID, SnapshotID: snap.ID, State: string(SnapshotVersionBuilding),
				Source: "build", BuildSandboxID: &sandboxID, BuildExecID: &execID,
				IdempotencyKey: key, BuildRequest: data,
			})
		})
	if err != nil {
		s.internal(w, "create build SnapshotVersion", err)
		return
	}
	if replayed {
		writeJSON(w, http.StatusOK, snapshotVersionToDTO(version))
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
