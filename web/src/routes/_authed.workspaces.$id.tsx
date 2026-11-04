import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { Plus, Trash } from 'lucide-react'
import type { ReactNode } from 'react'

import { api, type Workspace } from '@/api'
import { Breadcrumbs } from '@/components/breadcrumbs'
import { ConfirmButton } from '@/components/confirm-button'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { StatusBadge } from '@/components/status-badge'
import { buttonVariants } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { useLocale } from '@/lib/i18n'
import { causeLabel, conditionLabel, workspaceKindLabel } from '@/lib/labels'
import { workspacePollInterval } from '@/lib/resources'
import { formatTimestamp } from '@/lib/tokens'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/workspaces/$id')({
  component: WorkspaceDetailPage,
})

function Detail({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex min-w-0 flex-col gap-0.5">
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="min-w-0 text-sm">{children}</dd>
    </div>
  )
}

function WorkspaceDetailPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const { id } = Route.useParams()
  const navigate = useNavigate()
  const queryClient = useQueryClient()

  const workspace = useQuery({
    queryKey: ['workspace', id],
    queryFn: () => api<Workspace>('GET', `/workspaces/${id}`),
    refetchInterval: (query) => workspacePollInterval(query.state.data?.state),
  })
  const remove = useMutation({
    mutationFn: () => api<Workspace>('DELETE', `/workspaces/${id}`),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['workspaces'] })
      void navigate({ to: '/workspaces' })
    },
  })
  const current = workspace.data
  // Ephemeral Workspaces go away with their Sandbox (ADR-0018).
  const deletable =
    current?.kind === 'persistent' && current.state !== 'deleting' && current.state !== 'deleted'

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        above={
          <Breadcrumbs items={[{ label: m.nav_workspaces(), to: '/workspaces' }, { label: id }]} />
        }
        title={<span className="font-mono">{id}</span>}
        actions={
          current && (
            <>
              {current.kind === 'persistent' && current.state === 'ready' && !current.sandbox_id && (
                <Link
                  to="/sandboxes/new"
                  search={{ workspace: id }}
                  className={buttonVariants({ variant: 'outline' })}
                >
                  <Plus />
                  {m.workspace_new_sandbox()}
                </Link>
              )}
              {deletable && (
                <ConfirmButton
                  variant="outline"
                  title={m.workspace_delete_confirm({ id: id.slice(0, 8) })}
                  description={m.common_irreversible()}
                  disabled={remove.isPending}
                  onConfirm={() => remove.mutate()}
                >
                  <Trash />
                  {remove.isPending ? m.snapshot_deleting() : m.common_delete()}
                </ConfirmButton>
              )}
            </>
          )
        }
      />

      {remove.error && (
        <p className="text-sm text-destructive" role="alert">
          <Problem error={remove.error} />
        </p>
      )}
      {workspace.isPending && (
        <p className="py-8 text-sm text-muted-foreground">{m.common_loading()}</p>
      )}
      {workspace.error && (
        <p className="text-sm text-destructive" role="alert">
          {m.workspace_load_error()}: <Problem error={workspace.error} />
        </p>
      )}

      {current && (
        <Card>
          <CardContent>
            <dl className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
              <Detail label={m.workspace_kind()}>{workspaceKindLabel(current.kind)}</Detail>
              <Detail label={m.sandbox_state()}>
                <StatusBadge state={current.state} data-testid="workspace-state" />
                {current.unavailable && (
                  <span className="ml-2 text-xs text-muted-foreground">
                    {m.workspace_node_unreachable()}
                  </span>
                )}
              </Detail>
              <Detail label={m.sandbox_cause()}>
                {current.cause ? causeLabel(current.cause) : '—'}
              </Detail>
              <Detail label={m.sandbox_node()}>
                <span className="font-mono text-xs">{current.node_id ?? '—'}</span>
              </Detail>
              <Detail label={m.workspace_sandbox()}>
                {current.sandbox_id ? (
                  <Link
                    to="/sandboxes/$id"
                    params={{ id: current.sandbox_id }}
                    className="font-mono text-xs hover:underline"
                    data-testid="workspace-sandbox"
                  >
                    {current.sandbox_id}
                  </Link>
                ) : (
                  '—'
                )}
              </Detail>
              <Detail label={m.sandbox_conditions()}>
                {current.conditions.length > 0
                  ? current.conditions.map(conditionLabel).join(', ')
                  : '—'}
              </Detail>
              <Detail label={m.snapshot_created()}>{formatTimestamp(current.created_at)}</Detail>
            </dl>
            <p className="mt-4 text-xs text-muted-foreground">{m.workspace_files_hint()}</p>
          </CardContent>
        </Card>
      )}
    </div>
  )
}
