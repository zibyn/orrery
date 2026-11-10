import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { Download, File, Folder, FolderOpen, Link2, Package, Pin, Trash } from 'lucide-react'
import { useState } from 'react'

import {
  api,
  type ListResponse,
  type ManifestEntry,
  type Resource,
  type ResourceVersion,
} from '@/api'
import { PageHeader } from '@/components/page-header'
import { StatusBadge } from '@/components/status-badge'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from '@/components/ui/alert-dialog'
import { Badge } from '@/components/ui/badge'
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from '@/components/ui/breadcrumb'
import { Button, buttonVariants } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
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
import { cn } from '@/lib/utils'
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
  const [confirmOpen, setConfirmOpen] = useState(false)
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
          <Breadcrumb aria-label={m.common_breadcrumb()}>
            <BreadcrumbList>
              <BreadcrumbItem>
                <BreadcrumbLink render={<Link to="/resources" />}>{m.nav_resources()}</BreadcrumbLink>
              </BreadcrumbItem>
              <BreadcrumbSeparator />
              <BreadcrumbItem>
                <BreadcrumbPage>{current?.name ?? id}</BreadcrumbPage>
              </BreadcrumbItem>
            </BreadcrumbList>
          </Breadcrumb>
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
            <AlertDialog open={confirmOpen} onOpenChange={setConfirmOpen}>
              <AlertDialogTrigger render={<Button variant="outline" disabled={remove.isPending} />}>
                <Trash data-icon="inline-start" />
                {remove.isPending ? m.snapshot_deleting() : m.common_delete()}
              </AlertDialogTrigger>
              <AlertDialogContent>
                <AlertDialogHeader>
                  <AlertDialogTitle>{m.resource_delete_confirm({ name: current.name })}</AlertDialogTitle>
                  <AlertDialogDescription>{m.common_irreversible()}</AlertDialogDescription>
                </AlertDialogHeader>
                <AlertDialogFooter>
                  <AlertDialogCancel>{m.common_cancel()}</AlertDialogCancel>
                  <AlertDialogAction
                    variant="destructive"
                    data-testid="confirm-action"
                    onClick={() => {
                      setConfirmOpen(false)
                      remove.mutate()
                    }}
                  >
                    {m.common_delete()}
                  </AlertDialogAction>
                </AlertDialogFooter>
              </AlertDialogContent>
            </AlertDialog>
          )
        }
      />

      {resource.isPending && (
        <div className="flex items-center gap-2 py-8 text-sm text-muted-foreground">
          <Spinner />
          {m.common_loading()}
        </div>
      )}

      <Card>
        <CardHeader>
          <CardTitle>{m.snapshot_versions()}</CardTitle>
          <CardDescription>{m.resource_versions_description()}</CardDescription>
        </CardHeader>
        <CardContent className="px-0 pb-0">
          {versions.isPending && (
            <div className="flex items-center gap-2 px-5 pb-4 text-sm text-muted-foreground">
              <Spinner />
              {m.common_loading()}
            </div>
          )}
          {versions.data?.items.length === 0 && (
            <Empty>
              <EmptyHeader>
                <EmptyMedia variant="icon">
                  <Package />
                </EmptyMedia>
                <EmptyTitle>{m.snapshot_versions_empty_title()}</EmptyTitle>
                <EmptyDescription>{m.resource_versions_empty_description()}</EmptyDescription>
              </EmptyHeader>
            </Empty>
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
                              className={cn(buttonVariants({ variant: 'outline', size: 'sm' }))}
                            >
                              <FolderOpen data-icon="inline-start" />
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
                              <Pin data-icon="inline-start" />
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
    mutationFn: async (entry: ManifestEntry) => {
      const { urls } = await api<{ urls: string[] }>('POST', `${base}/downloads`, {
        sha256: [entry.sha256],
        filename: entry.path.split('/').pop(),
      })
      if (!urls[0]) throw new Error(m.resource_download_missing())
      // The URL is served as an attachment, so the console page stays put.
      window.location.assign(urls[0])
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
        {manifest.isPending && (
          <div className="flex items-center gap-2 text-sm text-muted-foreground">
            <Spinner />
            {m.common_loading()}
          </div>
        )}
        {manifest.data && entries.length === 0 && (
          <Empty className="border">
            <EmptyHeader>
              <EmptyTitle>{m.files_empty()}</EmptyTitle>
            </EmptyHeader>
          </Empty>
        )}
        {entries.length > 0 && (
          <ItemGroup className="gap-2">
            {entries.slice(0, MANIFEST_SHOWN).map((entry) => (
              <Item
                key={entry.path}
                variant="outline"
                size="sm"
                role="listitem"
                data-testid={`manifest-${entry.path}`}
              >
                <ItemMedia variant="icon">
                  {entry.type === 'dir' ? <Folder /> : <File />}
                </ItemMedia>
                <ItemContent className="min-w-0">
                  <ItemTitle className="font-mono">
                    {entry.path}
                    {entry.type === 'dir' ? '/' : ''}
                    {entry.type === 'symlink' ? ` → ${entry.target ?? ''}` : ''}
                  </ItemTitle>
                  <ItemDescription>
                    {entry.type === 'file'
                      ? formatBytes(entry.size ?? 0) + (entry.executable ? ' · +x' : '')
                      : entry.type === 'dir'
                        ? m.files_directory()
                        : m.resource_symlink()}
                  </ItemDescription>
                </ItemContent>
                {entry.type === 'file' && entry.sha256 && (
                  <ItemActions>
                    <Button
                      variant="ghost"
                      size="xs"
                      disabled={download.isPending}
                      onClick={() => download.mutate(entry)}
                    >
                      <Download data-icon="inline-start" />
                      {m.files_download()}
                    </Button>
                  </ItemActions>
                )}
              </Item>
            ))}
          </ItemGroup>
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
