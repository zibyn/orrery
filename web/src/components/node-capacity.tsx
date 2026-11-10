import type { CapacityNode, CapacityPools } from '@/api'
import { formatBytes } from '@/lib/files'
import { diskBelowWatermark } from '@/lib/nodes'
import { m } from '@/paraglide/messages'

// The capacity block shared by a Nodes list row and the Node detail page, so
// both read Allocation and disk the same way.
export function NodeCapacity({
  gpus,
  cpu,
  memory,
  disk,
}: Pick<CapacityNode, 'gpus' | 'cpu' | 'memory' | 'disk'>) {
  return (
    <div className="flex flex-col gap-1.5 text-xs text-muted-foreground">
      {gpus.length === 0 ? (
        <span>{m.nodes_no_gpu()}</span>
      ) : (
        gpus.map((gpu) => (
          <Bar
            key={gpu.model}
            label={gpu.model}
            value={gpu.allocated}
            total={gpu.total}
            text={`${gpu.allocated}/${gpu.total}`}
          />
        ))
      )}
      <Pools label={m.nodes_cpu()} pools={cpu} format={formatCores} />
      <Pools label={m.nodes_memory()} pools={memory} format={formatBytes} />
      {disk && (
        <>
          <Bar
            label={m.nodes_disk()}
            value={disk.total - disk.free}
            total={disk.total}
            // The mark sits where used space leaves only the watermark free.
            mark={1 - disk.watermark}
            markTitle={m.nodes_disk_watermark({
              percent: Math.round(disk.watermark * 100),
            })}
            text={m.nodes_disk_used({
              used: formatBytes(disk.total - disk.free),
              total: formatBytes(disk.total),
            })}
          />
          {diskBelowWatermark(disk) && (
            <span className="text-destructive">{m.nodes_disk_full()}</span>
          )}
        </>
      )}
    </div>
  )
}

function formatCores(cores: number): string {
  return String(Math.round(cores * 100) / 100)
}

// CPU or memory Allocation: a Node with GPUs keeps two pools (ADR-0015), so a
// CPU-only Sandbox's headroom is the general pool alone.
function Pools({
  label,
  pools,
  format,
}: {
  label: string
  pools: CapacityPools | null
  format: (value: number) => string
}) {
  if (!pools) return null
  const rows = pools.gpu_pool
    ? [
        { name: `${label} · ${m.nodes_pool_gpu()}`, pool: pools.gpu_pool },
        { name: `${label} · ${m.nodes_pool_general()}`, pool: pools.general_pool },
      ]
    : [{ name: label, pool: pools.general_pool }]
  return rows.map(({ name, pool }) => (
    <Bar
      key={name}
      label={name}
      value={pool.allocated}
      total={pool.total}
      text={m.nodes_allocated({ allocated: format(pool.allocated), total: format(pool.total) })}
    />
  ))
}

// No Progress component installed; a plain bar, with an optional mark at a
// fraction of the track.
function Bar({
  label,
  value,
  total,
  text,
  mark,
  markTitle,
}: {
  label: string
  value: number
  total: number
  text: string
  mark?: number
  markTitle?: string
}) {
  const percent = (fraction: number) => `${Math.min(100, Math.max(0, fraction * 100))}%`
  return (
    <div className="flex items-center gap-2">
      <span className="w-40 truncate" title={label}>
        {label}
      </span>
      <span className="relative h-1.5 w-20 shrink-0 overflow-hidden rounded-full bg-muted">
        <span
          className="block h-full rounded-full bg-brand"
          style={{ width: percent(total > 0 ? value / total : 0) }}
        />
        {mark !== undefined && (
          <span
            className="absolute inset-y-0 w-0.5 bg-foreground"
            style={{ left: percent(mark) }}
            title={markTitle}
          />
        )}
      </span>
      <span className="font-mono">{text}</span>
    </div>
  )
}
