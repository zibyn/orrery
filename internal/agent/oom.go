package agent

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/zibyn/orrery/internal/supervisor"
)

// oomBaselineFile is the host oom_kill count read before the spec was sent
// (runtime.md §3, §9).
const oomBaselineFile = "oom_kill"

// hostOOMCause is runtime.md §9's runsc rule: with the host counter risen
// since the Exec's baseline, a plain SIGKILL (the stub was killed) or an
// Agent-written runtime_lost (the Sentry was killed) becomes oom_killed.
func hostOOMCause(exit supervisor.Exit, rose bool) string {
	sigkill := exit.Signal != nil && *exit.Signal == int32(syscall.SIGKILL)
	if rose && (exit.Cause == "runtime_lost" || exit.Cause == "exited" && sigkill) {
		return "oom_killed"
	}
	return exit.Cause
}

func (r *Runtime) hostOOMKills(sandboxID uuid.UUID) int64 {
	return supervisor.OOMKills(filepath.Join(r.hostCgroup, containerdNamespace, containerName(sandboxID), "memory.events"))
}

// writeOOMBaseline records the host counter in the Exec's record, where it
// survives an Agent restart. A no-op off runsc.
func (r *Runtime) writeOOMBaseline(sandboxID uuid.UUID, dir string) {
	if r.hostCgroup == "" {
		return
	}
	tmp := filepath.Join(dir, oomBaselineFile+".tmp")
	err := os.WriteFile(tmp, []byte(strconv.FormatInt(r.hostOOMKills(sandboxID), 10)), 0o644)
	if err == nil {
		err = os.Rename(tmp, filepath.Join(dir, oomBaselineFile))
	}
	if err != nil {
		slog.Warn("write oom_kill baseline", "dir", dir, "error", err)
	}
}

// hostOOMRose reports whether the host counter is above the Exec's baseline.
// No baseline (or no runsc) means nothing to attribute.
func (r *Runtime) hostOOMRose(sandboxID uuid.UUID, dir string) bool {
	if r.hostCgroup == "" {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(dir, oomBaselineFile))
	if err != nil {
		return false
	}
	base, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	return err == nil && r.hostOOMKills(sandboxID) > base
}

// lostDeliveryWait bounds how long a lost Sandbox's report waits for its
// exit deliveries.
const lostDeliveryWait = 5 * time.Second

// awaitDeliveries lets the exit frames already in flight for a lost Sandbox
// land before it is reported gone: the Control Plane ends any Exec still
// active with runtime_lost, which would hide an oom_killed (runtime.md §9).
func (r *Runtime) awaitDeliveries(sandboxID string) {
	dirs, _ := filepath.Glob(filepath.Join(r.dataRoot, "sandboxes", sandboxID, "run", "*"))
	for deadline := time.Now().Add(lostDeliveryWait); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		r.execMu.Lock()
		busy := false
		for _, dir := range dirs {
			_, inFlight := r.execs[filepath.Base(dir)]
			busy = busy || inFlight
		}
		r.execMu.Unlock()
		if !busy {
			return
		}
	}
}

// attributeHostOOM rewrites a Supervisor-written SIGKILL exit as oom_killed
// when the host counter rose, so every redelivery reports the same cause.
func (r *Runtime) attributeHostOOM(sandboxID uuid.UUID, dir string, exit supervisor.Exit) supervisor.Exit {
	if exit.Cause != "exited" {
		return exit
	}
	if cause := hostOOMCause(exit, r.hostOOMRose(sandboxID, dir)); cause != exit.Cause {
		exit.Cause = cause
		if err := supervisor.WriteExit(dir, exit); err != nil {
			slog.Warn("rewrite exit as oom_killed", "dir", dir, "error", err)
		}
	}
	return exit
}
