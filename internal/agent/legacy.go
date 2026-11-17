package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// Before ADR-0031 every Sandbox mounted a Node-local directory at /workspace,
// kept at <data-root>/workspaces/<id>, and the Agent recorded which one in
// the Sandbox directory. Migration 000037 turned them into what replaces
// them: an ephemeral one is its Sandbox's /workspace, a persistent one the
// Volume with its ID. The Control Plane keeps such a Sandbox on this Node
// until it has been created here once and committed; that Create merges the
// directory (runtime.md §16). A running Sandbox keeps its old mount until it
// stops.
//
// shortcut: a directory whose Sandbox or Volume is deleted before that
// Create stays on disk; remove <data-root>/workspaces by hand once the
// Control Plane's legacy_dirs is empty, or sweep it if that ever matters.

// legacyDirPath is a directory from before ADR-0031.
func legacyDirPath(dataRoot, id string) string {
	return filepath.Join(dataRoot, "workspaces", id)
}

// legacyRecord is what the Agent wrote at <sandbox>/workspace.
type legacyRecord struct {
	ID   string `json:"id"`
	Mode string `json:"mode"`
}

// legacyRecordPath shares its name with a Local Sandbox's working directory,
// so only a regular file there is a record.
func legacyRecordPath(dataRoot string, id uuid.UUID) string {
	return filepath.Join(sandboxPath(dataRoot, id), "workspace")
}

func readLegacyRecord(dataRoot string, id uuid.UUID) (legacyRecord, bool) {
	path := legacyRecordPath(dataRoot, id)
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return legacyRecord{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return legacyRecord{}, false
	}
	var rec legacyRecord
	if json.Unmarshal(raw, &rec) != nil {
		return legacyRecord{}, false
	}
	if _, err := uuid.Parse(rec.ID); err != nil {
		return legacyRecord{}, false
	}
	return rec, true
}

// legacyRunningDir is the directory a Sandbox started before the upgrade
// still runs in, or "".
func legacyRunningDir(dataRoot string, id uuid.UUID) string {
	rec, ok := readLegacyRecord(dataRoot, id)
	if !ok {
		return ""
	}
	dir := legacyDirPath(dataRoot, rec.ID)
	if _, err := os.Stat(dir); err != nil {
		return ""
	}
	return dir
}

// takeLegacySandboxDir removes the Sandbox's record and returns its own
// directory, if any: one it mounted read-write that is not among its
// Volumes. A read-only mount is dropped, and a persistent directory is
// merged into its Volume by seedLegacyVolumes instead.
func takeLegacySandboxDir(dataRoot string, id uuid.UUID, volumes []volumeMount) (string, error) {
	rec, ok := readLegacyRecord(dataRoot, id)
	if !ok {
		return "", nil
	}
	if err := os.Remove(legacyRecordPath(dataRoot, id)); err != nil {
		return "", err
	}
	if rec.Mode == "ro" {
		return "", nil
	}
	for _, v := range volumes {
		if v.ID == rec.ID {
			return "", nil
		}
	}
	dir := legacyDirPath(dataRoot, rec.ID)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	return dir, nil
}

// mergeLegacyDir moves src's contents into dst, keeping modes, owners,
// times, xattrs and hard links, then removes src. dst itself keeps its own
// attributes. cp -a is the whole-tree copy that keeps all of that; this runs
// once per directory, at the first Create after the upgrade.
func mergeLegacyDir(ctx context.Context, src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		args := []string{"-a", "--"}
		for _, e := range entries {
			args = append(args, filepath.Join(src, e.Name()))
		}
		out, err := exec.CommandContext(ctx, "cp", append(args, dst+"/")...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("copy %s into %s: %w: %s", src, dst, err, strings.TrimSpace(string(out)))
		}
	}
	return os.RemoveAll(src)
}

// seedLegacyVolumes fills each never-committed Volume whose directory is
// still here from that directory, through its writable mount; the Volume's
// next commit uploads it.
func (r *Runtime) seedLegacyVolumes(ctx context.Context, volumes []volumeMount) error {
	for _, v := range volumes {
		dir := legacyDirPath(r.dataRoot, v.ID)
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if v.head != "" {
			slog.Warn("a Volume already committed elsewhere keeps its head; its old directory stays", "volume_id", v.ID, "dir", dir)
			continue
		}
		if err := mergeLegacyDir(ctx, dir, volumeDaemonPath(r.dataRoot, v.ID)); err != nil {
			return fmt.Errorf("seed Volume %s: %w", v.ID, err)
		}
	}
	return nil
}
