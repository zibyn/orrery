package api

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/zibyn/stars-sandbox/internal/store"
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

type nodeHardware struct {
	CPUCores      float64 `json:"cpu_cores"`
	MemoryBytes   int64   `json:"memory_bytes"`
	DiskFree      uint64  `json:"disk_free"`
	DiskTotal     uint64  `json:"disk_total"`
	DiskWatermark float64 `json:"disk_watermark"`
}

func fitFor(node store.Node, used store.NodeAllocatedRow) (nodeFit, bool) {
	var hw nodeHardware
	if len(node.Hardware) == 0 || json.Unmarshal(node.Hardware, &hw) != nil {
		return nodeFit{}, false
	}
	return nodeFit{
		cpu:           hw.CPUCores - node.ReservedCpu,
		memory:        float64(hw.MemoryBytes - node.ReservedMemory),
		usedCPU:       used.Cpu,
		usedMemory:    float64(used.Memory),
		diskFree:      hw.DiskFree,
		diskTotal:     hw.DiskTotal,
		diskWatermark: hw.DiskWatermark,
	}, true
}

// admissionExhausted reports whether every usable Node is too small for the
// request, so waiting for capacity cannot help. Unknown hardware (a Node that
// has not reported yet) is not proof either way. A Sandbox bound to a Node can
// only ever go there.
func admissionExhausted(nodes []store.Node, sb store.Sandbox) bool {
	known, unknown := false, false
	for _, node := range nodes {
		if sb.NodeID != nil && node.ID != *sb.NodeID {
			continue
		}
		fit, ok := fitFor(node, store.NodeAllocatedRow{})
		if !ok {
			unknown = true
			continue
		}
		known = true
		if fit.cpu >= sb.Cpu && fit.memory >= float64(sb.Memory) {
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
	now := time.Now()
	for _, req := range requests {
		sb := req.Sandbox
		if !req.AdmissionRequest.DeadlineAt.Valid || !req.AdmissionRequest.DeadlineAt.Time.After(now) {
			r.failAdmission(ctx, q, sb, "admission_timeout")
			continue
		}
		if sb.State != "pending" || sb.DesiredState != "running" {
			// A concurrent stop or delete won: the request is no longer
			// waiting for capacity.
			r.endAdmission(ctx, q, sb.ID, "canceled")
			continue
		}
		placed, err := r.tryAdmit(ctx, q, sb, nodes, now)
		if err != nil {
			return err
		}
		if placed {
			r.endAdmission(ctx, q, sb.ID, "admitted")
			continue
		}
		if admissionExhausted(nodes, sb) {
			r.failAdmission(ctx, q, sb, "unsatisfiable")
		}
	}
	return nil
}

// tryAdmit places a Sandbox on the first Node that fits. A Sandbox bound to a
// Node stays there (a Workspace never moves), so only that Node is considered
// on a restart. Placement ignores GPU devices until GPU allocation lands;
// User Quota still bounds the GPU count.
func (r *reconciler) tryAdmit(ctx context.Context, q *store.Queries, sb store.Sandbox, nodes []store.Node, now time.Time) (bool, error) {
	for _, node := range nodes {
		if sb.NodeID != nil && node.ID != *sb.NodeID {
			continue
		}
		if deriveNodeStatus(node, now) != "ready" || !r.hub.connected(node.ID) {
			continue
		}
		used, err := q.NodeAllocated(ctx, node.ID)
		if err != nil {
			return false, err
		}
		fit, ok := fitFor(node, used)
		if !ok || !fit.admits(sb.Cpu, sb.Memory) {
			continue
		}
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
			ws, err := q.GetSandboxWorkspace(ctx, &sb.ID)
			if err != nil {
				return false, nil // the next sweep fails the Sandbox with runtime_create_failed
			}
			if ws.NodeID == nil {
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
	if err := q.DeleteAllocation(ctx, sb.ID); err != nil {
		r.s.log.Warn("reconcile: release allocation", "sandbox_id", sb.ID, "error", err)
	}
	if err := q.DeleteAttachment(ctx, sb.ID); err != nil {
		r.s.log.Warn("reconcile: release attachment", "sandbox_id", sb.ID, "error", err)
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
