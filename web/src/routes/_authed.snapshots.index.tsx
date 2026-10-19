import { useQuery } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'
import { Layers } from 'lucide-react'

import { api, type ListResponse, type Snapshot } from '@/api'
import { EmptyState } from '@/components/empty-state'
import { PageHeader } from '@/components/page-header'
import { snapshotOwnerLabel } from '@/lib/snapshots'
import { formatTimestamp } from '@/lib/tokens'

export const Route = createFileRoute('/_authed/snapshots/')({
  component: SnapshotsPage,
})

function SnapshotsPage() {
  const { user } = Route.useRouteContext()
  const snapshots = useQuery({
    queryKey: ['snapshots'],
    queryFn: () => api<ListResponse<Snapshot>>('GET', '/snapshots?limit=200'),
  })

  const count = snapshots.data?.items.length

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="Snapshots"
        description="Your Snapshots and the deployment's platform Snapshots. Importing, building and rollback are done with the CLI."
        meta={count === undefined ? undefined : `${count} ${count === 1 ? 'snapshot' : 'snapshots'}`}
      />

      {snapshots.isPending && (
        <p className="py-8 text-sm text-muted-foreground">Loading snapshots…</p>
      )}
      {snapshots.error && (
        <p className="text-sm text-destructive" role="alert">
          Could not load Snapshots: {snapshots.error.message}
        </p>
      )}

      {snapshots.data?.items.length === 0 && (
        <EmptyState
          icon={<Layers />}
          title="No Snapshots yet"
          description="Import a Snapshot by digest or build one with BuildKit. Both are available with the CLI."
        />
      )}

      {snapshots.data && snapshots.data.items.length > 0 && (
        <div className="divide-y divide-border overflow-hidden rounded-xl border border-border bg-card">
          {snapshots.data.items.map((snapshot) => (
            <div
              key={snapshot.id}
              className="flex items-center gap-3 px-4 py-3 transition-colors hover:bg-muted/40"
              data-testid={`snapshot-${snapshot.id}`}
            >
              <span className="flex size-8 shrink-0 items-center justify-center rounded-lg border border-border bg-muted/50 text-muted-foreground">
                <Layers className="size-4" />
              </span>
              <div className="min-w-0 flex-1">
                <Link
                  to="/snapshots/$id"
                  params={{ id: snapshot.id }}
                  className="text-sm font-medium hover:underline"
                >
                  {snapshot.name}
                </Link>
                <p className="truncate text-xs text-muted-foreground">
                  {snapshotOwnerLabel(snapshot, user.id)} · created{' '}
                  {formatTimestamp(snapshot.created_at)}
                </p>
              </div>
              <div className="hidden min-w-0 flex-col items-end gap-0.5 sm:flex">
                <span className="text-[11px] text-muted-foreground">current version</span>
                <span className="max-w-48 truncate font-mono text-xs">
                  {snapshot.current_version_id ?? '—'}
                </span>
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
