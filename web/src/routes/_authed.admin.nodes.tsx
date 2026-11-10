import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { Pause, Play, Server, Trash } from 'lucide-react'
import { useState } from 'react'

import { api, type CapacityNode, type ListResponse, type Node } from '@/api'
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
import { mergeNodeCapacity, nodeListPollInterval } from '@/lib/nodes'
import { useLocale } from '@/lib/i18n'
import { m } from '@/paraglide/messages'

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
                  {row.name}
                  <StatusBadge state={row.status} />
                </ItemTitle>
                <ItemDescription className="font-mono" title={row.id}>
                  {row.id.slice(0, 8)}
                </ItemDescription>
                <div className="flex flex-col gap-1.5 text-xs text-muted-foreground">
                  {row.gpus.length === 0 ? (
                    <span>{m.nodes_no_gpu()}</span>
                  ) : (
                    row.gpus.map((gpu) => (
                      <div key={gpu.model} className="flex items-center gap-2">
                        <span className="w-28 truncate">{gpu.model}</span>
                        {/* No Progress component installed; a plain bar. */}
                        <span className="h-1.5 w-20 overflow-hidden rounded-full bg-muted">
                          <span
                            className="block h-full rounded-full bg-brand"
                            style={{
                              width: `${gpu.total > 0 ? (gpu.allocated / gpu.total) * 100 : 0}%`,
                            }}
                          />
                        </span>
                        <span className="font-mono">
                          {gpu.allocated}/{gpu.total}
                        </span>
                      </div>
                    ))
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
