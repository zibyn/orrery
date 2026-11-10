package api

import (
	"net/http"
	"time"

	"github.com/zibyn/orrery/internal/store"
)

type capacityGPU struct {
	Model     string `json:"model"`
	Total     int    `json:"total"`
	Allocated int    `json:"allocated"`
}

type capacityPool struct {
	Total     float64 `json:"total"`
	Allocated float64 `json:"allocated"`
}

// capacityPools splits CPU or memory the way admission does (ADR-0015): the
// GPU pool is what the cards reserve, the general pool the rest. GPUPool is
// nil on a Node without cards.
type capacityPools struct {
	GPUPool     *capacityPool `json:"gpu_pool"`
	GeneralPool capacityPool  `json:"general_pool"`
}

// capacityDisk is observed, not allocated: admission gates on free space
// against the watermark.
type capacityDisk struct {
	Total     uint64  `json:"total"`
	Free      uint64  `json:"free"`
	Watermark float64 `json:"watermark"`
}

type capacityNode struct {
	NodeID string         `json:"node_id"`
	Name   string         `json:"name"`
	Status string         `json:"status"`
	GPUs   []capacityGPU  `json:"gpus"`
	CPU    *capacityPools `json:"cpu"`
	Memory *capacityPools `json:"memory"`
	Disk   *capacityDisk  `json:"disk"`
}

type capacityList struct {
	Items      []capacityNode `json:"items"`
	NextCursor *string        `json:"next_cursor"` // no pagination yet: always null
}

// capacityFor fills a Node's CPU, memory and disk from the same fit admission
// uses. A Node that has not reported hardware leaves all three nil.
func capacityFor(node store.Node, used store.NodePlacementRow) capacityNode {
	fit, ok := fitFor(node, used)
	if !ok {
		return capacityNode{}
	}
	pools := func(total, allocated, allocatedNoGPU, gpuReserved float64) *capacityPools {
		if fit.gpuTotal == 0 {
			return &capacityPools{GeneralPool: capacityPool{Total: total, Allocated: allocated}}
		}
		return &capacityPools{
			GPUPool:     &capacityPool{Total: gpuReserved, Allocated: allocated - allocatedNoGPU},
			GeneralPool: capacityPool{Total: total - gpuReserved, Allocated: allocatedNoGPU},
		}
	}
	gpuCPU, gpuMemory := fit.gpuReserved()
	return capacityNode{
		CPU:    pools(fit.cpu, fit.usedCPU, fit.usedCPUNoGPU, gpuCPU),
		Memory: pools(fit.memory, fit.usedMemory, fit.usedMemNoGPU, gpuMemory),
		Disk:   &capacityDisk{Total: fit.diskTotal, Free: fit.diskFree, Watermark: fit.watermark()},
	}
}

// handleGetCapacity is the read-only capacity view (api.md §3): per Node, the
// GPU models it reports with their totals, and the CPU, memory and disk
// admission places against (ADR-0015).
func (s *Server) handleGetCapacity(w http.ResponseWriter, r *http.Request, p *principal) {
	ctx := r.Context()
	nodes, err := s.q.ListAllNodes(ctx)
	if err != nil {
		s.internal(w, "list nodes", err)
		return
	}
	now := time.Now()
	out := capacityList{Items: make([]capacityNode, 0, len(nodes))}
	for _, node := range nodes {
		usage, err := s.q.ListNodeGPUUsage(ctx, node.ID)
		if err != nil {
			s.internal(w, "read node GPUs", err)
			return
		}
		used, err := s.q.NodePlacement(ctx, node.ID)
		if err != nil {
			s.internal(w, "read node allocations", err)
			return
		}
		entry := capacityFor(node, used)
		entry.NodeID, entry.Name = node.ID.String(), node.Name
		entry.Status, _ = deriveNodeStatus(node, now)
		entry.GPUs = make([]capacityGPU, 0, len(usage))
		for _, row := range usage {
			entry.GPUs = append(entry.GPUs, capacityGPU{
				Model: row.Model, Total: int(row.Total), Allocated: int(row.Allocated),
			})
		}
		out.Items = append(out.Items, entry)
	}
	writeJSON(w, http.StatusOK, out)
}
