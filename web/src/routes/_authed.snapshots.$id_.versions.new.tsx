import { useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link, Navigate, useNavigate } from '@tanstack/react-router'
import { useState } from 'react'

import { api, type Snapshot, type SnapshotVersion } from '@/api'
import { PageHeader } from '@/components/page-header'
import { SnapshotVersionForm } from '@/components/snapshot-version-form'
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from '@/components/ui/breadcrumb'
import { useLocale } from '@/lib/i18n'
import { buildSnapshotVersionBody, formFieldsFromRequest } from '@/lib/snapshot-form'
import { snapshotIsWritable } from '@/lib/snapshots'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/snapshots/$id_/versions/new')({
  // from: the version whose request fills the form.
  validateSearch: (search: Record<string, unknown>): { from?: string } => ({
    from: typeof search.from === 'string' ? search.from : undefined,
  }),
  component: NewSnapshotVersionPage,
})

function NewSnapshotVersionPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const { id } = Route.useParams()
  const { from } = Route.useSearch()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  // One key per form mount: repeated submits cannot create a second version (api.md §2).
  const [idempotencyKey] = useState(() => crypto.randomUUID())

  const snapshot = useQuery({
    queryKey: ['snapshot', id],
    queryFn: () => api<Snapshot>('GET', `/snapshots/${id}`),
  })
  const source = useQuery({
    queryKey: ['snapshot-version', id, from],
    queryFn: () => api<SnapshotVersion>('GET', `/snapshots/${id}/versions/${from}`),
    enabled: from !== undefined,
  })
  const name = snapshot.data?.name ?? id

  return (
    <div className="flex max-w-3xl flex-col gap-6">
      <PageHeader
        above={
          <Breadcrumb aria-label={m.common_breadcrumb()}>
            <BreadcrumbList>
              <BreadcrumbItem>
                <BreadcrumbLink render={<Link to="/snapshots" />}>{m.nav_snapshots()}</BreadcrumbLink>
              </BreadcrumbItem>
              <BreadcrumbSeparator />
              <BreadcrumbItem>
                <BreadcrumbLink render={<Link to="/snapshots/$id" params={{ id }} />}>
                  {name}
                </BreadcrumbLink>
              </BreadcrumbItem>
              <BreadcrumbSeparator />
              <BreadcrumbItem>
                <BreadcrumbPage>{m.snapshot_new_version()}</BreadcrumbPage>
              </BreadcrumbItem>
            </BreadcrumbList>
          </Breadcrumb>
        }
        title={m.snapshot_new_version()}
        description={
          from ? m.version_new_from_description({ version: from }) : m.version_new_description()
        }
      />


      {/* A Platform Snapshot takes no versions from the console; send the URL back to it. */}
      {snapshot.data && !snapshotIsWritable(snapshot.data) && (
        <Navigate to="/snapshots/$id" params={{ id }} replace />
      )}
      {snapshotIsWritable(snapshot.data) && (from === undefined || source.data) && (
        <SnapshotVersionForm
          key={from ?? ''}
          initial={formFieldsFromRequest(source.data?.request)}
          onCancel={() => navigate({ to: '/snapshots/$id', params: { id } })}
          onSubmit={async (values) => {
            const created = await api<SnapshotVersion>(
              'POST',
              `/snapshots/${id}/versions`,
              buildSnapshotVersionBody(values),
              { 'Idempotency-Key': idempotencyKey },
            )
            await queryClient.invalidateQueries({ queryKey: ['snapshot-versions', id] })
            await queryClient.invalidateQueries({ queryKey: ['snapshot', id] })
            void navigate({ to: '/snapshots/$id', params: { id }, search: { version: created.id } })
          }}
        />
      )}
    </div>
  )
}
