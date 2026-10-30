package agent

import (
	"context"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
)

// gpuInfo is one card as nvidia-smi names it: the platform UUID and the model.
type gpuInfo struct {
	UUID  string
	Model string
}

// enumerateGPUs lists the host's NVIDIA cards. A missing nvidia-smi is an
// empty inventory, not an error: a CPU-only Node is a normal Node.
func enumerateGPUs(ctx context.Context) []gpuInfo {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// --format=csv emits "GPU-<uuid>, <model>" per card. The inventory is
	// static for the life of the connection; a hot-added card waits for the
	// next one.
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=uuid,name", "--format=csv,noheader,nounits").Output()
	if err != nil {
		slog.Debug("nvidia-smi inventory failed; reporting no GPUs", "error", err)
		return nil
	}
	return parseGPUList(string(out))
}

// parseGPUList reads nvidia-smi's "uuid, name" lines. MIG instances and any
// other non-whole-card entry are not allocatable, so they are dropped.
func parseGPUList(out string) []gpuInfo {
	var gpus []gpuInfo
	for _, line := range strings.Split(out, "\n") {
		uuid, model, found := strings.Cut(line, ",")
		uuid = strings.TrimSpace(uuid)
		if !found || !strings.HasPrefix(uuid, "GPU-") {
			continue
		}
		gpus = append(gpus, gpuInfo{UUID: uuid, Model: strings.TrimSpace(model)})
	}
	return gpus
}

// gpuHealth reports each known card as healthy while nvidia-smi still lists it
// and unhealthy when it has disappeared, which is what a Node's Heartbeat
// carries (node-protocol.md).
func gpuHealth(ctx context.Context, known []gpuInfo) []*nodev1.GPUHealth {
	current := make(map[string]bool)
	for _, gpu := range enumerateGPUs(ctx) {
		current[gpu.UUID] = true
	}
	out := make([]*nodev1.GPUHealth, 0, len(known))
	for _, gpu := range known {
		state := "unhealthy"
		if current[gpu.UUID] {
			state = "healthy"
		}
		out = append(out, &nodev1.GPUHealth{Uuid: gpu.UUID, State: state})
	}
	return out
}

// gpuUUIDs is the inventory's platform UUIDs.
func gpuUUIDs(gpus []gpuInfo) []string {
	ids := make([]string, 0, len(gpus))
	for _, gpu := range gpus {
		ids = append(ids, gpu.UUID)
	}
	return ids
}

// cdiDeviceRequest is the HostConfig entry that attaches whole cards
// (runtime.md §9). The `nvidia` driver is not portable; CDI is the one
// expression Docker and Podman share. It is nil when no cards are requested.
func cdiDeviceRequest(uuids []string) []map[string]any {
	if len(uuids) == 0 {
		return nil
	}
	return []map[string]any{{"Driver": "cdi", "DeviceIDs": cdiDeviceNames(uuids)}}
}

// cdiDeviceNames is the fully qualified CDI name of each whole card.
func cdiDeviceNames(uuids []string) []string {
	ids := make([]string, len(uuids))
	for i, uuid := range uuids {
		ids[i] = "nvidia.com/gpu=" + uuid
	}
	return ids
}

// numberedDeviceCount counts /dev/nvidiaN entries: the per-card character
// devices CDI injects. Control nodes (/dev/nvidiactl, /dev/nvidia-uvm) do not
// count.
func numberedDeviceCount(devices []string) int {
	count := 0
	for _, device := range devices {
		base := filepath.Base(device)
		index := strings.TrimPrefix(base, "nvidia")
		if index == base {
			continue
		}
		if _, err := strconv.Atoi(index); err == nil {
			count++
		}
	}
	return count
}
