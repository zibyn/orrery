import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { HardDrive, Plus } from 'lucide-react'
import { useState } from 'react'

import { api, type ListResponse, type Workspace } from '@/api'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { StatusBadge } from '@/components/status-badge'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from '@/components/ui/empty'
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
            <Plus data-icon="inline-start" />
            {m.workspaces_new()}
          </Button>
        }
      />

      {create.error && (
        <Alert variant="destructive">
          <AlertDescription>
            <Problem error={create.error} />
          </AlertDescription>
        </Alert>
      )}
      {workspaces.isPending && (
        <div className="flex items-center gap-2 py-8 text-sm text-muted-foreground">
          <Spinner />
          {m.common_loading()}
        </div>
      )}
      {workspaces.error && (
        <Alert variant="destructive">
          <AlertDescription>
            {m.workspaces_load_error()}: <Problem error={workspaces.error} />
          </AlertDescription>
        </Alert>
      )}

      {workspaces.data?.items.length === 0 && (
        <Empty className="border">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <HardDrive />
            </EmptyMedia>
            <EmptyTitle>{m.workspaces_empty_title()}</EmptyTitle>
            <EmptyDescription>{m.workspaces_empty_description()}</EmptyDescription>
          </EmptyHeader>
        </Empty>
      )}

      {workspaces.data && workspaces.data.items.length > 0 && (
        <ItemGroup>
          {workspaces.data.items.map((workspace) => (
            // The row itself is the link, so the listitem role goes on a wrapper.
            <div key={workspace.id} role="listitem">
              <Item
                variant="outline"
                data-testid={`workspace-${workspace.id}`}
                render={<Link to="/workspaces/$id" params={{ id: workspace.id }} />}
              >
                <ItemMedia variant="icon">
                  <HardDrive />
                </ItemMedia>
                <ItemContent>
                  <ItemTitle className="font-mono">{workspace.id}</ItemTitle>
                  <ItemDescription>
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
                        <span className="font-mono">{workspace.sandbox_id.slice(0, 8)}</span>
                      </>
                    )}
                  </ItemDescription>
                </ItemContent>
                <ItemActions>
                  <StatusBadge state={workspace.state} />
                </ItemActions>
              </Item>
            </div>
          ))}
        </ItemGroup>
      )}
    </div>
  )
}
