import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { Play, ScrollText, Square, Terminal, Trash } from 'lucide-react'
import type { ReactNode } from 'react'

import { api, type Sandbox } from '@/api'
import { Breadcrumbs } from '@/components/breadcrumbs'
import { ConfirmButton } from '@/components/confirm-button'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { SandboxFiles } from '@/components/sandbox-files'
import { SandboxPorts } from '@/components/sandbox-ports'
import { StatusBadge } from '@/components/status-badge'
import { Button, buttonVariants } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { useLocale } from '@/lib/i18n'
import {
  causeLabel,
  conditionLabel,
  idleStopLabel,
  runtimeLabel,
  workspaceModeLabel,
} from '@/lib/labels'
import { sandboxPollInterval } from '@/lib/sandbox'
import { formatTimestamp } from '@/lib/tokens'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/sandboxes/$id')({
  component: SandboxDetailPage,
})

function Detail({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex min-w-0 flex-col gap-0.5">
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="min-w-0 text-sm">{children}</dd>
    </div>
  )
}

function SandboxDetailPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
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
  const busy = current?.state === 'stopping' || current?.state === 'deleting' || current?.state === 'deleted'
  const error = sandbox.error ?? action.error ?? remove.error

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        above={
          <Breadcrumbs
            items={[
              { label: m.nav_sandboxes(), to: '/sandboxes' },
              { label: current?.name ?? id },
            ]}
          />
        }
        title={current?.name ?? id}
        meta={
          current ? (
            <>
              <StatusBadge state={current.state} data-testid="sandbox-state" />
              <span>{runtimeLabel(current.runtime)}</span>
              <span aria-hidden="true">·</span>
              <span>
                {m.common_created({ time: formatTimestamp(current.created_at) })}
              </span>
            </>
          ) : undefined
        }
        actions={
          current ? (
            <>
              <Link
                to="/sandboxes/$id/execs"
                params={{ id }}
                className={buttonVariants({ variant: 'outline' })}
              >
                <ScrollText />
                {m.sandbox_execs()}
              </Link>
              {current.state === 'ready' ? (
                <Link
                  to="/sandboxes/$id/terminal"
                  params={{ id }}
                  className={buttonVariants({ variant: 'outline' })}
                >
                  <Terminal />
                  {m.sandbox_terminal()}
                </Link>
              ) : (
                <Button variant="outline" disabled title={m.sandbox_terminal_not_ready()}>
                  <Terminal />
                  {m.sandbox_terminal()}
                </Button>
              )}
              {current.state === 'stopped' ? (
                <Button
                  variant="outline"
                  disabled={action.isPending || remove.isPending}
                  onClick={() => action.mutate('start')}
                >
                  <Play />
                  {m.sandbox_start()}
                </Button>
              ) : (
                <Button
                  variant="outline"
                  disabled={busy || action.isPending || remove.isPending}
                  onClick={() => action.mutate('stop')}
                >
                  <Square />
                  {m.sandbox_stop()}
                </Button>
              )}
              <ConfirmButton
                variant="destructive"
                title={m.sandbox_delete_title({ name: current.name })}
                description={m.sandbox_delete_description()}
                disabled={busy || remove.isPending}
                onConfirm={() => remove.mutate()}
              >
                <Trash />
                {m.sandbox_delete()}
              </ConfirmButton>
            </>
          ) : undefined
        }
      />

      {sandbox.isPending && (
        <p className="py-8 text-sm text-muted-foreground">{m.sandbox_loading()}</p>
      )}
      {error && (
        <p className="text-sm text-destructive" role="alert">
          <Problem error={error} />
        </p>
      )}

      {current && (
        <Card>
          <CardHeader>
            <CardTitle>{m.sandbox_details()}</CardTitle>
          </CardHeader>
          <CardContent>
            <dl className="grid grid-cols-2 gap-x-6 gap-y-4 sm:grid-cols-3 lg:grid-cols-4">
              {current.cause && (
                <Detail label={m.sandbox_cause()}>{causeLabel(current.cause)}</Detail>
              )}
              {current.queue_position != null && (
                <Detail label={m.sandbox_queue_position()}>{current.queue_position}</Detail>
              )}
              {current.blocked_reason && (
                <Detail label={m.sandbox_blocked_reason()}>{current.blocked_reason}</Detail>
              )}
              {current.conditions.length > 0 && (
                <Detail label={m.sandbox_conditions()}>
                  {current.conditions.map(conditionLabel).join(', ')}
                </Detail>
              )}
              <Detail label="CPU">{current.cpu}</Detail>
              <Detail label={m.sandbox_memory()}>
                {(current.memory / 1024 / 1024).toFixed(0)} MiB
              </Detail>
              <Detail label="GPU">
                {current.gpu
                  ? `${current.gpu.count}${current.gpu.model ? ` × ${current.gpu.model}` : ''}`
                  : '—'}
              </Detail>
              <Detail label={m.sandbox_idle_stop()}>{idleStopLabel(current.idle_stop)}</Detail>
              <Detail label={m.label_workspace()}>
                {current.workspace ? (
                  <Link
                    to="/workspaces/$id"
                    params={{ id: current.workspace.id }}
                    className="hover:underline"
                  >
                    <span className="font-mono text-xs">{current.workspace.id.slice(0, 8)}</span>{' '}
                    ({workspaceModeLabel(current.workspace.mode)})
                  </Link>
                ) : (
                  '—'
                )}
              </Detail>
              {current.node_id && (
                <Detail label={m.sandbox_node()}>
                  <span className="font-mono text-xs" title={current.node_id}>
                    {current.node_id.slice(0, 8)}
                  </span>
                </Detail>
              )}
              <div className="col-span-2">
                <Detail label={m.label_snapshot()}>
                  <span className="font-mono text-xs break-all">{current.snapshot}</span>
                </Detail>
              </div>
            </dl>
          </CardContent>
        </Card>
      )}

      {current && current.state !== 'ready' && !busy && (
        <p className="text-sm text-muted-foreground">{m.sandbox_ready_hint()}</p>
      )}

      {current?.state === 'ready' && (
        <div className="grid gap-4 lg:grid-cols-2">
          <SandboxFiles sandboxId={id} workspaceId={current.workspace?.id} />
          <SandboxPorts sandboxId={id} />
        </div>
      )}
    </div>
  )
}
