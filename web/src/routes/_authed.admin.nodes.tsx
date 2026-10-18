import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'

import { api, type CapacityNode, type ListResponse, type Node } from '@/api'
import { Button } from '@/components/ui/button'
import { mergeNodeCapacity, nodeListPollInterval } from '@/lib/nodes'

export const Route = createFileRoute('/_authed/admin/nodes')({
  component: NodesPage,
})

function NodesPage() {
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
      <div>
        <h1 className="text-2xl font-semibold">Nodes</h1>
        <p className="text-sm text-muted-foreground">
          GPU totals and allocations come from GET /capacity. Declaring a Node lost and
          Enrollment Packets stay CLI-only (api.md §8).
        </p>
      </div>

      {(nodes.isPending || capacity.isPending) && (
        <p className="text-sm text-muted-foreground">Loading…</p>
      )}
      {error && (
        <p className="text-sm text-destructive" data-testid="node-error">
          {error.message}
        </p>
      )}
      {nodes.data && rows.length === 0 && (
        <p className="text-sm text-muted-foreground">No Nodes yet.</p>
      )}

      {rows.length > 0 && (
        <table className="w-full border-collapse text-sm">
          <thead>
            <tr className="border-b border-border text-left text-muted-foreground">
              <th className="p-2 font-medium">Node</th>
              <th className="p-2 font-medium">Status</th>
              <th className="p-2 font-medium">GPUs (allocated / total)</th>
              <th className="p-2 font-medium">Actions</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((row) => (
              <tr key={row.id} className="border-b border-border align-top" data-testid="node-row">
                <td className="p-2">
                  <span className="font-medium">{row.name}</span>
                  <span className="block font-mono text-xs text-muted-foreground">{row.id}</span>
                </td>
                <td className="p-2">{row.status}</td>
                <td className="p-2">
                  {row.gpus.length === 0
                    ? '—'
                    : row.gpus.map((gpu) => (
                        <span key={gpu.model} className="block">
                          {gpu.model}: {gpu.allocated}/{gpu.total}
                        </span>
                      ))}
                </td>
                <td className="p-2">
                  <div className="flex flex-wrap gap-2">
                    {row.paused ? (
                      <Button
                        variant="outline"
                        size="sm"
                        disabled={action.isPending || remove.isPending || row.status === 'lost'}
                        onClick={() => action.mutate({ id: row.id, verb: 'resume' })}
                      >
                        Resume
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
                        Pause
                      </Button>
                    )}
                    <Button
                      variant="outline"
                      size="sm"
                      disabled={action.isPending || remove.isPending}
                      onClick={() => {
                        if (window.confirm(`Delete Node ${row.name}?`)) remove.mutate(row.id)
                      }}
                    >
                      Delete
                    </Button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  )
}
