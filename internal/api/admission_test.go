package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/zibyn/orrery/internal/nodeproto"
	"github.com/zibyn/orrery/internal/store"
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
		Capabilities: json.RawMessage(`{"runtime_provider":"containerd","entries":[{"name":"gpu_device_visibility","value":"enforced"}]}`),
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
	noCDI.Capabilities = json.RawMessage(`{"runtime_provider":"containerd","entries":[{"name":"gpu_device_visibility","value":"unsupported"}]}`)
	if !admissionExhausted([]store.Node{noCDI}, gpus, request, nil) {
		t.Fatal("a Node without CDI must be exhausted for GPU requests")
	}
	noReport := gpuNode
	noReport.Capabilities = nil
	if admissionExhausted([]store.Node{noReport}, gpus, request, nil) {
		t.Fatal("a Node that has not reported capabilities is not proof either way")
	}
}

// orderPlacementNodes implements ADR-0015's placement policy among feasible
// Nodes: GPU requests pack onto the Node with the fewest free cards of the
// requested model, GPU-less requests prefer Nodes without cards, and ties
// break by created_at and then id. The fixtures are store.Node values; no
// database is involved.
func TestOrderPlacementNodes(t *testing.T) {
	at := func(unix int64) pgtype.Timestamptz {
		return pgtype.Timestamptz{Time: time.Unix(unix, 0).UTC(), Valid: true}
	}
	mk := func(id byte, createdAt int64, cards int, free []store.Gpu) placementNode {
		return placementNode{
			node: store.Node{
				ID:        uuid.UUID{id},
				CreatedAt: at(createdAt),
			},
			gpus: cards,
			free: free,
		}
	}
	h100 := func(n int) []store.Gpu {
		free := make([]store.Gpu, n)
		for i := range free {
			free[i] = store.Gpu{Model: "H100"}
		}
		return free
	}
	names := func(nodes []placementNode) []byte {
		ids := make([]byte, len(nodes))
		for i, node := range nodes {
			ids[i] = node.node.ID[0]
		}
		return ids
	}
	order := func(nodes []placementNode, sb store.Sandbox) []byte {
		orderPlacementNodes(nodes, sb)
		return names(nodes)
	}

	model := "H100"
	for name, tt := range map[string]struct {
		sb    store.Sandbox
		nodes []placementNode
		want  []byte
	}{
		"GPU request packs onto the fewest free cards of the model": {
			sb: store.Sandbox{GpuCount: 1, GpuModel: &model},
			nodes: []placementNode{
				mk(1, 10, 3, h100(3)),
				mk(2, 20, 1, h100(1)),
				mk(3, 30, 2, h100(2)),
			},
			want: []byte{2, 3, 1},
		},
		"an empty model counts free cards of any model": {
			sb: store.Sandbox{GpuCount: 1},
			nodes: []placementNode{
				mk(1, 10, 3, []store.Gpu{{Model: "A100"}, {Model: "H100"}, {Model: "H100"}}),
				mk(2, 20, 3, []store.Gpu{{Model: "A100"}}),
				mk(3, 30, 3, []store.Gpu{{Model: "A100"}, {Model: "A100"}}),
			},
			want: []byte{2, 3, 1},
		},
		"GPU packing ties break by created_at": {
			sb: store.Sandbox{GpuCount: 1, GpuModel: &model},
			nodes: []placementNode{
				mk(1, 20, 2, h100(2)),
				mk(2, 10, 2, h100(2)),
			},
			want: []byte{2, 1},
		},
		"GPU-less request prefers a Node without cards": {
			sb: store.Sandbox{},
			nodes: []placementNode{
				mk(1, 10, 4, nil),
				mk(2, 20, 0, nil),
			},
			want: []byte{2, 1},
		},
		"GPU-less requests tie break by created_at": {
			sb: store.Sandbox{},
			nodes: []placementNode{
				mk(1, 20, 4, nil),
				mk(2, 30, 8, nil),
				mk(3, 10, 4, nil),
			},
			want: []byte{3, 1, 2},
		},
		"fully allocated cards still count as a GPU Node": {
			sb: store.Sandbox{},
			nodes: []placementNode{
				mk(1, 10, 4, nil),
				mk(2, 20, 0, nil),
			},
			want: []byte{2, 1},
		},
		"a complete created_at tie breaks by id": {
			sb: store.Sandbox{},
			nodes: []placementNode{
				mk(3, 10, 0, nil),
				mk(1, 10, 0, nil),
				mk(2, 10, 0, nil),
			},
			want: []byte{1, 2, 3},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := order(tt.nodes, tt.sb); string(got) != string(tt.want) {
				t.Fatalf("order = %v, want %v", got, tt.want)
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

// capabilitySnapshot builds a stored Capabilities report for a Provider.
func capabilitySnapshot(t *testing.T, provider string, values map[string]string) json.RawMessage {
	t.Helper()
	entries := make([]nodeproto.CapabilityEntry, 0, len(values))
	for name, value := range values {
		entries = append(entries, nodeproto.CapabilityEntry{Name: name, Value: value})
	}
	raw, err := json.Marshal(nodeproto.CapabilityReport{RuntimeProvider: provider, Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// api.md §3 / ADR-0009: a container request only goes to a container Node, a
// local request only to a Local Node. Build rows are filtered elsewhere.
func TestRuntimeMatching(t *testing.T) {
	docker := capabilitySnapshot(t, "docker", nil)
	podman := capabilitySnapshot(t, "podman", nil)
	containerd := capabilitySnapshot(t, "containerd", nil)
	local := capabilitySnapshot(t, "local", nil)
	for _, tt := range []struct {
		caps    json.RawMessage
		runtime string
		want    bool
	}{
		// The removed Providers (ADR-0026) take nothing.
		{docker, "", false},
		{docker, "container", false},
		{docker, "local", false},
		{podman, "container", false},
		{containerd, "container", true},
		{containerd, "", true},
		{containerd, "local", false},
		{local, "container", false},
		{local, "local", true},
		{podman, "local", false},
		{nil, "container", false},
		{nil, "local", false},
	} {
		if got := runtimeMatches(tt.caps, tt.runtime); got != tt.want {
			t.Errorf("runtimeMatches(provider %q, %q) = %v, want %v",
				nodeproto.RuntimeProvider(tt.caps), tt.runtime, got, tt.want)
		}
	}
}

// ADR-0009's single exception: Local GPU visibility may be best_effort.
func TestGPUVisibilityAccepted(t *testing.T) {
	for _, tt := range []struct {
		runtime string
		value   string
		want    bool
	}{
		{"container", nodeproto.ValueEnforced, true},
		{"container", nodeproto.ValueBestEffort, false},
		{"local", nodeproto.ValueEnforced, true},
		{"local", nodeproto.ValueBestEffort, true},
		{"local", nodeproto.ValueUnsupported, false},
		{"local", "", false},
	} {
		caps := capabilitySnapshot(t, "local", map[string]string{
			nodeproto.CapGPUDeviceVisibility: tt.value,
		})
		if got := gpuVisibilityAccepted(caps, tt.runtime); got != tt.want {
			t.Errorf("gpuVisibilityAccepted(%q, %q) = %v, want %v", tt.runtime, tt.value, got, tt.want)
		}
	}
}

// A local Sandbox with no Local Node can never be admitted; the queue must end
// it `unsatisfiable` instead of waiting for the deadline.
func TestAdmissionExhaustedRuntime(t *testing.T) {
	const gib = int64(1) << 30
	hw := mustHardware(t, nodeHardware{
		CPUCores: 8, MemoryBytes: 32 * gib, DiskFree: 50, DiskTotal: 100,
	})
	caps := func(provider string) json.RawMessage {
		values := map[string]string{}
		for _, name := range nodeproto.RequiredFor(provider) {
			values[name] = nodeproto.ValueEnforced
		}
		return capabilitySnapshot(t, provider, values)
	}
	containerNode := store.Node{
		ID: uuid.New(), Hardware: hw, Capabilities: caps("containerd"),
		ReservedCpu: 1, ReservedMemory: 2 * gib,
	}
	localNode := store.Node{
		ID: uuid.New(), Hardware: hw, Capabilities: caps("local"),
		ReservedCpu: 1, ReservedMemory: 2 * gib,
	}
	localSB := store.Sandbox{Cpu: 1, Memory: gib, Runtime: "local"}
	containerSB := store.Sandbox{Cpu: 1, Memory: gib, Runtime: "container"}

	if !admissionExhausted([]store.Node{containerNode}, nil, localSB, nil) {
		t.Fatal("a local Sandbox among containerd Nodes is not provably unsatisfiable")
	}
	if admissionExhausted([]store.Node{containerNode, localNode}, nil, localSB, nil) {
		t.Fatal("a local Sandbox with a Local Node is not exhausted")
	}
	if !admissionExhausted([]store.Node{localNode}, nil, containerSB, nil) {
		t.Fatal("a container Sandbox among Local Nodes is not provably unsatisfiable")
	}
	if admissionExhausted([]store.Node{containerNode, localNode}, nil, containerSB, nil) {
		t.Fatal("a container Sandbox with a Docker Node is not exhausted")
	}
}

// api.md §3: a Mode other than open only goes to a Node where egress_policy is
// enforced, on restart too; open is not constrained. A reported Node without
// it can never take the Sandbox.
func TestAdmissionExhaustedEgress(t *testing.T) {
	const gib = int64(1) << 30
	hw := mustHardware(t, nodeHardware{
		CPUCores: 8, MemoryBytes: 32 * gib, DiskFree: 50, DiskTotal: 100,
	})
	node := func(egress string) store.Node {
		values := map[string]string{nodeproto.CapEgressPolicy: egress}
		for _, name := range nodeproto.RequiredFor("containerd") {
			values[name] = nodeproto.ValueEnforced
		}
		return store.Node{
			ID: uuid.New(), Hardware: hw, Capabilities: capabilitySnapshot(t, "containerd", values),
			ReservedCpu: 1, ReservedMemory: 2 * gib,
		}
	}
	filtered, unfiltered := node(nodeproto.ValueEnforced), node(nodeproto.ValueUnsupported)
	sandbox := func(mode string) store.Sandbox {
		return store.Sandbox{Cpu: 1, Memory: gib, Runtime: "container", EgressMode: mode}
	}
	for _, tt := range []struct {
		mode  string
		nodes []store.Node
		want  bool
	}{
		{"", []store.Node{unfiltered}, false},
		{"open", []store.Node{unfiltered}, false},
		{"none", []store.Node{unfiltered}, true},
		{"allowlist", []store.Node{unfiltered}, true},
		{"none", []store.Node{unfiltered, filtered}, false},
		{"allowlist", []store.Node{filtered}, false},
	} {
		if got := admissionExhausted(tt.nodes, nil, sandbox(tt.mode), nil); got != tt.want {
			t.Errorf("admissionExhausted(mode %q, %d Nodes) = %v, want %v", tt.mode, len(tt.nodes), got, tt.want)
		}
	}
}

// api.md §3: a Sandbox with links only goes to a Node where sandbox_links is
// enforced, on restart too; one without links is not constrained.
func TestAdmissionExhaustedLinks(t *testing.T) {
	const gib = int64(1) << 30
	hw := mustHardware(t, nodeHardware{
		CPUCores: 8, MemoryBytes: 32 * gib, DiskFree: 50, DiskTotal: 100,
	})
	node := func(links string) store.Node {
		values := map[string]string{nodeproto.CapSandboxLinks: links}
		for _, name := range nodeproto.RequiredFor("containerd") {
			values[name] = nodeproto.ValueEnforced
		}
		return store.Node{
			ID: uuid.New(), Hardware: hw, Capabilities: capabilitySnapshot(t, "containerd", values),
			ReservedCpu: 1, ReservedMemory: 2 * gib,
		}
	}
	enforced, unsupported := node(nodeproto.ValueEnforced), node(nodeproto.ValueUnsupported)
	sandbox := func(from ...uuid.UUID) store.Sandbox {
		return store.Sandbox{Cpu: 1, Memory: gib, Runtime: "container", LinksFrom: from}
	}
	source := uuid.New()
	for _, tt := range []struct {
		name  string
		sb    store.Sandbox
		nodes []store.Node
		want  bool
	}{
		{"no links", sandbox(), []store.Node{unsupported}, false},
		{"links, no enforcing Node", sandbox(source), []store.Node{unsupported}, true},
		{"links, one enforcing Node", sandbox(source), []store.Node{unsupported, enforced}, false},
	} {
		if got := admissionExhausted(tt.nodes, nil, tt.sb, nil); got != tt.want {
			t.Errorf("%s: admissionExhausted = %v, want %v", tt.name, got, tt.want)
		}
	}
}
