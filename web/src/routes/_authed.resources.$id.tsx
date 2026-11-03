import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { Download, File, Folder, FolderOpen, Link2, Package, Pin, Trash } from 'lucide-react'

import {
  api,
  type ListResponse,
  type ManifestEntry,
  type Resource,
  type ResourceVersion,
} from '@/api'
import { Breadcrumbs } from '@/components/breadcrumbs'
import { EmptyState } from '@/components/empty-state'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { StatusBadge } from '@/components/status-badge'
import { Badge } from '@/components/ui/badge'
import { Button, buttonVariants } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { formatBytes } from '@/lib/files'
import { useLocale } from '@/lib/i18n'
import { causeLabel } from '@/lib/labels'
import { parseProvenance, resourceVersionsPollInterval } from '@/lib/resources'
import { formatTimestamp } from '@/lib/tokens'
import { m } from '@/paraglide/messages'

// The manifest may hold up to 1,000,000 entries (api.md §3).
// shortcut: renders only the first entries, add paging if users browse huge trees here.
const MANIFEST_SHOWN = 1000

export const Route = createFileRoute('/_authed/resources/$id')({
  validateSearch: (search: Record<string, unknown>): { version?: string } => ({
    version: typeof search.version === 'string' ? search.version : undefined,
  }),
  component: ResourceDetailPage,
})

function ResourceDetailPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const { id } = Route.useParams()
  const { version: selectedId } = Route.useSearch()
  const navigate = useNavigate()
  const queryClient = useQueryClient()

  const versions = useQuery({
    queryKey: ['resource-versions', id],
    queryFn: async () => {
      const list = await api<ListResponse<ResourceVersion>>('GET', `/resources/${id}/versions?limit=200`)
      // The current pointer moves when a publish becomes ready, so every
      // version poll re-reads the Resource too, including the one that sees ready.
      void queryClient.invalidateQueries({ queryKey: ['resource', id] })
      return list
    },
    refetchInterval: (query) => resourceVersionsPollInterval(query.state.data?.items),
  })
  const resource = useQuery({
    queryKey: ['resource', id],
    queryFn: () => api<Resource>('GET', `/resources/${id}`),
  })
  const current = resource.data

  const remove = useMutation({
    mutationFn: () => api<Resource>('DELETE', `/resources/${id}`),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['resources'] })
      void navigate({ to: '/resources' })
    },
  })
  // The server decides which versions may become current; its conflict is shown as is.
  const setCurrent = useMutation({
    mutationFn: (version: string) => api('PUT', `/resources/${id}/current`, { version }),
    onSuccess: () =>
      Promise.all([
        queryClient.invalidateQueries({ queryKey: ['resource', id] }),
        queryClient.invalidateQueries({ queryKey: ['resources'] }),
      ]),
  })
  const selected = versions.data?.items.find((item) => item.id === selectedId)

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        above={
          <Breadcrumbs
            items={[
              { label: m.nav_resources(), to: '/resources' },
              { label: current?.name ?? id },
            ]}
          />
        }
        title={current?.name ?? id}
        meta={
          current ? (
            <span>
              {m.resources_current_version()}{' '}
              <span className="font-mono">{current.current_version_id ?? '—'}</span>
            </span>
          ) : undefined
        }
        actions={
          current &&
          !current.deleted_at && (
            <Button
              variant="outline"
              disabled={remove.isPending}
              onClick={() => {
                if (window.confirm(m.resource_delete_confirm({ name: current.name }))) remove.mutate()
              }}
            >
              <Trash />
              {remove.isPending ? m.snapshot_deleting() : m.common_delete()}
            </Button>
          )
        }
      />

      {remove.error && (
        <p className="text-sm text-destructive" role="alert">
          <Problem error={remove.error} />
        </p>
      )}
      {setCurrent.error && (
        <p className="text-sm text-destructive" role="alert" data-testid="version-action-error">
          <Problem error={setCurrent.error} />
        </p>
      )}
      {resource.isPending && (
        <p className="py-8 text-sm text-muted-foreground">{m.common_loading()}</p>
      )}
      {resource.error && (
        <p className="text-sm text-destructive" role="alert">
          {m.resource_load_error()}: <Problem error={resource.error} />
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
          <CardDescription>{m.resource_versions_description()}</CardDescription>
        </CardHeader>
        <CardContent className="px-0 pb-0">
          {versions.isPending && (
            <p className="px-5 pb-4 text-sm text-muted-foreground">{m.common_loading()}</p>
          )}
          {versions.data?.items.length === 0 && (
            <EmptyState
              icon={<Package />}
              title={m.snapshot_versions_empty_title()}
              description={m.resource_versions_empty_description()}
              className="mx-5 mb-5 border-0 bg-muted/20"
            />
          )}
          {versions.data && versions.data.items.length > 0 && (
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead className="pl-5">{m.snapshot_version()}</TableHead>
                  <TableHead>{m.snapshot_state()}</TableHead>
                  <TableHead>{m.resource_size()}</TableHead>
                  <TableHead>{m.snapshot_source()}</TableHead>
                  <TableHead>{m.snapshot_created()}</TableHead>
                  <TableHead>{m.resource_expires()}</TableHead>
                  <TableHead className="pr-5 text-right">{m.snapshot_version_actions()}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {versions.data.items.map((version) => {
                  const isCurrent = current?.current_version_id === version.id
                  return (
                    <TableRow key={version.id} data-testid={`version-${version.id}`}>
                      <TableCell className="pl-5">
                        <div className="flex items-center gap-2">
                          <span className="font-mono text-xs">{version.id}</span>
                          {isCurrent && (
                            <Badge variant="secondary" data-testid={`version-current-${version.id}`}>
                              {m.common_current()}
                            </Badge>
                          )}
                        </div>
                      </TableCell>
                      <TableCell>
                        <div className="flex flex-col gap-0.5">
                          <StatusBadge
                            state={version.state}
                            data-testid={`version-state-${version.id}`}
                          />
                          {version.cause && (
                            <span className="text-xs text-muted-foreground">
                              {causeLabel(version.cause)}
                            </span>
                          )}
                        </div>
                      </TableCell>
                      <TableCell className="text-xs whitespace-nowrap text-muted-foreground">
                        {version.state === 'pending' ? '—' : formatBytes(version.size)}
                      </TableCell>
                      <TableCell className="text-xs text-muted-foreground">
                        <ProvenanceCell provenance={version.provenance} />
                      </TableCell>
                      <TableCell className="text-xs whitespace-nowrap text-muted-foreground">
                        {formatTimestamp(version.created_at)}
                      </TableCell>
                      <TableCell className="text-xs whitespace-nowrap text-muted-foreground">
                        {version.expires_at ? formatTimestamp(version.expires_at) : '—'}
                      </TableCell>
                      <TableCell className="pr-5 text-right">
                        <div className="flex justify-end gap-2">
                          {version.state === 'ready' && (
                            <Link
                              to="/resources/$id"
                              params={{ id }}
                              search={{ version: version.id }}
                              data-testid={`version-files-${version.id}`}
                              className={buttonVariants({ variant: 'outline', size: 'sm' })}
                            >
                              <FolderOpen />
                              {m.files_title()}
                            </Link>
                          )}
                          {current && !current.deleted_at && !isCurrent && (
                            <Button
                              variant="outline"
                              size="sm"
                              disabled={setCurrent.isPending}
                              data-testid={`version-set-current-${version.id}`}
                              onClick={() => setCurrent.mutate(version.id)}
                            >
                              <Pin />
                              {m.snapshot_version_set_current()}
                            </Button>
                          )}
                        </div>
                      </TableCell>
                    </TableRow>
                  )
                })}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {selectedId && !selected && !versions.isPending && (
        <p className="text-sm text-muted-foreground">{m.resource_not_in_list()}</p>
      )}
      {selected && <VersionFiles resourceId={id} versionId={selected.id} />}
    </div>
  )
}

function ProvenanceCell({ provenance }: { provenance: Record<string, unknown> }) {
  const source = parseProvenance(provenance)
  if (source.source === 'upload') return <>{m.resource_source_upload()}</>
  return (
    <span className="inline-flex items-center gap-1">
      <Link2 className="size-3" />
      <Link
        to="/workspaces/$id"
        params={{ id: source.workspaceId }}
        className="font-mono hover:underline"
      >
        {source.workspaceId.slice(0, 8)}
      </Link>
      <span className="font-mono">/{source.path}</span>
    </span>
  )
}

function VersionFiles({ resourceId, versionId }: { resourceId: string; versionId: string }) {
  const base = `/resources/${resourceId}/versions/${versionId}`
  const manifest = useQuery({
    queryKey: ['resource-manifest', resourceId, versionId],
    queryFn: () => api<{ entries: ManifestEntry[] }>('GET', `${base}/manifest`),
    staleTime: Infinity, // a version's manifest never changes
  })
  const download = useMutation({
    mutationFn: async ({ sha256, tab }: { sha256: string; tab: Window | null }) => {
      try {
        const { urls } = await api<{ urls: string[] }>('POST', `${base}/downloads`, {
          sha256: [sha256],
        })
        if (!urls[0]) throw new Error(m.resource_download_missing())
        if (tab) {
          tab.opener = null // the object store page must not reach the console
          tab.location.href = urls[0]
        } else window.location.assign(urls[0])
      } catch (err) {
        tab?.close()
        throw err
      }
    },
  })
  const entries = manifest.data?.entries ?? []

  return (
    <Card>
      <CardHeader>
        <CardTitle>{m.files_title()}</CardTitle>
        <CardDescription className="font-mono">{versionId}</CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        {manifest.isPending && <p className="text-muted-foreground">{m.common_loading()}</p>}
        {manifest.error && (
          <p className="text-destructive" role="alert">
            <Problem error={manifest.error} />
          </p>
        )}
        {download.error && (
          <p className="text-destructive" role="alert">
            <Problem error={download.error} />
          </p>
        )}
        {manifest.data && (
          <ul className="divide-y divide-border overflow-hidden rounded-lg border border-border">
            {entries.length === 0 && (
              <li className="px-3 py-6 text-center text-xs text-muted-foreground">
                {m.files_empty()}
              </li>
            )}
            {entries.slice(0, MANIFEST_SHOWN).map((entry) => (
              <li
                key={entry.path}
                className="flex min-w-0 items-center gap-2.5 px-3 py-2"
                data-testid={`manifest-${entry.path}`}
              >
                {entry.type === 'dir' ? (
                  <Folder className="size-4 shrink-0 text-muted-foreground" />
                ) : (
                  <File className="size-4 shrink-0 text-muted-foreground" />
                )}
                <div className="flex min-w-0 flex-1 flex-col">
                  <span className="truncate font-mono text-sm">
                    {entry.path}
                    {entry.type === 'dir' ? '/' : ''}
                    {entry.type === 'symlink' ? ` → ${entry.target ?? ''}` : ''}
                  </span>
                  <span className="text-xs text-muted-foreground">
                    {entry.type === 'file'
                      ? formatBytes(entry.size ?? 0) + (entry.executable ? ' · +x' : '')
                      : entry.type === 'dir'
                        ? m.files_directory()
                        : m.resource_symlink()}
                  </span>
                </div>
                {entry.type === 'file' && entry.sha256 && (
                  <Button
                    variant="ghost"
                    size="xs"
                    disabled={download.isPending}
                    onClick={() =>
                      // Open the tab inside the click so popup blockers allow it;
                      // the presigned URL arrives after the request.
                      download.mutate({ sha256: entry.sha256!, tab: window.open('', '_blank') })
                    }
                  >
                    <Download />
                    {m.files_download()}
                  </Button>
                )}
              </li>
            ))}
          </ul>
        )}
        {entries.length > MANIFEST_SHOWN && (
          <p className="text-xs text-muted-foreground">
            {m.resource_manifest_truncated({ shown: MANIFEST_SHOWN, total: entries.length })}
          </p>
        )}
      </CardContent>
    </Card>
  )
}
