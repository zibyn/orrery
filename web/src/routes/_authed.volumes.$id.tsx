import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { Plus, Trash } from 'lucide-react'
import { type ReactNode, useState } from 'react'

import { api, type Volume } from '@/api'
import { PageHeader } from '@/components/page-header'
import { SandboxFiles } from '@/components/sandbox-files'
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
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from '@/components/ui/breadcrumb'
import { Button, buttonVariants } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Spinner } from '@/components/ui/spinner'
import { formatBytes } from '@/lib/files'
import { useLocale } from '@/lib/i18n'
import { formatTimestamp } from '@/lib/tokens'
import { cn } from '@/lib/utils'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/volumes/$id')({
  component: VolumeDetailPage,
})

function Detail({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex min-w-0 flex-col gap-0.5">
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="min-w-0 text-sm">{children}</dd>
    </div>
  )
}

function VolumeDetailPage() {
  const [confirmOpen, setConfirmOpen] = useState(false)
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const { id } = Route.useParams()
  const navigate = useNavigate()
  const queryClient = useQueryClient()

  const volume = useQuery({
    queryKey: ['volume', id],
    queryFn: () => api<Volume>('GET', `/volumes/${id}`),
  })
  const remove = useMutation({
    mutationFn: () => api<Volume>('DELETE', `/volumes/${id}`),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['volumes'] })
      void navigate({ to: '/volumes' })
    },
  })
  const current = volume.data
  const live = current?.state === 'ready'

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        above={
          <Breadcrumb aria-label={m.common_breadcrumb()}>
            <BreadcrumbList>
              <BreadcrumbItem>
                <BreadcrumbLink render={<Link to="/volumes" />}>{m.nav_volumes()}</BreadcrumbLink>
              </BreadcrumbItem>
              <BreadcrumbSeparator />
              <BreadcrumbItem>
                <BreadcrumbPage>{current?.name ?? id}</BreadcrumbPage>
              </BreadcrumbItem>
            </BreadcrumbList>
          </Breadcrumb>
        }
        title={current?.name ?? id}
        actions={
          current &&
          live && (
            <>
              <Link
                to="/sandboxes/new"
                search={{ volume: current.id }}
                className={cn(buttonVariants({ variant: 'outline' }))}
              >
                <Plus data-icon="inline-start" />
                {m.volume_new_sandbox()}
              </Link>
              <AlertDialog open={confirmOpen} onOpenChange={setConfirmOpen}>
                <AlertDialogTrigger
                  render={
                    <Button
                      variant="outline"
                      disabled={remove.isPending || current.held_by !== null}
                      title={current.held_by ? m.volume_delete_held() : undefined}
                    />
                  }
                >
                  <Trash data-icon="inline-start" />
                  {m.common_delete()}
                </AlertDialogTrigger>
                <AlertDialogContent>
                  <AlertDialogHeader>
                    <AlertDialogTitle>{m.volume_delete_confirm({ name: current.name })}</AlertDialogTitle>
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
          )
        }
      />

      {volume.isPending && (
        <div className="flex items-center gap-2 py-8 text-sm text-muted-foreground">
          <Spinner />
          {m.common_loading()}
        </div>
      )}

      {current && (
        <Card>
          <CardContent>
            <dl className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
              <Detail label={m.sandbox_state()}>
                <StatusBadge state={current.state} data-testid="volume-state" />
              </Detail>
              <Detail label={m.volume_held_by()}>
                {current.held_by ? (
                  <Link
                    to="/sandboxes/$id"
                    params={{ id: current.held_by }}
                    className="font-mono text-xs hover:underline"
                    data-testid="volume-holder"
                  >
                    {current.held_by}
                  </Link>
                ) : (
                  '—'
                )}
              </Detail>
              <Detail label={m.volume_committed()}>
                {current.committed_at ? formatTimestamp(current.committed_at) : m.volume_never_committed()}
              </Detail>
              <Detail label={m.volume_size()}>{formatBytes(current.size)}</Detail>
              <Detail label={m.snapshot_created()}>{formatTimestamp(current.created_at)}</Detail>
            </dl>
            {live && current.held_by && (
              <p className="mt-4 text-xs text-muted-foreground">{m.volume_delete_held()}</p>
            )}
          </CardContent>
        </Card>
      )}

      {current && live && (
        <div className="flex flex-col gap-2">
          <p className="text-sm text-muted-foreground" data-testid="volume-files-as-of">
            {current.pending_migration
              ? m.volume_pending_migration()
              : current.committed_at
                ? m.volume_files_as_of({ time: formatTimestamp(current.committed_at) })
                : m.volume_files_never()}
          </p>
          <SandboxFiles source={`/volumes/${id}`} readOnly />
        </div>
      )}
    </div>
  )
}
