import { useQuery } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'
import { ArrowLeft, Layers, ScrollText } from 'lucide-react'

import { api, type ListResponse, type Snapshot, type SnapshotVersion } from '@/api'
import { BuildLog } from '@/components/build-log'
import { EmptyState } from '@/components/empty-state'
import { PageHeader } from '@/components/page-header'
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
import {
  buildLogMode,
  isSnapshotVersionTerminal,
  snapshotOwnerLabel,
  snapshotVersionsPollInterval,
  versionIsCurrent,
} from '@/lib/snapshots'
import { formatTimestamp } from '@/lib/tokens'

export const Route = createFileRoute('/_authed/snapshots/$id')({
  validateSearch: (search: Record<string, unknown>): { version?: string } => ({
    version: typeof search.version === 'string' ? search.version : undefined,
  }),
  component: SnapshotDetailPage,
})

function SnapshotDetailPage() {
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
            Snapshots
          </Link>
        }
        title={current?.name ?? id}
        meta={
          current ? (
            <>
              <span>{snapshotOwnerLabel(current, user.id)}</span>
              <span aria-hidden="true">·</span>
              <span>
                current version{' '}
                <span className="font-mono">{current.current_version_id ?? '—'}</span>
              </span>
            </>
          ) : undefined
        }
      />

      {snapshot.isPending && (
        <p className="py-8 text-sm text-muted-foreground">Loading snapshot…</p>
      )}
      {snapshot.error && (
        <p className="text-sm text-destructive" role="alert">
          Could not load Snapshot: {snapshot.error.message}
        </p>
      )}
      {versions.error && (
        <p className="text-sm text-destructive" role="alert">
          Could not load versions: {versions.error.message}
        </p>
      )}

      <Card>
        <CardHeader>
          <CardTitle>Versions</CardTitle>
          <CardDescription>Every version of this Snapshot, newest first.</CardDescription>
        </CardHeader>
        <CardContent className="px-0 pb-0">
          {versions.isPending && (
            <p className="px-5 pb-4 text-sm text-muted-foreground">Loading versions…</p>
          )}
          {versions.data?.items.length === 0 && (
            <EmptyState
              icon={<Layers />}
              title="No versions yet"
              description="Importing a Snapshot by digest or building it creates the first version."
              className="mx-5 mb-5 border-0 bg-muted/20"
            />
          )}
          {versions.data && versions.data.items.length > 0 && (
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead className="pl-5">Version</TableHead>
                  <TableHead>State</TableHead>
                  <TableHead>Source</TableHead>
                  <TableHead>Created</TableHead>
                  <TableHead className="pr-5 text-right">Build log</TableHead>
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
                            current
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
                      {version.source}
                      {version.cause ? ` · ${version.cause}` : ''}
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
                          Build log
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
        <p className="text-sm text-muted-foreground">
          That version is not in this Snapshot's list.
        </p>
      )}

      {selected && (
        <Card>
          <CardHeader>
            <CardTitle>Build log</CardTitle>
            <CardDescription>
              {selected.source} · state {selected.state}
              {selected.cause ? ` · cause ${selected.cause}` : ''}
            </CardDescription>
          </CardHeader>
          <CardContent>
            {logMode !== 'none' && selected.build_exec_id ? (
              <BuildLog execId={selected.build_exec_id} mode={logMode} />
            ) : (
              <p className="text-sm text-muted-foreground">
                This version has no build log; imports do not run an Exec.
              </p>
            )}
          </CardContent>
        </Card>
      )}
    </div>
  )
}
