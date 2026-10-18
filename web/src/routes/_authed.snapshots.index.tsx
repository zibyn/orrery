import { useQuery } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'

import { api, type ListResponse, type Snapshot } from '@/api'
import { snapshotOwnerLabel } from '@/lib/snapshots'

export const Route = createFileRoute('/_authed/snapshots/')({
  component: SnapshotsPage,
})

function SnapshotsPage() {
  const { user } = Route.useRouteContext()
  const snapshots = useQuery({
    queryKey: ['snapshots'],
    queryFn: () => api<ListResponse<Snapshot>>('GET', '/snapshots?limit=200'),
  })

  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="text-2xl font-semibold">Snapshots</h1>
        <p className="text-sm text-muted-foreground">
          Your Snapshots and the deployment's platform Snapshots. Importing, building and
          rollback are done with the CLI.
        </p>
      </div>

      {snapshots.isPending && <p className="text-sm text-muted-foreground">Loading…</p>}
      {snapshots.error && (
        <p className="text-sm text-destructive">
          Could not load Snapshots: {snapshots.error.message}
        </p>
      )}

      {snapshots.data?.items.length === 0 && (
        <p className="text-sm text-muted-foreground">No Snapshots yet.</p>
      )}

      {snapshots.data && snapshots.data.items.length > 0 && (
        <ul className="divide-y divide-border rounded-md border border-border">
          {snapshots.data.items.map((snapshot) => (
            <li
              key={snapshot.id}
              className="flex flex-wrap items-center justify-between gap-3 p-4"
              data-testid={`snapshot-${snapshot.id}`}
            >
              <div className="flex flex-col gap-1">
                <Link
                  to="/snapshots/$id"
                  params={{ id: snapshot.id }}
                  className="font-medium hover:underline"
                >
                  {snapshot.name}
                </Link>
                <span className="text-xs text-muted-foreground">
                  {snapshotOwnerLabel(snapshot, user.id)} · created {snapshot.created_at ?? '—'}
                </span>
              </div>
              <div className="flex flex-col items-end gap-1">
                <span className="text-xs text-muted-foreground">current version</span>
                <span className="font-mono text-xs">{snapshot.current_version_id ?? '—'}</span>
              </div>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
