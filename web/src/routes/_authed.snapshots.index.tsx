import { useQuery } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'
import { Layers, Plus } from 'lucide-react'

import { listAll, type Snapshot } from '@/api'
import { PageHeader } from '@/components/page-header'
import { buttonVariants } from '@/components/ui/button'
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
import { snapshotOwnerLabel } from '@/lib/snapshots'
import { formatTimestamp } from '@/lib/tokens'
import { cn } from '@/lib/utils'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/snapshots/')({
  component: SnapshotsPage,
})

function SnapshotsPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const { user } = Route.useRouteContext()
  const snapshots = useQuery({
    queryKey: ['snapshots'],
    queryFn: () => listAll<Snapshot>('/snapshots'),
  })

  const count = snapshots.data?.length

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
        actions={
          <Link
            to="/snapshots/new"
            className={cn(buttonVariants())}
          >
            <Plus data-icon="inline-start" />
            {m.snapshots_new()}
          </Link>
        }
      />

      {snapshots.isPending && (
        <div className="flex items-center gap-2 py-8 text-sm text-muted-foreground">
          <Spinner />
          {m.snapshots_loading()}
        </div>
      )}

      {snapshots.data?.length === 0 && (
        <Empty className="border">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <Layers />
            </EmptyMedia>
            <EmptyTitle>{m.snapshots_empty_title()}</EmptyTitle>
            <EmptyDescription>{m.snapshots_empty_description()}</EmptyDescription>
          </EmptyHeader>
        </Empty>
      )}

      {snapshots.data && snapshots.data.length > 0 && (
        <ItemGroup>
          {snapshots.data.map((snapshot) => (
            // The row itself is the link, so the listitem role goes on a wrapper.
            <div key={snapshot.id} role="listitem">
              <Item
                variant="outline"
                data-testid={`snapshot-${snapshot.id}`}
                render={<Link to="/snapshots/$id" params={{ id: snapshot.id }} />}
              >
                <ItemMedia variant="icon">
                  <Layers />
                </ItemMedia>
                <ItemContent>
                  <ItemTitle>{snapshot.name}</ItemTitle>
                  <ItemDescription>
                    {snapshotOwnerLabel(snapshot, user.id)} ·{' '}
                    {m.common_created({ time: formatTimestamp(snapshot.created_at) })}
                  </ItemDescription>
                </ItemContent>
                <ItemActions className="hidden flex-col items-end gap-0.5 sm:flex">
                  <span className="text-xs text-muted-foreground">
                    {m.snapshots_current_version()}
                  </span>
                  {snapshot.current_version_id ? (
                    <span className="font-mono text-xs">{snapshot.current_version_id.slice(0, 8)}</span>
                  ) : (
                    <span className="text-xs text-muted-foreground">{m.snapshot_no_current()}</span>
                  )}
                </ItemActions>
              </Item>
            </div>
          ))}
        </ItemGroup>
      )}
    </div>
  )
}
