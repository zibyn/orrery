import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { Pause, Play, Server, Trash } from 'lucide-react'

import { api, type CapacityNode, type ListResponse, type Node } from '@/api'
import { EmptyState } from '@/components/empty-state'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { mergeNodeCapacity, nodeListPollInterval } from '@/lib/nodes'
import { useLocale } from '@/lib/i18n'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/admin/nodes')({
  component: NodesPage,
})

function NodesPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const queryClient = useQueryClient()

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
  const error = nodes.error ?? capacity.error ?? action.error ?? remove.error

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
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
        <p className="py-8 text-sm text-muted-foreground">{m.nodes_loading()}</p>
      )}
      {error && (
        <p className="text-sm text-destructive" role="alert" data-testid="node-error">
          <Problem error={error} />
        </p>
      )}

      {nodes.data && rows.length === 0 && (
        <EmptyState
          icon={<Server />}
          title={m.nodes_empty_title()}
          description={m.nodes_empty_description()}
        />
      )}

      {rows.length > 0 && (
        <Card className="overflow-hidden">
          <CardHeader>
            <CardTitle>{m.nodes_enrolled()}</CardTitle>
            <CardDescription>{m.nodes_enrolled_description()}</CardDescription>
          </CardHeader>
          <CardContent className="px-0 pb-0">
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead className="pl-5">{m.sandbox_node()}</TableHead>
                  <TableHead>{m.nodes_status()}</TableHead>
                  <TableHead>{m.nodes_gpus()}</TableHead>
                  <TableHead className="pr-5 text-right">{m.nodes_actions()}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((row) => (
                  <TableRow key={row.id} data-testid="node-row">
                    <TableCell className="pl-5">
                      <span className="block text-sm font-medium">{row.name}</span>
                      <span className="block font-mono text-xs text-muted-foreground">
                        {row.id}
                      </span>
                    </TableCell>
                    <TableCell>
                      <StatusBadge state={row.status} />
                    </TableCell>
                    <TableCell>
                      {row.gpus.length === 0 ? (
                        <span className="text-xs text-muted-foreground">—</span>
                      ) : (
                        <div className="flex flex-col gap-1.5">
                          {row.gpus.map((gpu) => (
                            <div key={gpu.model} className="flex items-center gap-2">
                              <span className="w-28 truncate text-xs">{gpu.model}</span>
                              <span className="h-1.5 w-20 overflow-hidden rounded-full bg-muted">
                                <span
                                  className="block h-full rounded-full bg-brand"
                                  style={{
                                    width: `${gpu.total > 0 ? (gpu.allocated / gpu.total) * 100 : 0}%`,
                                  }}
                                />
                              </span>
                              <span className="font-mono text-xs text-muted-foreground">
                                {gpu.allocated}/{gpu.total}
                              </span>
                            </div>
                          ))}
                        </div>
                      )}
                    </TableCell>
                    <TableCell className="pr-5">
                      <div className="flex items-center justify-end gap-2">
                        {row.paused ? (
                          <Button
                            variant="outline"
                            size="sm"
                            disabled={action.isPending || remove.isPending || row.status === 'lost'}
                            onClick={() => action.mutate({ id: row.id, verb: 'resume' })}
                          >
                            <Play />
                            {m.nodes_resume()}
                          </Button>
                        ) : (
                          <Button
                            variant="outline"
                            size="sm"
                            disabled={
                              action.isPending || remove.isPending || row.paused === null
                            }
                            onClick={() => action.mutate({ id: row.id, verb: 'pause' })}
                          >
                            <Pause />
                            {m.nodes_pause()}
                          </Button>
                        )}
                        <Button
                          variant="ghost"
                          size="sm"
                          disabled={action.isPending || remove.isPending}
                          onClick={() => {
                            if (window.confirm(m.nodes_delete_confirm({ name: row.name }))) {
                              remove.mutate(row.id)
                            }
                          }}
                        >
                          <Trash />
                          {m.common_delete()}
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      )}
    </div>
  )
}
