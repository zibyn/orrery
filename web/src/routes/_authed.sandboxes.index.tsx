import { useQuery } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'
import { useState } from 'react'

import { api, type ListResponse, type Sandbox, type User } from '@/api'
import { buttonVariants } from '@/components/ui/button'
import { sandboxListPollInterval } from '@/lib/sandbox'

export const Route = createFileRoute('/_authed/sandboxes/')({
  component: SandboxesPage,
})

function SandboxesPage() {
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

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold">Sandboxes</h1>
          <p className="text-sm text-muted-foreground">
            Sandboxes run your chosen Snapshot on one Node.
          </p>
        </div>
        <div className="flex items-end gap-2">
          {user.is_admin && (
            <label className="flex flex-col gap-1 text-sm">
              <span className="text-muted-foreground">Owner</span>
              <select
                className="h-9 rounded-md border border-input bg-transparent px-3 text-sm"
                value={owner}
                onChange={(e) => setOwner(e.target.value)}
              >
                <option value="">Me ({user.name})</option>
                {users.data?.items.map((item) => (
                  <option key={item.id} value={item.id}>
                    {item.name}
                  </option>
                ))}
              </select>
            </label>
          )}
          <Link to="/sandboxes/new" className={buttonVariants()}>
            New Sandbox
          </Link>
        </div>
      </div>

      {sandboxes.isPending && <p className="text-sm text-muted-foreground">Loading…</p>}
      {sandboxes.error && (
        <p className="text-sm text-destructive">Could not load Sandboxes: {sandboxes.error.message}</p>
      )}
      {users.error && (
        <p className="text-sm text-destructive">Could not load Users: {users.error.message}</p>
      )}

      {sandboxes.data?.items.length === 0 && (
        <p className="text-sm text-muted-foreground">No Sandboxes yet.</p>
      )}

      {sandboxes.data && sandboxes.data.items.length > 0 && (
        <ul className="divide-y divide-border rounded-md border border-border">
          {sandboxes.data.items.map((sandbox) => (
            <li key={sandbox.id} className="flex flex-wrap items-center justify-between gap-3 p-4">
              <div className="flex flex-col gap-1">
                <Link
                  to="/sandboxes/$id"
                  params={{ id: sandbox.id }}
                  className="font-medium hover:underline"
                >
                  {sandbox.name}
                </Link>
                <span className="text-xs text-muted-foreground">
                  {sandbox.cpu} CPU · {(sandbox.memory / 1024 / 1024).toFixed(0)} MiB
                  {sandbox.gpu ? ` · ${sandbox.gpu.count} GPU` : ''}
                  {sandbox.workspace ? ` · workspace ${sandbox.workspace.mode}` : ''}
                </span>
              </div>
              <span
                className="rounded-full border border-border px-2 py-0.5 text-xs font-medium"
                data-testid={`sandbox-state-${sandbox.id}`}
              >
                {sandbox.state}
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
