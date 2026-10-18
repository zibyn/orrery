// Package supervisor is the in-container half of the Runtime contract: PID 1
// reaping (runtime.md §1) and the capability probe (runtime.md §9).
package supervisor

import (
	"encoding/json"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Result is what `supervisor probe` prints and what the Agent compares against
// the limits it requested.
type Result struct {
	CPUMax            string   `json:"cpu_max"`
	MemoryMax         string   `json:"memory_max"`
	PidsMax           string   `json:"pids_max"`
	NvidiaDevices     []string `json:"nvidia_devices"`
	PIDNamespace      string   `json:"pid_namespace"`
	MountNamespace    string   `json:"mount_namespace"`
	WorkspaceWritable bool     `json:"workspace_writable"`
}

// Init reaps orphans forever. As PID 1 it also has to catch the termination
// signals the kernel would otherwise ignore for init.
func Init() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-signals
		os.Exit(0)
	}()
	for {
		_, err := syscall.Wait4(-1, nil, 0, nil)
		switch {
		case errors.Is(err, syscall.EINTR):
		case errors.Is(err, syscall.ECHILD):
			time.Sleep(time.Second)
		}
	}
}

// Probe reports the container's own view of its limits. It never fails the
// process: a missing file is reported as an empty value so the Agent can mark
// that one capability unsupported.
func Probe(workspace string) (Result, error) {
	result := Result{
		CPUMax:            cpuMax(),
		MemoryMax:         readTrimmed(memoryMaxPath()),
		PidsMax:           readTrimmed(cgroupFile("pids.max")),
		PIDNamespace:      NamespaceID("/proc/self/ns/pid"),
		MountNamespace:    NamespaceID("/proc/self/ns/mnt"),
		WorkspaceWritable: workspaceWritable(workspace),
	}
	result.NvidiaDevices, _ = filepath.Glob("/dev/nvidia*")
	sort.Strings(result.NvidiaDevices)
	return result, nil
}

// WriteProbeJSON is the `supervisor probe` command body.
func WriteProbeJSON(workspace string) error {
	result, err := Probe(workspace)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	return enc.Encode(result)
}

func cpuMax() string {
	if quota := readTrimmed(cgroupFile("cpu.max")); quota != "" {
		return quota
	}
	quota := readTrimmed(cgroupFile("cpu/cpu.cfs_quota_us"))
	period := readTrimmed(cgroupFile("cpu/cpu.cfs_period_us"))
	if quota == "" {
		return ""
	}
	return quota + " " + period
}

func memoryMaxPath() string {
	if _, err := os.Stat(cgroupFile("cgroup.controllers")); err == nil {
		return cgroupFile("memory.max")
	}
	return cgroupFile("memory/memory.limit_in_bytes")
}

// cgroupV2Dir is this process's own cgroup under the cgroup v2 mount. Inside a
// container with a private cgroup namespace /proc/self/cgroup reports "/", so
// the result is the fixed /sys/fs/cgroup; a host service reports its slice
// path, which is where Local Runtime limits live (runtime.md §10).
func cgroupV2Dir() string {
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "/sys/fs/cgroup"
	}
	return cgroupPathFrom(string(raw))
}

func cgroupPathFrom(data string) string {
	for _, line := range strings.Split(data, "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			return filepath.Join("/sys/fs/cgroup", strings.TrimSpace(path))
		}
	}
	return "/sys/fs/cgroup"
}

func cgroupFile(name string) string {
	return filepath.Join(cgroupV2Dir(), name)
}

func readTrimmed(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// NamespaceID uses Readlink: reading the magic link itself is blocked by some
// sandbox seccomp profiles, while its target carries the inode. Agents compare
// this value against the probe container's.
func NamespaceID(path string) string {
	target, err := os.Readlink(path)
	if err != nil {
		return ""
	}
	return target
}

func workspaceWritable(dir string) bool {
	probe := filepath.Join(dir, ".stars-probe-"+strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		return false
	}
	_ = os.Remove(probe)
	return true
}
