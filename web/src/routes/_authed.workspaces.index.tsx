import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { HardDrive, Plus } from 'lucide-react'
import { useState } from 'react'

import { api, type ListResponse, type Workspace } from '@/api'
import { EmptyState } from '@/components/empty-state'
import { ListCard, ListRow, listTitleClass } from '@/components/list'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import { useLocale } from '@/lib/i18n'
import { causeLabel, workspaceKindLabel } from '@/lib/labels'
import { workspacesPollInterval } from '@/lib/resources'
import { formatTimestamp } from '@/lib/tokens'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/workspaces/')({
  component: WorkspacesPage,
})

function WorkspacesPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  // One key per page visit: a retried click replays instead of creating twice.
  const [idempotencyKey] = useState(() => crypto.randomUUID())
  const workspaces = useQuery({
    queryKey: ['workspaces'],
    queryFn: () => api<ListResponse<Workspace>>('GET', '/workspaces?limit=200'),
    refetchInterval: (query) => workspacesPollInterval(query.state.data?.items),
  })
  const create = useMutation({
    mutationFn: () =>
      api<Workspace>('POST', '/workspaces', undefined, { 'Idempotency-Key': idempotencyKey }),
    onSuccess: async (workspace) => {
      await queryClient.invalidateQueries({ queryKey: ['workspaces'] })
      void navigate({ to: '/workspaces/$id', params: { id: workspace.id } })
    },
  })

  const count = workspaces.data?.items.length

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title={m.workspaces_title()}
        description={m.workspaces_description()}
        meta={
          count === undefined
            ? undefined
            : count === 1
              ? m.workspaces_count_one({ count })
              : m.workspaces_count_other({ count })
        }
        actions={
          <Button disabled={create.isPending} onClick={() => create.mutate()}>
            <Plus />
            {m.workspaces_new()}
          </Button>
        }
      />

      {create.error && (
        <p className="text-sm text-destructive" role="alert">
          <Problem error={create.error} />
        </p>
      )}
      {workspaces.isPending && (
        <p className="py-8 text-sm text-muted-foreground">{m.common_loading()}</p>
      )}
      {workspaces.error && (
        <p className="text-sm text-destructive" role="alert">
          {m.workspaces_load_error()}: <Problem error={workspaces.error} />
        </p>
      )}

      {workspaces.data?.items.length === 0 && (
        <EmptyState
          icon={<HardDrive />}
          title={m.workspaces_empty_title()}
          description={m.workspaces_empty_description()}
        />
      )}

      {workspaces.data && workspaces.data.items.length > 0 && (
        <ListCard>
          {workspaces.data.items.map((workspace) => (
            <ListRow
              key={workspace.id}
              data-testid={`workspace-${workspace.id}`}
              icon={<HardDrive />}
              title={
                <Link
                  to="/workspaces/$id"
                  params={{ id: workspace.id }}
                  className={`${listTitleClass} font-mono hover:underline`}
                >
                  {workspace.id}
                </Link>
              }
              description={
                <>
                  {workspaceKindLabel(workspace.kind)} ·{' '}
                  {m.common_created({ time: formatTimestamp(workspace.created_at) })}
                  {workspace.cause && <> · {causeLabel(workspace.cause)}</>}
                  {workspace.node_id && (
                    <>
                      {' '}
                      · {m.sandbox_node()} <span className="font-mono">{workspace.node_id.slice(0, 8)}</span>
                    </>
                  )}
                  {workspace.sandbox_id && (
                    <>
                      {' '}
                      · {m.workspace_sandbox()}{' '}
                      <Link
                        to="/sandboxes/$id"
                        params={{ id: workspace.sandbox_id }}
                        className="font-mono hover:underline"
                      >
                        {workspace.sandbox_id.slice(0, 8)}
                      </Link>
                    </>
                  )}
                </>
              }
              trailing={<StatusBadge state={workspace.state} />}
            />
          ))}
        </ListCard>
      )}
    </div>
  )
}
