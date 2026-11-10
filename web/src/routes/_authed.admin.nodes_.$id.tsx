import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { Pause, Play, Server, Trash } from 'lucide-react'
import { useState, type ReactNode } from 'react'

import { api, ApiError, type CapacityNode, type ListResponse, type Node } from '@/api'
import { NODE_STATUS_REASONS, NodeCapacity } from '@/components/node-capacity'
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
import { Badge } from '@/components/ui/badge'
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from '@/components/ui/breadcrumb'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from '@/components/ui/empty'
import { Spinner } from '@/components/ui/spinner'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { useLocale } from '@/lib/i18n'
import { nodeCapabilities, nodeGPUs } from '@/lib/node-detail'
import { mergeNodeCapacity, nodeListPollInterval } from '@/lib/nodes'
import { formatTimestamp } from '@/lib/tokens'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/admin/nodes_/$id')({
  component: NodeDetailPage,
})

const CAPABILITY_LEVELS: Record<string, { label: () => string; variant: 'success' | 'warning' | 'secondary' }> = {
  enforced: { label: m.capability_enforced, variant: 'success' },
  best_effort: { label: m.capability_best_effort, variant: 'warning' },
  unsupported: { label: m.capability_unsupported, variant: 'secondary' },
}

function Detail({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex min-w-0 flex-col gap-0.5">
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="min-w-0 text-sm">{children}</dd>
    </div>
  )
}

function NodeDetailPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const { id } = Route.useParams()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const [confirmOpen, setConfirmOpen] = useState(false)

  const node = useQuery({
    queryKey: ['node', id],
    queryFn: () => api<Node>('GET', `/nodes/${id}`),
    // A missing or deleted Node stays missing.
    retry: (count, error) => !(error instanceof ApiError && error.status === 404) && count < 3,
    refetchInterval: (query) => nodeListPollInterval(query.state.data && [query.state.data]),
  })
  const capacity = useQuery({
    queryKey: ['capacity'],
    queryFn: () => api<ListResponse<CapacityNode>>('GET', '/capacity'),
    refetchInterval: nodeListPollInterval(node.data && [node.data]),
  })

  const invalidate = () =>
    Promise.all([
      queryClient.invalidateQueries({ queryKey: ['node', id] }),
      queryClient.invalidateQueries({ queryKey: ['nodes'] }),
      queryClient.invalidateQueries({ queryKey: ['capacity'] }),
    ])
  const action = useMutation({
    mutationFn: (verb: 'pause' | 'resume') => api<Node>('POST', `/nodes/${id}/${verb}`),
    onSuccess: invalidate,
  })
  const remove = useMutation({
    mutationFn: () => api<Node>('DELETE', `/nodes/${id}`),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['nodes'] })
      void navigate({ to: '/admin/nodes' })
    },
  })

  const current = node.data
  const notFound = node.error instanceof ApiError && node.error.status === 404
  const usage = capacity.data?.items.find((entry) => entry.node_id === id)
  const capabilities = nodeCapabilities(current?.capabilities)
  const gpus = current ? nodeGPUs(current.gpus, current.hardware) : []
  const busy = action.isPending || remove.isPending

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        above={
          <Breadcrumb aria-label={m.common_breadcrumb()}>
            <BreadcrumbList>
              <BreadcrumbItem>{m.nav_group_admin()}</BreadcrumbItem>
              <BreadcrumbSeparator />
              <BreadcrumbItem>
                <BreadcrumbLink render={<Link to="/admin/nodes" />}>{m.nodes_title()}</BreadcrumbLink>
              </BreadcrumbItem>
              <BreadcrumbSeparator />
              <BreadcrumbItem>
                <BreadcrumbPage>{current?.name ?? id}</BreadcrumbPage>
              </BreadcrumbItem>
            </BreadcrumbList>
          </Breadcrumb>
        }
        title={current?.name ?? id}

        actions={
          current && (
            <>
              {current.paused ? (
                <Button
                  variant="outline"
                  disabled={busy || current.status === 'lost'}
                  onClick={() => action.mutate('resume')}
                >
                  <Play data-icon="inline-start" />
                  {m.nodes_resume()}
                </Button>
              ) : (
                <Button variant="outline" disabled={busy} onClick={() => action.mutate('pause')}>
                  <Pause data-icon="inline-start" />
                  {m.nodes_pause()}
                </Button>
              )}
              <AlertDialog open={confirmOpen} onOpenChange={setConfirmOpen}>
                <AlertDialogTrigger render={<Button variant="outline" disabled={busy} />}>
                  <Trash data-icon="inline-start" />
                  {m.common_delete()}
                </AlertDialogTrigger>
                <AlertDialogContent>
                  <AlertDialogHeader>
                    <AlertDialogTitle>{m.nodes_delete_confirm({ name: current.name })}</AlertDialogTitle>
                    <AlertDialogDescription>{m.common_irreversible()}</AlertDialogDescription>
                  </AlertDialogHeader>
                  <AlertDialogFooter>
                    <AlertDialogCancel>{m.common_cancel()}</AlertDialogCancel>
                    <AlertDialogAction
                      variant="destructive"
                      data-testid="confirm-action"
                      onClick={() => {
                        setConfirmOpen(false)
                        remove.mutate()
                      }}
                    >
                      {m.common_delete()}
                    </AlertDialogAction>
                  </AlertDialogFooter>
                </AlertDialogContent>
              </AlertDialog>
            </>
          )
        }
      />

      {node.isPending && (
        <div className="flex items-center gap-2 py-8 text-sm text-muted-foreground">
          <Spinner />
          {m.node_loading()}
        </div>
      )}

      {notFound && (
        <Empty className="border">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <Server />
            </EmptyMedia>
            <EmptyTitle>{m.node_not_found_title()}</EmptyTitle>
            <EmptyDescription>{m.node_not_found_description()}</EmptyDescription>
          </EmptyHeader>
        </Empty>
      )}

      {current && (
        <>
          <Card>
            <CardHeader>
              <CardTitle>{m.node_identity()}</CardTitle>
            </CardHeader>
            <CardContent>
              <dl className="grid grid-cols-2 gap-x-6 gap-y-4 sm:grid-cols-3 lg:grid-cols-4">
                <Detail label={m.node_id()}>
                  <span className="font-mono text-xs break-all">{current.id}</span>
                </Detail>
                <Detail label={m.node_created()}>{formatTimestamp(current.created_at)}</Detail>
                <Detail label={m.node_agent_version()}>{current.agent_version ?? '—'}</Detail>
                <Detail label={m.node_protocol()}>
                  <span className="flex items-center gap-2">
                    <span className="font-mono">
                      {current.proto_major == null ? '—' : `${current.proto_major}.${current.proto_minor ?? 0}`}
                    </span>
                    {current.status === 'outdated' && <StatusBadge state="outdated" />}
                  </span>
                </Detail>
              </dl>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>{m.node_liveness()}</CardTitle>
            </CardHeader>
            <CardContent>
              <dl className="grid grid-cols-2 gap-x-6 gap-y-4 sm:grid-cols-3 lg:grid-cols-4">
                <Detail label={m.node_status()}>
                  <span className="flex flex-wrap items-center gap-2">
                    <StatusBadge state={current.status} data-testid="node-status" />
                    {current.status_reason && (
                      <span className="text-muted-foreground" data-testid="node-status-reason">
                        {NODE_STATUS_REASONS[current.status_reason]?.() ?? current.status_reason}
                      </span>
                    )}
                  </span>
                </Detail>
                <Detail label={m.node_last_seen()}>{formatTimestamp(current.last_seen_at)}</Detail>
              </dl>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>{m.node_runtime()}</CardTitle>
            </CardHeader>
            <CardContent className="flex flex-col gap-4">
              <dl className="grid grid-cols-2 gap-x-6 gap-y-4 sm:grid-cols-3 lg:grid-cols-4">
                <Detail label={m.node_runtime_provider()}>{capabilities.provider ?? '—'}</Detail>
                <Detail label={m.node_runtime_health()}>
                  {current.runtime_health ? <StatusBadge state={current.runtime_health} /> : '—'}
                </Detail>
              </dl>
              {capabilities.entries.length === 0 ? (
                <p className="text-sm text-muted-foreground">{m.node_capabilities_empty()}</p>
              ) : (
                <Table>
                  <TableHeader>
                    <TableRow className="hover:bg-transparent">
                      <TableHead>{m.node_capability()}</TableHead>
                      <TableHead>{m.node_capability_level()}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {capabilities.entries.map((entry) => {
                      const level = CAPABILITY_LEVELS[entry.value]
                      return (
                        <TableRow key={entry.name}>
                          <TableCell className="font-mono text-xs">{entry.name}</TableCell>
                          <TableCell>
                            <Badge variant={level?.variant ?? 'secondary'}>
                              {level?.label() ?? entry.value}
                            </Badge>
                          </TableCell>
                        </TableRow>
                      )
                    })}
                  </TableBody>
                </Table>
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>{m.node_capacity()}</CardTitle>
              <CardDescription>{m.node_capacity_description()}</CardDescription>
            </CardHeader>
            <CardContent className="flex flex-col gap-1.5">
              {/* Same block as the Nodes list row; GPU totals fall back to the inventory. */}
              <NodeCapacity
                gpus={usage?.gpus ?? mergeNodeCapacity([current], [])[0].gpus}
                cpu={usage?.cpu ?? null}
                memory={usage?.memory ?? null}
                disk={usage?.disk ?? null}
              />
              {!usage?.cpu && (
                <p className="text-xs text-muted-foreground">{m.node_not_reported()}</p>
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>{m.node_gpus()}</CardTitle>
            </CardHeader>
            <CardContent className={gpus.length > 0 ? 'px-0 pb-0' : undefined}>
              {gpus.length === 0 ? (
                <p className="text-sm text-muted-foreground">{m.nodes_no_gpu()}</p>
              ) : (
                <Table>
                  <TableHeader>
                    <TableRow className="hover:bg-transparent">
                      <TableHead className="pl-5">{m.node_gpu_uuid()}</TableHead>
                      <TableHead>{m.node_gpu_model()}</TableHead>
                      <TableHead className="pr-5">{m.node_gpu_health()}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {gpus.map((gpu) => (
                      <TableRow key={gpu.uuid}>
                        <TableCell className="pl-5 font-mono text-xs">{gpu.uuid}</TableCell>
                        <TableCell>{gpu.model}</TableCell>
                        <TableCell className="pr-5">
                          {gpu.health ? <StatusBadge state={gpu.health} /> : '—'}
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              )}
            </CardContent>
          </Card>
        </>
      )}
    </div>
  )
}
