import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'
import { Pause, Play, Server, Trash } from 'lucide-react'
import { useState } from 'react'

import { api, type CapacityNode, type CapacityPools, type ListResponse, type Node } from '@/api'
import { PageHeader } from '@/components/page-header'
import { StatusBadge } from '@/components/status-badge'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from '@/components/ui/alert-dialog'
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from '@/components/ui/breadcrumb'
import { Button } from '@/components/ui/button'
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from '@/components/ui/empty'
import {
  Item,
  ItemActions,
  ItemContent,
  ItemDescription,
  ItemGroup,
  ItemMedia,
  ItemTitle,
} from '@/components/ui/item'
import { Spinner } from '@/components/ui/spinner'
import { formatBytes } from '@/lib/files'
import {
  diskBelowWatermark,
  formatSince,
  mergeNodeCapacity,
  nodeListPollInterval,
} from '@/lib/nodes'
import { useLocale } from '@/lib/i18n'
import { formatTimestamp } from '@/lib/tokens'
import { m } from '@/paraglide/messages'
import { getLocale } from '@/paraglide/runtime'

export const Route = createFileRoute('/_authed/admin/nodes')({
  component: NodesPage,
})

function NodesPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const queryClient = useQueryClient()
  // The Node whose delete is being confirmed.
  const [confirming, setConfirming] = useState<string | null>(null)

  const nodes = useQuery({
    queryKey: ['nodes'],
    queryFn: () => api<ListResponse<Node>>('GET', '/nodes?limit=200'),
    refetchInterval: (query) => nodeListPollInterval(query.state.data?.items),
  })
  const capacity = useQuery({
    queryKey: ['capacity'],
    queryFn: () => api<ListResponse<CapacityNode>>('GET', '/capacity'),
  })

  const invalidate = () => {
    queryClient.invalidateQueries({ queryKey: ['nodes'] })
    queryClient.invalidateQueries({ queryKey: ['capacity'] })
  }
  const action = useMutation({
    mutationFn: ({ id, verb }: { id: string; verb: 'pause' | 'resume' }) =>
      api<Node>('POST', `/nodes/${id}/${verb}`),
    onSuccess: invalidate,
  })
  const remove = useMutation({
    mutationFn: (id: string) => api<Node>('DELETE', `/nodes/${id}`),
    onSuccess: invalidate,
  })

  const rows = mergeNodeCapacity(nodes.data?.items ?? [], capacity.data?.items ?? [])
  // The server's problem detail explains why a pause/resume/delete was refused
  // (for example delete preconditions, or resuming a lost Node).

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        above={
          <Breadcrumb aria-label={m.common_breadcrumb()}>
            <BreadcrumbList>
              <BreadcrumbItem>{m.nav_group_admin()}</BreadcrumbItem>
              <BreadcrumbSeparator />
              <BreadcrumbItem>
                <BreadcrumbPage>{m.nodes_title()}</BreadcrumbPage>
              </BreadcrumbItem>
            </BreadcrumbList>
          </Breadcrumb>
        }
        title={m.nodes_title()}
        description={m.nodes_description()}
        meta={
          rows.length > 0
            ? rows.length === 1
              ? m.nodes_count_one({ count: rows.length })
              : m.nodes_count_other({ count: rows.length })
            : undefined
        }
      />

      {(nodes.isPending || capacity.isPending) && (
        <div className="flex items-center gap-2 py-8 text-sm text-muted-foreground">
          <Spinner />
          {m.nodes_loading()}
        </div>
      )}

      {nodes.data && rows.length === 0 && (
        <Empty className="border">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <Server />
            </EmptyMedia>
            <EmptyTitle>{m.nodes_empty_title()}</EmptyTitle>
            <EmptyDescription>{m.nodes_empty_description()}</EmptyDescription>
          </EmptyHeader>
        </Empty>
      )}

      {rows.length > 0 && (
        <ItemGroup className="gap-2">
          {rows.map((row) => (
            <Item key={row.id} variant="outline" role="listitem" data-testid="node-row">
              <ItemMedia variant="icon">
                <Server />
              </ItemMedia>
              <ItemContent>
                <ItemTitle>
                  <Link to="/admin/nodes/$id" params={{ id: row.id }} className="hover:underline">
                    {row.name}
                  </Link>
                  <StatusBadge state={row.status} />
                  {row.status === 'unavailable' && row.statusReason && (
                    <span className="text-xs font-normal text-muted-foreground">
                      {REASONS[row.statusReason]()}
                    </span>
                  )}
                </ItemTitle>
                <ItemDescription>
                  <span className="font-mono" title={row.id}>
                    {row.id.slice(0, 8)}
                  </span>
                  {row.paused !== null && (
                    <>
                      {' · '}
                      <span title={formatTimestamp(row.lastSeenAt)}>
                        {m.nodes_last_seen({ time: formatSince(row.lastSeenAt, Date.now(), getLocale()) })}
                      </span>
                    </>
                  )}
                </ItemDescription>
                {row.status === 'outdated' && (
                  <p className="text-xs text-warning">{m.nodes_outdated()}</p>
                )}
                {row.unhealthyGPUs > 0 && (
                  <p className="text-xs text-destructive">
                    {row.unhealthyGPUs === 1
                      ? m.nodes_gpu_unhealthy_one({ count: row.unhealthyGPUs })
                      : m.nodes_gpu_unhealthy_other({ count: row.unhealthyGPUs })}
                  </p>
                )}
                <div className="flex flex-col gap-1.5 text-xs text-muted-foreground">
                  {row.gpus.length === 0 ? (
                    <span>{m.nodes_no_gpu()}</span>
                  ) : (
                    row.gpus.map((gpu) => (
                      <Bar
                        key={gpu.model}
                        label={gpu.model}
                        value={gpu.allocated}
                        total={gpu.total}
                        text={`${gpu.allocated}/${gpu.total}`}
                      />
                    ))
                  )}
                  <Pools label={m.nodes_cpu()} pools={row.cpu} format={formatCores} />
                  <Pools label={m.nodes_memory()} pools={row.memory} format={formatBytes} />
                  {row.disk && (
                    <>
                      <Bar
                        label={m.nodes_disk()}
                        value={row.disk.total - row.disk.free}
                        total={row.disk.total}
                        // The mark sits where used space leaves only the watermark free.
                        mark={1 - row.disk.watermark}
                        markTitle={m.nodes_disk_watermark({
                          percent: Math.round(row.disk.watermark * 100),
                        })}
                        text={m.nodes_disk_used({
                          used: formatBytes(row.disk.total - row.disk.free),
                          total: formatBytes(row.disk.total),
                        })}
                      />
                      {diskBelowWatermark(row.disk) && (
                        <span className="text-destructive">{m.nodes_disk_full()}</span>
                      )}
                    </>
                  )}
                </div>
              </ItemContent>
              <ItemActions>
                {row.paused ? (
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={action.isPending || remove.isPending || row.status === 'lost'}
                    onClick={() => action.mutate({ id: row.id, verb: 'resume' })}
                  >
                    <Play data-icon="inline-start" />
                    {m.nodes_resume()}
                  </Button>
                ) : (
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={action.isPending || remove.isPending || row.paused === null}
                    onClick={() => action.mutate({ id: row.id, verb: 'pause' })}
                  >
                    <Pause data-icon="inline-start" />
                    {m.nodes_pause()}
                  </Button>
                )}
                <AlertDialog open={confirming === row.id} onOpenChange={(next) => setConfirming(next ? row.id : null)}>
                  <AlertDialogTrigger
                    render={
                      <Button
                        variant="ghost"
                        size="sm"
                        disabled={action.isPending || remove.isPending}
                      />
                    }
                  >
                    <Trash data-icon="inline-start" />
                    {m.common_delete()}
                  </AlertDialogTrigger>
                  <AlertDialogContent>
                    <AlertDialogHeader>
                      <AlertDialogTitle>{m.nodes_delete_confirm({ name: row.name })}</AlertDialogTitle>
                      <AlertDialogDescription>{m.common_irreversible()}</AlertDialogDescription>
                    </AlertDialogHeader>
                    <AlertDialogFooter>
                      <AlertDialogCancel>{m.common_cancel()}</AlertDialogCancel>
                      <AlertDialogAction
                        variant="destructive"
                        data-testid="confirm-action"
                        onClick={() => {
                          setConfirming(null)
                          remove.mutate(row.id)
                        }}
                      >
                        {m.common_delete()}
                      </AlertDialogAction>
                    </AlertDialogFooter>
                  </AlertDialogContent>
                </AlertDialog>
              </ItemActions>
            </Item>
          ))}
        </ItemGroup>
      )}
    </div>
  )
}

const REASONS: Record<NonNullable<Node['status_reason']>, () => string> = {
  heartbeat_stale: m.nodes_reason_heartbeat_stale,
  runtime_unhealthy: m.nodes_reason_runtime_unhealthy,
  capability_missing: m.nodes_reason_capability_missing,
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
