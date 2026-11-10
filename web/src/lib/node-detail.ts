import type { Node } from '@/api'
import { gpuHealth } from '@/lib/nodes'

export type NodeCapability = { name: string; value: string }

// `capabilities` is the stored Capabilities report (nodeproto.CapabilityReport),
// null until the Agent reports one.
export function nodeCapabilities(raw: unknown): {
  provider: string | null
  entries: NodeCapability[]
} {
  const report = (raw ?? {}) as { runtime_provider?: string; entries?: NodeCapability[] | null }
  return { provider: report.runtime_provider || null, entries: report.entries ?? [] }
}

// GPU health arrives with each Heartbeat in `hardware.gpu_health`; a card the
// Heartbeat has not mentioned yet has no health.
export function nodeGPUs(
  gpus: Node['gpus'],
  hardware: unknown,
): { uuid: string; model: string; health: string | null }[] {
  const health = new Map(gpuHealth(hardware).map((gpu) => [gpu.uuid, gpu.state]))
  return gpus.map((gpu) => ({ ...gpu, health: health.get(gpu.uuid) ?? null }))
}
