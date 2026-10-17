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
	used := store.NodeAllocatedRow{Cpu: 6, Memory: 20 * gib}
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
		name  string
		nodes []store.Node
		sb    store.Sandbox
		want  bool
	}{
		{"no Node has enrolled yet", nil, request, false},
		{"unknown hardware is not proof", []store.Node{noHardware}, request, false},
		{"unknown hardware beside a small Node is not proof", []store.Node{small, noHardware}, request, false},
		{"one Node is large enough", []store.Node{small, big}, request, false},
		{"every Node is too small", []store.Node{small}, request, true},
		{
			"a bound Sandbox only considers its Node",
			[]store.Node{small, big},
			store.Sandbox{Cpu: 4, Memory: 8 << 30, NodeID: &small.ID},
			true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := admissionExhausted(tt.nodes, tt.sb); got != tt.want {
				t.Fatalf("admissionExhausted = %v, want %v", got, tt.want)
			}
		})
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
