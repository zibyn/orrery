package api

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// legacyDirsVersion is the migration that turns the Node-local directories
// Sandboxes mounted before ADR-0031 into Sandbox filesystems and Volumes.
const legacyDirsVersion = 35

// ADR-0031 "Migration": an ephemeral directory becomes its Sandbox's
// /workspace (uploaded by its Node), a persistent one a Volume with its ID
// mounted at /workspace by its read-write user, and a read-only mount is
// dropped.
func TestMigrateDirectoriesToSandboxesAndVolumes(t *testing.T) {
	dsn := os.Getenv("ORRERY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORRERY_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, dbDSN := scratchDatabaseAt(t, ctx, dsn, legacyDirsVersion-1)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	user, node, lostNode := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, name) VALUES ($1, 'alice')`, user)
	exec(`INSERT INTO nodes (id, name, token_hash) VALUES ($1, 'n1', 'x')`, node)
	exec(`INSERT INTO nodes (id, name, token_hash, lost_at) VALUES ($1, 'n2', 'y', now())`, lostNode)

	sandbox := func(name, state string, nodeID uuid.UUID) uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO sandboxes (id, owner_id, name, snapshot_digest, cpu, memory, state, node_id)
			VALUES ($1, $2, $3, 'sha256:x', 1, 1024, $4, $5)`, id, user, name, state, nodeID)
		return id
	}
	dir := func(kind string, sandboxID *uuid.UUID, nodeID uuid.UUID) uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO workspaces (id, owner_id, kind, sandbox_id, node_id, state)
			VALUES ($1, $2, $3, $4, $5, 'ready')`, id, user, kind, sandboxID, nodeID)
		return id
	}
	mount := func(sandboxID, dirID uuid.UUID, mode string) {
		exec(`UPDATE sandboxes SET workspace_id = $2, workspace_mode = $3 WHERE id = $1`, sandboxID, dirID, mode)
	}

	ephemeral := sandbox("ephemeral", "stopped", node)
	ephemeralDir := dir("ephemeral", &ephemeral, node)
	mount(ephemeral, ephemeralDir, "rw")

	persistent := dir("persistent", nil, node)
	writer := sandbox("writer", "ready", node)
	mount(writer, persistent, "rw")
	reader := sandbox("reader", "stopped", node)
	mount(reader, persistent, "ro")

	lostSandbox := sandbox("lost", "stopped", lostNode)
	lostDir := dir("ephemeral", &lostSandbox, lostNode)
	mount(lostSandbox, lostDir, "rw")

	resource := uuid.New()
	exec(`INSERT INTO resources (id, owner_id, name) VALUES ($1, $2, 'r')`, resource, user)
	version := func(state string, dirID uuid.UUID) uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO resource_versions (id, resource_id, provenance, state, publish_workspace_id)
			VALUES ($1, $2, jsonb_build_object('source', 'workspace', 'workspace', jsonb_build_object('id', $3::uuid::text, 'path', 'out')), $4, $3::uuid)`,
			id, resource, dirID, state)
		return id
	}
	fromVolume := version("ready", persistent)
	fromSandbox := version("ready", ephemeralDir)
	pending := version("pending", persistent)

	if err := Migrate(ctx, pool, dbDSN, ""); err != nil {
		t.Fatal(err)
	}

	type legacy struct {
		Node    uuid.UUID
		Sandbox *uuid.UUID
		Volume  *uuid.UUID
	}
	dirs := map[uuid.UUID]legacy{}
	rows, err := pool.Query(ctx, `SELECT id, node_id, sandbox_id, volume_id FROM legacy_dirs`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id uuid.UUID
		var d legacy
		if err := rows.Scan(&id, &d.Node, &d.Sandbox, &d.Volume); err != nil {
			t.Fatal(err)
		}
		dirs[id] = d
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 2 {
		t.Fatalf("legacy directories = %+v, want the ephemeral and the persistent one on the live Node", dirs)
	}
	if d := dirs[ephemeralDir]; d.Node != node || d.Sandbox == nil || *d.Sandbox != ephemeral || d.Volume != nil {
		t.Fatalf("ephemeral directory = %+v, want uploaded by %s as Sandbox %s", d, node, ephemeral)
	}
	if d := dirs[persistent]; d.Node != node || d.Volume == nil || *d.Volume != persistent || d.Sandbox != nil {
		t.Fatalf("persistent directory = %+v, want uploaded by %s as Volume %s", d, node, persistent)
	}

	var name, state string
	if err := pool.QueryRow(ctx, `SELECT name, state FROM volumes WHERE id = $1`, persistent).Scan(&name, &state); err != nil {
		t.Fatal(err)
	}
	if want := "workspace-" + strings.ReplaceAll(persistent.String(), "-", "")[:8]; name != want || state != "ready" {
		t.Fatalf("Volume = %s %s, want %s ready", name, state, want)
	}
	mounts := func(sandboxID uuid.UUID) []string {
		rows, err := pool.Query(ctx, `SELECT volume_id::text || ' ' || path || ' ' || held::text FROM sandbox_volumes WHERE sandbox_id = $1`, sandboxID)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}
	if got := mounts(writer); len(got) != 1 || got[0] != persistent.String()+" /workspace true" {
		t.Fatalf("writer mounts = %v, want the Volume held at /workspace", got)
	}
	if got := mounts(reader); len(got) != 0 {
		t.Fatalf("read-only mounts = %v, want dropped", got)
	}
	if got := mounts(ephemeral); len(got) != 0 {
		t.Fatalf("ephemeral Sandbox mounts = %v, want none", got)
	}

	provenance := func(id uuid.UUID) (map[string]any, *uuid.UUID, *uuid.UUID, string) {
		var raw json.RawMessage
		var sb, vol *uuid.UUID
		var state string
		if err := pool.QueryRow(ctx, `SELECT provenance, publish_sandbox_id, publish_volume_id, state FROM resource_versions WHERE id = $1`, id).
			Scan(&raw, &sb, &vol, &state); err != nil {
			t.Fatal(err)
		}
		var p map[string]any
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		return p, sb, vol, state
	}
	if p, _, vol, _ := provenance(fromVolume); p["source"] != "volume" || vol == nil || *vol != persistent ||
		p["volume"].(map[string]any)["path"] != "out" {
		t.Fatalf("version published from the persistent directory = %v (volume %v), want volume provenance", p, vol)
	}
	if p, sb, _, _ := provenance(fromSandbox); p["source"] != "sandbox" || sb == nil || *sb != ephemeral ||
		p["sandbox"].(map[string]any)["id"] != ephemeral.String() {
		t.Fatalf("version published from the ephemeral directory = %v (sandbox %v), want sandbox provenance", p, sb)
	}
	if _, _, _, state := provenance(pending); state != "failed" {
		t.Fatalf("pending publish state = %s, want failed", state)
	}

	var tables int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name = 'workspaces'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	var columns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE column_name IN ('workspace_id', 'workspace_mode', 'publish_workspace_id')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if tables != 0 || columns != 0 {
		t.Fatalf("%d workspaces tables and %d Workspace columns remain", tables, columns)
	}
}
