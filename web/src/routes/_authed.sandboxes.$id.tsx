import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { Play, ScrollText, Square, Terminal, Trash } from 'lucide-react'
import { type ReactNode, useState } from 'react'

import { api, listAll, type Sandbox, type Snapshot } from '@/api'
import { PageHeader } from '@/components/page-header'
import { SandboxFiles } from '@/components/sandbox-files'
import { SandboxPorts } from '@/components/sandbox-ports'
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
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from '@/components/ui/breadcrumb'
import { Button, buttonVariants } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Spinner } from '@/components/ui/spinner'
import { useLocale } from '@/lib/i18n'
import {
  causeLabel,
  idleStopLabel,
  runtimeLabel,
  workspaceModeLabel,
} from '@/lib/labels'
import { sandboxPollInterval } from '@/lib/sandbox'
import { snapshotForRef } from '@/lib/snapshots'
import { formatTimestamp } from '@/lib/tokens'
import { cn } from '@/lib/utils'
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
  const [confirmOpen, setConfirmOpen] = useState(false)
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const { id } = Route.useParams()
  const navigate = useNavigate()
  const queryClient = useQueryClient()

  const sandbox = useQuery({
    queryKey: ['sandbox', id],
    queryFn: () => api<Sandbox>('GET', `/sandboxes/${id}`),
    refetchInterval: (query) => sandboxPollInterval(query.state.data?.state),
  })

  const snapshots = useQuery({
    queryKey: ['snapshots'],
    queryFn: () => listAll<Snapshot>('/snapshots'),
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

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        above={
          <Breadcrumb aria-label={m.common_breadcrumb()}>
            <BreadcrumbList>
              <BreadcrumbItem>
                <BreadcrumbLink render={<Link to="/sandboxes" />}>{m.nav_sandboxes()}</BreadcrumbLink>
              </BreadcrumbItem>
              <BreadcrumbSeparator />
              <BreadcrumbItem>
                <BreadcrumbPage>{current?.name ?? id}</BreadcrumbPage>
              </BreadcrumbItem>
            </BreadcrumbList>
          </Breadcrumb>
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
                className={cn(buttonVariants({ variant: 'outline' }))}
              >
                <ScrollText data-icon="inline-start" />
                {m.sandbox_execs()}
              </Link>
              {current.state === 'ready' ? (
                <Link
                  to="/sandboxes/$id/terminal"
                  params={{ id }}
                  className={cn(buttonVariants({ variant: 'outline' }))}
                >
                  <Terminal data-icon="inline-start" />
                  {m.sandbox_terminal()}
                </Link>
              ) : (
                <Button variant="outline" disabled title={m.sandbox_terminal_not_ready()}>
                  <Terminal data-icon="inline-start" />
                  {m.sandbox_terminal()}
                </Button>
              )}
              {current.state === 'stopped' ? (
                <Button
                  variant="outline"
                  disabled={action.isPending || remove.isPending}
                  onClick={() => action.mutate('start')}
                >
                  <Play data-icon="inline-start" />
                  {m.sandbox_start()}
                </Button>
              ) : (
                <Button
                  variant="outline"
                  disabled={busy || action.isPending || remove.isPending}
                  onClick={() => action.mutate('stop')}
                >
                  <Square data-icon="inline-start" />
                  {m.sandbox_stop()}
                </Button>
              )}
              <AlertDialog open={confirmOpen} onOpenChange={setConfirmOpen}>
                <AlertDialogTrigger
                  render={<Button variant="destructive" disabled={busy || remove.isPending} />}
                >
                  <Trash data-icon="inline-start" />
                  {m.sandbox_delete()}
                </AlertDialogTrigger>
                <AlertDialogContent>
                  <AlertDialogHeader>
                    <AlertDialogTitle>{m.sandbox_delete_title({ name: current.name })}</AlertDialogTitle>
                    <AlertDialogDescription>{m.sandbox_delete_description()}</AlertDialogDescription>
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
          ) : undefined
        }
      />

      {sandbox.isPending && (
        <div className="flex items-center gap-2 py-8 text-sm text-muted-foreground">
          <Spinner />
          {m.sandbox_loading()}
        </div>
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
                  {current.conditions.join(', ')}
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
              <SnapshotDetail refName={current.snapshot} snapshots={snapshots.data ?? []} />
            </dl>
          </CardContent>
        </Card>
      )}

      {current && current.state !== 'ready' && !busy && (
        <p className="text-sm text-muted-foreground">
          {current.state === 'stopped' ? m.sandbox_stopped_hint() : m.sandbox_ready_hint()}
        </p>
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

function SnapshotDetail({ refName, snapshots }: { refName: string; snapshots: Snapshot[] }) {
  const { snapshot, label, digest } = snapshotForRef(refName, snapshots)
  return (
    <Detail label={m.label_snapshot()}>
      <span title={refName}>
        {snapshot ? (
          <Link to="/snapshots/$id" params={{ id: snapshot.id }} className="hover:underline">
            {label}
          </Link>
        ) : (
          <span className="break-all">{label}</span>
        )}
        {digest && (
          <span className="ml-1.5 font-mono text-xs text-muted-foreground">
            {digest.slice(0, 19)}
          </span>
        )}
      </span>
    </Detail>
  )
}
