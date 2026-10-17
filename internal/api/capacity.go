package api

import (
	"net/http"
	"sort"
	"time"
)

type capacityGPU struct {
	Model     string `json:"model"`
	Total     int    `json:"total"`
	Allocated int    `json:"allocated"`
}

type capacityNode struct {
	NodeID string        `json:"node_id"`
	Name   string        `json:"name"`
	Status string        `json:"status"`
	GPUs   []capacityGPU `json:"gpus"`
}

type capacityList struct {
	Items      []capacityNode `json:"items"`
	NextCursor *string        `json:"next_cursor"` // no pagination yet: always null
}

// handleGetCapacity is the read-only capacity view (api.md §3): per Node, the
// GPU models it reports with their totals (ADR-0015).
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
		gpus, err := s.q.ListNodeGPUs(ctx, node.ID)
		if err != nil {
			s.internal(w, "read node GPUs", err)
			return
		}
		counts := map[string]int{}
		for _, gpu := range gpus {
			counts[gpu.Model]++
		}
		models := make([]string, 0, len(counts))
		for model := range counts {
			models = append(models, model)
		}
		sort.Strings(models)
		entry := capacityNode{
			NodeID: node.ID.String(), Name: node.Name,
			Status: deriveNodeStatus(node, now), GPUs: make([]capacityGPU, 0, len(models)),
		}
		for _, model := range models {
			// ponytail: GPUs are not allocated until the GPU ticket; quota is
			// enforced now, device allocation is not.
			entry.GPUs = append(entry.GPUs, capacityGPU{Model: model, Total: counts[model], Allocated: 0})
		}
		out.Items = append(out.Items, entry)
	}
	writeJSON(w, http.StatusOK, out)
}
