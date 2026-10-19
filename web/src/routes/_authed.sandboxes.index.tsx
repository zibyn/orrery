import { useQuery } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'
import { Box, ChevronRight, Plus } from 'lucide-react'
import { useState } from 'react'

import { api, type ListResponse, type Sandbox, type User } from '@/api'
import { EmptyState } from '@/components/empty-state'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { StatusBadge } from '@/components/status-badge'
import { buttonVariants } from '@/components/ui/button'
import { useLocale } from '@/lib/i18n'
import { workspaceModeLabel } from '@/lib/labels'
import { sandboxListPollInterval } from '@/lib/sandbox'
import { selectClass } from '@/lib/utils'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/sandboxes/')({
  component: SandboxesPage,
})

function SandboxesPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const { user } = Route.useRouteContext()
  // An admin's list defaults to their own Sandboxes: without `owner` the API
  // already answers with the caller's, so '' means "me" (api.md §8).
  const [owner, setOwner] = useState('')

  const sandboxes = useQuery({
    queryKey: ['sandboxes', owner],
    queryFn: () =>
      api<ListResponse<Sandbox>>(
        'GET',
        `/sandboxes${owner ? `?owner=${encodeURIComponent(owner)}` : ''}`,
      ),
    refetchInterval: (query) => sandboxListPollInterval(query.state.data?.items),
  })
  const users = useQuery({
    queryKey: ['users'],
    queryFn: () => api<ListResponse<User>>('GET', '/users'),
    enabled: user.is_admin,
  })

  const count = sandboxes.data?.items.length

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title={m.sandboxes_title()}
        description={m.sandboxes_description()}
        meta={
          count === undefined
            ? undefined
            : count === 1
              ? m.sandboxes_count_one({ count })
              : m.sandboxes_count_other({ count })
        }
        actions={
          <>
            {user.is_admin && (
              <label className="flex items-center gap-2 text-xs text-muted-foreground">
                {m.sandboxes_owner()}
                <select
                  className={`${selectClass} w-auto min-w-32`}
                  value={owner}
                  onChange={(e) => setOwner(e.target.value)}
                >
                  <option value="">{m.sandboxes_owner_me({ name: user.name })}</option>
                  {users.data?.items.map((item) => (
                    <option key={item.id} value={item.id}>
                      {item.name}
                    </option>
                  ))}
                </select>
              </label>
            )}
            <Link to="/sandboxes/new" className={buttonVariants()}>
              <Plus />
              {m.sandboxes_new()}
            </Link>
          </>
        }
      />

      {sandboxes.isPending && (
        <p className="py-8 text-sm text-muted-foreground">{m.sandboxes_loading()}</p>
      )}
      {sandboxes.error && (
        <p className="text-sm text-destructive" role="alert">
          {m.sandboxes_load_error()}: <Problem error={sandboxes.error} />
        </p>
      )}
      {users.error && (
        <p className="text-sm text-destructive" role="alert">
          {m.users_load_error()}: <Problem error={users.error} />
        </p>
      )}

      {sandboxes.data?.items.length === 0 && (
        <EmptyState
          icon={<Box />}
          title={m.sandboxes_empty_title()}
          description={m.sandboxes_empty_description()}
        />
      )}

      {sandboxes.data && sandboxes.data.items.length > 0 && (
        <div className="divide-y divide-border overflow-hidden rounded-xl border border-border bg-card">
          {sandboxes.data.items.map((sandbox) => (
            <div
              key={sandbox.id}
              className="group flex items-center gap-3 px-4 py-3 transition-colors hover:bg-muted/40"
            >
              <span className="flex size-8 shrink-0 items-center justify-center rounded-lg border border-border bg-muted/50 text-muted-foreground">
                <Box className="size-4" />
              </span>
              <div className="min-w-0 flex-1">
                <Link
                  to="/sandboxes/$id"
                  params={{ id: sandbox.id }}
                  className="text-sm font-medium hover:underline"
                >
                  {sandbox.name}
                </Link>
                <p className="truncate text-xs text-muted-foreground">
                  {sandbox.cpu} CPU · {(sandbox.memory / 1024 / 1024).toFixed(0)} MiB
                  {sandbox.gpu ? ` · ${sandbox.gpu.count} GPU` : ''}
                  {sandbox.workspace
                    ? ` · workspace ${workspaceModeLabel(sandbox.workspace.mode)}`
                    : ''}
                  {sandbox.node_id ? ` · ${sandbox.node_id}` : ''}
                </p>
              </div>
              <StatusBadge
                state={sandbox.state}
                data-testid={`sandbox-state-${sandbox.id}`}
              />
              <ChevronRight className="size-4 shrink-0 text-muted-foreground/50 transition-transform group-hover:translate-x-0.5" />
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
