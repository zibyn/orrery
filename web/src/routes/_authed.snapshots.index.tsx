import { useQuery } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'
import { Layers } from 'lucide-react'

import { api, type ListResponse, type Snapshot } from '@/api'
import { EmptyState } from '@/components/empty-state'
import { ListCard, ListRow, listTitleClass } from '@/components/list'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { useLocale } from '@/lib/i18n'
import { snapshotOwnerLabel } from '@/lib/snapshots'
import { formatTimestamp } from '@/lib/tokens'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/snapshots/')({
  component: SnapshotsPage,
})

function SnapshotsPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const { user } = Route.useRouteContext()
  const snapshots = useQuery({
    queryKey: ['snapshots'],
    queryFn: () => api<ListResponse<Snapshot>>('GET', '/snapshots?limit=200'),
  })

  const count = snapshots.data?.items.length

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title={m.snapshots_title()}
        description={m.snapshots_description()}
        meta={
          count === undefined
            ? undefined
            : count === 1
              ? m.snapshots_count_one({ count })
              : m.snapshots_count_other({ count })
        }
      />

      {snapshots.isPending && (
        <p className="py-8 text-sm text-muted-foreground">{m.snapshots_loading()}</p>
      )}
      {snapshots.error && (
        <p className="text-sm text-destructive" role="alert">
          {m.snapshots_load_error()}: <Problem error={snapshots.error} />
        </p>
      )}

      {snapshots.data?.items.length === 0 && (
        <EmptyState
          icon={<Layers />}
          title={m.snapshots_empty_title()}
          description={m.snapshots_empty_description()}
        />
      )}

      {snapshots.data && snapshots.data.items.length > 0 && (
        <ListCard>
          {snapshots.data.items.map((snapshot) => (
            <ListRow
              key={snapshot.id}
              data-testid={`snapshot-${snapshot.id}`}
              icon={<Layers />}
              title={
                <Link
                  to="/snapshots/$id"
                  params={{ id: snapshot.id }}
                  className={`${listTitleClass} hover:underline`}
                >
                  {snapshot.name}
                </Link>
              }
              description={
                <>
                  {snapshotOwnerLabel(snapshot, user.id)} ·{' '}
                  {m.common_created({ time: formatTimestamp(snapshot.created_at) })}
                </>
              }
              trailing={
                <div className="hidden min-w-0 flex-col items-end gap-0.5 sm:flex">
                  <span className="text-[11px] text-muted-foreground">
                    {m.snapshots_current_version()}
                  </span>
                  <span className="max-w-48 truncate font-mono text-xs">
                    {snapshot.current_version_id ?? '—'}
                  </span>
                </div>
              }
            />
          ))}
        </ListCard>
      )}
    </div>
  )
}
