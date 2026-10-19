import { useQuery } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'
import { ArrowLeft, Layers, ScrollText } from 'lucide-react'

import { api, type ListResponse, type Snapshot, type SnapshotVersion } from '@/api'
import { BuildLog } from '@/components/build-log'
import { EmptyState } from '@/components/empty-state'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { StatusBadge } from '@/components/status-badge'
import { Badge } from '@/components/ui/badge'
import { buttonVariants } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { useLocale } from '@/lib/i18n'
import { causeLabel, sourceLabel, stateLabel } from '@/lib/labels'
import {
  buildLogMode,
  isSnapshotVersionTerminal,
  snapshotOwnerLabel,
  snapshotVersionsPollInterval,
  versionIsCurrent,
} from '@/lib/snapshots'
import { formatTimestamp } from '@/lib/tokens'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/snapshots/$id')({
  validateSearch: (search: Record<string, unknown>): { version?: string } => ({
    version: typeof search.version === 'string' ? search.version : undefined,
  }),
  component: SnapshotDetailPage,
})

function SnapshotDetailPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const { id } = Route.useParams()
  const { user } = Route.useRouteContext()
  const { version: selectedId } = Route.useSearch()

  const versions = useQuery({
    queryKey: ['snapshot-versions', id],
    queryFn: () => api<ListResponse<SnapshotVersion>>('GET', `/snapshots/${id}/versions?limit=200`),
    refetchInterval: (query) => snapshotVersionsPollInterval(query.state.data?.items),
  })
  const snapshot = useQuery({
    queryKey: ['snapshot', id],
    queryFn: () => api<Snapshot>('GET', `/snapshots/${id}`),
    // The current pointer moves when a build becomes ready.
    refetchInterval: versions.data?.items.some(
      (version) => !isSnapshotVersionTerminal(version.state),
    )
      ? 2000
      : false,
  })

  const current = snapshot.data
  const selected = versions.data?.items.find((item) => item.id === selectedId)
  const logMode = selected ? buildLogMode(selected) : 'none'

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        above={
          <Link
            to="/snapshots"
            className="inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
          >
            <ArrowLeft className="size-3.5" />
            {m.snapshot_back()}
          </Link>
        }
        title={current?.name ?? id}
        meta={
          current ? (
            <>
              <span>{snapshotOwnerLabel(current, user.id)}</span>
              <span aria-hidden="true">·</span>
              <span>
                {m.snapshots_current_version()}{' '}
                <span className="font-mono">{current.current_version_id ?? '—'}</span>
              </span>
            </>
          ) : undefined
        }
      />

      {snapshot.isPending && (
        <p className="py-8 text-sm text-muted-foreground">{m.snapshot_loading()}</p>
      )}
      {snapshot.error && (
        <p className="text-sm text-destructive" role="alert">
          {m.snapshot_load_error()}: <Problem error={snapshot.error} />
        </p>
      )}
      {versions.error && (
        <p className="text-sm text-destructive" role="alert">
          {m.snapshot_versions_load_error()}: <Problem error={versions.error} />
        </p>
      )}

      <Card>
        <CardHeader>
          <CardTitle>{m.snapshot_versions()}</CardTitle>
          <CardDescription>{m.snapshot_versions_description()}</CardDescription>
        </CardHeader>
        <CardContent className="px-0 pb-0">
          {versions.isPending && (
            <p className="px-5 pb-4 text-sm text-muted-foreground">
              {m.snapshot_versions_loading()}
            </p>
          )}
          {versions.data?.items.length === 0 && (
            <EmptyState
              icon={<Layers />}
              title={m.snapshot_versions_empty_title()}
              description={m.snapshot_versions_empty_description()}
              className="mx-5 mb-5 border-0 bg-muted/20"
            />
          )}
          {versions.data && versions.data.items.length > 0 && (
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead className="pl-5">{m.snapshot_version()}</TableHead>
                  <TableHead>{m.snapshot_state()}</TableHead>
                  <TableHead>{m.snapshot_source()}</TableHead>
                  <TableHead>{m.snapshot_created()}</TableHead>
                  <TableHead className="pr-5 text-right">{m.snapshot_build_log()}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {versions.data.items.map((version) => (
                  <TableRow key={version.id} data-testid={`version-${version.id}`}>
                    <TableCell className="pl-5">
                      <div className="flex items-center gap-2">
                        <span className="font-mono text-xs">{version.id}</span>
                        {versionIsCurrent(current, version.id) && (
                          <Badge
                            variant="secondary"
                            data-testid={`version-current-${version.id}`}
                          >
                            {m.common_current()}
                          </Badge>
                        )}
                      </div>
                    </TableCell>
                    <TableCell>
                      <StatusBadge
                        state={version.state}
                        data-testid={`version-state-${version.id}`}
                      />
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">
                      {sourceLabel(version.source)}
                      {version.cause ? ` · ${causeLabel(version.cause)}` : ''}
                    </TableCell>
                    <TableCell className="text-xs whitespace-nowrap text-muted-foreground">
                      {formatTimestamp(version.created_at)}
                    </TableCell>
                    <TableCell className="pr-5 text-right">
                      {version.build_exec_id ? (
                        <Link
                          to="/snapshots/$id"
                          params={{ id }}
                          search={{ version: version.id }}
                          data-testid={`build-log-link-${version.id}`}
                          className={buttonVariants({ variant: 'outline', size: 'sm' })}
                        >
                          <ScrollText />
                          {m.snapshot_build_log()}
                        </Link>
                      ) : (
                        <span className="text-xs text-muted-foreground">—</span>
                      )}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {selectedId && !selected && !versions.isPending && (
        <p className="text-sm text-muted-foreground">{m.snapshot_not_in_list()}</p>
      )}

      {selected && (
        <Card>
          <CardHeader>
            <CardTitle>{m.snapshot_build_log()}</CardTitle>
            <CardDescription>
              {selected.cause
                ? m.snapshot_build_log_meta_cause({
                    source: sourceLabel(selected.source),
                    state: stateLabel(selected.state),
                    cause: causeLabel(selected.cause),
                  })
                : m.snapshot_build_log_meta({
                    source: sourceLabel(selected.source),
                    state: stateLabel(selected.state),
                  })}
            </CardDescription>
          </CardHeader>
          <CardContent>
            {logMode !== 'none' && selected.build_exec_id ? (
              <BuildLog execId={selected.build_exec_id} mode={logMode} />
            ) : (
              <p className="text-sm text-muted-foreground">{m.snapshot_no_build_log()}</p>
            )}
          </CardContent>
        </Card>
      )}
    </div>
  )
}
