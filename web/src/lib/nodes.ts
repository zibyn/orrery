import type { CapacityGPU, CapacityNode, Node } from '@/api'

// The Node page needs one row per Node: identity and pause state from
// GET /nodes, GPU totals and allocations from GET /capacity (api.md §8).
// `paused` is null for a Node the (paginated) Node list did not return.
export type NodeRow = {
  id: string
  name: string
  status: string
  paused: boolean | null
  gpus: CapacityGPU[]
}

export function mergeNodeCapacity(nodes: Node[], capacity: CapacityNode[]): NodeRow[] {
  const capacityByID = new Map(capacity.map((entry) => [entry.node_id, entry]))
  const rows: NodeRow[] = nodes.map((node) => ({
    id: node.id,
    name: node.name,
    status: node.status,
    paused: node.paused,
    gpus: capacityByID.get(node.id)?.gpus ?? totalsFromInventory(node),
  }))
  const known = new Set(nodes.map((node) => node.id))
  for (const entry of capacity) {
    if (known.has(entry.node_id)) continue
    rows.push({
      id: entry.node_id,
      name: entry.name,
      status: entry.status,
      paused: null,
      gpus: entry.gpus,
    })
  }
  return rows
}

// Fallback for a Node with no capacity row (for example a race between the two
// requests): the Node reports which GPUs it has, but not their usage.
function totalsFromInventory(node: Node): CapacityGPU[] {
  const totals = new Map<string, number>()
  for (const gpu of node.gpus) totals.set(gpu.model, (totals.get(gpu.model) ?? 0) + 1)
  return Array.from(totals, ([model, total]) => ({ model, total, allocated: 0 }))
}
