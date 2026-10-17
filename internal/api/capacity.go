package api

import (
	"net/http"
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
		usage, err := s.q.ListNodeGPUUsage(ctx, node.ID)
		if err != nil {
			s.internal(w, "read node GPUs", err)
			return
		}
		entry := capacityNode{
			NodeID: node.ID.String(), Name: node.Name,
			Status: deriveNodeStatus(node, now), GPUs: make([]capacityGPU, 0, len(usage)),
		}
		for _, row := range usage {
			entry.GPUs = append(entry.GPUs, capacityGPU{
				Model: row.Model, Total: int(row.Total), Allocated: int(row.Allocated),
			})
		}
		out.Items = append(out.Items, entry)
	}
	writeJSON(w, http.StatusOK, out)
}
