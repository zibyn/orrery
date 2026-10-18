import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'

import { api, type Sandbox } from '@/api'
import { Button, buttonVariants } from '@/components/ui/button'
import { sandboxPollInterval } from '@/lib/sandbox'

export const Route = createFileRoute('/_authed/sandboxes/$id')({
  component: SandboxDetailPage,
})

function SandboxDetailPage() {
  const { id } = Route.useParams()
  const navigate = useNavigate()
  const queryClient = useQueryClient()

  const sandbox = useQuery({
    queryKey: ['sandbox', id],
    queryFn: () => api<Sandbox>('GET', `/sandboxes/${id}`),
    refetchInterval: (query) => sandboxPollInterval(query.state.data?.state),
  })

  const action = useMutation({
    mutationFn: (verb: 'start' | 'stop') => api<Sandbox>('POST', `/sandboxes/${id}/${verb}`),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['sandbox', id] })
      queryClient.invalidateQueries({ queryKey: ['sandboxes'] })
    },
  })
  const remove = useMutation({
    mutationFn: () => api<Sandbox>('DELETE', `/sandboxes/${id}`),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['sandboxes'] })
      navigate({ to: '/sandboxes' })
    },
  })

  const current = sandbox.data
  const error = sandbox.error ?? action.error ?? remove.error

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex flex-col gap-1">
          <Link to="/sandboxes" className="text-sm text-muted-foreground hover:underline">
            ← Sandboxes
          </Link>
          <h1 className="text-2xl font-semibold">{current?.name ?? id}</h1>
        </div>
        {current && (
          <div className="flex gap-2">
            <Button
              variant="outline"
              disabled={current.state !== 'stopped' || action.isPending || remove.isPending}
              onClick={() => action.mutate('start')}
            >
              Start
            </Button>
            <Button
              variant="outline"
              disabled={
                current.state === 'stopped' ||
                current.state === 'deleting' ||
                current.state === 'deleted' ||
                action.isPending ||
                remove.isPending
              }
              onClick={() => action.mutate('stop')}
            >
              Stop
            </Button>
            <Button
              variant="outline"
              disabled={
                current.state === 'deleting' || current.state === 'deleted' || remove.isPending
              }
              onClick={() => remove.mutate()}
            >
              Delete
            </Button>
            <Link
              to="/sandboxes/$id/execs"
              params={{ id }}
              className={buttonVariants({ variant: 'outline' })}
            >
              Execs
            </Link>
            <Link
              to="/sandboxes/$id/terminal"
              params={{ id }}
              className={buttonVariants({ variant: 'outline' })}
            >
              Terminal
            </Link>
          </div>
        )}
      </div>

      {sandbox.isPending && <p className="text-sm text-muted-foreground">Loading…</p>}
      {error && <p className="text-sm text-destructive">{error.message}</p>}

      {current && (
        <dl className="grid grid-cols-[10rem_1fr] gap-x-4 gap-y-2 rounded-md border border-border p-4 text-sm">
          <dt className="text-muted-foreground">State</dt>
          <dd data-testid="sandbox-state">{current.state}</dd>
          <dt className="text-muted-foreground">Cause</dt>
          <dd>{current.cause ?? '—'}</dd>
          <dt className="text-muted-foreground">Queue position</dt>
          <dd>{current.queue_position ?? '—'}</dd>
          <dt className="text-muted-foreground">Blocked reason</dt>
          <dd>{current.blocked_reason ?? '—'}</dd>
          <dt className="text-muted-foreground">Conditions</dt>
          <dd>{current.conditions.length > 0 ? current.conditions.join(', ') : '—'}</dd>
          <dt className="text-muted-foreground">Runtime</dt>
          <dd>{current.runtime}</dd>
          <dt className="text-muted-foreground">Snapshot</dt>
          <dd className="break-all font-mono text-xs">{current.snapshot}</dd>
          <dt className="text-muted-foreground">CPU</dt>
          <dd>{current.cpu}</dd>
          <dt className="text-muted-foreground">Memory</dt>
          <dd>{(current.memory / 1024 / 1024).toFixed(0)} MiB</dd>
          <dt className="text-muted-foreground">GPU</dt>
          <dd>
            {current.gpu
              ? `${current.gpu.count}${current.gpu.model ? ` × ${current.gpu.model}` : ''}`
              : '—'}
          </dd>
          <dt className="text-muted-foreground">Workspace</dt>
          <dd>
            {current.workspace ? `${current.workspace.id} (${current.workspace.mode})` : '—'}
          </dd>
          <dt className="text-muted-foreground">Node</dt>
          <dd className="font-mono text-xs">{current.node_id ?? '—'}</dd>
          <dt className="text-muted-foreground">Idle stop</dt>
          <dd>{current.idle_stop === 0 ? 'off' : `${current.idle_stop} s`}</dd>
          <dt className="text-muted-foreground">Created</dt>
          <dd>{current.created_at}</dd>
        </dl>
      )}
    </div>
  )
}
