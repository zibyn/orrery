package api

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/zibyn/orrery/internal/nodeproto"
	"github.com/zibyn/orrery/internal/store"
)

// defaultDiskWatermark covers an Agent that predates the reported value
// (defaults.md, Node configuration).
const defaultDiskWatermark = 0.10

// nodeFit is a Node's schedulable capacity after its host reservation, minus
// what is already allocated. The zero value is not a fit (ok=false).
type nodeFit struct {
	cpu           float64
	memory        float64
	usedCPU       float64
	usedMemory    float64
	usedCPUNoGPU  float64
	usedMemNoGPU  float64
	cpuPerGPU     float64
	memoryPerGPU  float64
	gpuTotal      int
	diskFree      uint64
	diskTotal     uint64
	diskWatermark float64
}

// admits reports whether a request of cpu cores and bytes of memory fits. The
// disk watermark gates new allocations before the resource comparison.
func (f nodeFit) admits(cpu float64, memory int64) bool {
	watermark := f.diskWatermark
	if watermark <= 0 {
		watermark = defaultDiskWatermark
	}
	if f.diskTotal > 0 && f.diskFree < uint64(float64(f.diskTotal)*watermark) {
		return false
	}
	return f.cpu-f.usedCPU >= cpu && f.memory-f.usedMemory >= float64(memory)
}

// gpuShare reports the CPU and memory that count cards reserve on this Node
// (defaults.md: three quarters of the allocatable amount, split by card
// count). ok is false when the Node publishes no share yet.
func (f nodeFit) gpuShare(count int32) (cpu, memory float64, ok bool) {
	if count <= 0 || f.cpuPerGPU <= 0 || f.memoryPerGPU <= 0 {
		return 0, 0, false
	}
	return float64(count) * f.cpuPerGPU, float64(count) * f.memoryPerGPU, true
}

// admitsGPU reports whether a GPU Sandbox fits: its CPU and memory stay inside
// the share its own cards reserve, and the Node has the total capacity.
func (f nodeFit) admitsGPU(cpu float64, memory int64, count int32) bool {
	shareCPU, shareMemory, ok := f.gpuShare(count)
	if !ok || cpu > shareCPU || float64(memory) > shareMemory {
		return false
	}
	return f.admits(cpu, memory)
}

// admitsNonGPU keeps GPU-less Sandboxes out of the shares reserved for the
// cards, so they cannot strand an idle GPU (ADR-0015). On a Node without GPUs
// the whole allocatable amount is theirs.
func (f nodeFit) admitsNonGPU(cpu float64, memory int64) bool {
	if !f.admits(cpu, memory) {
		return false
	}
	if f.gpuTotal == 0 {
		return true
	}
	freeCPU := f.cpu - float64(f.gpuTotal)*f.cpuPerGPU
	freeMemory := f.memory - float64(f.gpuTotal)*f.memoryPerGPU
	return f.usedCPUNoGPU+cpu <= freeCPU && f.usedMemNoGPU+float64(memory) <= freeMemory
}

// takeGPUs picks count free cards, matching the requested model when one is
// given. The inventory is ordered by UUID, so placements are reproducible.
func takeGPUs(free []store.Gpu, count int32, model *string) ([]string, bool) {
	if count <= 0 {
		return nil, true
	}
	ids := make([]string, 0, count)
	for _, gpu := range free {
		if model != nil && *model != "" && gpu.Model != *model {
			continue
		}
		ids = append(ids, gpu.Uuid)
		if len(ids) == int(count) {
			return ids, true
		}
	}
	return nil, false
}

// runtimeMatches reports whether a Node's Provider can run a Sandbox's
// requested Runtime (api.md §3, ADR-0009). A container request goes to a
// containerd Node; a local request only to a Local Node. An outdated Agent
// still reporting the removed docker or podman gets nothing (ADR-0026).
func runtimeMatches(capabilities json.RawMessage, runtime string) bool {
	provider := nodeproto.RuntimeProvider(capabilities)
	if runtime == "local" {
		return provider == "local"
	}
	return provider == "containerd"
}

// gpuVisibilityAccepted applies ADR-0009's one exception: a local request
// accepts best-effort GPU visibility, because the caller accepted the Local
// Runtime's low isolation. Everything else and every container request
// requires `enforced`.
func gpuVisibilityAccepted(capabilities json.RawMessage, runtime string) bool {
	value := nodeproto.CapabilityValue(capabilities, nodeproto.CapGPUDeviceVisibility)
	return value == nodeproto.ValueEnforced ||
		(runtime == "local" && value == nodeproto.ValueBestEffort)
}

// gpuFit reports whether a GPU Sandbox fits this Node and which cards it
// takes. free is the Node's whole inventory for the exhaust check and its free
// cards for placement.
func gpuFit(node store.Node, fit nodeFit, free []store.Gpu, sb store.Sandbox) ([]string, bool) {
	if !gpuVisibilityAccepted(node.Capabilities, sandboxRuntimeValue(sb.Runtime)) ||
		!fit.admitsGPU(sb.Cpu, sb.Memory, sb.GpuCount) {
		return nil, false
	}
	return takeGPUs(free, sb.GpuCount, sb.GpuModel)
}

type nodeHardware struct {
	CPUCores      float64 `json:"cpu_cores"`
	MemoryBytes   int64   `json:"memory_bytes"`
	DiskFree      uint64  `json:"disk_free"`
	DiskTotal     uint64  `json:"disk_total"`
	DiskWatermark float64 `json:"disk_watermark"`
}

func fitFor(node store.Node, used store.NodePlacementRow) (nodeFit, bool) {
	var hw nodeHardware
	if len(node.Hardware) == 0 || json.Unmarshal(node.Hardware, &hw) != nil {
		return nodeFit{}, false
	}
	return nodeFit{
		cpu:           hw.CPUCores - node.ReservedCpu,
		memory:        float64(hw.MemoryBytes - node.ReservedMemory),
		usedCPU:       used.UsedCpu,
		usedMemory:    float64(used.UsedMemory),
		usedCPUNoGPU:  used.UsedCpuNoGpu,
		usedMemNoGPU:  float64(used.UsedMemoryNoGpu),
		cpuPerGPU:     node.CpuPerGpu,
		memoryPerGPU:  float64(node.MemoryPerGpu),
		gpuTotal:      int(used.GpuTotal),
		diskFree:      hw.DiskFree,
		diskTotal:     hw.DiskTotal,
		diskWatermark: hw.DiskWatermark,
	}, true
}

// admissionExhausted reports whether every usable Node is too small for the
// request, so waiting for capacity cannot help. Unknown hardware (a Node that
// has not reported yet) is not proof either way. A Sandbox bound to a Node can
// only ever go there. gpus is each Node's reported inventory.
func admissionExhausted(nodes []store.Node, gpus map[uuid.UUID][]store.Gpu, sb store.Sandbox, workspaceNode *uuid.UUID) bool {
	if sb.Kind == string(SandboxKindBuild) && len(nodes) == 0 {
		// No Node can enforce snapshot_build (ADR-0021).
		return true
	}
	known, unknown := false, false
	for _, node := range nodes {
		if sb.NodeID != nil && node.ID != *sb.NodeID {
			continue
		}
		// A Workspace never moves: its Sandbox can only ever run there.
		if workspaceNode != nil && node.ID != *workspaceNode {
			continue
		}
		// A non-build request can only run on a Node with a matching Runtime
		// Provider (api.md §3). A Node that has not reported Capabilities is
		// treated as potentially matching, so it is not proof of exhaustion.
		if sb.Kind != string(SandboxKindBuild) && len(node.Capabilities) > 0 &&
			(!runtimeMatches(node.Capabilities, sandboxRuntimeValue(sb.Runtime)) || !egressPlaceable(sb.EgressMode)(node) ||
				!linksCapable(sb.LinksFrom)(node)) {
			known = true
			continue
		}
		if sb.Kind == string(SandboxKindBuild) {
			if len(node.Capabilities) == 0 {
				// A Node that has not reported its Capabilities may still turn
				// out to support builds.
				unknown = true
				continue
			}
			if !nodeproto.CapabilityEnforced(node.Capabilities, nodeproto.CapSnapshotBuild) {
				// Reported and incapable: this Node can never take the build.
				known = true
				continue
			}
		}
		fit, ok := fitFor(node, store.NodePlacementRow{GpuTotal: int64(len(gpus[node.ID]))})
		if !ok {
			unknown = true
			continue
		}
		known = true
		if sb.GpuCount > 0 {
			if len(node.Capabilities) == 0 {
				// A Node that has not reported its Runtime Capabilities may
				// still turn out to support CDI.
				unknown = true
				continue
			}
			if _, ok := gpuFit(node, fit, gpus[node.ID], sb); ok {
				return false
			}
			continue
		}
		if fit.admitsNonGPU(sb.Cpu, sb.Memory) {
			return false
		}
	}
	return known && !unknown
}

// admit runs the feasible-set FIFO queue inside the sweep transaction, so an
// admission is atomic with the capacity it observed: requests are considered
// in (priority, enqueued_at) order and a request that does not fit is skipped
// rather than blocking later ones (ADR-0015). An error rolls the whole sweep
// transaction back, so a half-written admission never survives.
func (r *reconciler) admit(ctx context.Context, tx pgx.Tx) error {
	q := r.s.q.WithTx(tx)
	requests, err := q.ListOpenAdmissionRequests(ctx)
	if err != nil {
		return err
	}
	if len(requests) == 0 {
		return nil
	}
	nodes, err := q.ListAllNodes(ctx)
	if err != nil {
		return err
	}
	nodes = r.hub.currentReports(nodes)
	// GPU inventory is a hardware fact, independent of what this sweep
	// allocates, so one read serves every request's exhaust check.
	inventory := make(map[uuid.UUID][]store.Gpu, len(nodes))
	for _, node := range nodes {
		gpus, err := q.ListNodeGPUs(ctx, node.ID)
		if err != nil {
			return err
		}
		inventory[node.ID] = gpus
	}
	now := time.Now()
	for _, req := range requests {
		sb := req.Sandbox
		if !req.AdmissionRequest.DeadlineAt.Valid || !req.AdmissionRequest.DeadlineAt.Time.After(now) {
			r.failAdmission(ctx, q, sb, "admission_timeout")
			continue
		}
		if SandboxState(sb.State) != SandboxPending || SandboxDesiredState(sb.DesiredState) != SandboxDesiredRunning {
			// A concurrent stop or delete won: the request is no longer
			// waiting for capacity.
			r.endAdmission(ctx, q, sb.ID, "canceled")
			continue
		}
		ws, err := q.GetSandboxWorkspace(ctx, sb.ID)
		if sb.Kind == string(SandboxKindBuild) {
			// A build row has no Workspace or Attachment (schema.md, ADR-0021).
			placed, err := r.tryAdmit(ctx, q, sb, store.Workspace{}, nodes, now)
			if err != nil {
				return err
			}
			if placed {
				r.endAdmission(ctx, q, sb.ID, "admitted")
				continue
			}
			if admissionExhausted(nodes, inventory, sb, nil) {
				r.failAdmission(ctx, q, sb, "unsatisfiable")
			}
			continue
		}
		if errors.Is(err, pgx.ErrNoRows) {
			r.failAdmission(ctx, q, sb, "runtime_create_failed")
			continue
		}
		if err != nil {
			return err
		}
		if !workspaceUsable(ws) {
			// The Workspace is failed, lost or going away; waiting cannot help.
			r.failAdmission(ctx, q, sb, "runtime_create_failed")
			continue
		}
		placed, err := r.tryAdmit(ctx, q, sb, ws, nodes, now)
		if err != nil {
			return err
		}
		if placed {
			r.endAdmission(ctx, q, sb.ID, "admitted")
			continue
		}
		if admissionExhausted(nodes, inventory, sb, ws.NodeID) {
			r.failAdmission(ctx, q, sb, "unsatisfiable")
		}
	}
	return nil
}

// nodeSchedulable reports whether a Node may take this admission. A paused
// Node takes no new Workspace, but a Workspace already bound there can still
// start Sandboxes (ADR-0017). Paused only relaxes that rule: the Node must
// still be reachable, healthy and compatible, so clear the flag and derive the
// rest of the status.
func nodeSchedulable(node store.Node, now time.Time, workspaceHere bool) bool {
	if deriveNodeStatus(node, now) == "ready" {
		return true
	}
	if !workspaceHere || !node.Paused {
		return false
	}
	node.Paused = false
	return deriveNodeStatus(node, now) == "ready"
}

// placementNode is a feasible Node and the cards it offers. gpus is the
// reported card count, used for the GPU-less preference, and free is the cards
// still available for packing.
type placementNode struct {
	node       store.Node
	gpus       int
	free       []store.Gpu
	gpuIDs     []string
	linkSource bool // a placed Link source runs here
}

// freeGPUs counts free cards of the requested model; an empty model counts
// every free card (ADR-0015).
func freeGPUs(free []store.Gpu, model *string) int {
	if model == nil || *model == "" {
		return len(free)
	}
	n := 0
	for _, gpu := range free {
		if gpu.Model == *model {
			n++
		}
	}
	return n
}

// orderPlacementNodes sorts feasible Nodes in place (ADR-0015): a Link target
// goes first to a Node of its sources (ADR-0027); a GPU request
// is packed onto the Node left with the fewest free cards of the requested
// model; a GPU-less request prefers Nodes without cards, then the first
// feasible one. Ties break by created_at and then id, so placement is
// deterministic. The policy is not configurable.
func orderPlacementNodes(nodes []placementNode, sb store.Sandbox) {
	sort.Slice(nodes, func(i, j int) bool {
		a, b := nodes[i], nodes[j]
		if a.linkSource != b.linkSource {
			return a.linkSource
		}
		if sb.GpuCount > 0 {
			if fa, fb := freeGPUs(a.free, sb.GpuModel), freeGPUs(b.free, sb.GpuModel); fa != fb {
				return fa < fb
			}
		} else if (a.gpus == 0) != (b.gpus == 0) {
			return a.gpus == 0
		}
		if !a.node.CreatedAt.Time.Equal(b.node.CreatedAt.Time) {
			return a.node.CreatedAt.Time.Before(b.node.CreatedAt.Time)
		}
		return a.node.ID.String() < b.node.ID.String()
	})
}

// tryAdmit places a Sandbox on the best feasible Node (ADR-0015). A Sandbox
// bound to a Node stays there (a Workspace never moves), and so does one whose
// Workspace is already bound (locality). A GPU Sandbox claims whole cards;
// their primary key keeps the cards exclusive.
func (r *reconciler) tryAdmit(ctx context.Context, q *store.Queries, sb store.Sandbox, ws store.Workspace, nodes []store.Node, now time.Time) (bool, error) {
	// A Sandbox with Resources can only run where the Runtime can bind-mount
	// them (runtime.md §10-11). Filtering placement rather than rejecting the
	// create keeps a mixed fleet working: api.md's Local `422 unsupported` is
	// the single-Runtime case, and here a fleet with no capable Node ends the
	// admission request `unsatisfiable` instead.
	isBuild := sb.Kind == string(SandboxKindBuild)
	var resourceMounts int64
	var linkSourceNodes []uuid.UUID
	if !isBuild {
		var err error
		resourceMounts, err = q.CountSandboxResources(ctx, sb.ID)
		if err != nil {
			return false, err
		}
		// A Link target waits for its sources' Node (api.md §3).
		if len(sb.LinksFrom) > 0 {
			if linkSourceNodes, err = q.ListLinkSourceNodes(ctx, sb.ID); err != nil {
				return false, err
			}
		}
	}
	meshLive := func(a, b uuid.UUID) bool { return r.s.mesh.live(a, b, now) }
	var candidates []placementNode
	for _, node := range nodes {
		if sb.NodeID != nil && node.ID != *sb.NodeID {
			continue
		}
		if !isBuild && ws.NodeID != nil && node.ID != *ws.NodeID {
			continue
		}
		if isBuild {
			// Builds run on the host build API only (ADR-0021), independent of
			// the Node's Runtime.
			if !nodeproto.CapabilityEnforced(node.Capabilities, nodeproto.CapSnapshotBuild) {
				continue
			}
		} else {
			// The request's Runtime is an admission constraint (api.md §3): a
			// container Sandbox only goes to containerd, a local one only
			// to a Local Node.
			if !runtimeMatches(node.Capabilities, sandboxRuntimeValue(sb.Runtime)) {
				continue
			}
			if resourceMounts > 0 && !nodeproto.CapabilityEnforced(node.Capabilities, nodeproto.CapResourceMount) {
				continue
			}
			if !egressPlaceable(sb.EgressMode)(node) {
				continue
			}
			if !linksCapable(sb.LinksFrom)(node) || !linkPlaceable(linkSourceNodes, node.ID, meshLive) {
				continue
			}
		}
		if !nodeSchedulable(node, now, !isBuild && ws.NodeID != nil) || !r.hub.connected(node.ID) {
			continue
		}
		used, err := q.NodePlacement(ctx, node.ID)
		if err != nil {
			return false, err
		}
		fit, ok := fitFor(node, used)
		if !ok {
			continue
		}
		candidate := placementNode{node: node, gpus: fit.gpuTotal, linkSource: slices.Contains(linkSourceNodes, node.ID)}
		if sb.GpuCount > 0 {
			free, err := q.ListNodeFreeGPUs(ctx, node.ID)
			if err != nil {
				return false, err
			}
			if candidate.gpuIDs, ok = gpuFit(node, fit, free, sb); !ok {
				continue
			}
			candidate.free = free
		} else if !fit.admitsNonGPU(sb.Cpu, sb.Memory) {
			continue
		}
		candidates = append(candidates, candidate)
	}
	orderPlacementNodes(candidates, sb)
	for _, candidate := range candidates {
		node := candidate.node
		// Lock and re-check the state read above: a concurrent stop or delete
		// must not be handed an Allocation.
		rows, err := q.TouchPendingSandbox(ctx, store.TouchPendingSandboxParams{
			ID: sb.ID, Generation: sb.Generation,
		})
		if err != nil {
			return false, err
		}
		if rows == 0 {
			return false, nil
		}
		if sb.NodeID == nil {
			rows, err := q.SetSandboxNode(ctx, store.SetSandboxNodeParams{ID: sb.ID, NodeID: &node.ID})
			if err != nil {
				return false, err
			}
			if rows == 0 {
				return false, nil
			}
			if !isBuild && ws.NodeID == nil {
				if _, err := q.SetWorkspaceNode(ctx, store.SetWorkspaceNodeParams{
					ID: ws.ID, NodeID: &node.ID,
				}); err != nil {
					return false, err
				}
			}
		}
		if err := q.CreateAllocation(ctx, store.CreateAllocationParams{
			SandboxID: sb.ID, NodeID: node.ID, Cpu: sb.Cpu, Memory: sb.Memory,
			Generation: sb.Generation,
		}); err != nil {
			return false, err
		}
		for _, gpuUUID := range candidate.gpuIDs {
			if err := q.CreateGPUAllocation(ctx, store.CreateGPUAllocationParams{
				GpuUuid: gpuUUID, SandboxID: sb.ID,
			}); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	return false, nil
}

// failAdmission ends a queued Sandbox and releases its hold. If a concurrent
// state change won, the request ends canceled instead.
func (r *reconciler) failAdmission(ctx context.Context, q *store.Queries, sb store.Sandbox, cause string) {
	rows, err := q.FailPendingSandbox(ctx, store.FailPendingSandboxParams{
		ID: sb.ID, Generation: sb.Generation, Cause: &cause,
	})
	if err != nil {
		r.s.log.Warn("reconcile: fail admission", "sandbox_id", sb.ID, "error", err)
		return
	}
	if rows == 0 {
		r.endAdmission(ctx, q, sb.ID, "canceled")
		return
	}
	if err := releaseRuntime(ctx, q, sb.ID); err != nil {
		r.s.log.Warn("reconcile: release runtime", "sandbox_id", sb.ID, "error", err)
	}
	r.endAdmission(ctx, q, sb.ID, cause)
}

func (r *reconciler) endAdmission(ctx context.Context, q *store.Queries, sandboxID uuid.UUID, outcome string) {
	if _, err := q.EndAdmissionRequest(ctx, store.EndAdmissionRequestParams{
		SandboxID: sandboxID, Outcome: &outcome,
	}); err != nil {
		r.s.log.Warn("reconcile: end admission request", "sandbox_id", sandboxID, "error", err)
	}
}
