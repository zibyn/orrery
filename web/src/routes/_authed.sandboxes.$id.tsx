import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { ArrowLeft, Play, ScrollText, Square, Terminal, Trash } from 'lucide-react'
import type { ReactNode } from 'react'

import { api, type Sandbox } from '@/api'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { SandboxFiles } from '@/components/sandbox-files'
import { SandboxPorts } from '@/components/sandbox-ports'
import { StatusBadge } from '@/components/status-badge'
import { Button, buttonVariants } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useLocale } from '@/lib/i18n'
import {
  causeLabel,
  conditionLabel,
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
  const error = sandbox.error ?? action.error ?? remove.error

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        above={
          <Link
            to="/sandboxes"
            className="inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
          >
            <ArrowLeft className="size-3.5" />
            {m.sandbox_back()}
          </Link>
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
              <Link
                to="/sandboxes/$id/terminal"
                params={{ id }}
                className={buttonVariants({ variant: 'outline' })}
              >
                <Terminal />
                {m.sandbox_terminal()}
              </Link>
              <Button
                variant="outline"
                disabled={current.state !== 'stopped' || action.isPending || remove.isPending}
                onClick={() => action.mutate('start')}
              >
                <Play />
                {m.sandbox_start()}
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
                <Square />
                {m.sandbox_stop()}
              </Button>
              <Button
                variant="destructive"
                disabled={
                  current.state === 'deleting' || current.state === 'deleted' || remove.isPending
                }
                onClick={() => remove.mutate()}
              >
                <Trash />
                {m.sandbox_delete()}
              </Button>
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
        <div className="grid gap-4 lg:grid-cols-3">
          <Card className="lg:col-span-2">
            <CardHeader>
              <CardTitle>{m.sandbox_status()}</CardTitle>
              <CardDescription>{m.sandbox_status_description()}</CardDescription>
            </CardHeader>
            <CardContent>
              <dl className="grid grid-cols-2 gap-x-6 gap-y-4 sm:grid-cols-3">
                <Detail label={m.sandbox_state()}>
                  <StatusBadge state={current.state} />
                </Detail>
                <Detail label={m.sandbox_cause()}>
                  {current.cause ? causeLabel(current.cause) : '—'}
                </Detail>
                <Detail label={m.sandbox_queue_position()}>{current.queue_position ?? '—'}</Detail>
                <Detail label={m.sandbox_blocked_reason()}>
                  {current.blocked_reason ?? '—'}
                </Detail>
                <Detail label={m.sandbox_conditions()}>
                  {current.conditions.length > 0
                    ? current.conditions.map(conditionLabel).join(', ')
                    : '—'}
                </Detail>
                <Detail label={m.sandbox_node()}>
                  <span className="font-mono text-xs">{current.node_id ?? '—'}</span>
                </Detail>
              </dl>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>{m.sandbox_configuration()}</CardTitle>
              <CardDescription>{m.sandbox_configuration_description()}</CardDescription>
            </CardHeader>
            <CardContent>
              <dl className="grid grid-cols-2 gap-x-6 gap-y-4">
                <Detail label="CPU">{current.cpu}</Detail>
                <Detail label={m.sandbox_memory()}>
                  {(current.memory / 1024 / 1024).toFixed(0)} MiB
                </Detail>
                <Detail label="GPU">
                  {current.gpu
                    ? `${current.gpu.count}${current.gpu.model ? ` × ${current.gpu.model}` : ''}`
                    : '—'}
                </Detail>
                <Detail label={m.sandbox_idle_stop()}>
                  {current.idle_stop === 0
                    ? m.common_off()
                    : `${current.idle_stop} s`}
                </Detail>
                <Detail label="Workspace">
                  {current.workspace
                    ? `${current.workspace.id} (${workspaceModeLabel(current.workspace.mode)})`
                    : '—'}
                </Detail>
                <Detail label="Runtime">{runtimeLabel(current.runtime)}</Detail>
                <div className="col-span-2">
                  <Detail label="Snapshot">
                    <span className="font-mono text-xs break-all">{current.snapshot}</span>
                  </Detail>
                </div>
              </dl>
            </CardContent>
          </Card>
        </div>
      )}

      {current?.state === 'ready' && (
        <div className="grid gap-4 lg:grid-cols-2">
          <SandboxFiles sandboxId={id} />
          <SandboxPorts sandboxId={id} />
        </div>
      )}
    </div>
  )
}
