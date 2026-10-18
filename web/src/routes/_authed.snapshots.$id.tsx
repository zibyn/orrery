import { useQuery } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'

import { api, type ListResponse, type Snapshot, type SnapshotVersion } from '@/api'
import { BuildLog } from '@/components/build-log'
import { buttonVariants } from '@/components/ui/button'
import {
  buildLogMode,
  isSnapshotVersionTerminal,
  snapshotOwnerLabel,
  snapshotVersionsPollInterval,
  versionIsCurrent,
} from '@/lib/snapshots'

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
      <div className="flex flex-col gap-1">
        <Link to="/snapshots" className="text-sm text-muted-foreground hover:underline">
          ← Snapshots
        </Link>
        <h1 className="text-2xl font-semibold">{current?.name ?? id}</h1>
        {current && (
          <p className="text-sm text-muted-foreground">
            {snapshotOwnerLabel(current, user.id)} · current version{' '}
            <span className="font-mono text-xs">{current.current_version_id ?? '—'}</span>
          </p>
        )}
      </div>

      {snapshot.isPending && <p className="text-sm text-muted-foreground">Loading…</p>}
      {snapshot.error && (
        <p className="text-sm text-destructive">Could not load Snapshot: {snapshot.error.message}</p>
      )}
      {versions.isPending && <p className="text-sm text-muted-foreground">Loading versions…</p>}
      {versions.error && (
        <p className="text-sm text-destructive">
          Could not load versions: {versions.error.message}
        </p>
      )}

      {versions.data?.items.length === 0 && (
        <p className="text-sm text-muted-foreground">This Snapshot has no versions yet.</p>
      )}

      {versions.data && versions.data.items.length > 0 && (
        <ul className="divide-y divide-border rounded-md border border-border">
          {versions.data.items.map((version) => (
            <li
              key={version.id}
              className="flex flex-wrap items-center justify-between gap-3 p-4"
              data-testid={`version-${version.id}`}
            >
              <div className="flex flex-col gap-1">
                <span className="flex items-center gap-2">
                  <span className="font-mono text-sm">{version.id}</span>
                  {versionIsCurrent(current, version.id) && (
                    <span
                      data-testid={`version-current-${version.id}`}
                      className="rounded-full border border-border bg-muted px-2 py-0.5 text-xs font-medium"
                    >
                      current
                    </span>
                  )}
                </span>
                <span className="text-xs text-muted-foreground">
                  {version.source} · created {version.created_at ?? '—'}
                  {version.cause ? ` · cause ${version.cause}` : ''}
                </span>
              </div>
              <span className="flex items-center gap-2">
                <span
                  className="rounded-full border border-border px-2 py-0.5 text-xs font-medium"
                  data-testid={`version-state-${version.id}`}
                >
                  {version.state}
                </span>
                {version.build_exec_id && (
                  <Link
                    to="/snapshots/$id"
                    params={{ id }}
                    search={{ version: version.id }}
                    data-testid={`build-log-link-${version.id}`}
                    className={buttonVariants({ variant: 'outline', size: 'sm' })}
                  >
                    Build log
                  </Link>
                )}
              </span>
            </li>
          ))}
        </ul>
      )}

      {selectedId && !selected && !versions.isPending && (
        <p className="text-sm text-muted-foreground">
          That version is not in this Snapshot's list.
        </p>
      )}

      {selected && (
        <section className="flex flex-col gap-3">
          <div className="flex flex-col gap-1">
            <h2 className="text-lg font-semibold">Build log</h2>
            <p className="text-xs text-muted-foreground">
              {selected.source} · state {selected.state}
              {selected.cause ? ` · cause ${selected.cause}` : ''}
            </p>
          </div>
          {logMode !== 'none' && selected.build_exec_id ? (
            <BuildLog execId={selected.build_exec_id} mode={logMode} />
          ) : (
            <p className="text-sm text-muted-foreground">
              This version has no build log; imports do not run an Exec.
            </p>
          )}
        </section>
      )}
    </div>
  )
}
