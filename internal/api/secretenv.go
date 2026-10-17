package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/zibyn/stars-sandbox/internal/keyring"
	"github.com/zibyn/stars-sandbox/internal/store"
)

// A Sandbox's `secrets` mapping is snapshotted into `sandboxes.secret_env_ct`
// at every start; a StartExec decrypts it into `secret_env` (schema.md).

// secretEnvEntry is one variable of a Sandbox's Secret snapshot. The value
// stays the owner-bound ciphertext from the secrets table; only the Control
// Plane decrypts it, per StartExec.
type secretEnvEntry struct {
	Name       string `json:"name"`
	KeyID      string `json:"key_id"`
	Ciphertext []byte `json:"ciphertext"`
}

// secretEnvSnapshot is `sandboxes.secret_env_ct`: ENV_NAME → Secret. It keeps
// the Secret names so the next start can re-resolve them.
type secretEnvSnapshot map[string]secretEnvEntry

func encodeSecretEnv(snapshot secretEnvSnapshot) ([]byte, error) {
	if len(snapshot) == 0 {
		return nil, nil
	}
	return json.Marshal(snapshot)
}

func decodeSecretEnv(raw []byte) (secretEnvSnapshot, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var snapshot secretEnvSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil, fmt.Errorf("decode Secret snapshot: %w", err)
	}
	return snapshot, nil
}

// secretEnvRefs turns the create request's {ENV_NAME: secret_name} into
// name references; resolveSecretEnv fills in the ciphertexts.
func secretEnvRefs(mapping map[string]string) secretEnvSnapshot {
	if len(mapping) == 0 {
		return nil
	}
	refs := make(secretEnvSnapshot, len(mapping))
	for env, name := range mapping {
		refs[env] = secretEnvEntry{Name: name}
	}
	return refs
}

// resolveSecretEnv copies each named Secret's current ciphertext into a new
// snapshot (schema.md). The first name that no longer exists is reported and
// stops the walk; lookups are cached per name so a mapping that repeats one
// Secret costs one query, and a 1 MiB body cannot turn into a query storm.
func resolveSecretEnv(ctx context.Context, q *store.Queries, ownerID uuid.UUID, refs secretEnvSnapshot) (secretEnvSnapshot, []string, error) {
	envNames := make([]string, 0, len(refs))
	for env := range refs {
		envNames = append(envNames, env)
	}
	slices.Sort(envNames)
	resolved := make(secretEnvSnapshot, len(refs))
	cache := make(map[string]secretEnvEntry, len(refs))
	for _, env := range envNames {
		entry := refs[env]
		if cached, ok := cache[entry.Name]; ok {
			resolved[env] = cached
			continue
		}
		secret, err := q.GetSecretByName(ctx, store.GetSecretByNameParams{
			OwnerID: ownerID, Name: entry.Name,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, []string{entry.Name}, nil
		}
		if err != nil {
			return nil, nil, err
		}
		entry = secretEnvEntry{
			Name: secret.Name, KeyID: secret.KeyID, Ciphertext: secret.Ciphertext,
		}
		cache[entry.Name] = entry
		resolved[env] = entry
	}
	return resolved, nil, nil
}

// writeSecretSnapshot resolves refs and returns the snapshot bytes plus the
// resolved entries for the Audit Record. A missing Secret writes a problem
// with missingCode.
func (s *Server) writeSecretSnapshot(w http.ResponseWriter, ctx context.Context, q *store.Queries, ownerID uuid.UUID, refs secretEnvSnapshot, missingCode string) ([]byte, secretEnvSnapshot, bool) {
	resolved, missing, err := resolveSecretEnv(ctx, q, ownerID, refs)
	if err != nil {
		s.internal(w, "resolve Secrets", err)
		return nil, nil, false
	}
	if len(missing) > 0 {
		writeProblem(w, missingCode, "no such Secret: "+strings.Join(missing, ", "))
		return nil, nil, false
	}
	raw, err := encodeSecretEnv(resolved)
	if err != nil {
		s.internal(w, "encode Secret snapshot", err)
		return nil, nil, false
	}
	return raw, resolved, true
}

// referencedSecretNames is the distinct Secret names a Sandbox was created
// with, for the Audit Record. Never a value (ADR-0013).
func referencedSecretNames(snapshot secretEnvSnapshot) []string {
	seen := make(map[string]struct{}, len(snapshot))
	names := make([]string, 0, len(snapshot))
	for _, entry := range snapshot {
		if _, ok := seen[entry.Name]; ok {
			continue
		}
		seen[entry.Name] = struct{}{}
		names = append(names, entry.Name)
	}
	slices.Sort(names)
	return names
}

// decryptSecretEnv opens the start-time snapshot for one StartExec. The key
// stays in the Control Plane; the Agent only relays the values
// (node-protocol.md).
func decryptSecretEnv(keys *keyring.Keyring, ownerID uuid.UUID, raw []byte) (map[string]string, error) {
	snapshot, err := decodeSecretEnv(raw)
	if err != nil || len(snapshot) == 0 {
		return nil, err
	}
	if keys == nil {
		return nil, errors.New("secret storage is not configured")
	}
	env := make(map[string]string, len(snapshot))
	for name, entry := range snapshot {
		value, err := keys.Decrypt(entry.Ciphertext, entry.KeyID, []byte(ownerID.String()))
		if err != nil {
			return nil, fmt.Errorf("decrypt Secret %s: %w", entry.Name, err)
		}
		env[name] = string(value)
	}
	return env, nil
}

// secretEnvNameProblem applies the Secret name rules to an environment
// variable name in a Sandbox's secrets mapping. The platform injects these
// variables, so the same reserved names are off limits (ticket #75,
// ADR-0017).
func secretEnvNameProblem(name string) (code, detail string) {
	if !validDisplayName(name) || strings.ContainsAny(name, "=\x00") {
		return "invalid_request", "secrets keys must be non-empty, trimmed, and contain no '=' or NUL"
	}
	if reservedSecretName(name) {
		return "invalid_request", "secrets keys may not use a reserved name: GIT_*, LD_*, STARS_*, PATH and HOME are not allowed"
	}
	return "", ""
}
