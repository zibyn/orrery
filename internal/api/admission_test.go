package api

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/zibyn/stars-sandbox/internal/store"
)

// The numbers are the defaults from docs/spec/defaults.md: 1 CPU + 2 GiB of
// host reservation and a 10% disk watermark.
func TestNodeFitAdmits(t *testing.T) {
	const gib = int64(1) << 30
	node := store.Node{
		ReservedCpu:    1,
		ReservedMemory: 2 * gib,
		Hardware: mustHardware(t, nodeHardware{
			CPUCores: 8, MemoryBytes: 32 * gib,
			DiskFree: 50, DiskTotal: 100,
		}),
	}
	used := store.NodePlacementRow{UsedCpu: 6, UsedMemory: 20 * gib}
	fit, ok := fitFor(node, used)
	if !ok {
		t.Fatal("reported hardware must produce a fit")
	}

	tests := []struct {
		name   string
		cpu    float64
		memory int64
		want   bool
	}{
		{"exactly the free CPU and memory", 1, 10 * gib, true},
		{"too much CPU", 1.5, 1 * gib, false},
		{"too much memory", 0.5, 10*gib + 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fit.admits(tt.cpu, tt.memory); got != tt.want {
				t.Fatalf("admits(%v, %d) = %v, want %v", tt.cpu, tt.memory, got, tt.want)
			}
		})
	}

}
func TestNodeFitDiskWatermark(t *testing.T) {
	for _, tt := range []struct {
		name      string
		free      uint64
		total     uint64
		watermark float64
		want      bool
	}{
		{"below the default watermark", 9, 100, 0, false},
		{"at the default watermark", 10, 100, 0, true},
		{"unknown total is not gated", 0, 0, 0, true},
		{"the Node's own watermark applies", 24, 100, 0.25, false},
		{"at the Node's watermark", 25, 100, 0.25, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fit := nodeFit{
				cpu: 1, memory: 1, diskFree: tt.free, diskTotal: tt.total,
				diskWatermark: tt.watermark,
			}
			if got := fit.admits(1, 1); got != tt.want {
				t.Fatalf("admits with %d/%d free (watermark %v) = %v, want %v",
					tt.free, tt.total, tt.watermark, got, tt.want)
			}
		})
	}
}

func TestAdmissionExhausted(t *testing.T) {
	small := store.Node{
		ID:             uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		ReservedCpu:    1,
		ReservedMemory: 2 << 30,
		Hardware: mustHardware(t, nodeHardware{
			CPUCores: 2, MemoryBytes: 4 << 30, DiskFree: 50, DiskTotal: 100,
		}),
	}
	big := store.Node{
		ID:             uuid.MustParse("00000000-0000-0000-0000-000000000002"),
		ReservedCpu:    1,
		ReservedMemory: 2 << 30,
		Hardware: mustHardware(t, nodeHardware{
			CPUCores: 16, MemoryBytes: 64 << 30, DiskFree: 50, DiskTotal: 100,
		}),
	}
	noHardware := store.Node{}
	request := store.Sandbox{Cpu: 4, Memory: 8 << 30}

	for _, tt := range []struct {
		name          string
		nodes         []store.Node
		sb            store.Sandbox
		workspaceNode *uuid.UUID
		want          bool
	}{
		{"no Node has enrolled yet", nil, request, nil, false},
		{"unknown hardware is not proof", []store.Node{noHardware}, request, nil, false},
		{"unknown hardware beside a small Node is not proof", []store.Node{small, noHardware}, request, nil, false},
		{"one Node is large enough", []store.Node{small, big}, request, nil, false},
		{"every Node is too small", []store.Node{small}, request, nil, true},
		{
			"a bound Sandbox only considers its Node",
			[]store.Node{small, big},
			store.Sandbox{Cpu: 4, Memory: 8 << 30, NodeID: &small.ID},
			nil,
			true,
		},
		{
			"a bound Workspace only considers its Node",
			[]store.Node{small, big},
			store.Sandbox{Cpu: 4, Memory: 8 << 30},
			&small.ID,
			true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := admissionExhausted(tt.nodes, nil, tt.sb, tt.workspaceNode); got != tt.want {
				t.Fatalf("admissionExhausted = %v, want %v", got, tt.want)
			}
		})
	}
}

// The per-GPU share is three quarters of the allocatable amount split by card
// count; GPU-less Sandboxes get only the remaining quarter (defaults.md).
func TestGPUPlacement(t *testing.T) {
	const gib = int64(1) << 30
	node := store.Node{
		CpuPerGpu:    1.5,
		MemoryPerGpu: 6 * gib,
		Hardware: mustHardware(t, nodeHardware{
			CPUCores: 8, MemoryBytes: 32 << 30,
		}),
	}
	used := store.NodePlacementRow{GpuTotal: 4}
	fit, ok := fitFor(node, used)
	if !ok {
		t.Fatal("reported hardware must produce a fit")
	}
	if shareCPU, shareMem, ok := fit.gpuShare(1); !ok || shareCPU != 1.5 || shareMem != float64(6*gib) {
		t.Fatalf("gpuShare(1) = %v, %v, %v", shareCPU, shareMem, ok)
	}

	for _, tt := range []struct {
		name   string
		cpu    float64
		memory int64
		count  int32
		want   bool
	}{
		{"exactly the share of one card", 1.5, 6 << 30, 1, true},
		{"two cards carry two shares", 3, 12 << 30, 2, true},
		{"more CPU than the cards reserve", 1.6, 6 << 30, 1, false},
		{"more memory than the cards reserve", 1.5, 6<<30 + 1, 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := fit.admitsGPU(tt.cpu, tt.memory, tt.count); got != tt.want {
				t.Fatalf("admitsGPU(%v, %d, %d) = %v, want %v", tt.cpu, tt.memory, tt.count, got, tt.want)
			}
		})
	}

	// A Node whose heartbeat has not sized the share yet cannot place a GPU
	// Sandbox.
	unsized := node
	unsized.CpuPerGpu, unsized.MemoryPerGpu = 0, 0
	unsizedFit, _ := fitFor(unsized, used)
	if unsizedFit.admitsGPU(0, 0, 1) {
		t.Fatal("a Node with no published share admitted a GPU Sandbox")
	}

	// The four shares hold 6 of the 8 allocatable cores; GPU-less Sandboxes
	// may use the remaining 2.
	if !fit.admitsNonGPU(2, 8<<30) {
		t.Fatal("a GPU-less Sandbox must fit the quarter the cards leave")
	}
	if fit.admitsNonGPU(2.1, 1<<30) {
		t.Fatal("a GPU-less Sandbox must not take the cards' share")
	}
	busy, _ := fitFor(node, store.NodePlacementRow{
		GpuTotal: 4, UsedCpu: 1.5, UsedMemory: 6 << 30, UsedCpuNoGpu: 1.5, UsedMemoryNoGpu: 6 << 30,
	})
	if !busy.admitsNonGPU(0.5, 2<<30) {
		t.Fatal("the rest of the GPU-less pool must remain available")
	}
	if busy.admitsNonGPU(0.6, 1<<30) {
		t.Fatal("the GPU-less pool must not grow past the remainder")
	}
}

func TestTakeGPUs(t *testing.T) {
	free := []store.Gpu{
		{Uuid: "GPU-a", Model: "A100"},
		{Uuid: "GPU-b", Model: "H100"},
		{Uuid: "GPU-c", Model: "H100"},
	}
	if ids, ok := takeGPUs(free, 2, nil); !ok || len(ids) != 2 || ids[0] != "GPU-a" {
		t.Fatalf("unfiltered takeGPUs = %v, %v", ids, ok)
	}
	if ids, ok := takeGPUs(free, 2, strPtr("H100")); !ok || ids[0] != "GPU-b" || ids[1] != "GPU-c" {
		t.Fatalf("model-filtered takeGPUs = %v, %v", ids, ok)
	}
	if _, ok := takeGPUs(free, 2, strPtr("A100")); ok {
		t.Fatal("two A100s were taken from a Node that has one")
	}
}

func TestAdmissionExhaustedGPU(t *testing.T) {
	const gib = int64(1) << 30
	gpuNode := store.Node{
		ID:           uuid.MustParse("00000000-0000-0000-0000-000000000003"),
		CpuPerGpu:    1.5,
		MemoryPerGpu: 6 * gib,
		Hardware: mustHardware(t, nodeHardware{
			CPUCores: 8, MemoryBytes: 32 * gib,
		}),
		Capabilities: json.RawMessage(`{"runtime":"docker","entries":[{"name":"gpu_device_visibility","value":"enforced"}]}`),
	}
	gpus := map[uuid.UUID][]store.Gpu{gpuNode.ID: {
		{Uuid: "GPU-a", NodeID: gpuNode.ID, Model: "H100"},
		{Uuid: "GPU-b", NodeID: gpuNode.ID, Model: "H100"},
	}}
	request := store.Sandbox{Cpu: 1.5, Memory: 6 * gib, GpuCount: 1}

	if admissionExhausted([]store.Node{gpuNode}, gpus, request, nil) {
		t.Fatal("a Node with a free card of the model must not be exhausted")
	}
	if !admissionExhausted([]store.Node{gpuNode}, gpus, store.Sandbox{
		Cpu: 1.5, Memory: 6 * gib, GpuCount: 3,
	}, nil) {
		t.Fatal("three cards on a two-card Node must be exhausted")
	}
	if !admissionExhausted([]store.Node{gpuNode}, gpus, store.Sandbox{
		Cpu: 1.5, Memory: 6 * gib, GpuCount: 1, GpuModel: strPtr("A100"),
	}, nil) {
		t.Fatal("a model no Node reports must be exhausted")
	}

	// Without the CDI capability the cards exist but cannot be attached.
	noCDI := gpuNode
	noCDI.Capabilities = json.RawMessage(`{"runtime":"docker","entries":[{"name":"gpu_device_visibility","value":"unsupported"}]}`)
	if !admissionExhausted([]store.Node{noCDI}, gpus, request, nil) {
		t.Fatal("a Node without CDI must be exhausted for GPU requests")
	}
	noReport := gpuNode
	noReport.Capabilities = nil
	if admissionExhausted([]store.Node{noReport}, gpus, request, nil) {
		t.Fatal("a Node that has not reported capabilities is not proof either way")
	}
}

func mustHardware(t *testing.T, hw nodeHardware) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(hw)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
