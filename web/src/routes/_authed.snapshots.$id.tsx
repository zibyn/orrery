import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { Ban, Box, Copy, Layers, Pin, Plus, ScrollText, Trash } from 'lucide-react'
import { useState } from 'react'

import { api, listAll, type Snapshot, type SnapshotVersion } from '@/api'
import { BuildLog } from '@/components/build-log'
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
import { Spinner } from '@/components/ui/spinner'
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
  snapshotIsWritable,
  snapshotOwnerLabel,
  snapshotVersionsPollInterval,
  versionActions,
  type VersionAction,
  versionIsCurrent,
} from '@/lib/snapshots'
import { formatTimestamp } from '@/lib/tokens'
import { cn } from '@/lib/utils'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/snapshots/$id')({
  validateSearch: (search: Record<string, unknown>): { version?: string } => ({
    version: typeof search.version === 'string' ? search.version : undefined,
  }),
  component: SnapshotDetailPage,
})

function SnapshotDetailPage() {
  const [confirmOpen, setConfirmOpen] = useState(false)
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const { id } = Route.useParams()
  const { user } = Route.useRouteContext()
  const { version: selectedId } = Route.useSearch()
  const navigate = useNavigate()
  const queryClient = useQueryClient()

  const versions = useQuery({
    queryKey: ['snapshot-versions', id],
    queryFn: () => listAll<SnapshotVersion>(`/snapshots/${id}/versions`),
    refetchInterval: (query) => snapshotVersionsPollInterval(query.state.data),
  })
  const listed = versions.data?.find((item) => item.id === selectedId)
  // A ?version= the list does not hold (it may have been created since) is read on its own.
  const unlisted = useQuery({
    queryKey: ['snapshot-version', id, selectedId],
    queryFn: () => api<SnapshotVersion>('GET', `/snapshots/${id}/versions/${selectedId}`),
    enabled: selectedId !== undefined && versions.isSuccess && !listed,
    retry: false,
    refetchInterval: (query) => snapshotVersionsPollInterval(query.state.data && [query.state.data]),
  })
  const selected = listed ?? unlisted.data
  const snapshot = useQuery({
    queryKey: ['snapshot', id],
    queryFn: () => api<Snapshot>('GET', `/snapshots/${id}`),
    // The current pointer moves when a build becomes ready.
    refetchInterval: [...(versions.data ?? []), ...(selected ? [selected] : [])].some(
      (version) => !isSnapshotVersionTerminal(version.state),
    )
      ? 2000
      : false,
  })

  const current = snapshot.data

  const remove = useMutation({
    mutationFn: () => api<Snapshot>('DELETE', `/snapshots/${id}`),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['snapshots'] })
      void navigate({ to: '/snapshots' })
    },
  })
  const versionAction = useMutation({
    mutationFn: ({ verb, version }: { verb: VersionAction; version: string }) => {
      if (verb === 'cancel') return api('POST', `/snapshots/${id}/versions/${version}/cancel`)
      if (verb === 'setCurrent') return api('PUT', `/snapshots/${id}/current`, { version })
      return api('DELETE', `/snapshots/${id}/versions/${version}`)
    },
    onSuccess: () =>
      Promise.all([
        queryClient.invalidateQueries({ queryKey: ['snapshot', id] }),
        queryClient.invalidateQueries({ queryKey: ['snapshot-versions', id] }),
      ]),
  })
  const logMode = selected ? buildLogMode(selected) : 'none'

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        above={
          <Breadcrumb aria-label={m.common_breadcrumb()}>
            <BreadcrumbList>
              <BreadcrumbItem>
                <BreadcrumbLink render={<Link to="/snapshots" />}>{m.nav_snapshots()}</BreadcrumbLink>
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
            <>
              <span>{snapshotOwnerLabel(current, user.id)}</span>
              <span aria-hidden="true">·</span>
              <span>
                {m.snapshots_current_version()}{' '}
                {current.current_version_id ? (
                  <span className="font-mono">{current.current_version_id}</span>
                ) : (
                  m.snapshot_no_current()
                )}
              </span>
            </>
          ) : undefined
        }
        actions={
          current && (
            <>
              {current.current_version_id && (
                <Link
                  to="/sandboxes/new"
                  search={{ snapshot: current.name }}
                  className={cn(buttonVariants())}
                >
                  <Box data-icon="inline-start" />
                  {m.snapshot_new_sandbox()}
                </Link>
              )}
              {snapshotIsWritable(current) && (
                <>
                  <Link
                    to="/snapshots/$id/versions/new"
                    params={{ id }}
                    search={{ from: current.current_version_id ?? undefined }}
                    className={cn(buttonVariants({ variant: 'outline' }))}
                  >
                    <Plus data-icon="inline-start" />
                    {m.snapshot_new_version()}
                  </Link>
                  <AlertDialog open={confirmOpen} onOpenChange={setConfirmOpen}>
                    <AlertDialogTrigger
                      render={
                        <Button
                          variant="outline"
                          disabled={remove.isPending}
                          data-testid="snapshot-delete"
                        />
                      }
                    >
                      <Trash data-icon="inline-start" />
                      {remove.isPending ? m.snapshot_deleting() : m.common_delete()}
                    </AlertDialogTrigger>
                    <AlertDialogContent>
                      <AlertDialogHeader>
                        <AlertDialogTitle>
                          {m.snapshot_delete_confirm({ name: current.name })}
                        </AlertDialogTitle>
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
                </>
              )}
            </>
          )
        }
      />



      {snapshot.isPending && (
        <div className="flex items-center gap-2 py-8 text-sm text-muted-foreground">
          <Spinner />
          {m.snapshot_loading()}
        </div>
      )}

      <Card>
        <CardHeader>
          <CardTitle>{m.snapshot_versions()}</CardTitle>
          <CardDescription>{m.snapshot_versions_description()}</CardDescription>
        </CardHeader>
        <CardContent className="px-0 pb-0">
          {versions.isPending && (
            <div className="flex items-center gap-2 px-5 pb-4 text-sm text-muted-foreground">
              <Spinner />
              {m.snapshot_versions_loading()}
            </div>
          )}
          {versions.data?.length === 0 && (
            <Empty>
              <EmptyHeader>
                <EmptyMedia variant="icon">
                  <Layers />
                </EmptyMedia>
                <EmptyTitle>{m.snapshot_versions_empty_title()}</EmptyTitle>
                <EmptyDescription>{m.snapshot_versions_empty_description()}</EmptyDescription>
              </EmptyHeader>
            </Empty>
          )}
          {versions.data && versions.data.length > 0 && (
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead className="pl-5">{m.snapshot_version()}</TableHead>
                  <TableHead>{m.snapshot_state()}</TableHead>
                  <TableHead>{m.snapshot_source()}</TableHead>
                  <TableHead>{m.snapshot_created()}</TableHead>
                  <TableHead className="text-right">{m.snapshot_build_log()}</TableHead>
                  <TableHead className="pr-5 text-right">{m.snapshot_version_actions()}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {versions.data.map((version) => (
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
                    <TableCell className="text-right">
                      {version.build_exec_id ? (
                        <Link
                          to="/snapshots/$id"
                          params={{ id }}
                          search={{ version: version.id }}
                          data-testid={`build-log-link-${version.id}`}
                          className={cn(buttonVariants({ variant: 'outline', size: 'sm' }))}
                        >
                          <ScrollText data-icon="inline-start" />
                          {m.snapshot_build_log()}
                        </Link>
                      ) : (
                        <span className="text-xs text-muted-foreground">—</span>
                      )}
                    </TableCell>
                    <TableCell className="pr-5 text-right">
                      <VersionActions
                        actions={versionActions(current, version)}
                        snapshotId={id}
                        versionId={version.id}
                        newFrom={snapshotIsWritable(current) && version.request !== undefined}
                        disabled={versionAction.isPending}
                        onAction={(verb) => versionAction.mutate({ verb, version: version.id })}
                      />
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {selectedId && !selected && unlisted.isError && (
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

function VersionActions({
  actions,
  snapshotId,
  versionId,
  newFrom,
  disabled,
  onAction,
}: {
  actions: ReturnType<typeof versionActions>
  snapshotId: string
  versionId: string
  // The version kept its request, so a new version can start from it.
  newFrom: boolean
  disabled: boolean
  onAction: (verb: VersionAction) => void
}) {
  const [confirmOpen, setConfirmOpen] = useState(false)
  return (
    <div className="flex justify-end gap-2">
      {newFrom && (
        <Link
          to="/snapshots/$id/versions/new"
          params={{ id: snapshotId }}
          search={{ from: versionId }}
          data-testid={`version-new-from-${versionId}`}
          className={cn(buttonVariants({ variant: 'outline', size: 'sm' }))}
        >
          <Copy data-icon="inline-start" />
          {m.snapshot_version_new_from()}
        </Link>
      )}
      {actions.cancel && (
        <Button
          variant="outline"
          size="sm"
          disabled={disabled}
          data-testid={`version-cancel-${versionId}`}
          onClick={() => onAction('cancel')}
        >
          <Ban data-icon="inline-start" />
          {m.common_cancel()}
        </Button>
      )}
      {actions.setCurrent && (
        <Button
          variant="outline"
          size="sm"
          disabled={disabled}
          data-testid={`version-set-current-${versionId}`}
          onClick={() => onAction('setCurrent')}
        >
          <Pin data-icon="inline-start" />
          {m.snapshot_version_set_current()}
        </Button>
      )}
      {actions.delete && (
        <AlertDialog open={confirmOpen} onOpenChange={setConfirmOpen}>
          <AlertDialogTrigger
            render={
              <Button
                variant="ghost"
                size="sm"
                disabled={disabled}
                data-testid={`version-delete-${versionId}`}
              />
            }
          >
            <Trash data-icon="inline-start" />
            {m.common_delete()}
          </AlertDialogTrigger>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>
                {m.snapshot_version_delete_confirm({ id: versionId.slice(0, 8) })}
              </AlertDialogTitle>
              <AlertDialogDescription>{m.common_irreversible()}</AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel>{m.common_cancel()}</AlertDialogCancel>
              <AlertDialogAction
                variant="destructive"
                data-testid="confirm-action"
                onClick={() => {
                  setConfirmOpen(false)
                  onAction('delete')
                }}
              >
                {m.common_delete()}
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      )}
    </div>
  )
}
