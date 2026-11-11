import type { CapacityGPU, CapacityNode, CapacityPools, Node } from '@/api'

// The Node page needs one row per Node: identity and pause state from
// GET /nodes, GPU/CPU/memory allocations and disk from GET /capacity (api.md §3).
// `paused` is null for a Node the (paginated) Node list did not return; such a
// row also has no status reason, heartbeat time or GPU health.
export type NodeRow = {
  id: string
  name: string
  status: string
  paused: boolean | null
  statusReason: Node['status_reason']
  lastSeenAt: string | null
  unhealthyGPUs: number
  gpus: CapacityGPU[]
  cpu: CapacityPools | null
  memory: CapacityPools | null
  disk: CapacityNode['disk']
}

export function mergeNodeCapacity(nodes: Node[], capacity: CapacityNode[]): NodeRow[] {
  const capacityByID = new Map(capacity.map((entry) => [entry.node_id, entry]))
  const rows: NodeRow[] = nodes.map((node) => {
    const entry = capacityByID.get(node.id)
    return {
      id: node.id,
      name: node.name,
      status: node.status,
      paused: node.paused,
      statusReason: node.status_reason,
      lastSeenAt: node.last_seen_at,
      unhealthyGPUs: unhealthyGPUs(node.hardware),
      gpus: entry?.gpus ?? totalsFromInventory(node),
      cpu: entry?.cpu ?? null,
      memory: entry?.memory ?? null,
      disk: entry?.disk ?? null,
    }
  })
  const known = new Set(nodes.map((node) => node.id))
  for (const entry of capacity) {
    if (known.has(entry.node_id)) continue
    rows.push({
      id: entry.node_id,
      name: entry.name,
      status: entry.status,
      paused: null,
      statusReason: null,
      lastSeenAt: null,
      unhealthyGPUs: 0,
      gpus: entry.gpus,
      cpu: entry.cpu,
      memory: entry.memory,
      disk: entry.disk,
    })
  }
  return rows
}

// api.md §8: a list polls every 5 s while a resource is non-terminal. `lost`
// is the only terminal Node status; a ready/paused/unavailable/outdated Node
// can still change.
export function nodeListPollInterval(
  nodes: { status: string }[] | undefined,
): number | false {
  return nodes?.some((node) => node.status !== 'lost') ? 5000 : false
}

// Fallback for a Node with no capacity row (for example a race between the two
// requests): the Node reports which GPUs it has, but not their usage.
export function totalsFromInventory(node: Pick<Node, 'gpus'>): CapacityGPU[] {
  const totals = new Map<string, number>()
  for (const gpu of node.gpus) totals.set(gpu.model, (totals.get(gpu.model) ?? 0) + 1)
  return Array.from(totals, ([model, total]) => ({ model, total, allocated: 0 }))
}

export type GPUHealth = { uuid: string; state: string }

// The last Heartbeat's per-card health, stored as nodes.hardware.gpu_health;
// empty before the first Heartbeat reports it.
export function gpuHealth(hardware: unknown): GPUHealth[] {
  const health = (hardware as { gpu_health?: unknown } | null)?.gpu_health
  return Array.isArray(health) ? (health as GPUHealth[]) : []
}

function unhealthyGPUs(hardware: unknown): number {
  return gpuHealth(hardware).filter((gpu) => gpu.state !== 'healthy').length
}

// "3 minutes ago" in the console's language. Pass the query's dataUpdatedAt as `now`: an unchanged poll does not re-render otherwise.
export function formatSince(value: string | null, now: number, locale: string): string {
  if (!value) return '—'
  const seconds = Math.max(0, Math.round((now - Date.parse(value)) / 1000))
  if (Number.isNaN(seconds)) return value
  const format = new Intl.RelativeTimeFormat(locale, { numeric: 'auto' })
  if (seconds < 60) return format.format(-seconds, 'second')
  if (seconds < 3600) return format.format(-Math.floor(seconds / 60), 'minute')
  if (seconds < 86400) return format.format(-Math.floor(seconds / 3600), 'hour')
  return format.format(-Math.floor(seconds / 86400), 'day')
}

// api.md §3: a Node whose observed free disk is under its watermark takes no
// new allocations.
export function diskBelowWatermark(disk: CapacityNode['disk']): boolean {
  return disk !== null && disk.free < disk.total * disk.watermark
}
